package tui

import (
	"strings"
	"testing"

	"github.com/nihaal-prasad/oline/internal/core"
	"github.com/nihaal-prasad/oline/internal/engine"
)

func TestCycleMode(t *testing.T) {
	m := testModel(t)
	m.sess = &engine.Session{Mode: core.Automatic}
	want := []core.RunMode{core.Planner, core.Watcher, core.Automatic}
	for i, w := range want {
		m.cycleMode()
		if m.sess.Mode != w {
			t.Errorf("cycle %d: mode = %v, want %v", i, m.sess.Mode, w)
		}
	}
}

func TestPaneTextTerminal(t *testing.T) {
	m, _ := termModel(t)
	m.openTermPane()
	ts := m.focusTerm()
	if ts == nil {
		t.Fatal("no terminal pane")
	}
	ts.emu.Write([]byte("operator ran nmap"))
	label, content := m.paneText(m.focusPane)
	if !strings.Contains(label, "terminal") {
		t.Errorf("terminal pane label should say terminal, got %q", label)
	}
	if !strings.Contains(content, "operator ran nmap") {
		t.Errorf("paneText should capture the terminal screen, got:\n%s", content)
	}
}
