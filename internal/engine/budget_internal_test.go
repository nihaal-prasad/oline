package engine

import (
	"testing"
	"time"

	"github.com/nihaal-prasad/oline/internal/core"
	"github.com/nihaal-prasad/oline/internal/event"
	"github.com/nihaal-prasad/oline/internal/tools"
)

func newBudgetAgent(t *testing.T, budgetMin int) (*Agent, *Session) {
	t.Helper()
	events := make(chan event.Event, 128)
	cfgFor := func(string) (AgentConfig, bool) { return AgentConfig{}, false }
	s := NewSession(tools.NewRegistry(), nil, nil, cfgFor, events, 5)
	a := newAgent("recon#1", AgentConfig{Role: "recon", BudgetMinutes: budgetMin}, nil, "m", s, "", "")
	s.router.register(a)
	return a, s
}

func drainBudget(a *Agent) (string, bool) {
	select {
	case env := <-a.box:
		return env.Body, env.Kind == MsgBudget
	default:
		return "", false
	}
}

// TestBudgetRemindersThenKill checks the engine's hardcoded escalation: three
// reminders past budget, then termination 5 minutes after the third.
func TestBudgetRemindersThenKill(t *testing.T) {
	a, _ := newBudgetAgent(t, 10) // 10-minute budget

	// Reminder 1 at >= budget.
	a.budgetFrom = time.Now().Add(-11 * time.Minute)
	a.checkBudget()
	if body, ok := drainBudget(a); !ok || a.budgetSent != 1 {
		t.Fatalf("reminder 1 not sent (sent=%d body=%q)", a.budgetSent, body)
	}
	// Reminder 2 at >= budget + gap.
	a.budgetFrom = time.Now().Add(-(10*time.Minute + budgetReminderGap + time.Second))
	a.checkBudget()
	if _, ok := drainBudget(a); !ok || a.budgetSent != 2 {
		t.Fatalf("reminder 2 not sent (sent=%d)", a.budgetSent)
	}
	// Reminder 3 at >= budget + 2*gap.
	a.budgetFrom = time.Now().Add(-(10*time.Minute + 2*budgetReminderGap + time.Second))
	a.checkBudget()
	if _, ok := drainBudget(a); !ok || a.budgetSent != 3 {
		t.Fatalf("reminder 3 not sent (sent=%d)", a.budgetSent)
	}
	// Still within grace -> no kill.
	a.checkBudget()
	if a.sm.State() == core.Killed {
		t.Fatal("agent killed before the kill grace elapsed")
	}
	// Past budget + 2*gap + killGrace -> terminated.
	a.budgetFrom = time.Now().Add(-(10*time.Minute + 2*budgetReminderGap + budgetKillGrace + time.Second))
	a.checkBudget()
	if a.sm.State() != core.Killed {
		t.Fatalf("agent not terminated after 3 reminders + grace (state=%v)", a.sm.State())
	}
}

// TestBudgetRemoveNotifies checks that removing a running agent's budget tells it.
func TestBudgetRemoveNotifies(t *testing.T) {
	a, _ := newBudgetAgent(t, 10)
	a.setBudget(0) // user removes the limit
	body, ok := drainBudget(a)
	if !ok {
		t.Fatal("no message sent when budget removed")
	}
	if want := "removed your time limit"; !contains(body, want) {
		t.Errorf("removal message = %q, want it to mention %q", body, want)
	}
	if a.budget != 0 {
		t.Errorf("budget not cleared: %v", a.budget)
	}
	// With no budget, the monitor never kills.
	a.started = time.Now().Add(-10 * time.Hour)
	a.checkBudget()
	if a.sm.State() == core.Killed {
		t.Error("agent killed despite having no budget")
	}
}

// TestSetBudgetRebasesClock: a manual budget change means "N minutes from now" —
// Remaining jumps to ~N regardless of prior elapsed, and the reminder schedule
// restarts (so a bump doesn't instantly land in kill territory).
func TestSetBudgetRebasesClock(t *testing.T) {
	a, _ := newBudgetAgent(t, 10)
	a.budgetFrom = time.Now().Add(-8 * time.Minute) // 8m already elapsed on the old budget
	a.budgetSent = 2

	a.setBudget(30 * time.Minute)

	rem, ok := a.s.router.Remaining("recon#1")
	if !ok {
		t.Fatal("expected a remaining budget after setting one")
	}
	if rem < 29*time.Minute {
		t.Errorf("manual budget change did not rebase the clock: remaining=%v, want ~30m", rem)
	}
	if a.budgetSent != 0 {
		t.Errorf("reminder schedule not reset: sent=%d", a.budgetSent)
	}
	a.checkBudget() // fresh 30m budget: must not fire a reminder yet
	if a.budgetSent != 0 {
		t.Errorf("checkBudget fired immediately after a fresh 30m budget (sent=%d)", a.budgetSent)
	}
}

// TestRuntimeReportsRemaining: the runtime tool reports time LEFT, not just elapsed.
func TestRuntimeReportsRemaining(t *testing.T) {
	a, _ := newBudgetAgent(t, 30)
	a.budgetFrom = time.Now().Add(-5 * time.Minute) // 5 of 30 minutes used
	out := a.doRuntime()
	if !contains(out, "TIME REMAINING") {
		t.Errorf("runtime should report remaining time: %s", out)
	}
	if !contains(out, "25m") && !contains(out, "24m") {
		t.Errorf("expected ~25m remaining, got: %s", out)
	}
}

// TestFinalBodyFallback: a subagent never returns an empty result.
func TestFinalBodyFallback(t *testing.T) {
	a, _ := newBudgetAgent(t, 10)
	if got := a.finalBody("done"); got != "done" {
		t.Errorf("non-empty text should pass through: %q", got)
	}
	a.lastText = "did the recon"
	if got := a.finalBody("   "); got != "did the recon" {
		t.Errorf("empty final turn should fall back to the last substantive text: %q", got)
	}
	a.lastText = ""
	if got := a.finalBody(""); !contains(got, "no summary") {
		t.Errorf("empty with no prior text should be a clear marker: %q", got)
	}
}

// TestOrchestratorNoBudget: the orchestrator is never time-limited.
func TestOrchestratorNoBudget(t *testing.T) {
	events := make(chan event.Event, 16)
	s := NewSession(tools.NewRegistry(), nil, nil, func(string) (AgentConfig, bool) { return AgentConfig{}, false }, events, 5)
	a := newAgent(core.Main, AgentConfig{Role: "Main", IsOrchestrator: true, BudgetMinutes: 1}, nil, "m", s, "", "")
	s.router.register(a)
	a.started = time.Now().Add(-10 * time.Hour)
	a.checkBudget()
	if a.sm.State() == core.Killed {
		t.Error("orchestrator was time-limited")
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
