package runtime

import (
	"context"
	"errors"
	"time"
)

var ErrMihomoLinkLost = errors.New("mihomo control link lost")

type Link interface {
	Heartbeat(context.Context) error
	RunCycle(context.Context) error
}

type Reloadable interface {
	Reload() error
}

type CycleInterval interface {
	CycleInterval() time.Duration
}

type ReloadInterval interface {
	ReloadInterval() time.Duration
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
	tickerInterval := r.cfg.Tick
	ticker := time.NewTicker(tickerInterval)
	defer ticker.Stop()
	var lostAt time.Time
	var lastCycle time.Time
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
				return ErrMihomoLinkLost
			}
		} else {
			lostAt = time.Time{}
			if reloadable, ok := r.link.(Reloadable); ok {
				_ = reloadable.Reload()
			}
			cycleInterval := r.cfg.Tick
			if intervalProvider, ok := r.link.(CycleInterval); ok {
				if interval := intervalProvider.CycleInterval(); interval > 0 {
					cycleInterval = interval
				}
			}
			now := time.Now()
			if lastCycle.IsZero() || now.Sub(lastCycle) >= cycleInterval {
				if err := r.link.RunCycle(ctx); err != nil {
					return err
				}
				lastCycle = now
			}
			nextTickerInterval := r.cfg.Tick
			if intervalProvider, ok := r.link.(ReloadInterval); ok {
				if interval := intervalProvider.ReloadInterval(); interval > 0 {
					nextTickerInterval = interval
				}
			}
			if nextTickerInterval != tickerInterval {
				ticker.Stop()
				ticker = time.NewTicker(nextTickerInterval)
				tickerInterval = nextTickerInterval
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}
