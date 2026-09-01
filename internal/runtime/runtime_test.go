package runtime

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

type fakeLink struct {
	heartbeats     []error
	heartbeatCalls atomic.Int32
	decisionCalls  int
	terminated     bool
}

type reloadableFakeLink struct {
	fakeLink
	reloadCalls   int
	cycleInterval time.Duration
}

func (f *reloadableFakeLink) Reload() error {
	f.reloadCalls++
	return nil
}

func (f *reloadableFakeLink) CycleInterval() time.Duration { return f.cycleInterval }

func (f *fakeLink) Heartbeat(context.Context) error {
	index := int(f.heartbeatCalls.Add(1)) - 1
	if index >= len(f.heartbeats) {
		return errors.New("mihomo down")
	}
	return f.heartbeats[index]
}

func (f *fakeLink) RunCycle(context.Context) error {
	f.decisionCalls++
	return nil
}

func (f *fakeLink) TerminateMihomo(context.Context) error {
	f.terminated = true
	return nil
}

func TestRuntimeExitsAfterMihomoLinkGrace(t *testing.T) {
	fake := &fakeLink{heartbeats: []error{nil, errors.New("down"), errors.New("down"), errors.New("down")}}
	rt := NewRuntime(fake, RuntimeConfig{LinkLossGrace: 20 * time.Millisecond, Tick: 5 * time.Millisecond})
	err := rt.Run(context.Background())
	if !errors.Is(err, ErrMihomoLinkLost) {
		t.Fatalf("err=%v", err)
	}
	if fake.decisionCalls != 1 {
		t.Fatalf("decision ran after lost link: %d", fake.decisionCalls)
	}
	if fake.terminated {
		t.Fatal("guardian failure terminated mihomo")
	}
}

func TestRuntimeStopsCleanlyOnContextCancellation(t *testing.T) {
	fake := &fakeLink{heartbeats: []error{nil}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	rt := NewRuntime(fake, RuntimeConfig{LinkLossGrace: time.Second, Tick: time.Millisecond})
	if err := rt.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v", err)
	}
	if fake.terminated {
		t.Fatal("cancel unexpectedly terminated mihomo")
	}
}

func TestRuntimeChecksOptionalConfigReloadAfterHeartbeat(t *testing.T) {
	fake := &reloadableFakeLink{fakeLink: fakeLink{heartbeats: []error{nil}}}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		for fake.heartbeatCalls.Load() == 0 {
			time.Sleep(time.Millisecond)
		}
		cancel()
	}()
	rt := NewRuntime(fake, RuntimeConfig{LinkLossGrace: time.Second, Tick: time.Millisecond})
	if err := rt.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v", err)
	}
	if fake.reloadCalls != 1 {
		t.Fatalf("reload calls=%d", fake.reloadCalls)
	}
}

func TestRuntimeUsesReloadedCycleIntervalInsteadOfRunningEveryReloadTick(t *testing.T) {
	fake := &reloadableFakeLink{
		fakeLink:      fakeLink{heartbeats: make([]error, 100)},
		cycleInterval: 50 * time.Millisecond,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	rt := NewRuntime(fake, RuntimeConfig{LinkLossGrace: time.Second, Tick: time.Millisecond})
	if err := rt.Run(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err=%v", err)
	}
	if fake.decisionCalls != 1 {
		t.Fatalf("decision ran on every reload tick: %d", fake.decisionCalls)
	}
}
