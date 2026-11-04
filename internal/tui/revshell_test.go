package tui

import (
	"strings"
	"testing"

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
