package quality

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"mihomo-guardian/internal/config"
)

func testNodeKey(ip string) NodeKey {
	return NodeKey{
		Target:   "primary",
		Provider: "provider-a",
		Node:     "node-01",
		IPFamily: "ipv4",
		IP:       ip,
	}
}

func testReport(key NodeKey, score int, observedAt time.Time) Report {
	return Report{
		Identity:             key,
		ObservedAt:           observedAt,
		QualityScore:         score,
		StabilityScore:       score,
		EffectiveScore:       score,
		ConfidencePercent:    95,
		Complete:             true,
		Eligible:             true,
		ProviderAlive:        true,
		ProviderHistoryFresh: true,
		VendorResults: map[string]VendorResult{
			"openai": {Vendor: "openai", Reachable: true, Attempts: 2},
		},
	}
}

func TestQualityStoreKeepsImmutableBaselineAndRaisesBest(t *testing.T) {
	root := t.TempDir()
	store := NewStore(root)
	firstAt := time.Date(2026, 8, 28, 1, 2, 3, 0, time.UTC)
	key := testNodeKey("1.2.3.4")

	if _, err := store.SaveReport(testReport(key, 80, firstAt)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SaveReport(testReport(key, 94, firstAt.Add(time.Hour))); err != nil {
		t.Fatal(err)
	}

	got, err := store.LoadNode(key)
	if err != nil {
		t.Fatal(err)
	}
	if got.Baseline == nil || got.Baseline.Score != 80 {
		t.Fatalf("baseline=%+v, want immutable score 80", got.Baseline)
	}
	if got.Latest == nil || got.Latest.EffectiveScore != 94 {
		t.Fatalf("latest=%+v, want score 94", got.Latest)
	}
	if got.Best == nil || got.Best.EffectiveScore != 94 || got.BestScore != 94 {
		t.Fatalf("best=%+v best_score=%d, want score 94", got.Best, got.BestScore)
	}
	if got.LastGood == nil || got.LastGood.EffectiveScore != 94 {
		t.Fatalf("last_good=%+v, want latest eligible report", got.LastGood)
	}

	if _, err := store.SaveReport(testReport(key, 90, firstAt.Add(2*time.Hour))); err != nil {
		t.Fatal(err)
	}
	got, err = store.LoadNode(key)
	if err != nil {
		t.Fatal(err)
	}
	if got.Baseline == nil || got.Baseline.Score != 80 || got.BestScore != 94 {
		t.Fatalf("score rise/fall changed baseline or best: %+v", got)
	}
}

func TestQualityStoreTreatsIPChangeAsNewIdentityAndRetainsHistory(t *testing.T) {
	store := NewStore(t.TempDir())
	first := testNodeKey("1.2.3.4")
	second := testNodeKey("5.6.7.8")
	now := time.Date(2026, 8, 28, 2, 0, 0, 0, time.UTC)

	if _, err := store.SaveReport(testReport(first, 81, now)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SaveReport(testReport(second, 73, now.Add(time.Minute))); err != nil {
		t.Fatal(err)
	}

	firstRecord, err := store.LoadNode(first)
	if err != nil {
		t.Fatal(err)
	}
	secondRecord, err := store.LoadNode(second)
	if err != nil {
		t.Fatal(err)
	}
	if firstRecord.Baseline == nil || firstRecord.Baseline.Score != 81 {
		t.Fatalf("old IP baseline=%+v", firstRecord.Baseline)
	}
	if secondRecord.Baseline == nil || secondRecord.Baseline.Score != 73 {
		t.Fatalf("new IP baseline=%+v", secondRecord.Baseline)
	}
	records, err := store.ListNodeRecords()
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 2 {
		t.Fatalf("records=%d, want separate records for both IPs", len(records))
	}

	if _, err := store.SaveReport(testReport(first, 82, now.Add(2*time.Minute))); err != nil {
		t.Fatal(err)
	}
	restored, err := store.LoadNode(first)
	if err != nil {
		t.Fatal(err)
	}
	if restored.Baseline == nil || restored.Baseline.Score != 81 {
		t.Fatalf("restored old IP baseline=%+v", restored.Baseline)
	}
}

func TestQualityStoreUsesHashedSafeNodeFilenames(t *testing.T) {
	store := NewStore(t.TempDir())
	key := NodeKey{
		Target:   "target/with/slash",
		Provider: "provider?token=secret",
		Node:     "../../node name?password=secret",
		IPFamily: "ipv4",
		IP:       "1.2.3.4",
	}
	if _, err := store.SaveReport(testReport(key, 80, time.Now().UTC())); err != nil {
		t.Fatal(err)
	}
	files, err := filepath.Glob(filepath.Join(store.NodesDir(), "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 {
		t.Fatalf("node files=%v, want one hashed file", files)
	}
	base := filepath.Base(files[0])
	if strings.Contains(base, "secret") || strings.Contains(base, "node name") || strings.Contains(base, "/") {
		t.Fatalf("unsafe node filename %q", base)
	}
	if base != key.ID()+".json" {
		t.Fatalf("node filename=%q, want hash %q", base, key.ID()+".json")
	}
}

func TestQualityStoreWritesAtomic0600FilesAndPreservesCorruptJSON(t *testing.T) {
	root := t.TempDir()
	store := NewStore(root)
	key := testNodeKey("1.2.3.4")
	report := testReport(key, 80, time.Now().UTC())
	report.SourceEvidence = []SourceEvidence{{
		Source: "identity-source",
		URL:    "https://identity.example/check?token=secret-value",
	}}
	if _, err := store.SaveReport(report); err != nil {
		t.Fatal(err)
	}

	for _, path := range []string{
		filepath.Join(root, "scan.lock"),
		filepath.Join(store.NodesDir(), key.ID()+".json"),
		filepath.Join(root, "latest-primary.json"),
	} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat %s: %v", path, err)
		}
		if got := info.Mode().Perm(); got != 0600 {
			t.Errorf("%s mode=%o, want 0600", path, got)
		}
	}
	for _, pattern := range []string{
		filepath.Join(root, ".*.tmp-*"),
		filepath.Join(store.NodesDir(), ".*.tmp-*"),
	} {
		matches, err := filepath.Glob(pattern)
		if err != nil {
			t.Fatal(err)
		}
		if len(matches) != 0 {
			t.Fatalf("temporary files remain: %v", matches)
		}
	}
	for _, pattern := range []string{
		filepath.Join(store.NodesDir(), "*.json"),
		filepath.Join(store.HistoryDir(), "*.json"),
		filepath.Join(root, "latest-*.json"),
	} {
		matches, err := filepath.Glob(pattern)
		if err != nil {
			t.Fatal(err)
		}
		for _, path := range matches {
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(data), "secret-value") {
				t.Fatalf("%s leaked an evidence URL credential", path)
			}
		}
	}

	progressPath := filepath.Join(root, "scan-progress.json")
	if err := os.WriteFile(progressPath, []byte("{malformed"), 0600); err != nil {
		t.Fatal(err)
	}
	progress, err := store.LoadScanProgress()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(progress, ScanProgress{}) {
		t.Fatalf("recovered progress=%+v, want empty progress", progress)
	}
	corrupt, err := filepath.Glob(progressPath + ".corrupt-*")
	if err != nil {
		t.Fatal(err)
	}
	if len(corrupt) != 1 {
		t.Fatalf("corrupt backups=%v, want one preserved backup", corrupt)
	}
	if _, err := os.Stat(progressPath); !os.IsNotExist(err) {
		t.Fatalf("malformed original should be moved, stat err=%v", err)
	}
}

func TestQualityStorePersistsProgressCursorAndRetention(t *testing.T) {
	root := t.TempDir()
	store := NewStore(root)
	progress := ScanProgress{
		Target:              "reserve",
		Provider:            "provider-b",
		Cursor:              "node-02",
		CursorIndex:         2,
		ProviderFingerprint: "sha256:provider",
		Attempted:           2,
		Completed:           1,
		LastAttemptAt:       time.Date(2026, 8, 28, 1, 0, 0, 0, time.UTC),
	}
	if err := store.SaveScanProgress(progress); err != nil {
		t.Fatal(err)
	}
	got, err := store.LoadScanProgress()
	if err != nil {
		t.Fatal(err)
	}
	if got.Target != progress.Target || got.Cursor != progress.Cursor || got.CursorIndex != progress.CursorIndex || got.ProviderFingerprint != progress.ProviderFingerprint {
		t.Fatalf("progress=%+v, want cursor=%+v", got, progress)
	}

	key := testNodeKey("1.2.3.4")
	base := time.Date(2026, 8, 20, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 4; i++ {
		if _, err := store.SaveReport(testReport(key, 70+i, base.Add(time.Duration(i)*time.Hour))); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Prune(config.QualityRetentionConfig{Reports: 2, HistoryDays: 365}, base.Add(24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	history, err := filepath.Glob(filepath.Join(store.HistoryDir(), "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 2 {
		t.Fatalf("history files=%v, want two after retention", history)
	}
}

func TestQualityStorePersistsStabilityAndRecommendationsSeparatelyFromState(t *testing.T) {
	root := t.TempDir()
	store := NewStore(root)
	key := testNodeKey("1.2.3.4")
	now := time.Date(2026, 8, 28, 3, 0, 0, 0, time.UTC)
	snapshot := StabilitySnapshot{
		Identity:            key,
		ObservedAt:          now,
		WindowStart:         now.Add(-time.Hour),
		WindowEnd:           now,
		Known:               true,
		Samples:             10,
		CoveragePercent:     95,
		AvailabilityPercent: 90,
		P50MS:               120,
		P95MS:               220,
		MaxMS:               400,
		JitterMS:            100,
		StabilityScore:      88,
	}
	if err := store.SaveStability(snapshot); err != nil {
		t.Fatal(err)
	}
	recommendations := []Recommendation{{
		ID:                   "rec-1",
		Target:               "primary",
		SourceGroup:          "MAIN",
		Provider:             key.Provider,
		Node:                 key.Node,
		Identity:             key,
		ReportedAt:           now,
		EffectiveScore:       88,
		BaselineScore:        80,
		ConfidencePercent:    95,
		Complete:             true,
		Connected:            true,
		ProviderAlive:        true,
		ProviderHistoryFresh: true,
		Reason:               "validated",
	}}
	if err := store.SaveRecommendations(recommendations); err != nil {
		t.Fatal(err)
	}
	gotSnapshots, err := store.LoadStability()
	if err != nil {
		t.Fatal(err)
	}
	if len(gotSnapshots) != 1 || gotSnapshots[key.ID()].StabilityScore != snapshot.StabilityScore {
		t.Fatalf("stability=%+v, want=%+v", gotSnapshots, snapshot)
	}
	gotRecommendations, err := store.LoadRecommendations()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(gotRecommendations, recommendations) {
		t.Fatalf("recommendations=%+v, want=%+v", gotRecommendations, recommendations)
	}
	for _, path := range []string{
		filepath.Join(root, "stability.json"),
		filepath.Join(root, "stability-history.jsonl"),
		filepath.Join(root, "recommendations.json"),
	} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("missing %s: %v", path, err)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "state.json")); !os.IsNotExist(err) {
		t.Fatalf("quality store must not create state.json, err=%v", err)
	}

	allData := make([]byte, 0)
	for _, path := range []string{
		filepath.Join(root, "stability.json"),
		filepath.Join(root, "stability-history.jsonl"),
		filepath.Join(root, "recommendations.json"),
	} {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		allData = append(allData, data...)
	}
	if strings.Contains(string(allData), "secret") {
		t.Fatal("quality persistence leaked a sensitive value")
	}
	var decoded []Recommendation
	if err := json.Unmarshal(mustReadFile(t, filepath.Join(root, "recommendations.json")), &decoded); err != nil {
		t.Fatal(err)
	}
}

func TestQualityStoreSerializesConcurrentWritersAcrossStoreInstances(t *testing.T) {
	root := t.TempDir()
	firstStore := NewStore(root)
	secondStore := NewStore(root)
	key := testNodeKey("1.2.3.4")
	start := time.Date(2026, 8, 28, 4, 0, 0, 0, time.UTC)
	var wait sync.WaitGroup
	for index, store := range []*Store{firstStore, secondStore} {
		wait.Add(1)
		go func(store *Store, score int) {
			defer wait.Done()
			if _, err := store.SaveReport(testReport(key, score, start.Add(time.Duration(score)*time.Minute))); err != nil {
				t.Errorf("concurrent save: %v", err)
			}
		}(store, 80+index)
	}
	wait.Wait()

	record, err := firstStore.LoadNode(key)
	if err != nil {
		t.Fatal(err)
	}
	if record.ReportCount != 2 || record.Latest == nil || record.Best == nil {
		t.Fatalf("record after concurrent writes=%+v, want two complete reports", record)
	}
	reports, err := firstStore.ListReports(key)
	if err != nil {
		t.Fatal(err)
	}
	if len(reports) != 2 {
		t.Fatalf("history after concurrent writes=%d, want 2", len(reports))
	}
}

func mustReadFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
