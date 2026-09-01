package quality

import (
	"context"
	"errors"
	"testing"
	"time"

	"mihomo-guardian/internal/config"
	"mihomo-guardian/internal/mihomo"
	"mihomo-guardian/internal/probe"
)

func TestScannerRejectsConcurrentRunBeforeSecondSelection(t *testing.T) {
	apiData := map[string]mihomo.Proxy{
		"GROUP": {Name: "GROUP", All: []string{"node"}},
	}
	providers := map[string]mihomo.Provider{
		"provider": {Name: "provider", Proxies: []mihomo.Proxy{scannerProxy("node", true, 0)}},
	}
	apiOne := &scannerFakeAPI{proxies: apiData, providers: providers}
	apiTwo := &scannerFakeAPI{proxies: apiData, providers: providers}
	store := NewStore(t.TempDir())
	cfg := scannerConfig([]string{"target"}, []config.QualityTarget{{
		ID: "target", SourceGroup: "GROUP", Provider: "provider", Scope: "all", Listener: "http://127.0.0.1:17990",
	}})
	started := make(chan struct{})
	release := make(chan struct{})
	first := &Scanner{
		API: apiOne, Reports: store,
		External: func(string, time.Duration) (*probe.ExternalClient, error) { return nil, nil },
		Collect: func(context.Context, *probe.ExternalClient, []SourceSpec, []VendorProbeSpec) (Collection, error) {
			close(started)
			<-release
			return scannerCompleteCollection(), nil
		},
	}
	second := &Scanner{
		API: apiTwo, Reports: store,
		External: func(string, time.Duration) (*probe.ExternalClient, error) { return nil, nil },
		Collect: func(context.Context, *probe.ExternalClient, []SourceSpec, []VendorProbeSpec) (Collection, error) {
			t.Fatal("concurrent scanner reached evidence collection")
			return Collection{}, nil
		},
	}
	firstDone := make(chan error, 1)
	go func() { firstDone <- first.Scan(context.Background(), cfg) }()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("first scanner did not reach evidence collection")
	}
	if err := second.Scan(context.Background(), cfg); !errors.Is(err, ErrScanBusy) {
		t.Fatalf("second scanner error=%v, want scan-busy before any selection", err)
	}
	if len(apiTwo.selections) != 0 || apiTwo.heartbeats != 0 {
		t.Fatalf("second scanner API activity: selections=%v heartbeats=%d", apiTwo.selections, apiTwo.heartbeats)
	}
	close(release)
	if err := <-firstDone; err != nil {
		t.Fatal(err)
	}
}
