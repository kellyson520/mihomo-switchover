package decision

import (
	"testing"
	"time"

	"mihomo-guardian/internal/state"
)

func TestDecisionSwitchesOnlyAfterThreeCorroboratedFailures(t *testing.T) {
	e := NewEngine(DecisionConfig{FailuresBeforeSwitch: 3, RecoveriesBeforeSwitch: 2, MinHold: 0})
	current := state.Default("MAIN")
	input := Input{CurrentHealthy: false, CurrentHealthySample: true, CurrentFailureEligible: true, BackupHealthy: true, BackupNode: "backup-1", Now: time.Unix(100, 0)}
	for i := 1; i <= 2; i++ {
		action := e.Evaluate(current, input)
		if action.Kind != Noop {
			t.Fatalf("failure %d action=%s", i, action.Kind)
		}
		current = action.State
	}
	action := e.Evaluate(current, input)
	if action.Kind != SwitchChannel || action.Channel != "BACKUP-USA" {
		t.Fatalf("action=%+v", action)
	}
}

func TestStickyNodeWinsOverFasterAlternative(t *testing.T) {
	got := ChooseNode("main-old", []Candidate{{Name: "main-fast", Healthy: true, Score: 99, Order: 0}, {Name: "main-old", Healthy: true, Score: 10, Order: 1}})
	if got != "main-old" {
		t.Fatalf("node=%s", got)
	}
}

func TestDecisionDoesNotCountCachedHealthAsANewFailure(t *testing.T) {
	e := NewEngine(DecisionConfig{FailuresBeforeSwitch: 3, RecoveriesBeforeSwitch: 2, MinHold: 0})
	current := state.Default("MAIN")
	now := time.Unix(100, 0)

	first := e.Evaluate(current, Input{CurrentHealthy: false, CurrentHealthySample: true, CurrentFailureEligible: true, BackupHealthy: true, Now: now})
	if first.Kind != Noop || first.State.FailureStreak != 1 {
		t.Fatalf("first failed sample=%+v", first)
	}

	cached := e.Evaluate(first.State, Input{CurrentHealthy: false, CurrentHealthySample: false, CurrentFailureEligible: true, BackupHealthy: true, Now: now.Add(15 * time.Second)})
	if cached.Kind != Noop || cached.State.FailureStreak != 1 {
		t.Fatalf("cached failure was counted again=%+v", cached)
	}

	second := e.Evaluate(cached.State, Input{CurrentHealthy: false, CurrentHealthySample: true, CurrentFailureEligible: true, BackupHealthy: true, Now: now.Add(5 * time.Minute)})
	if second.Kind != Noop || second.State.FailureStreak != 2 {
		t.Fatalf("second failed sample=%+v", second)
	}

	third := e.Evaluate(second.State, Input{CurrentHealthy: false, CurrentHealthySample: true, CurrentFailureEligible: true, BackupHealthy: true, Now: now.Add(10 * time.Minute)})
	if third.Kind != SwitchChannel || third.Channel != "BACKUP-USA" {
		t.Fatalf("third failed sample=%+v", third)
	}
}

func TestDecisionDoesNotCountNonRouteFailure(t *testing.T) {
	e := NewEngine(DecisionConfig{FailuresBeforeSwitch: 1, RecoveriesBeforeSwitch: 2, MinHold: 0})
	action := e.Evaluate(state.Default("MAIN"), Input{
		CurrentHealthy:         false,
		CurrentHealthySample:   true,
		CurrentFailureEligible: false,
		BackupHealthy:          true,
		Now:                    time.Unix(100, 0),
	})
	if action.Kind != Noop || action.State.FailureStreak != 0 {
		t.Fatalf("non-route failure changed decision state: %+v", action)
	}
}

func TestNoVerifiedBackupNeverSwitches(t *testing.T) {
	action := NewEngine(DecisionConfig{FailuresBeforeSwitch: 1}).Evaluate(
		state.Default("MAIN"), Input{CurrentHealthy: false, CurrentHealthySample: true, CurrentFailureEligible: true, BackupHealthy: false, Now: time.Unix(100, 0)})
	if action.Kind != Noop {
		t.Fatalf("action=%s", action.Kind)
	}
}

func TestRecoveryNeedsTwoHealthyCycles(t *testing.T) {
	e := NewEngine(DecisionConfig{FailuresBeforeSwitch: 1, RecoveriesBeforeSwitch: 2, MinHold: 0})
	current := state.Default("MAIN")
	current.CurrentChannel = "BACKUP-USA"
	input := Input{CurrentHealthy: true, CurrentHealthySample: true, BackupHealthy: true, Now: time.Unix(100, 0)}
	first := e.Evaluate(current, input)
	if first.Kind != Noop {
		t.Fatalf("first recovery action=%s", first.Kind)
	}
	second := e.Evaluate(first.State, input)
	if second.Kind != SwitchChannel || second.Channel != "MAIN" {
		t.Fatalf("second recovery action=%+v", second)
	}
}

func TestPurityWarningCannotCreateSwitchAction(t *testing.T) {
	action := NewEngine(DecisionConfig{FailuresBeforeSwitch: 1}).Evaluate(
		state.Default("MAIN"), Input{CurrentHealthy: true, CurrentHealthySample: true, BackupHealthy: true, PurityWarning: "datacenter", Now: time.Unix(100, 0)})
	if action.Kind != Noop {
		t.Fatalf("purity changed routing: %s", action.Kind)
	}
}

func TestForcedChannelBlocksAutomaticSwitchUntilExpiry(t *testing.T) {
	e := NewEngine(DecisionConfig{FailuresBeforeSwitch: 1, MinHold: 0})
	now := time.Unix(100, 0)
	current := state.Default("MAIN")
	current.CurrentChannel = "BACKUP-USA"
	current.ForcedChannel = "BACKUP-USA"
	current.ForceUntil = now.Add(time.Minute)
	action := e.Evaluate(current, Input{CurrentHealthy: true, CurrentHealthySample: true, BackupHealthy: true, Now: now})
	if action.Kind != Noop || action.State.CurrentChannel != "BACKUP-USA" {
		t.Fatalf("forced action=%+v", action)
	}

	expired := action.State
	expired.ForceUntil = now.Add(-time.Second)
	recovered := e.Evaluate(expired, Input{CurrentHealthy: true, CurrentHealthySample: true, BackupHealthy: true, Now: now})
	if recovered.State.ForcedChannel != "" || recovered.State.ForceUntil != (time.Time{}) {
		t.Fatalf("force did not expire: %+v", recovered.State)
	}
}
