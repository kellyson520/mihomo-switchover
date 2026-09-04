package runtime

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"mihomo-guardian/internal/config"
	"mihomo-guardian/internal/mihomo"
	"mihomo-guardian/internal/probe"
	"mihomo-guardian/internal/quality"
	"mihomo-guardian/internal/state"
)

type fakeAPI struct {
	groups             map[string]mihomo.Proxy
	providers          map[string]mihomo.Provider
	delays             map[string]error
	heartbeatErr       error
	delayCalls         []string
	healthCheckCalls   []string
	healthCheckUpdates map[string]mihomo.Provider
	setCalls           []struct{ group, node string }
}

func (f *fakeAPI) Heartbeat(context.Context) error { return f.heartbeatErr }

func (f *fakeAPI) GetProxy(_ context.Context, name string) (mihomo.Proxy, error) {
	return f.groups[name], nil
}

func (f *fakeAPI) GetProvider(_ context.Context, name string) (mihomo.Provider, error) {
	provider, ok := f.providers[name]
	if !ok {
		return mihomo.Provider{}, errors.New("provider not found")
	}
	return provider, nil
}

func (f *fakeAPI) HealthCheckProvider(_ context.Context, name string) error {
	f.healthCheckCalls = append(f.healthCheckCalls, name)
	if provider, ok := f.healthCheckUpdates[name]; ok {
		f.providers[name] = provider
	}
	return nil
}

func (f *fakeAPI) SetProxy(_ context.Context, group, node string) error {
	f.setCalls = append(f.setCalls, struct{ group, node string }{group, node})
	proxy := f.groups[group]
	proxy.Now = node
	f.groups[group] = proxy
	return nil
}

func (f *fakeAPI) Delay(_ context.Context, node, _ string, _ time.Duration) (int, error) {
	f.delayCalls = append(f.delayCalls, node)
	if err := f.delays[node]; err != nil {
		return 0, err
	}
	return 50, nil
}

func TestServiceUsesProviderHealthWhenNodeDelayEndpointIsUnsupported(t *testing.T) {
	api := &fakeAPI{
		groups: map[string]mihomo.Proxy{
			"BACKUP-USA": {Name: "BACKUP-USA", Now: "backup-new", All: []string{"backup-old", "backup-new"}},
		},
		providers: map[string]mihomo.Provider{
			"backup-provider": {Name: "backup-provider", Proxies: []mihomo.Proxy{
				{Name: "backup-old", Alive: true, History: []mihomo.DelayHistory{{Delay: 80}}},
				{Name: "backup-new", Alive: false, History: []mihomo.DelayHistory{{Delay: 0}}},
			}},
		},
		delays: map[string]error{"backup-old": errors.New("mihomo node delay endpoint is unsupported")},
	}
	cfg := testServiceConfig()
	cfg.Providers = config.ProvidersConfig{Backup: "backup-provider"}
	service := NewService(cfg, api, fakeExternal{healthy: false}, state.NewStore(t.TempDir()+"/state.json", "MAIN"), nil)

	chosen, healthy := service.ensureProvider(context.Background(), "backup", cfg.Groups.Backup, api.groups[cfg.Groups.Backup])
	if !healthy || chosen != "backup-old" {
		t.Fatalf("chosen=%q healthy=%v calls=%+v", chosen, healthy, api.setCalls)
	}
	if len(api.setCalls) != 1 || api.setCalls[0].node != "backup-old" {
		t.Fatalf("set calls=%+v", api.setCalls)
	}
	if len(api.delayCalls) != 0 {
		t.Fatalf("unsupported direct node delay was called: %v", api.delayCalls)
	}
}

func TestServiceDoesNotTreatProviderWithoutHealthHistoryAsVerified(t *testing.T) {
	api := &fakeAPI{
		groups: map[string]mihomo.Proxy{
			"BACKUP-USA": {Name: "BACKUP-USA", Now: "backup-old", All: []string{"backup-old"}},
		},
		providers: map[string]mihomo.Provider{
			"backup-provider": {Name: "backup-provider", Proxies: []mihomo.Proxy{{Name: "backup-old", Alive: true}}},
		},
		delays: map[string]error{},
	}
	cfg := testServiceConfig()
	cfg.Providers = config.ProvidersConfig{Backup: "backup-provider"}
	service := NewService(cfg, api, fakeExternal{healthy: false}, state.NewStore(t.TempDir()+"/state.json", "MAIN"), nil)

	chosen, healthy := service.ensureProvider(context.Background(), "backup", cfg.Groups.Backup, api.groups[cfg.Groups.Backup])
	if healthy || chosen != "" {
		t.Fatalf("provider without health history was accepted: chosen=%q healthy=%v", chosen, healthy)
	}
	if len(api.delayCalls) != 0 {
		t.Fatalf("provider metadata failure fell back to direct node delay: %v", api.delayCalls)
	}
}

func TestServiceTriggersNativeProviderHealthCheckForUnverifiedProvider(t *testing.T) {
	api := &fakeAPI{
		groups: map[string]mihomo.Proxy{
			"MAIN": {Name: "MAIN", Now: "main-current", All: []string{"main-current"}},
		},
		providers: map[string]mihomo.Provider{
			"main-provider": {Name: "main-provider", Proxies: []mihomo.Proxy{{Name: "main-current", Alive: false, History: []mihomo.DelayHistory{{Delay: 0}}}}},
		},
	}
	cfg := testServiceConfig()
	cfg.Providers = config.ProvidersConfig{Main: "main-provider"}
	cfg.Decision.ProbeInterval = time.Hour
	service := NewService(cfg, api, fakeExternal{healthy: false}, state.NewStore(t.TempDir()+"/state.json", "MAIN"), nil)

	if _, healthy := service.ensureProvider(context.Background(), "main", cfg.Groups.Main, api.groups[cfg.Groups.Main]); healthy {
		t.Fatal("unhealthy provider was accepted")
	}
	if _, healthy := service.ensureProvider(context.Background(), "main", cfg.Groups.Main, api.groups[cfg.Groups.Main]); healthy {
		t.Fatal("unhealthy provider was accepted on retry")
	}
	if len(api.healthCheckCalls) != 1 || api.healthCheckCalls[0] != "main-provider" {
		t.Fatalf("health check calls=%v, want one rate-limited native check", api.healthCheckCalls)
	}
}

func TestServiceReturnsToMainAfterNativeProviderHealthCheckRecoversIt(t *testing.T) {
	api := &fakeAPI{
		groups: map[string]mihomo.Proxy{
			"CHANNEL":    {Name: "CHANNEL", Now: "BACKUP-USA", All: []string{"MAIN", "BACKUP-USA"}},
			"MAIN":       {Name: "MAIN", Now: "main-current", All: []string{"main-current"}},
			"BACKUP-USA": {Name: "BACKUP-USA", Now: "backup-current", All: []string{"backup-current"}},
		},
		providers: map[string]mihomo.Provider{
			"main-provider":   {Name: "main-provider", Proxies: []mihomo.Proxy{{Name: "main-current", Alive: false, History: []mihomo.DelayHistory{{Delay: 0}}}}},
			"backup-provider": {Name: "backup-provider", Proxies: []mihomo.Proxy{{Name: "backup-current", Alive: true, History: []mihomo.DelayHistory{{Delay: 100}}}}},
		},
		healthCheckUpdates: map[string]mihomo.Provider{
			"main-provider": {Name: "main-provider", Proxies: []mihomo.Proxy{{Name: "main-current", Alive: true, History: []mihomo.DelayHistory{{Delay: 120}}}}},
		},
	}
	cfg := testServiceConfig()
	cfg.Providers = config.ProvidersConfig{Main: "main-provider", Backup: "backup-provider"}
	cfg.Decision.RecoveriesBeforeSwitch = 2
	cfg.Decision.ProbeInterval = time.Hour
	service := NewService(cfg, api, fakeExternal{healthy: true}, state.NewStore(t.TempDir()+"/state.json", "MAIN"), nil)

	for cycle := 0; cycle < 3; cycle++ {
		if err := service.RunCycle(context.Background()); err != nil {
			t.Fatalf("cycle %d: %v", cycle+1, err)
		}
	}
	if api.groups["CHANNEL"].Now != "MAIN" {
		t.Fatalf("channel=%q, want MAIN after native recovery check", api.groups["CHANNEL"].Now)
	}
	if len(api.healthCheckCalls) != 1 || api.healthCheckCalls[0] != "main-provider" {
		t.Fatalf("health check calls=%v, want one main-provider check", api.healthCheckCalls)
	}
}

func TestServiceKeepsCurrentHealthyProviderNodeWithoutPersistedLock(t *testing.T) {
	api := &fakeAPI{
		groups: map[string]mihomo.Proxy{
			"BACKUP-USA": {Name: "BACKUP-USA", Now: "backup-current", All: []string{"backup-first", "backup-current"}},
		},
		providers: map[string]mihomo.Provider{
			"backup-provider": {Name: "backup-provider", Proxies: []mihomo.Proxy{
				{Name: "backup-first", Alive: true, History: []mihomo.DelayHistory{{Delay: 10}}},
				{Name: "backup-current", Alive: true, History: []mihomo.DelayHistory{{Delay: 100}}},
			}},
		},
		delays: map[string]error{},
	}
	cfg := testServiceConfig()
	cfg.Providers = config.ProvidersConfig{Backup: "backup-provider"}
	service := NewService(cfg, api, fakeExternal{healthy: true}, state.NewStore(t.TempDir()+"/state.json", "MAIN"), nil)

	chosen, healthy := service.ensureProvider(context.Background(), "backup", cfg.Groups.Backup, api.groups[cfg.Groups.Backup])
	if !healthy || chosen != "backup-current" {
		t.Fatalf("current healthy node was not retained: chosen=%q healthy=%v", chosen, healthy)
	}
	if len(api.setCalls) != 0 {
		t.Fatalf("current healthy node was unnecessarily changed: %v", api.setCalls)
	}
}

type fakeExternal struct{ healthy bool }

func (f fakeExternal) Check(_ context.Context, spec config.ProbeSpec) probe.Result {
	result := probe.Result{ProbeID: spec.ID}
	if f.healthy {
		result.Class = probe.ReachableHTTP
	} else {
		result.Class = probe.NetworkError
	}
	return result
}

type countingExternal struct {
	healthy bool
	checks  int
}

func (f *countingExternal) Check(_ context.Context, spec config.ProbeSpec) probe.Result {
	f.checks++
	result := probe.Result{ProbeID: spec.ID}
	if f.healthy {
		result.Class = probe.ReachableHTTP
	} else {
		result.Class = probe.NetworkError
	}
	return result
}

func testServiceConfig() config.Config {
	return config.Config{
		Mihomo:   config.MihomoConfig{API: "http://127.0.0.1:9090", Proxy: "http://127.0.0.1:7890", SecretFile: "/tmp/secret"},
		Groups:   config.GroupsConfig{Channel: "CHANNEL", Main: "MAIN", Backup: "BACKUP-USA"},
		Decision: config.DecisionConfig{Mode: "auto", Interval: time.Second, FailuresBeforeSwitch: 1, RecoveriesBeforeSwitch: 1, MinHold: 0, LinkLossGrace: time.Second, CriticalQuorum: 1},
		Probes:   []config.ProbeSpec{{ID: "openai", URL: "https://api.openai.com/v1/models", Critical: true, Enabled: true, ExpectedMin: 200, ExpectedMax: 499, Timeout: time.Second, DelayTimeout: time.Second}},
	}
}

func TestServiceCachesPublicProbeDuringProbeInterval(t *testing.T) {
	api := &fakeAPI{groups: map[string]mihomo.Proxy{
		"CHANNEL":    {Name: "CHANNEL", Now: "MAIN", All: []string{"MAIN", "BACKUP-USA"}},
		"MAIN":       {Name: "MAIN", Now: "main", All: []string{"main"}},
		"BACKUP-USA": {Name: "BACKUP-USA", Now: "backup", All: []string{"backup"}},
	}, delays: map[string]error{}}
	external := &countingExternal{healthy: true}
	cfg := testServiceConfig()
	cfg.Decision.ProbeInterval = time.Hour
	service := NewService(cfg, api, external, state.NewStore(t.TempDir()+"/state.json", "MAIN"), nil)

	if err := service.RunCycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := service.RunCycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	if external.checks != 1 {
		t.Fatalf("public probe checks=%d, want one fresh check within interval", external.checks)
	}
}

func TestServiceUsesFastFailureRechecksAfterFirstFailure(t *testing.T) {
	api := &fakeAPI{groups: map[string]mihomo.Proxy{
		"CHANNEL":    {Name: "CHANNEL", Now: "MAIN", All: []string{"MAIN", "BACKUP-USA"}},
		"MAIN":       {Name: "MAIN", Now: "main", All: []string{"main"}},
		"BACKUP-USA": {Name: "BACKUP-USA", Now: "backup", All: []string{"backup"}},
	}, delays: map[string]error{}}
	external := &countingResultExternal{class: probe.NetworkError}
	cfg := testServiceConfig()
	cfg.Providers = config.ProvidersConfig{}
	cfg.Decision.ProbeInterval = time.Hour
	cfg.Decision.FailureRecheckInterval = 30 * time.Second
	cfg.Decision.FailuresBeforeSwitch = 3
	cfg.Decision.MinHold = 0
	cfg.Probes = append(cfg.Probes, config.ProbeSpec{ID: "gemini", URL: "https://gemini.example/health", Critical: true, Enabled: true})
	store := state.NewStore(t.TempDir()+"/state.json", "MAIN")
	service := NewService(cfg, api, external, store, nil)
	clock := time.Unix(1000, 0).UTC()
	service.clock = func() time.Time { return clock }

	if err := service.RunCycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	if external.Count() != 2 {
		t.Fatalf("first probe count=%d, want 2", external.Count())
	}
	clock = clock.Add(15 * time.Second)
	if err := service.RunCycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	if external.Count() != 2 {
		t.Fatalf("cached failure was probed again at 15s: count=%d", external.Count())
	}
	clock = clock.Add(15 * time.Second)
	if err := service.RunCycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	if external.Count() != 4 {
		t.Fatalf("first fast recheck count=%d, want 4", external.Count())
	}
	clock = clock.Add(30 * time.Second)
	if err := service.RunCycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	if api.groups["CHANNEL"].Now != "BACKUP-USA" {
		t.Fatalf("channel=%q, want BACKUP-USA after three fresh failures", api.groups["CHANNEL"].Now)
	}
}

func TestServiceMihomoNetworkHintInvalidatesHealthyProbeCache(t *testing.T) {
	api := &fakeAPI{groups: map[string]mihomo.Proxy{
		"CHANNEL":    {Name: "CHANNEL", Now: "MAIN", All: []string{"MAIN", "BACKUP-USA"}},
		"MAIN":       {Name: "MAIN", Now: "main", All: []string{"main"}},
		"BACKUP-USA": {Name: "BACKUP-USA", Now: "backup", All: []string{"backup"}},
	}, delays: map[string]error{}}
	external := &countingResultExternal{class: probe.ReachableHTTP}
	cfg := testServiceConfig()
	cfg.Providers = config.ProvidersConfig{}
	cfg.Decision.ProbeInterval = time.Hour
	cfg.Probes = append(cfg.Probes, config.ProbeSpec{ID: "gemini", URL: "https://gemini.example/health", Critical: true, Enabled: true})
	store := state.NewStore(t.TempDir()+"/state.json", "MAIN")
	service := NewService(cfg, api, external, store, nil)
	clock := time.Unix(2000, 0).UTC()
	service.clock = func() time.Time { return clock }

	if err := service.RunCycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	if external.Count() != 2 {
		t.Fatalf("initial probe count=%d, want 2", external.Count())
	}
	service.ObserveMihomoError(mihomo.LogHint{Category: mihomo.LogHintNetwork})
	clock = clock.Add(15 * time.Second)
	if err := service.RunCycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	if external.Count() != 4 {
		t.Fatalf("network hint did not invalidate cache: count=%d", external.Count())
	}
	service.ObserveMihomoError(mihomo.LogHint{Category: mihomo.LogHintNone})
	clock = clock.Add(15 * time.Second)
	if err := service.RunCycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	if external.Count() != 4 {
		t.Fatalf("non-network hint caused another probe: count=%d", external.Count())
	}
}

func TestServiceRunsCriticalProbesConcurrently(t *testing.T) {
	api := &fakeAPI{groups: map[string]mihomo.Proxy{
		"CHANNEL":    {Name: "CHANNEL", Now: "MAIN", All: []string{"MAIN", "BACKUP-USA"}},
		"MAIN":       {Name: "MAIN", Now: "main", All: []string{"main"}},
		"BACKUP-USA": {Name: "BACKUP-USA", Now: "backup", All: []string{"backup"}},
	}, delays: map[string]error{}}
	external := newBarrierExternal(2)
	cfg := testServiceConfig()
	cfg.Providers = config.ProvidersConfig{}
	cfg.Decision.ProbeInterval = time.Hour
	cfg.Probes = append(cfg.Probes, config.ProbeSpec{ID: "gemini", URL: "https://gemini.example/health", Critical: true, Enabled: true})
	service := NewService(cfg, api, external, state.NewStore(t.TempDir()+"/state.json", "MAIN"), nil)

	done := make(chan error, 1)
	go func() { done <- service.RunCycle(context.Background()) }()
	seen := map[string]bool{}
	for i := 0; i < 2; i++ {
		select {
		case id := <-external.entered:
			seen[id] = true
		case <-time.After(time.Second):
			t.Fatal("critical probes were not started concurrently")
		}
	}
	close(external.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if len(seen) != 2 {
		t.Fatalf("probe ids=%v, want two distinct probes", seen)
	}
}

type countingResultExternal struct {
	mu    sync.Mutex
	count int
	class probe.ResultClass
}

func (f *countingResultExternal) Check(_ context.Context, spec config.ProbeSpec) probe.Result {
	f.mu.Lock()
	f.count++
	f.mu.Unlock()
	return probe.Result{ProbeID: spec.ID, Class: f.class}
}

func (f *countingResultExternal) Count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.count
}

type barrierExternal struct {
	entered chan string
	release chan struct{}
}

func newBarrierExternal(size int) *barrierExternal {
	return &barrierExternal{entered: make(chan string, size), release: make(chan struct{})}
}

func (f *barrierExternal) Check(_ context.Context, spec config.ProbeSpec) probe.Result {
	f.entered <- spec.ID
	<-f.release
	return probe.Result{ProbeID: spec.ID, Class: probe.ReachableHTTP}
}

func TestServiceSwitchesToVerifiedStickyBackup(t *testing.T) {
	api := &fakeAPI{groups: map[string]mihomo.Proxy{
		"CHANNEL":    {Name: "CHANNEL", Now: "MAIN", All: []string{"MAIN", "BACKUP-USA"}},
		"MAIN":       {Name: "MAIN", Now: "main-old", All: []string{"main-old", "main-new"}},
		"BACKUP-USA": {Name: "BACKUP-USA", Now: "backup-old", All: []string{"backup-old", "backup-new"}},
	}, delays: map[string]error{}}
	store := state.NewStore(t.TempDir()+"/state.json", "MAIN")
	service := NewService(testServiceConfig(), api, fakeExternal{healthy: false}, store, nil)
	if err := service.RunCycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(api.setCalls) != 1 || api.setCalls[0].group != "CHANNEL" || api.setCalls[0].node != "BACKUP-USA" {
		t.Fatalf("set calls=%+v", api.setCalls)
	}
	got, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if got.CurrentChannel != "BACKUP-USA" || got.ProviderLocks["backup"].Node != "backup-old" {
		t.Fatalf("state=%+v", got)
	}
}

func TestServiceRestoresPersistedMainNodeBeforeRecovery(t *testing.T) {
	api := &fakeAPI{groups: map[string]mihomo.Proxy{
		"CHANNEL":    {Name: "CHANNEL", Now: "BACKUP-USA", All: []string{"MAIN", "BACKUP-USA"}},
		"MAIN":       {Name: "MAIN", Now: "main-new", All: []string{"main-new", "main-old"}},
		"BACKUP-USA": {Name: "BACKUP-USA", Now: "backup-old", All: []string{"backup-old"}},
	}, delays: map[string]error{}}
	store := state.NewStore(t.TempDir()+"/state.json", "MAIN")
	initial := state.Default("BACKUP-USA")
	initial.ProviderLocks["main"] = state.ProviderLock{Provider: "main", Group: "MAIN", Node: "main-old"}
	if err := store.Save(initial); err != nil {
		t.Fatal(err)
	}
	service := NewService(testServiceConfig(), api, fakeExternal{healthy: true}, store, nil)
	if err := service.RunCycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(api.setCalls) != 2 || api.setCalls[0].group != "MAIN" || api.setCalls[0].node != "main-old" || api.setCalls[1].group != "CHANNEL" {
		t.Fatalf("set calls=%+v", api.setCalls)
	}
}

func TestServiceRefusesUnexpectedChannelSelection(t *testing.T) {
	api := &fakeAPI{groups: map[string]mihomo.Proxy{
		"CHANNEL":    {Name: "CHANNEL", Now: "DIRECT", All: []string{"MAIN", "BACKUP-USA"}},
		"MAIN":       {Name: "MAIN", Now: "main-old", All: []string{"main-old"}},
		"BACKUP-USA": {Name: "BACKUP-USA", Now: "backup-old", All: []string{"backup-old"}},
	}, delays: map[string]error{}}
	service := NewService(testServiceConfig(), api, fakeExternal{healthy: true}, state.NewStore(t.TempDir()+"/state.json", "MAIN"), nil)

	err := service.RunCycle(context.Background())
	if err == nil || !strings.Contains(err.Error(), "unexpected channel") {
		t.Fatalf("unexpected channel was not rejected: %v", err)
	}
	for _, call := range api.setCalls {
		if call.group == "CHANNEL" {
			t.Fatalf("unexpected channel caused a channel write: %+v", api.setCalls)
		}
	}
}

func TestServiceReloadsStateBeforeApplyingTheNextCycle(t *testing.T) {
	api := &fakeAPI{groups: map[string]mihomo.Proxy{
		"CHANNEL":    {Name: "CHANNEL", Now: "MAIN", All: []string{"MAIN", "BACKUP-USA"}},
		"MAIN":       {Name: "MAIN", Now: "main-old", All: []string{"main-old"}},
		"BACKUP-USA": {Name: "BACKUP-USA", Now: "backup-old", All: []string{"backup-old"}},
	}, delays: map[string]error{}}
	external := &fakeExternal{healthy: true}
	store := state.NewStore(t.TempDir()+"/state.json", "MAIN")
	service := NewService(testServiceConfig(), api, external, store, nil)
	if err := service.RunCycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	forced := state.Default("MAIN")
	forced.ForcedChannel = "MAIN"
	forced.ForceUntil = time.Now().Add(time.Hour)
	if err := store.Save(forced); err != nil {
		t.Fatal(err)
	}
	external.healthy = false
	api.setCalls = nil
	if err := service.RunCycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, call := range api.setCalls {
		if call.group == "CHANNEL" {
			t.Fatalf("stale in-memory state ignored persisted force: %+v", api.setCalls)
		}
	}
}

func serviceQualityConfig() config.QualityConfig {
	return config.QualityConfig{
		Enabled: true, FullScanInterval: 720 * time.Hour,
		Thresholds: config.QualityThresholds{BaselineDropPoints: 20, MinimumConfidence: 70, CandidateMinimumScore: 60},
		Stability:  config.QualityStabilityConfig{StaleAfter: 26 * time.Hour},
		Targets:    []config.QualityTarget{{ID: "primary", SourceGroup: "MAIN", Provider: "main-provider", Scope: "locked", LockKey: "main", Listener: "http://127.0.0.1:17990"}},
		Order:      []string{"primary"},
	}
}

func serviceQualityRecommendation(target config.QualityTarget, node, ip string, score, baseline int, now time.Time) quality.Recommendation {
	return quality.Recommendation{
		Target: target.ID, SourceGroup: target.SourceGroup, Provider: target.Provider, Node: node,
		Identity:   quality.NodeKey{Target: target.ID, Provider: target.Provider, Node: node, IPFamily: "ipv4", IP: ip},
		ReportedAt: now.Add(-time.Hour), ExpiresAt: now.Add(24 * time.Hour), EffectiveScore: score,
		BaselineScore: baseline, ConfidencePercent: 85, Complete: true, Connected: true,
		ProviderAlive: true, ProviderHistoryFresh: true, Reason: "eligible",
	}
}

func TestServiceAppliesValidatedRecommendationOnlyToItsSourceGroup(t *testing.T) {
	now := time.Now().UTC()
	api := &fakeAPI{
		groups: map[string]mihomo.Proxy{
			"CHANNEL":    {Name: "CHANNEL", Now: "MAIN", All: []string{"MAIN", "BACKUP-USA"}},
			"MAIN":       {Name: "MAIN", Now: "sticky", All: []string{"sticky", "better"}},
			"BACKUP-USA": {Name: "BACKUP-USA", Now: "backup", All: []string{"backup"}},
		},
		providers: map[string]mihomo.Provider{
			"main-provider": {Name: "main-provider", Proxies: []mihomo.Proxy{
				{Name: "sticky", Alive: true, History: []mihomo.DelayHistory{{Time: now, Delay: 80}}},
				{Name: "better", Alive: true, History: []mihomo.DelayHistory{{Time: now, Delay: 40}}},
			}},
			"backup-provider": {Name: "backup-provider", Proxies: []mihomo.Proxy{{Name: "backup", Alive: true, History: []mihomo.DelayHistory{{Time: now, Delay: 60}}}}},
		},
	}
	cfg := testServiceConfig()
	cfg.Providers = config.ProvidersConfig{Main: "main-provider", Backup: "backup-provider"}
	cfg.Quality = serviceQualityConfig()
	stateStore := state.NewStore(t.TempDir()+"/state.json", "MAIN")
	initial := state.Default("MAIN")
	initial.ProviderLocks["main"] = state.ProviderLock{Provider: "main-provider", Group: "MAIN", Node: "sticky"}
	if err := stateStore.Save(initial); err != nil {
		t.Fatal(err)
	}
	qualityStore := quality.NewStore(t.TempDir() + "/quality")
	target := cfg.Quality.Targets[0]
	if err := qualityStore.SaveRecommendations([]quality.Recommendation{
		serviceQualityRecommendation(target, "sticky", "198.51.100.10", 80, 100, now),
		serviceQualityRecommendation(target, "better", "198.51.100.11", 95, 95, now),
	}); err != nil {
		t.Fatal(err)
	}

	service := NewService(cfg, api, fakeExternal{healthy: true}, stateStore, nil, qualityStore)
	if err := service.RunCycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(api.setCalls) != 1 || api.setCalls[0].group != "MAIN" || api.setCalls[0].node != "better" {
		t.Fatalf("set calls=%+v, want one source-group recommendation write", api.setCalls)
	}
	for _, call := range api.setCalls {
		if call.group == cfg.Groups.Channel {
			t.Fatalf("quality recommendation wrote production CHANNEL: %+v", api.setCalls)
		}
	}
}

func TestServiceKeepsStickyNodeWhenHigherRecommendationIsNotNeeded(t *testing.T) {
	now := time.Now().UTC()
	api := &fakeAPI{
		groups: map[string]mihomo.Proxy{
			"CHANNEL":    {Name: "CHANNEL", Now: "MAIN", All: []string{"MAIN", "BACKUP-USA"}},
			"MAIN":       {Name: "MAIN", Now: "sticky", All: []string{"sticky", "better"}},
			"BACKUP-USA": {Name: "BACKUP-USA", Now: "backup", All: []string{"backup"}},
		},
		providers: map[string]mihomo.Provider{
			"main-provider": {Name: "main-provider", Proxies: []mihomo.Proxy{
				{Name: "sticky", Alive: true, History: []mihomo.DelayHistory{{Time: now, Delay: 80}}},
				{Name: "better", Alive: true, History: []mihomo.DelayHistory{{Time: now, Delay: 40}}},
			}},
			"backup-provider": {Name: "backup-provider", Proxies: []mihomo.Proxy{{Name: "backup", Alive: true, History: []mihomo.DelayHistory{{Time: now, Delay: 60}}}}},
		},
	}
	cfg := testServiceConfig()
	cfg.Providers = config.ProvidersConfig{Main: "main-provider", Backup: "backup-provider"}
	cfg.Quality = serviceQualityConfig()
	stateStore := state.NewStore(t.TempDir()+"/state.json", "MAIN")
	initial := state.Default("MAIN")
	initial.ProviderLocks["main"] = state.ProviderLock{Provider: "main-provider", Group: "MAIN", Node: "sticky"}
	if err := stateStore.Save(initial); err != nil {
		t.Fatal(err)
	}
	qualityStore := quality.NewStore(t.TempDir() + "/quality")
	target := cfg.Quality.Targets[0]
	if err := qualityStore.SaveRecommendations([]quality.Recommendation{
		serviceQualityRecommendation(target, "sticky", "198.51.100.10", 90, 100, now),
		serviceQualityRecommendation(target, "better", "198.51.100.11", 95, 95, now),
	}); err != nil {
		t.Fatal(err)
	}

	service := NewService(cfg, api, fakeExternal{healthy: true}, stateStore, nil, qualityStore)
	if err := service.RunCycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(api.setCalls) != 0 {
		t.Fatalf("healthy sticky node was replaced: %+v", api.setCalls)
	}
}

func TestServiceIgnoresQualityHeartbeatFailureAndKeepsRealtimePath(t *testing.T) {
	cfg := testServiceConfig()
	cfg.Providers = config.ProvidersConfig{}
	cfg.Quality = serviceQualityConfig()
	api := &fakeAPI{heartbeatErr: errors.New("quality link unavailable"), groups: map[string]mihomo.Proxy{
		"CHANNEL":    {Name: "CHANNEL", Now: "MAIN", All: []string{"MAIN", "BACKUP-USA"}},
		"MAIN":       {Name: "MAIN", Now: "main-current", All: []string{"main-current"}},
		"BACKUP-USA": {Name: "BACKUP-USA", Now: "backup-current", All: []string{"backup-current"}},
	}}
	service := NewService(cfg, api, fakeExternal{healthy: true}, state.NewStore(t.TempDir()+"/state.json", "MAIN"), nil, quality.NewStore(t.TempDir()+"/quality"))

	if err := service.RunCycle(context.Background()); err != nil {
		t.Fatalf("quality link failure must not stop realtime guardian: %v", err)
	}
	for _, call := range api.setCalls {
		if call.group == cfg.Groups.Channel {
			t.Fatalf("quality link failure caused an unexpected channel write: %+v", api.setCalls)
		}
	}
}

func TestServiceReleasesDroppedStickyBelowCandidateMinimum(t *testing.T) {
	now := time.Now().UTC()
	api := &fakeAPI{
		groups: map[string]mihomo.Proxy{
			"CHANNEL":    {Name: "CHANNEL", Now: "MAIN", All: []string{"MAIN", "BACKUP-USA"}},
			"MAIN":       {Name: "MAIN", Now: "sticky", All: []string{"sticky", "better"}},
			"BACKUP-USA": {Name: "BACKUP-USA", Now: "backup", All: []string{"backup"}},
		},
		providers: map[string]mihomo.Provider{
			"main-provider": {Name: "main-provider", Proxies: []mihomo.Proxy{
				{Name: "sticky", Alive: true, History: []mihomo.DelayHistory{{Time: now, Delay: 80}}},
				{Name: "better", Alive: true, History: []mihomo.DelayHistory{{Time: now, Delay: 40}}},
			}},
			"backup-provider": {Name: "backup-provider", Proxies: []mihomo.Proxy{{Name: "backup", Alive: true, History: []mihomo.DelayHistory{{Time: now, Delay: 60}}}}},
		},
	}
	cfg := testServiceConfig()
	cfg.Providers = config.ProvidersConfig{Main: "main-provider", Backup: "backup-provider"}
	cfg.Quality = serviceQualityConfig()
	stateStore := state.NewStore(t.TempDir()+"/state.json", "MAIN")
	initial := state.Default("MAIN")
	initial.ProviderLocks["main"] = state.ProviderLock{Provider: "main-provider", Group: "MAIN", Node: "sticky"}
	if err := stateStore.Save(initial); err != nil {
		t.Fatal(err)
	}
	qualityStore := quality.NewStore(t.TempDir() + "/quality")
	target := cfg.Quality.Targets[0]
	if err := qualityStore.SaveRecommendations([]quality.Recommendation{
		serviceQualityRecommendation(target, "sticky", "198.51.100.10", 50, 70, now),
		serviceQualityRecommendation(target, "better", "198.51.100.11", 90, 90, now),
	}); err != nil {
		t.Fatal(err)
	}

	service := NewService(cfg, api, fakeExternal{healthy: true}, stateStore, nil, qualityStore)
	if err := service.RunCycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(api.setCalls) != 1 || api.setCalls[0].group != "MAIN" || api.setCalls[0].node != "better" {
		t.Fatalf("set calls=%+v, want exact baseline drop to release low-scoring sticky", api.setCalls)
	}
}

func TestServiceQualityLinkLossDoesNotTriggerQualityDrivenSelection(t *testing.T) {
	cfg := testServiceConfig()
	cfg.Quality = serviceQualityConfig()
	api := &fakeAPI{heartbeatErr: errors.New("mihomo disconnected")}
	stateStore := state.NewStore(t.TempDir()+"/state.json", "MAIN")
	service := NewService(cfg, api, fakeExternal{healthy: true}, stateStore, nil, quality.NewStore(t.TempDir()+"/quality"))

	err := service.RunCycle(context.Background())
	if err == nil || errors.Is(err, quality.ErrQualityLink) {
		t.Fatalf("error=%v, quality link must be advisory and the real API failure must remain visible", err)
	}
	if len(api.setCalls) != 0 {
		t.Fatalf("selection occurred after mihomo link loss: %+v", api.setCalls)
	}
}
