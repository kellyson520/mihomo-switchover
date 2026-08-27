package supervisor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"sync"
	"syscall"
	"time"
)

type Child interface {
	Start() error
	Wait() error
	Terminate() error
}

type SupervisorConfig struct {
	StartupAPITimeout time.Duration
}

type Supervisor struct {
	child Child
	cfg   SupervisorConfig
	once  sync.Once
	err   error
}

func New(child Child, cfg SupervisorConfig) *Supervisor {
	if cfg.StartupAPITimeout <= 0 {
		cfg.StartupAPITimeout = 30 * time.Second
	}
	return &Supervisor{child: child, cfg: cfg}
}

func (s *Supervisor) Run(ctx context.Context, ready func(context.Context) error, work func(context.Context, func() error) error) error {
	if err := s.child.Start(); err != nil {
		return fmt.Errorf("start mihomo: %w", err)
	}
	readyCtx, cancelReady := context.WithTimeout(ctx, s.cfg.StartupAPITimeout)
	readyErr := ready(readyCtx)
	cancelReady()
	if readyErr != nil {
		_ = s.terminate()
		return readyErr
	}

	workCtx, cancelWork := context.WithCancel(ctx)
	defer cancelWork()
	waitCh := make(chan error, 1)
	go func() { waitCh <- s.child.Wait() }()
	workCh := make(chan error, 1)
	go func() { workCh <- work(workCtx, s.terminate) }()

	select {
	case err := <-waitCh:
		cancelWork()
		if err == nil {
			return errors.New("mihomo exited")
		}
		return err
	case err := <-workCh:
		cancelWork()
		if err == nil {
			if stopErr := s.terminate(); stopErr != nil {
				return stopErr
			}
			return nil
		}
		_ = s.terminate()
		return err
	case <-ctx.Done():
		cancelWork()
		_ = s.terminate()
		return ctx.Err()
	}
}

func (s *Supervisor) terminate() error {
	s.once.Do(func() { s.err = s.child.Terminate() })
	return s.err
}

type ExecChild struct {
	cmd *exec.Cmd
}

func NewExecChild(binary string, args ...string) *ExecChild {
	return &ExecChild{cmd: exec.Command(binary, args...)}
}

func (c *ExecChild) Start() error {
	c.cmd.Stdout = os.Stdout
	c.cmd.Stderr = os.Stderr
	c.cmd.Stdin = os.Stdin
	return c.cmd.Start()
}

func (c *ExecChild) Wait() error { return c.cmd.Wait() }

func (c *ExecChild) Terminate() error {
	if c.cmd.Process == nil {
		return nil
	}
	if err := c.cmd.Process.Signal(syscall.SIGTERM); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return err
	}
	return nil
}

func SignalsContext(parent context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := signal.NotifyContext(parent, syscall.SIGTERM, syscall.SIGINT)
	return ctx, cancel
}
