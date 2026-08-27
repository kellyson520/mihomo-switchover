package decision

import (
	"testing"
	"time"

	"mihomo-guardian/internal/state"
)

func TestDecisionSwitchesOnlyAfterThreeCorroboratedFailures(t *testing.T) {
	e := NewEngine(DecisionConfig{FailuresBeforeSwitch: 3, RecoveriesBeforeSwitch: 2, MinHold: 0})
	current := state.Default("MAIN")
	input := Input{CurrentHealthy: false, BackupHealthy: true, BackupNode: "backup-1", Now: time.Unix(100, 0)}
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

func TestNoVerifiedBackupNeverSwitches(t *testing.T) {
	action := NewEngine(DecisionConfig{FailuresBeforeSwitch: 1}).Evaluate(
		state.Default("MAIN"), Input{CurrentHealthy: false, BackupHealthy: false, Now: time.Unix(100, 0)})
	if action.Kind != Noop {
		t.Fatalf("action=%s", action.Kind)
	}
}

func TestRecoveryNeedsTwoHealthyCycles(t *testing.T) {
	e := NewEngine(DecisionConfig{FailuresBeforeSwitch: 1, RecoveriesBeforeSwitch: 2, MinHold: 0})
	current := state.Default("MAIN")
	current.CurrentChannel = "BACKUP-USA"
	input := Input{CurrentHealthy: true, BackupHealthy: true, Now: time.Unix(100, 0)}
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
		state.Default("MAIN"), Input{CurrentHealthy: true, BackupHealthy: true, PurityWarning: "datacenter", Now: time.Unix(100, 0)})
	if action.Kind != Noop {
		t.Fatalf("purity changed routing: %s", action.Kind)
	}
}
