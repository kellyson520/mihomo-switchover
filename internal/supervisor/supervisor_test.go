package supervisor

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

type fakeChild struct {
	started    bool
	terminated bool
	exit       error
	wait       chan error
}

func (f *fakeChild) Start() error {
	f.started = true
	return nil
}

func (f *fakeChild) Wait() error {
	if f.wait != nil {
		return <-f.wait
	}
	return f.exit
}

func (f *fakeChild) Terminate() error {
	f.terminated = true
	return nil
}

func TestSupervisorStopsMihomoWhenControllerAPINeverComesUp(t *testing.T) {
	child := &fakeChild{}
	s := New(child, SupervisorConfig{StartupAPITimeout: 20 * time.Millisecond})
	err := s.Run(context.Background(), func(context.Context) error {
		return errors.New("mihomo API unavailable")
	}, func(context.Context, func() error) error {
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "mihomo API unavailable") {
		t.Fatalf("err=%v", err)
	}
	if !child.started || !child.terminated {
		t.Fatalf("started=%v terminated=%v", child.started, child.terminated)
	}
}

func TestSupervisorPropagatesMihomoExit(t *testing.T) {
	child := &fakeChild{exit: errors.New("mihomo exited")}
	s := New(child, SupervisorConfig{StartupAPITimeout: time.Second})
	err := s.Run(context.Background(), func(context.Context) error { return nil }, func(context.Context, func() error) error {
		time.Sleep(20 * time.Millisecond)
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "mihomo exited") {
		t.Fatalf("err=%v", err)
	}
}

func TestSupervisorTerminatesChildWhenWorkReturns(t *testing.T) {
	child := &fakeChild{wait: make(chan error, 1)}
	s := New(child, SupervisorConfig{StartupAPITimeout: time.Second})
	err := s.Run(context.Background(), func(context.Context) error { return nil }, func(context.Context, func() error) error {
		return errors.New("work stopped")
	})
	if err == nil || !strings.Contains(err.Error(), "work stopped") {
		t.Fatalf("err=%v", err)
	}
	if !child.terminated {
		t.Fatal("mihomo child was not terminated")
	}
}
