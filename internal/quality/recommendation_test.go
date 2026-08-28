package quality

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"mihomo-guardian/internal/config"
)

func recommendationTestTarget() config.QualityTarget {
	return config.QualityTarget{
		ID: "primary", SourceGroup: "MAIN", Provider: "main-provider", Scope: "locked", LockKey: "main",
	}
}

func recommendationTestValue(target config.QualityTarget, node, ip string, score, baseline int, now time.Time) Recommendation {
	return Recommendation{
		Target: target.ID, SourceGroup: target.SourceGroup, Provider: target.Provider, Node: node,
		Identity:   NodeKey{Target: target.ID, Provider: target.Provider, Node: node, IPFamily: "ipv4", IP: ip},
		ReportedAt: now.Add(-time.Hour), ExpiresAt: now.Add(24 * time.Hour), EffectiveScore: score,
		BaselineScore: baseline, QualityScore: score, StabilityScore: score, ConfidencePercent: 80,
		Complete: true, Connected: true, ProviderAlive: true, ProviderHistoryFresh: true,
		Reason: "quality scan eligible", CreatedAt: now.Add(-time.Hour),
	}
}

func recommendationTestValidation(target config.QualityTarget, rec Recommendation, now time.Time) RecommendationValidation {
	return RecommendationValidation{
		Target: target, CurrentNode: rec.Node, CurrentIP: rec.Identity.IP, CurrentProvider: rec.Provider,
		ProviderAlive: true, ProviderHistoryFresh: true, VendorConnectivityFresh: true,
		Now: now, MaxAge: 48 * time.Hour, MinimumScore: 60, MinimumConfidence: 70,
	}
}

func TestEvaluateReplacementKeepsConnectedStickyNodeAgainstHigherScore(t *testing.T) {
	now := time.Now().UTC()
	target := recommendationTestTarget()
	sticky := recommendationTestValue(target, "sticky", "198.51.100.10", 90, 90, now)
	candidate := recommendationTestValue(target, "better", "198.51.100.11", 96, 96, now)

	replace, reason := EvaluateReplacement(ReplacementRequest{
		Sticky: sticky, Candidate: candidate,
		StickyValidation:    recommendationTestValidation(target, sticky, now),
		CandidateValidation: recommendationTestValidation(target, candidate, now),
		Thresholds:          config.QualityThresholds{BaselineDropPoints: 20, MinimumConfidence: 70, CandidateMinimumScore: 60},
	})
	if replace {
		t.Fatalf("higher-scoring candidate replaced healthy sticky node: reason=%q", reason)
	}
	if !strings.Contains(reason, "sticky") {
		t.Fatalf("reason=%q does not explain sticky retention", reason)
	}
}

func TestEvaluateReplacementRequiresExactTwentyPointBaselineDrop(t *testing.T) {
	now := time.Now().UTC()
	target := recommendationTestTarget()
	candidate := recommendationTestValue(target, "better", "198.51.100.11", 95, 95, now)

	for _, test := range []struct {
		name       string
		sticky     Recommendation
		stickyEdit func(*RecommendationValidation)
		want       bool
	}{
		{name: "nineteen point drop", sticky: recommendationTestValue(target, "sticky", "198.51.100.10", 81, 100, now), want: false},
		{name: "incomplete sticky", sticky: func() Recommendation {
			value := recommendationTestValue(target, "sticky", "198.51.100.10", 80, 100, now)
			value.Complete = false
			return value
		}(), want: false},
		{name: "low confidence", sticky: func() Recommendation {
			value := recommendationTestValue(target, "sticky", "198.51.100.10", 80, 100, now)
			value.ConfidencePercent = 69
			return value
		}(), want: false},
		{name: "IP mismatch", sticky: recommendationTestValue(target, "sticky", "198.51.100.10", 80, 100, now), stickyEdit: func(value *RecommendationValidation) { value.CurrentIP = "198.51.100.99" }, want: false},
		{name: "provider not alive", sticky: recommendationTestValue(target, "sticky", "198.51.100.10", 80, 100, now), stickyEdit: func(value *RecommendationValidation) { value.ProviderAlive = false }, want: false},
		{name: "history stale", sticky: recommendationTestValue(target, "sticky", "198.51.100.10", 80, 100, now), stickyEdit: func(value *RecommendationValidation) { value.ProviderHistoryFresh = false }, want: false},
		{name: "vendor connectivity stale", sticky: recommendationTestValue(target, "sticky", "198.51.100.10", 80, 100, now), stickyEdit: func(value *RecommendationValidation) { value.VendorConnectivityFresh = false }, want: false},
		{name: "exact twenty point drop", sticky: recommendationTestValue(target, "sticky", "198.51.100.10", 80, 100, now), want: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			stickyValidation := recommendationTestValidation(target, test.sticky, now)
			if test.stickyEdit != nil {
				test.stickyEdit(&stickyValidation)
			}
			replace, _ := EvaluateReplacement(ReplacementRequest{
				Sticky: test.sticky, Candidate: candidate,
				StickyValidation:    stickyValidation,
				CandidateValidation: recommendationTestValidation(target, candidate, now),
				Thresholds:          config.QualityThresholds{BaselineDropPoints: 20, MinimumConfidence: 70, CandidateMinimumScore: 60},
			})
			if replace != test.want {
				t.Fatalf("replace=%v, want %v", replace, test.want)
			}
		})
	}
}

func TestValidateRecommendationRejectsStaleAndMismatchedIP(t *testing.T) {
	now := time.Now().UTC()
	target := recommendationTestTarget()
	recommendation := recommendationTestValue(target, "node", "198.51.100.10", 90, 90, now)

	stale := recommendation
	stale.ReportedAt = now.Add(-72 * time.Hour)
	if err := ValidateRecommendation(stale, recommendationTestValidation(target, stale, now)); err == nil || !strings.Contains(err.Error(), "stale") {
		t.Fatalf("stale recommendation error=%v", err)
	}

	wrongIP := recommendationTestValidation(target, recommendation, now)
	wrongIP.CurrentIP = "198.51.100.99"
	if err := ValidateRecommendation(recommendation, wrongIP); err == nil || !strings.Contains(err.Error(), "IP") {
		t.Fatalf("IP mismatch error=%v", err)
	}
}

func TestReadRecommendationsIgnoresStaleInvalidAndCorruptData(t *testing.T) {
	now := time.Now().UTC()
	target := recommendationTestTarget()
	fresh := recommendationTestValue(target, "fresh", "198.51.100.10", 90, 90, now)
	stale := recommendationTestValue(target, "stale", "198.51.100.11", 95, 95, now)
	stale.ReportedAt = now.Add(-72 * time.Hour)
	invalid := Recommendation{Target: target.ID, Node: "missing-identity"}
	store := NewStore(filepath.Join(t.TempDir(), "quality"))
	if err := store.SaveRecommendations([]Recommendation{fresh, stale, invalid}); err != nil {
		t.Fatal(err)
	}

	got, err := ReadRecommendations(store, now, 48*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Node != "fresh" {
		t.Fatalf("recommendations=%+v, want only fresh valid item", got)
	}

	if err := os.WriteFile(store.RecommendationsPath(), []byte("{not-json"), 0600); err != nil {
		t.Fatal(err)
	}
	got, err = ReadRecommendations(store, now, 48*time.Hour)
	if err != nil || len(got) != 0 {
		t.Fatalf("corrupt recommendations=%+v err=%v, want ignored", got, err)
	}
	if _, err := filepath.Glob(store.RecommendationsPath() + ".corrupt-*"); err != nil {
		t.Fatal(err)
	}
}

func TestGenerateRecommendationPreservesBaselineAndReportIdentity(t *testing.T) {
	now := time.Now().UTC()
	target := recommendationTestTarget()
	key := NodeKey{Target: target.ID, Provider: target.Provider, Node: "node", IPFamily: "ipv4", IP: "198.51.100.10"}
	report := Report{
		Identity: key, ObservedAt: now, QualityScore: 90, StabilityScore: 80, EffectiveScore: 87,
		ConfidencePercent: 85, Complete: true, Eligible: true, ProviderAlive: true,
		ProviderHistoryFresh: true, Provider: ProviderHealth{Alive: true, HistoryFresh: true},
	}
	record := NodeRecord{Identity: key, Baseline: &Baseline{Identity: key, Score: 70}}
	recommendation, err := GenerateRecommendation(report, record, target, now)
	if err != nil {
		t.Fatal(err)
	}
	if recommendation.Target != target.ID || recommendation.SourceGroup != target.SourceGroup || recommendation.Provider != target.Provider || recommendation.Node != "node" {
		t.Fatalf("recommendation routing fields=%+v", recommendation)
	}
	if recommendation.Identity != key || recommendation.ReportedAt != now || recommendation.EffectiveScore != 87 || recommendation.BaselineScore != 70 {
		t.Fatalf("recommendation identity/score=%+v", recommendation)
	}
}
