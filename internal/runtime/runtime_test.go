package runtime

import (
	"context"
	"errors"
	"testing"
	"time"
)

type fakeLink struct {
	heartbeats     []error
	heartbeatCalls int
	decisionCalls  int
	terminated     bool
}

func (f *fakeLink) Heartbeat(context.Context) error {
	index := f.heartbeatCalls
	f.heartbeatCalls++
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
	if !fake.terminated {
		t.Fatal("mihomo was not terminated")
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
