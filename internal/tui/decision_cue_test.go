package tui

import (
	"context"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"

	"github.com/nihaal-prasad/oline/internal/decide"
	"github.com/nihaal-prasad/oline/internal/engine"
)

// cueStubDecider is a no-op engine.Decider used only to flip the on/off cue.
type cueStubDecider struct{}

func (cueStubDecider) Predicate(context.Context, string, string) (float64, error) { return 0, nil }
func (cueStubDecider) Choice(context.Context, string, string, []decide.Choice) (string, float64, error) {
	return "", 0, nil
}
func (cueStubDecider) Score(context.Context, string, string, []string) (int, float64, error) {
	return 0, 0, nil
}

// TestRosterDecisionCue: the options sidebar shows whether the decision model is on.
func TestRosterDecisionCue(t *testing.T) {
	m := runningModel(t)
	m.sess = &engine.Session{}

	off := ansi.Strip(m.renderRoster(m.rightW, m.h))
	if !strings.Contains(off, "decision model: off") {
		t.Errorf("sidebar should show the decision-off cue, got:\n%s", off)
	}

	m.sess.Decider = cueStubDecider{}
	on := ansi.Strip(m.renderRoster(m.rightW, m.h))
	if !strings.Contains(on, "decision model: on") {
		t.Errorf("sidebar should show the decision-on cue, got:\n%s", on)
	}
}
