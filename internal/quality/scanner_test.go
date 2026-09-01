package quality

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"mihomo-guardian/internal/config"
	"mihomo-guardian/internal/mihomo"
	"mihomo-guardian/internal/probe"
	"mihomo-guardian/internal/state"
)

type scannerSelection struct {
	group string
	node  string
}

type scannerFakeAPI struct {
	heartbeatErr error
	heartbeats   int
	proxies      map[string]mihomo.Proxy
	providers    map[string]mihomo.Provider
	selections   []scannerSelection
	gets         []string
	providerGets []string
}

func (f *scannerFakeAPI) Heartbeat(context.Context) error {
	f.heartbeats++
	return f.heartbeatErr
}

func (f *scannerFakeAPI) GetProxy(_ context.Context, name string) (mihomo.Proxy, error) {
	f.gets = append(f.gets, name)
	proxy, ok := f.proxies[name]
	if !ok {
		return mihomo.Proxy{}, fmt.Errorf("proxy %q not found", name)
	}
	return proxy, nil
}

func (f *scannerFakeAPI) GetProvider(_ context.Context, name string) (mihomo.Provider, error) {
	f.providerGets = append(f.providerGets, name)
	provider, ok := f.providers[name]
	if !ok {
		return mihomo.Provider{}, fmt.Errorf("provider %q not found", name)
	}
	return provider, nil
}

func (f *scannerFakeAPI) SetProxy(_ context.Context, group, node string) error {
	f.selections = append(f.selections, scannerSelection{group: group, node: node})
	return nil
}

func scannerHistory(alive bool, age time.Duration) []mihomo.DelayHistory {
	now := time.Now().UTC().Add(-age)
	return []mihomo.DelayHistory{
		{Time: now.Add(-2 * time.Minute), Delay: map[bool]int{true: 120, false: 0}[alive]},
		{Time: now.Add(-time.Minute), Delay: map[bool]int{true: 130, false: 0}[alive]},
		{Time: now, Delay: map[bool]int{true: 125, false: 0}[alive]},
	}
}

func scannerProxy(name string, alive bool, age time.Duration) mihomo.Proxy {
	return mihomo.Proxy{Name: name, Alive: alive, History: scannerHistory(alive, age), ProviderName: "provider"}
}

func scannerConfig(order []string, targets []config.QualityTarget) config.Config {
	return config.Config{Quality: config.QualityConfig{
		Enabled:        true,
		Order:          order,
		Targets:        targets,
		PerNodeTimeout: 2 * time.Second,
		Thresholds: config.QualityThresholds{
			MinimumConfidence: 70,
		},
		Stability: config.QualityStabilityConfig{
			SummaryInterval: time.Hour,
			HistoryWindow:   24 * time.Hour,
			MinimumSamples:  3,
			MinimumCoverage: 1,
			StaleAfter:      time.Hour,
			GoodLatencyMS:   500,
			BadLatencyMS:    3000,
		},
	}}
}

func scannerCompleteCollection() Collection {
	return Collection{
		VendorResults: map[string]VendorResult{
			"openai": {Vendor: "openai", Attempts: 2, SuccessCount: 2, Reachable: true, StatusCodes: []int{401, 401}},
		},
		SourceEvidence: []SourceEvidence{
			{Source: "ip-a", Kind: "ip", Available: true, IP: "198.51.100.10", IPFamily: "ipv4", ASN: "AS64500", Country: "US"},
			{Source: "ip-b", Kind: "identity", Available: true, IP: "198.51.100.10", IPFamily: "ipv4", ASN: "AS64500", Country: "US"},
		},
		RiskEvidence: []SourceEvidence{
			{Source: "risk-a", Kind: "risk", Available: true, Proxy: boolPtr(false), VPN: boolPtr(false), Tor: boolPtr(false), Blacklisted: boolPtr(false)},
			{Source: "risk-b", Kind: "risk", Available: true, Proxy: boolPtr(false), VPN: boolPtr(false), Tor: boolPtr(false), Blacklisted: boolPtr(false)},
		},
		IdentityIP:       "198.51.100.10",
		IdentityFamily:   "ipv4",
		IdentityComplete: true,
	}
}

func TestScannerSourcesUsesExplicitRiskMetadata(t *testing.T) {
	cfg := config.Config{Purity: config.PurityConfig{
		Timeout: time.Second,
		Sources: []config.PuritySource{
			{ID: "identity", URL: "https://identity.example/json", Kind: "identity", Format: "json"},
			{ID: "risk", URL: "https://risk.example/check", Kind: "risk", Format: "json"},
		},
	}}
	sources := scannerSources(cfg)
	if len(sources) != 2 || sources[1].Kind != SourceKindRisk || sources[1].Format != SourceFormatJSON {
		t.Fatalf("sources=%+v, explicit risk metadata must be preserved", sources)
	}
}

func newScannerFixture(t *testing.T, api *scannerFakeAPI, cfg config.Config, collect func(context.Context, *probe.ExternalClient, []SourceSpec, []VendorProbeSpec) (Collection, error)) (*Scanner, *Store, *state.Store, *[]string) {
	t.Helper()
	root := t.TempDir()
	reports := NewStore(root + "/quality")
	stateStore := state.NewStore(root+"/state.json", "CHANNEL")
	listeners := []string{}
	scanner := &Scanner{
		API:     api,
		Reports: reports,
		State:   stateStore,
		External: func(listener string, _ time.Duration) (*probe.ExternalClient, error) {
			listeners = append(listeners, listener)
			return nil, nil
		},
		Collect: collect,
	}
	return scanner, reports, stateStore, &listeners
}

func TestScannerScansConfiguredOrderAndOnlyProviderSourceIntersection(t *testing.T) {
	api := &scannerFakeAPI{
		proxies: map[string]mihomo.Proxy{
			"RESERVE-GROUP": {Name: "RESERVE-GROUP", All: []string{"us-b", "eu-node", "us-a", "outside"}},
			"PRIMARY-GROUP": {Name: "PRIMARY-GROUP", All: []string{"locked-primary"}},
		},
		providers: map[string]mihomo.Provider{
			"reserve-provider": {Name: "reserve-provider", Proxies: []mihomo.Proxy{
				scannerProxy("us-a", true, 0), scannerProxy("us-b", true, 0), scannerProxy("outside", true, 0),
			}},
			"primary-provider": {Name: "primary-provider", Proxies: []mihomo.Proxy{scannerProxy("locked-primary", true, 0)}},
		},
	}
	cfg := scannerConfig(
		[]string{"reserve", "primary"},
		[]config.QualityTarget{
			{ID: "reserve", SourceGroup: "RESERVE-GROUP", Provider: "reserve-provider", Scope: "all", NodeFilter: `^us-`, Listener: "http://127.0.0.1:17991"},
			{ID: "primary", SourceGroup: "PRIMARY-GROUP", Provider: "primary-provider", Scope: "locked", LockKey: "main", Listener: "http://127.0.0.1:17990"},
		},
	)
	collectCalls := 0
	scanner, reports, _, listeners := newScannerFixture(t, api, cfg, func(context.Context, *probe.ExternalClient, []SourceSpec, []VendorProbeSpec) (Collection, error) {
		collectCalls++
		return scannerCompleteCollection(), nil
	})
	stateStore := scanner.State
	if err := stateStore.Save(state.State{ProviderLocks: map[string]state.ProviderLock{
		"main": {Provider: "primary-provider", Group: "PRIMARY-GROUP", Node: "locked-primary"},
	}}); err != nil {
		t.Fatal(err)
	}

	if err := scanner.Scan(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	want := []scannerSelection{
		{group: "GUARDIAN-QUALITY-reserve", node: "us-a"},
		{group: "GUARDIAN-QUALITY-reserve", node: "us-b"},
		{group: "GUARDIAN-QUALITY-primary", node: "locked-primary"},
	}
	if !reflect.DeepEqual(api.selections, want) {
		t.Fatalf("selections=%+v, want=%+v", api.selections, want)
	}
	if !reflect.DeepEqual(*listeners, []string{"http://127.0.0.1:17991", "http://127.0.0.1:17991", "http://127.0.0.1:17990"}) {
		t.Fatalf("listeners=%v, want per-target listener order", *listeners)
	}
	if collectCalls != 3 {
		t.Fatalf("collect calls=%d, want one per selected node", collectCalls)
	}
	for _, selection := range api.selections {
		if selection.group == "RESERVE-GROUP" || selection.group == "PRIMARY-GROUP" || selection.group == "CHANNEL" {
			t.Fatalf("scanner wrote a production group: %+v", selection)
		}
	}
	records, err := reports.ListNodeRecords()
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 3 {
		t.Fatalf("saved records=%d, want three successful node reports", len(records))
	}
}

func TestScannerContinuesAfterNodeFailureAndPersistsCursor(t *testing.T) {
	api := &scannerFakeAPI{
		proxies: map[string]mihomo.Proxy{
			"GROUP": {Name: "GROUP", All: []string{"bad", "good"}},
		},
		providers: map[string]mihomo.Provider{
			"provider": {Name: "provider", Proxies: []mihomo.Proxy{scannerProxy("bad", true, 0), scannerProxy("good", true, 0)}},
		},
	}
	cfg := scannerConfig([]string{"target"}, []config.QualityTarget{{
		ID: "target", SourceGroup: "GROUP", Provider: "provider", Scope: "all", Listener: "http://127.0.0.1:17990",
	}})
	collectCalls := 0
	scanner, reports, _, _ := newScannerFixture(t, api, cfg, func(context.Context, *probe.ExternalClient, []SourceSpec, []VendorProbeSpec) (Collection, error) {
		collectCalls++
		if collectCalls == 1 {
			return Collection{}, errors.New("simulated node failure")
		}
		return scannerCompleteCollection(), nil
	})

	if err := scanner.Scan(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	if got := []string{api.selections[0].node, api.selections[1].node}; !reflect.DeepEqual(got, []string{"bad", "good"}) {
		t.Fatalf("selected nodes=%v, want failed node followed by later node", got)
	}
	progress, err := reports.LoadScanProgress()
	if err != nil {
		t.Fatal(err)
	}
	targetProgress := progress.Targets["target"]
	if targetProgress.Complete || targetProgress.Failed != 1 || targetProgress.Cursor != "good" || targetProgress.CursorIndex != 2 || targetProgress.Attempted != 2 || targetProgress.Completed != 1 {
		t.Fatalf("progress=%+v, want incomplete cursor with one failed and one successful report", targetProgress)
	}
	records, err := reports.ListNodeRecords()
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || records[0].Identity.Node != "good" {
		t.Fatalf("records=%+v, want only good node report", records)
	}
}

func TestScannerRetriesFailedTargetAndRepeatsExpiredFullScan(t *testing.T) {
	now := time.Now().UTC().Add(time.Minute)
	api := &scannerFakeAPI{
		proxies:   map[string]mihomo.Proxy{"GROUP": {Name: "GROUP", All: []string{"node"}}},
		providers: map[string]mihomo.Provider{"provider": {Name: "provider", Proxies: []mihomo.Proxy{scannerProxy("node", true, 0)}}},
	}
	cfg := scannerConfig([]string{"target"}, []config.QualityTarget{{
		ID: "target", SourceGroup: "GROUP", Provider: "provider", Scope: "all", Listener: "http://127.0.0.1:17990",
	}})
	scanner, reports, _, _ := newScannerFixture(t, api, cfg, func(context.Context, *probe.ExternalClient, []SourceSpec, []VendorProbeSpec) (Collection, error) {
		return scannerCompleteCollection(), nil
	})
	scanner.Now = func() time.Time { return now }
	fingerprint := scannerCandidateFingerprint([]string{"node"})
	if err := reports.SaveScanProgress(ScanProgress{Targets: map[string]TargetScanProgress{
		"target": {Target: "target", Provider: "provider", ProviderFingerprint: fingerprint, Complete: true, LastFullScanAt: now.Add(-721 * time.Hour)},
	}}); err != nil {
		t.Fatal(err)
	}
	if err := scanner.Scan(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	if len(api.selections) != 1 {
		t.Fatalf("expired full scan selections=%v, want one rescan", api.selections)
	}
	progress, err := reports.LoadScanProgress()
	if err != nil {
		t.Fatal(err)
	}
	if progress.Targets["target"].LastFullScanAt != now {
		t.Fatalf("last full scan=%v, want %v", progress.Targets["target"].LastFullScanAt, now)
	}
}

func TestScannerResumesFromPersistedCursor(t *testing.T) {
	api := &scannerFakeAPI{
		proxies:   map[string]mihomo.Proxy{"GROUP": {Name: "GROUP", All: []string{"first", "second"}}},
		providers: map[string]mihomo.Provider{"provider": {Name: "provider", Proxies: []mihomo.Proxy{scannerProxy("first", true, 0), scannerProxy("second", true, 0)}}},
	}
	cfg := scannerConfig([]string{"target"}, []config.QualityTarget{{
		ID: "target", SourceGroup: "GROUP", Provider: "provider", Scope: "all", Listener: "http://127.0.0.1:17990",
	}})
	scanner, reports, _, _ := newScannerFixture(t, api, cfg, func(context.Context, *probe.ExternalClient, []SourceSpec, []VendorProbeSpec) (Collection, error) {
		return scannerCompleteCollection(), nil
	})
	if err := reports.SaveScanProgress(ScanProgress{Targets: map[string]TargetScanProgress{
		"target": {Target: "target", Provider: "provider", Cursor: "first", CursorIndex: 1, ProviderFingerprint: scannerCandidateFingerprint([]string{"first", "second"}), Attempted: 1, Completed: 1},
	}}); err != nil {
		t.Fatal(err)
	}

	if err := scanner.Scan(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	if len(api.selections) != 1 || api.selections[0].node != "second" {
		t.Fatalf("selections=%+v, want resume at second node", api.selections)
	}
}

func TestResumeIndexRetriesInFlightNodeAfterCrash(t *testing.T) {
	if got := resumeIndex(TargetScanProgress{Cursor: "first", CursorIndex: 0}, []string{"first", "second"}); got != 0 {
		t.Fatalf("resume index=%d, in-flight first node must be retried", got)
	}
	if got := resumeIndex(TargetScanProgress{Cursor: "first", CursorIndex: 1}, []string{"first", "second"}); got != 1 {
		t.Fatalf("resume index=%d, completed first node must not be repeated", got)
	}
}

func TestScannerMarksStaleHistoryUnverified(t *testing.T) {
	api := &scannerFakeAPI{
		proxies:   map[string]mihomo.Proxy{"GROUP": {Name: "GROUP", All: []string{"stale"}}},
		providers: map[string]mihomo.Provider{"provider": {Name: "provider", Proxies: []mihomo.Proxy{scannerProxy("stale", true, 2*time.Hour)}}},
	}
	cfg := scannerConfig([]string{"target"}, []config.QualityTarget{{
		ID: "target", SourceGroup: "GROUP", Provider: "provider", Scope: "all", Listener: "http://127.0.0.1:17990",
	}})
	scanner, reports, _, _ := newScannerFixture(t, api, cfg, func(context.Context, *probe.ExternalClient, []SourceSpec, []VendorProbeSpec) (Collection, error) {
		return scannerCompleteCollection(), nil
	})

	if err := scanner.Scan(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	key := NodeKey{Target: "target", Provider: "provider", Node: "stale", IPFamily: "ipv4", IP: "198.51.100.10"}
	record, err := reports.LoadNode(key)
	if err != nil {
		t.Fatal(err)
	}
	if record.Latest == nil || record.Latest.ProviderHistoryFresh || record.Latest.Eligible || record.Latest.Complete {
		t.Fatalf("stale report=%+v, want unverified and ineligible", record.Latest)
	}
}

func TestScannerHeartbeatFailureIsQualityLinkErrorAndDoesNoWork(t *testing.T) {
	api := &scannerFakeAPI{heartbeatErr: errors.New("mihomo disconnected")}
	cfg := scannerConfig([]string{"target"}, []config.QualityTarget{{
		ID: "target", SourceGroup: "GROUP", Provider: "provider", Scope: "all", Listener: "http://127.0.0.1:17990",
	}})
	externalCalls := 0
	collectCalls := 0
	scanner, _, _, _ := newScannerFixture(t, api, cfg, func(context.Context, *probe.ExternalClient, []SourceSpec, []VendorProbeSpec) (Collection, error) {
		collectCalls++
		return scannerCompleteCollection(), nil
	})
	scanner.External = func(string, time.Duration) (*probe.ExternalClient, error) {
		externalCalls++
		return nil, nil
	}

	for name, scan := range map[string]func() error{
		"scan":        func() error { return scanner.Scan(context.Background(), cfg) },
		"scan target": func() error { return scanner.ScanTarget(context.Background(), cfg, cfg.Quality.Targets[0]) },
	} {
		t.Run(name, func(t *testing.T) {
			api.selections = nil
			externalCalls = 0
			collectCalls = 0
			err := scan()
			if !errors.Is(err, ErrQualityLink) {
				t.Fatalf("error=%v, want ErrQualityLink", err)
			}
			if len(api.selections) != 0 || externalCalls != 0 || collectCalls != 0 {
				t.Fatalf("work after heartbeat failure: selections=%v external=%d collect=%d", api.selections, externalCalls, collectCalls)
			}
		})
	}
	if api.heartbeats != 2 {
		t.Fatalf("heartbeats=%d, want one before each scan entry point", api.heartbeats)
	}
}

func TestScannerUsesExactSourceGroupAndGeneratedQualityGroup(t *testing.T) {
	api := &scannerFakeAPI{
		proxies:   map[string]mihomo.Proxy{"Exact Group": {Name: "Exact Group", All: []string{"node"}}},
		providers: map[string]mihomo.Provider{"provider": {Name: "provider", Proxies: []mihomo.Proxy{scannerProxy("node", true, 0)}}},
	}
	cfg := scannerConfig([]string{"custom"}, []config.QualityTarget{{
		ID: "custom", SourceGroup: "Exact Group", Provider: "provider", Scope: "all", Listener: "http://127.0.0.1:17990",
	}})
	scanner, _, _, _ := newScannerFixture(t, api, cfg, func(context.Context, *probe.ExternalClient, []SourceSpec, []VendorProbeSpec) (Collection, error) {
		return scannerCompleteCollection(), nil
	})

	if err := scanner.ScanTarget(context.Background(), cfg, cfg.Quality.Targets[0]); err != nil {
		t.Fatal(err)
	}
	if len(api.gets) == 0 || api.gets[0] != "Exact Group" {
		t.Fatalf("GetProxy calls=%v, want exact source group", api.gets)
	}
	if len(api.selections) != 1 || api.selections[0].group != "GUARDIAN-QUALITY-custom" {
		t.Fatalf("selections=%+v, want only generated quality group", api.selections)
	}
	if strings.Contains(api.selections[0].group, "CHANNEL") || strings.Contains(api.selections[0].group, "Exact Group") {
		t.Fatalf("selection escaped generated group boundary: %+v", api.selections[0])
	}
}

func TestScannerRejectsEmptyProviderIntersection(t *testing.T) {
	api := &scannerFakeAPI{
		proxies:   map[string]mihomo.Proxy{"GROUP": {Name: "GROUP", All: []string{"node-a"}}},
		providers: map[string]mihomo.Provider{"provider": {Name: "provider", Proxies: []mihomo.Proxy{{Name: "node-b"}}}},
	}
	cfg := scannerConfig([]string{"target"}, []config.QualityTarget{{
		ID: "target", SourceGroup: "GROUP", Provider: "provider", Scope: "all", Listener: "http://127.0.0.1:17990",
	}})
	scanner, _, _, _ := newScannerFixture(t, api, cfg, func(context.Context, *probe.ExternalClient, []SourceSpec, []VendorProbeSpec) (Collection, error) {
		return scannerCompleteCollection(), nil
	})
	if err := scanner.Scan(context.Background(), cfg); err == nil || !strings.Contains(err.Error(), "no candidates") {
		t.Fatalf("scan error=%v, empty intersection must fail closed", err)
	}
}
