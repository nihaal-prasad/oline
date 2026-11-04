package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/nihaal-prasad/oline/internal/core"
)

// TestRevshellChannelPane checks that a reverse-shell channel reported by the engine
// appears under "command panes", that clicking it opens a reverse-shell pane, and
// that a close event removes it from the list.
func TestRevshellChannelPane(t *testing.T) {
	m := testModel(t)
	m.w, m.h = 120, 40
	m.paneRoot = newLeaf(core.Main)
	m.focusPane = m.paneRoot
	m.windows = []*windowState{{root: m.paneRoot, focus: m.paneRoot, name: "1"}}
	m.winIdx = 0
	m.running = true
	m.resize()

	// The engine announces an opened channel.
	m.applyChannel("foothold", 4444, "/opt/oline/engagements/x/channels/foothold", true)

	out := ansi.Strip(m.renderRoster(m.rightW, m.bodyH))
	if !strings.Contains(out, "command panes") {
		t.Fatalf("command panes section missing after a channel opened:\n%s", out)
	}
	if !strings.Contains(out, "foothold") || !strings.Contains(out, ":4444") {
		t.Errorf("channel row should list the name and port:\n%s", out)
	}

	// Clicking the channel's row opens a reverse-shell pane for it.
	var row int
	for y, name := range m.rosterChannelRows {
		if name == "foothold" {
			row = y
		}
	}
	if row == 0 {
		t.Fatal("channel 'foothold' has no clickable roster row")
	}
	m.mousePress(m.rosterX+1, row, true)
	if m.focusRev() == nil || m.focusPane.agent != revID("foothold") {
		t.Fatalf("clicking the channel row should open/focus its reverse-shell pane, got %q", m.focusPane.agent)
	}

	// A close event drops the channel from the command-panes list.
	m.applyChannel("foothold", 4444, "/opt/oline/engagements/x/channels/foothold", false)
	if _, ok := m.channels["foothold"]; ok {
		t.Error("channel should be removed from the model after a close event")
	}
}

// TestRevshellSendEchoesLine checks that sending on a focused reverse-shell pane
// echoes the typed line into the pane transcript (the shell's reply arrives via the
// tail stream, which needs a sandbox and so is not exercised here).
func TestRevshellSendEchoesLine(t *testing.T) {
	m := testModel(t)
	m.w, m.h = 120, 40
	m.paneRoot = newLeaf(core.Main)
	m.focusPane = m.paneRoot
	m.windows = []*windowState{{root: m.paneRoot, focus: m.paneRoot, name: "1"}}
	m.winIdx = 0
	m.running = true
	m.resize()

	m.applyChannel("c1", 9001, "/opt/oline/engagements/x/channels/c1", true)
	m.openRevPane("c1")
	rs := m.focusRev()
	if rs == nil {
		t.Fatal("expected a focused reverse-shell pane")
	}
	m.sendRev(rs, "whoami")
	tr := ansi.Strip(m.shellTranscript(rs.id).render(80, true, nil))
	if !strings.Contains(tr, "$ whoami") {
		t.Errorf("sent command should be echoed in the pane transcript, got:\n%s", tr)
	}
}

// TestInputLineFitsWidth guards the fix for a long entry wrapping off-screen: with
// any of the command-line prompts, the composed `<prompt><textinput view>` must fit
// within the terminal width so the end of a long entry stays visible. The widest
// prompt is the 2-cell answer/budget emoji with two spaces.
func TestInputLineFitsWidth(t *testing.T) {
	m := testModel(t)
	m.w, m.h = 80, 30
	m.running = true
	m.resize()
	m.input.Focus()
	long := strings.Repeat("abcdefghij ", 20) // ~220 cols, far wider than the line
	for _, r := range long {
		m.input, _ = m.input.Update(key(string(r)))
	}
	view := m.input.View()
	for _, prompt := range []string{" > ", " ❓ ", " ⏱ ", " $ ", " ⇄ "} {
		if w := lipgloss.Width(prompt) + lipgloss.Width(view); w > m.w {
			t.Errorf("input line with prompt %q is %d cols, exceeds width %d (wraps off-screen)", prompt, w, m.w)
		}
	}
	// The end of the typed text must be visible (the widget scrolled to the cursor).
	if !strings.Contains(ansi.Strip(view), "abcdefghij") {
		t.Errorf("input view does not show the end of a long entry:\n%s", ansi.Strip(view))
	}
}

// TestShellOnlyPaneCanRetarget guards the fix for the auto-opened Shell tab being
// stuck: when a shell pane is the only pane in its window, selecting an agent/info
// view must convert that pane in place (reaping the shell) rather than doing nothing.
func TestShellOnlyPaneCanRetarget(t *testing.T) {
	m := runningModel(t)
	// Make the active window contain ONLY a shell pane (like the default Shell tab).
	shell := m.createShellLeaf()
	m.paneRoot, m.focusPane = shell, shell
	m.windows[0].root, m.windows[0].focus = shell, shell
	id := shell.agent
	if m.focusShell() == nil {
		t.Fatal("precondition: focused pane should be the shell")
	}

	m.setFocus(core.Main)

	if m.focusPane.shell {
		t.Error("shell-only pane should have been converted away from a shell")
	}
	if m.focusPane.agent != core.Main {
		t.Errorf("pane should now show Main, got %q", m.focusPane.agent)
	}
	if _, ok := m.shells[id]; ok {
		t.Error("the shell's state should have been reaped on conversion")
	}
}
