package engine

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nihaal-prasad/oline/internal/core"
	"github.com/nihaal-prasad/oline/internal/decide"
)

// stubDecider is a canned Decider for tests.
type stubDecider struct {
	pred       float64
	predErr    error
	choiceVal  string
	choiceConf float64
	choiceErr  error
	scoreIdx   int
	scoreGrade float64
	scoreErr   error
	calls      int32
}

func (d *stubDecider) Predicate(_ context.Context, _, _ string) (float64, error) {
	atomic.AddInt32(&d.calls, 1)
	return d.pred, d.predErr
}
func (d *stubDecider) Choice(_ context.Context, _, _ string, _ []decide.Choice) (string, float64, error) {
	atomic.AddInt32(&d.calls, 1)
	return d.choiceVal, d.choiceConf, d.choiceErr
}
func (d *stubDecider) Score(_ context.Context, _, _ string, _ []string) (int, float64, error) {
	atomic.AddInt32(&d.calls, 1)
	return d.scoreIdx, d.scoreGrade, d.scoreErr
}

func drainKind(box chan Envelope, kind MsgKind) (Envelope, bool) {
	for {
		select {
		case env := <-box:
			if env.Kind == kind {
				return env, true
			}
		default:
			return Envelope{}, false
		}
	}
}

// TestProgressCheckLowScoreNudgesBoth: a "stuck" grade steers both the subagent and
// the orchestrator (the user's chosen "Both" behavior).
func TestProgressCheckLowScoreNudgesBoth(t *testing.T) {
	a, s := newBudgetAgent(t, 10)
	main := newAgent(core.Main, AgentConfig{Role: "Main", IsOrchestrator: true}, nil, "m", s, "", "")
	s.router.register(main)
	s.Goal, s.Target = "root the box", "10.0.0.5"
	s.Decider = &stubDecider{scoreIdx: 0} // stuck / rabbit-holing

	a.progressCheck()

	if _, ok := drainKind(a.box, MsgSteer); !ok {
		t.Error("subagent should be steered to refocus on a low progress grade")
	}
	if _, ok := drainKind(main.box, MsgSteer); !ok {
		t.Error("orchestrator should be notified on a low progress grade")
	}
}

// TestProgressCheckHealthyNoNudge: a "steady/strong" grade sends nothing.
func TestProgressCheckHealthyNoNudge(t *testing.T) {
	a, s := newBudgetAgent(t, 10)
	main := newAgent(core.Main, AgentConfig{Role: "Main", IsOrchestrator: true}, nil, "m", s, "", "")
	s.router.register(main)
	s.Decider = &stubDecider{scoreIdx: 3} // strong progress

	a.progressCheck()

	if _, ok := drainKind(a.box, MsgSteer); ok {
		t.Error("a healthy agent must not be nudged")
	}
	if _, ok := drainKind(main.box, MsgSteer); ok {
		t.Error("a healthy agent must not trigger an orchestrator notice")
	}
}

// TestSpiralScheduledAtThirds: the progress check is scheduled once past 1/3 and once
// past 2/3 of the budget, and not before.
func TestSpiralScheduledAtThirds(t *testing.T) {
	a, s := newBudgetAgent(t, 9) // 9-minute budget
	s.Decider = &stubDecider{scoreIdx: 3}
	budget := 9 * time.Minute

	a.checkBudget() // fresh: elapsed ~0, below 1/3
	if a.spiralChecked != 0 {
		t.Fatalf("no check should fire before 1/3, got %d", a.spiralChecked)
	}
	a.budgetFrom = time.Now().Add(-(budget/3 + time.Second))
	a.checkBudget()
	if a.spiralChecked != 1 {
		t.Fatalf("a check should fire past 1/3, got %d", a.spiralChecked)
	}
	a.budgetFrom = time.Now().Add(-(2*budget/3 + time.Second))
	a.checkBudget()
	if a.spiralChecked != 2 {
		t.Fatalf("a second check should fire past 2/3, got %d", a.spiralChecked)
	}
}

// TestSpiralOffWhenDecisionDisabled: no decision model ⇒ no progress checks.
func TestSpiralOffWhenDecisionDisabled(t *testing.T) {
	a, _ := newBudgetAgent(t, 9)
	a.budgetFrom = time.Now().Add(-(3*time.Minute + time.Second))
	a.checkBudget()
	if a.spiralChecked != 0 {
		t.Errorf("with no decision model, spiral checks must never fire, got %d", a.spiralChecked)
	}
}

// TestRoutedTaskSchema: with the decision model on, the orchestrator's `task` tool
// drops the `agent` argument (the router picks the specialist). Off, it keeps it.
func TestRoutedTaskSchema(t *testing.T) {
	_, s := newBudgetAgent(t, 0)
	orch := newAgent(core.Main, AgentConfig{Role: "Main", IsOrchestrator: true, Tools: []string{"task", "wait"}}, nil, "m", s, "", "")

	taskParams := func() string {
		for _, d := range orch.toolDefs() {
			if d.Name == "task" {
				return string(d.Parameters)
			}
		}
		return ""
	}

	if p := taskParams(); !strings.Contains(p, `"agent"`) {
		t.Errorf("with no decision model, task must take an agent arg: %s", p)
	}
	s.Decider = &stubDecider{}
	if p := taskParams(); strings.Contains(p, `"agent"`) {
		t.Errorf("with the decision router on, task must NOT take an agent arg: %s", p)
	}
}

// TestSpiralSkippedNoBudget: an agent with no time budget is never progress-checked.
func TestSpiralSkippedNoBudget(t *testing.T) {
	a, s := newBudgetAgent(t, 0) // no limit
	s.Decider = &stubDecider{scoreIdx: 0}
	a.checkBudget()
	if a.spiralChecked != 0 {
		t.Errorf("a budget-less agent must not be progress-checked, got %d", a.spiralChecked)
	}
}
