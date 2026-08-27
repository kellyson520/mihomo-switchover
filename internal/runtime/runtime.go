package runtime

import (
	"context"
	"errors"
	"fmt"
	"time"
)

var ErrMihomoLinkLost = errors.New("mihomo control link lost")

type Link interface {
	Heartbeat(context.Context) error
	RunCycle(context.Context) error
	TerminateMihomo(context.Context) error
}

type RuntimeConfig struct {
	LinkLossGrace time.Duration
	Tick          time.Duration
}

type Runtime struct {
	link Link
	cfg  RuntimeConfig
}

func NewRuntime(link Link, cfg RuntimeConfig) *Runtime {
	if cfg.LinkLossGrace <= 0 {
		cfg.LinkLossGrace = 15 * time.Second
	}
	if cfg.Tick <= 0 {
		cfg.Tick = time.Second
	}
	return &Runtime{link: link, cfg: cfg}
}

func (r *Runtime) Run(ctx context.Context) error {
	ticker := time.NewTicker(r.cfg.Tick)
	defer ticker.Stop()
	var lostAt time.Time
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := r.link.Heartbeat(ctx)
		if err != nil {
			if lostAt.IsZero() {
				lostAt = time.Now()
			}
			if time.Since(lostAt) >= r.cfg.LinkLossGrace {
				if stopErr := r.link.TerminateMihomo(context.Background()); stopErr != nil {
					return fmt.Errorf("%w: terminate mihomo: %v", ErrMihomoLinkLost, stopErr)
				}
				return ErrMihomoLinkLost
			}
		} else {
			lostAt = time.Time{}
			if err := r.link.RunCycle(ctx); err != nil {
				return err
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}
