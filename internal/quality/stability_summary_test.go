package quality

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"mihomo-guardian/internal/config"
	"mihomo-guardian/internal/mihomo"
	"mihomo-guardian/internal/state"
)

type stabilitySummaryFakeAPI struct {
	heartbeatErr  error
	providerErr   map[string]error
	proxies       map[string]mihomo.Proxy
	providers     map[string]mihomo.Provider
	getProxyCalls []string
	providerCalls []string
}

func (f *stabilitySummaryFakeAPI) Heartbeat(context.Context) error { return f.heartbeatErr }

func (f *stabilitySummaryFakeAPI) GetProxy(_ context.Context, name string) (mihomo.Proxy, error) {
	f.getProxyCalls = append(f.getProxyCalls, name)
	proxy, ok := f.proxies[name]
	if !ok {
		return mihomo.Proxy{}, errors.New("proxy not found")
	}
	return proxy, nil
}

func (f *stabilitySummaryFakeAPI) GetProvider(_ context.Context, name string) (mihomo.Provider, error) {
	f.providerCalls = append(f.providerCalls, name)
	if err := f.providerErr[name]; err != nil {
		return mihomo.Provider{}, err
	}
	provider, ok := f.providers[name]
	if !ok {
		return mihomo.Provider{}, errors.New("provider not found")
	}
	return provider, nil
}

func stabilitySummaryConfig(targets []config.QualityTarget, order []string) config.Config {
	return config.Config{Quality: config.QualityConfig{
		Enabled:        true,
		Targets:        targets,
		Order:          order,
		PerNodeTimeout: time.Second,
		Thresholds: config.QualityThresholds{
			MinimumConfidence: 70,
		},
		Stability: config.QualityStabilityConfig{
			SummaryInterval: time.Hour,
			HistoryWindow:   time.Hour,
			MinimumSamples:  3,
			MinimumCoverage: 1,
			StaleAfter:      90 * time.Minute,
			GoodLatencyMS:   500,
			BadLatencyMS:    3000,
		},
	}}
}

func stabilitySummaryProxy(name, provider string, now time.Time, alive bool) mihomo.Proxy {
	history := []mihomo.DelayHistory{
		{Time: now.Add(-15 * time.Minute), Delay: 120},
		{Time: now.Add(-10 * time.Minute), Delay: 130},
		{Time: now.Add(-5 * time.Minute), Delay: 125},
	}
	if !alive {
		for index := range history {
			history[index].Delay = 0
		}
	}
	return mihomo.Proxy{Name: name, ProviderName: provider, Alive: alive, History: history}
}

func stabilitySummaryReport(key NodeKey, now time.Time) Report {
	report := Report{
		Identity: key, ObservedAt: now.Add(-10 * time.Minute),
		VendorResults: map[string]VendorResult{
			"openai": {Vendor: "openai", Attempts: 2, SuccessCount: 2, Reachable: true, StatusCodes: []int{401, 401}},
		},
		SourceEvidence: []SourceEvidence{
			{Source: "ip-a", Kind: "ip", Available: true, IP: key.IP, IPFamily: key.IPFamily, ASN: "AS64500", Country: "US"},
			{Source: "ip-b", Kind: "identity", Available: true, IP: key.IP, IPFamily: key.IPFamily, ASN: "AS64500", Country: "US"},
		},
		RiskEvidence: []SourceEvidence{
			{Source: "risk-a", Kind: "risk", Available: true, Proxy: boolPtr(false), VPN: boolPtr(false), Tor: boolPtr(false), Blacklisted: boolPtr(false)},
			{Source: "risk-b", Kind: "risk", Available: true, Proxy: boolPtr(false), VPN: boolPtr(false), Tor: boolPtr(false), Blacklisted: boolPtr(false)},
		},
		ProviderAlive: true, ProviderHistoryFresh: true, StabilityScore: 80,
		Complete: true,
	}
	return ScoreReport(report)
}

func TestStabilitySummarizerProcessesConfiguredOrderWithoutSelectingAndUpdatesExistingIdentity(t *testing.T) {
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	targets := []config.QualityTarget{
		{ID: "reserve", SourceGroup: "RESERVE-GROUP", Provider: "reserve-provider", Scope: "all", Listener: "http://127.0.0.1:17991"},
		{ID: "primary", SourceGroup: "PRIMARY-GROUP", Provider: "primary-provider", Scope: "all", Listener: "http://127.0.0.1:17990"},
	}
	api := &stabilitySummaryFakeAPI{
		proxies: map[string]mihomo.Proxy{
			"RESERVE-GROUP": {Name: "RESERVE-GROUP", All: []string{"reserve-node"}},
			"PRIMARY-GROUP": {Name: "PRIMARY-GROUP", All: []string{"primary-node"}},
		},
		providers: map[string]mihomo.Provider{
			"reserve-provider": {Name: "reserve-provider", Proxies: []mihomo.Proxy{stabilitySummaryProxy("reserve-node", "reserve-provider", now, true)}},
			"primary-provider": {Name: "primary-provider", Proxies: []mihomo.Proxy{stabilitySummaryProxy("primary-node", "primary-provider", now, true)}},
		},
	}
	cfg := stabilitySummaryConfig(targets, []string{"reserve", "primary"})
	store := NewStore(t.TempDir())
	for _, key := range []NodeKey{
		{Target: "reserve", Provider: "reserve-provider", Node: "reserve-node", IPFamily: "ipv4", IP: "198.51.100.10"},
		{Target: "primary", Provider: "primary-provider", Node: "primary-node", IPFamily: "ipv4", IP: "198.51.100.11"},
	} {
		if _, err := store.SaveReport(stabilitySummaryReport(key, now)); err != nil {
			t.Fatal(err)
		}
	}

	summarizer := &StabilitySummarizer{
		API:     api,
		Reports: store,
		State:   state.NewStore(t.TempDir()+"/state.json", "CHANNEL"),
		Now:     func() time.Time { return now },
	}
	if err := summarizer.Summarize(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	if len(api.providerCalls) != 2 || !reflect.DeepEqual(api.providerCalls, []string{"reserve-provider", "primary-provider"}) {
		t.Fatalf("provider calls=%v, want configured order", api.providerCalls)
	}
	if len(api.getProxyCalls) != 2 || !reflect.DeepEqual(api.getProxyCalls, []string{"RESERVE-GROUP", "PRIMARY-GROUP"}) {
		t.Fatalf("source group calls=%v, want configured order", api.getProxyCalls)
	}
	if len(api.getProxyCalls) > 2 {
		t.Fatal("summary must not read every node through a selecting API")
	}
	history, err := store.LoadStabilityHistory()
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 2 {
		t.Fatalf("stability history entries=%d, want 2", len(history))
	}
	for _, key := range []NodeKey{
		{Target: "reserve", Provider: "reserve-provider", Node: "reserve-node", IPFamily: "ipv4", IP: "198.51.100.10"},
		{Target: "primary", Provider: "primary-provider", Node: "primary-node", IPFamily: "ipv4", IP: "198.51.100.11"},
	} {
		snapshot, err := store.LoadStabilitySnapshot(key)
		if err != nil {
			t.Fatal(err)
		}
		if !snapshot.Known || snapshot.Samples != 3 || snapshot.Identity != key {
			t.Fatalf("snapshot=%+v, want known three-sample snapshot for %s", snapshot, key.Node)
		}
		record, err := store.LoadNode(key)
		if err != nil {
			t.Fatal(err)
		}
		if record.Latest == nil || record.Latest.StabilityObservedAt != now || record.Latest.StabilityScore != snapshot.StabilityScore {
			t.Fatalf("latest=%+v, hourly stability must update recommendation input", record.Latest)
		}
	}
}

func TestStabilitySummarizerDoesNotInventIdentityForUnseenNode(t *testing.T) {
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	target := config.QualityTarget{ID: "reserve", SourceGroup: "GROUP", Provider: "provider", Scope: "all", Listener: "http://127.0.0.1:17990"}
	api := &stabilitySummaryFakeAPI{
		proxies:   map[string]mihomo.Proxy{"GROUP": {Name: "GROUP", All: []string{"new-node"}}},
		providers: map[string]mihomo.Provider{"provider": {Name: "provider", Proxies: []mihomo.Proxy{stabilitySummaryProxy("new-node", "provider", now, true)}}},
	}
	store := NewStore(t.TempDir())
	summarizer := &StabilitySummarizer{API: api, Reports: store, Now: func() time.Time { return now }}
	if err := summarizer.Summarize(context.Background(), stabilitySummaryConfig([]config.QualityTarget{target}, []string{"reserve"})); err != nil {
		t.Fatal(err)
	}
	if snapshots, err := store.LoadStability(); err != nil {
		t.Fatal(err)
	} else if len(snapshots) != 0 {
		t.Fatalf("stability snapshots=%v, must not create an IP-less identity", snapshots)
	}
}

func TestStabilitySummarizerMarksInsufficientHistoryUnknownWithoutChangingBaseline(t *testing.T) {
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	target := config.QualityTarget{ID: "target", SourceGroup: "GROUP", Provider: "provider", Scope: "all", Listener: "http://127.0.0.1:17990"}
	proxy := stabilitySummaryProxy("node", "provider", now, true)
	proxy.History = proxy.History[:1]
	api := &stabilitySummaryFakeAPI{
		proxies:   map[string]mihomo.Proxy{"GROUP": {Name: "GROUP", All: []string{"node"}}},
		providers: map[string]mihomo.Provider{"provider": {Name: "provider", Proxies: []mihomo.Proxy{proxy}}},
	}
	store := NewStore(t.TempDir())
	key := NodeKey{Target: "target", Provider: "provider", Node: "node", IPFamily: "ipv4", IP: "198.51.100.13"}
	if _, err := store.SaveReport(stabilitySummaryReport(key, now)); err != nil {
		t.Fatal(err)
	}
	before, err := store.LoadNode(key)
	if err != nil || before.Baseline == nil {
		t.Fatalf("initial record=%+v err=%v, want baseline", before, err)
	}
	summarizer := &StabilitySummarizer{API: api, Reports: store, Now: func() time.Time { return now }}
	if err := summarizer.Summarize(context.Background(), stabilitySummaryConfig([]config.QualityTarget{target}, []string{"target"})); err != nil {
		t.Fatal(err)
	}
	snapshot, err := store.LoadStabilitySnapshot(key)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Known || snapshot.StabilityScore != 0 {
		t.Fatalf("snapshot=%+v, insufficient history must stay unknown", snapshot)
	}
	after, err := store.LoadNode(key)
	if err != nil {
		t.Fatal(err)
	}
	if after.Baseline == nil || after.Baseline.Score != before.Baseline.Score {
		t.Fatalf("baseline changed from %+v to %+v", before.Baseline, after.Baseline)
	}
	if after.Latest == nil || after.Latest.Complete || after.Latest.Eligible || !after.Latest.ProviderHistoryFresh {
		t.Fatalf("latest=%+v, recent but insufficient history must be fresh yet non-eligible", after.Latest)
	}
}

func TestStabilitySummarizerContinuesAfterTargetFailure(t *testing.T) {
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	targets := []config.QualityTarget{
		{ID: "bad", SourceGroup: "BAD-GROUP", Provider: "bad-provider", Scope: "all", Listener: "http://127.0.0.1:17990"},
		{ID: "good", SourceGroup: "GOOD-GROUP", Provider: "good-provider", Scope: "all", Listener: "http://127.0.0.1:17991"},
	}
	api := &stabilitySummaryFakeAPI{
		providerErr: map[string]error{"bad-provider": errors.New("provider refresh failed")},
		proxies: map[string]mihomo.Proxy{
			"BAD-GROUP":  {Name: "BAD-GROUP", All: []string{"bad-node"}},
			"GOOD-GROUP": {Name: "GOOD-GROUP", All: []string{"good-node"}},
		},
		providers: map[string]mihomo.Provider{
			"good-provider": {Name: "good-provider", Proxies: []mihomo.Proxy{stabilitySummaryProxy("good-node", "good-provider", now, true)}},
		},
	}
	store := NewStore(t.TempDir())
	key := NodeKey{Target: "good", Provider: "good-provider", Node: "good-node", IPFamily: "ipv4", IP: "198.51.100.12"}
	if _, err := store.SaveReport(stabilitySummaryReport(key, now)); err != nil {
		t.Fatal(err)
	}
	summarizer := &StabilitySummarizer{API: api, Reports: store, Now: func() time.Time { return now }}
	err := summarizer.Summarize(context.Background(), stabilitySummaryConfig(targets, []string{"bad", "good"}))
	if err == nil || !strings.Contains(err.Error(), "bad") {
		t.Fatalf("summary error=%v, want failed first target", err)
	}
	if _, err := store.LoadStabilitySnapshot(key); err != nil {
		t.Fatalf("later target was not summarized after first failure: %v", err)
	}
}

func TestStabilitySummarizerStopsBeforeReadsWhenMihomoLinkIsLost(t *testing.T) {
	api := &stabilitySummaryFakeAPI{heartbeatErr: errors.New("connection refused")}
	target := config.QualityTarget{ID: "target", SourceGroup: "GROUP", Provider: "provider", Scope: "all", Listener: "http://127.0.0.1:17990"}
	summarizer := &StabilitySummarizer{API: api, Reports: NewStore(t.TempDir())}
	err := summarizer.Summarize(context.Background(), stabilitySummaryConfig([]config.QualityTarget{target}, []string{"target"}))
	if !errors.Is(err, ErrQualityLink) {
		t.Fatalf("summary error=%v, want quality-link error", err)
	}
	if len(api.getProxyCalls) != 0 || len(api.providerCalls) != 0 {
		t.Fatalf("API reads=%v/%v after link loss, want none", api.getProxyCalls, api.providerCalls)
	}
}
