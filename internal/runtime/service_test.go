package runtime

import (
	"context"
	"testing"
	"time"

	"mihomo-guardian/internal/config"
	"mihomo-guardian/internal/mihomo"
	"mihomo-guardian/internal/probe"
	"mihomo-guardian/internal/state"
)

type fakeAPI struct {
	groups   map[string]mihomo.Proxy
	delays   map[string]error
	setCalls []struct{ group, node string }
}

func (f *fakeAPI) Heartbeat(context.Context) error { return nil }

func (f *fakeAPI) GetProxy(_ context.Context, name string) (mihomo.Proxy, error) {
	return f.groups[name], nil
}

func (f *fakeAPI) SetProxy(_ context.Context, group, node string) error {
	f.setCalls = append(f.setCalls, struct{ group, node string }{group, node})
	proxy := f.groups[group]
	proxy.Now = node
	f.groups[group] = proxy
	return nil
}

func (f *fakeAPI) Delay(_ context.Context, node, _ string, _ time.Duration) (int, error) {
	if err := f.delays[node]; err != nil {
		return 0, err
	}
	return 50, nil
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

func testServiceConfig() config.Config {
	return config.Config{
		Mihomo:   config.MihomoConfig{API: "http://127.0.0.1:9090", Proxy: "http://127.0.0.1:7890", SecretFile: "/tmp/secret"},
		Groups:   config.GroupsConfig{Channel: "CHANNEL", Main: "MAIN", Backup: "BACKUP-USA"},
		Decision: config.DecisionConfig{Mode: "auto", Interval: time.Second, FailuresBeforeSwitch: 1, RecoveriesBeforeSwitch: 1, MinHold: 0, LinkLossGrace: time.Second, CriticalQuorum: 1},
		Probes:   []config.ProbeSpec{{ID: "openai", URL: "https://api.openai.com/v1/models", Critical: true, Enabled: true, ExpectedMin: 200, ExpectedMax: 499, Timeout: time.Second, DelayTimeout: time.Second}},
	}
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
