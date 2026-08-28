package quality

import (
	"testing"
)

func boolPtr(v bool) *bool { return &v }

func floatPtr(v float64) *float64 { return &v }

func TestScoreQualityTreatsAuthAndRateLimitAsReachable(t *testing.T) {
	got := ScoreQuality(map[string]VendorResult{
		"openai":    {Vendor: "openai", StatusCodes: []int{401}, Attempts: 2},
		"gemini":    {Vendor: "gemini", StatusCodes: []int{403}, Attempts: 2},
		"anthropic": {Vendor: "anthropic", StatusCodes: []int{429}, Attempts: 2},
	}, nil, nil)

	if got.VendorReachability != 100 {
		t.Fatalf("vendor reachability=%d, want 100 for 401/403/429", got.VendorReachability)
	}
}

func TestScoreQualityRejectsServerAndNetworkFailures(t *testing.T) {
	got := ScoreQuality(map[string]VendorResult{
		"openai": {
			Vendor:      "openai",
			StatusCodes: []int{503},
			Errors:      []ReportError{{Code: ErrorHTTP}},
		},
		"gemini": {
			Vendor: "gemini",
			Errors: []ReportError{{Code: ErrorTCP}},
		},
	}, nil, nil)

	if got.VendorReachability != 0 {
		t.Fatalf("vendor reachability=%d, want 0 for 5xx/network failure", got.VendorReachability)
	}
}

func TestScoreIdentityUsesTwoOfThreeIPConsensus(t *testing.T) {
	sources := []SourceEvidence{
		{Source: "ip-a", Kind: "ip", Available: true, IP: "203.0.113.10", ASN: "AS64500", Country: "US"},
		{Source: "ip-b", Kind: "identity", Available: true, IP: "203.0.113.10", ASN: "AS64500", Country: "US"},
		{Source: "ip-c", Kind: "identity", Available: true, IP: "203.0.113.11", ASN: "AS64500", Country: "US"},
	}

	got := ScoreIdentity(sources)
	if !got.Complete || got.ConsensusIP != "203.0.113.10" {
		t.Fatalf("identity=%+v, want complete two-of-three consensus", got)
	}
	if got.Score <= 0 {
		t.Fatalf("identity score=%d, want positive consensus score", got.Score)
	}
}

func TestScoreIdentityFlagsIPConflict(t *testing.T) {
	got := ScoreIdentity([]SourceEvidence{
		{Source: "a", Kind: "ip", Available: true, IP: "203.0.113.1"},
		{Source: "b", Kind: "ip", Available: true, IP: "203.0.113.2"},
		{Source: "c", Kind: "ip", Available: true, IP: "203.0.113.3"},
	})
	if !got.Conflict || got.Complete || got.Score != 0 {
		t.Fatalf("identity=%+v, want incomplete zero-scored IP conflict", got)
	}
}

func TestScoreRiskUsesSourceMajority(t *testing.T) {
	clean := []SourceEvidence{
		{Source: "risk-a", Kind: "risk", Available: true, Proxy: boolPtr(false), VPN: boolPtr(false), Tor: boolPtr(false), Blacklisted: boolPtr(false)},
		{Source: "risk-b", Kind: "risk", Available: true, Proxy: boolPtr(false), VPN: boolPtr(false), Tor: boolPtr(false), Blacklisted: boolPtr(false)},
		{Source: "risk-c", Kind: "risk", Available: true, Proxy: boolPtr(true), VPN: boolPtr(true), Tor: boolPtr(false), Blacklisted: boolPtr(false)},
	}
	got := ScoreRisk(clean)
	if got.Score <= 50 || !got.Complete {
		t.Fatalf("risk=%+v, want clean majority", got)
	}

	risky := []SourceEvidence{
		{Source: "risk-a", Kind: "risk", Available: true, Proxy: boolPtr(true), Blacklisted: boolPtr(true), AbuseScore: floatPtr(90)},
		{Source: "risk-b", Kind: "risk", Available: true, Proxy: boolPtr(true), Blacklisted: boolPtr(true), AbuseScore: floatPtr(85)},
		{Source: "risk-c", Kind: "risk", Available: true, Proxy: boolPtr(false), Blacklisted: boolPtr(false), AbuseScore: floatPtr(1)},
	}
	if got = ScoreRisk(risky); got.Score >= 50 {
		t.Fatalf("risk=%+v, want risky majority to reduce score", got)
	}
}

func TestScoreQualityLowersConfidenceWhenSourcesAreMissing(t *testing.T) {
	got := ScoreQuality(
		map[string]VendorResult{"openai": {Vendor: "openai", StatusCodes: []int{200}, Attempts: 2}},
		[]SourceEvidence{{Source: "only-ip", Kind: "ip", Available: true, IP: "203.0.113.10"}},
		nil,
	)
	if got.Confidence >= 100 || got.Complete {
		t.Fatalf("quality=%+v, want incomplete lower-confidence result", got)
	}
}

func TestScoresClampToZeroAndOneHundred(t *testing.T) {
	if got := clampScore(-42); got != 0 {
		t.Fatalf("clamp(-42)=%d, want 0", got)
	}
	if got := clampScore(142); got != 100 {
		t.Fatalf("clamp(142)=%d, want 100", got)
	}
}

func TestEffectiveScoreRoundsAndClamps(t *testing.T) {
	if got := EffectiveScore(90, 70); got != 84 {
		t.Fatalf("EffectiveScore(90,70)=%d, want 84", got)
	}
	if got := EffectiveScore(-10, 200); got != 30 {
		t.Fatalf("EffectiveScore(-10,200)=%d, want 30 after input clamp", got)
	}
}

func TestBaselineDropRequiresAtLeastExactThreshold(t *testing.T) {
	if BaselineDropped(100, 81, 20) {
		t.Fatal("19-point drop must not release sticky node")
	}
	if !BaselineDropped(100, 80, 20) {
		t.Fatal("exact 20-point drop must release sticky node")
	}
	if !BaselineDropped(100, 70, 0) {
		t.Fatal("zero threshold should use the safe default of 20")
	}
}
