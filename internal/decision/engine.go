package decision

import (
	"time"

	"mihomo-guardian/internal/state"
)

type ActionKind string

const (
	Noop          ActionKind = "noop"
	SwitchChannel ActionKind = "switch_channel"
)

type DecisionConfig struct {
	MainChannel            string
	BackupChannel          string
	FailuresBeforeSwitch   int
	RecoveriesBeforeSwitch int
	MinHold                time.Duration
}

type Input struct {
	CurrentHealthy         bool
	CurrentHealthySample   bool
	CurrentFailureEligible bool
	BackupHealthy          bool
	BackupNode             string
	PurityWarning          string
	Now                    time.Time
}

type Candidate struct {
	Name    string
	Healthy bool
	Score   int
	Order   int
}

type Action struct {
	Kind    ActionKind
	Channel string
	Reason  string
	State   state.State
}

type Engine struct {
	cfg DecisionConfig
}

func NewEngine(cfg DecisionConfig) *Engine {
	if cfg.MainChannel == "" {
		cfg.MainChannel = "MAIN"
	}
	if cfg.BackupChannel == "" {
		cfg.BackupChannel = "BACKUP-USA"
	}
	if cfg.FailuresBeforeSwitch < 1 {
		cfg.FailuresBeforeSwitch = 3
	}
	if cfg.RecoveriesBeforeSwitch < 1 {
		cfg.RecoveriesBeforeSwitch = 2
	}
	if cfg.MinHold < 0 {
		cfg.MinHold = 0
	}
	return &Engine{cfg: cfg}
}

func (e *Engine) Evaluate(current state.State, input Input) Action {
	now := input.Now
	if now.IsZero() {
		now = time.Now()
	}
	if current.CurrentChannel == "" {
		current.CurrentChannel = e.cfg.MainChannel
	}
	if current.ProviderLocks == nil {
		current.ProviderLocks = make(map[string]state.ProviderLock)
	}
	if current.ForcedChannel != "" {
		if current.ForceUntil.IsZero() || now.Before(current.ForceUntil) {
			return Action{Kind: Noop, Reason: "manual channel force is active", State: current}
		}
		current.ForcedChannel = ""
		current.ForceUntil = time.Time{}
	}
	if current.CurrentChannel == e.cfg.MainChannel {
		current.RecoveryStreak = 0
		if !input.CurrentHealthySample {
			return Action{Kind: Noop, Reason: "current health sample is cached", State: current}
		}
		if input.CurrentHealthy {
			current.FailureStreak = 0
			return Action{Kind: Noop, Reason: "main healthy", State: current}
		}
		if !input.CurrentFailureEligible {
			current.FailureStreak = 0
			return Action{Kind: Noop, Reason: "main failure is not attributable to route", State: current}
		}
		current.FailureStreak++
		if current.FailureStreak < e.cfg.FailuresBeforeSwitch {
			return Action{Kind: Noop, Reason: "main failure threshold not reached", State: current}
		}
		if !input.BackupHealthy {
			return Action{Kind: Noop, Reason: "backup candidate is not verified", State: current}
		}
		if now.Before(current.HoldUntil) {
			return Action{Kind: Noop, Reason: "channel is in hold period", State: current}
		}
		current.CurrentChannel = e.cfg.BackupChannel
		current.FailureStreak = 0
		current.RecoveryStreak = 0
		current.HoldUntil = now.Add(e.cfg.MinHold)
		return Action{Kind: SwitchChannel, Channel: e.cfg.BackupChannel, Reason: "main failed and verified backup is available", State: current}
	}

	if current.CurrentChannel == e.cfg.BackupChannel {
		current.FailureStreak = 0
		if !input.CurrentHealthySample {
			return Action{Kind: Noop, Reason: "main recovery sample is not fresh", State: current}
		}
		if !input.CurrentHealthy {
			current.RecoveryStreak = 0
			return Action{Kind: Noop, Reason: "main has not recovered", State: current}
		}
		current.RecoveryStreak++
		if current.RecoveryStreak < e.cfg.RecoveriesBeforeSwitch {
			return Action{Kind: Noop, Reason: "main recovery threshold not reached", State: current}
		}
		if now.Before(current.HoldUntil) {
			return Action{Kind: Noop, Reason: "channel is in hold period", State: current}
		}
		current.CurrentChannel = e.cfg.MainChannel
		current.RecoveryStreak = 0
		current.HoldUntil = now.Add(e.cfg.MinHold)
		return Action{Kind: SwitchChannel, Channel: e.cfg.MainChannel, Reason: "main recovered and passed hysteresis", State: current}
	}
	return Action{Kind: Noop, Reason: "unknown current channel", State: current}
}

func ChooseNode(sticky string, candidates []Candidate) string {
	for _, candidate := range candidates {
		if candidate.Name == sticky && candidate.Healthy {
			return candidate.Name
		}
	}
	best := Candidate{}
	found := false
	for _, candidate := range candidates {
		if !candidate.Healthy {
			continue
		}
		if !found || candidate.Order < best.Order || (candidate.Order == best.Order && candidate.Score > best.Score) {
			best = candidate
			found = true
		}
	}
	if !found {
		return ""
	}
	return best.Name
}
