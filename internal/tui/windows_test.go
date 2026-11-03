package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"

	"github.com/nihaal-prasad/oline/internal/core"
	"github.com/nihaal-prasad/oline/internal/event"
)

// TestSpawnPromptShownInChildPane verifies the orchestrator's task prompt is placed
// in the new subagent's pane (visible by scrolling to the top).
func TestSpawnPromptShownInChildPane(t *testing.T) {
	m := runningModel(t)
	m.apply(event.Spawned{Parent: core.Main, Child: "recon#1", Role: "recon", Prompt: "scan 10.0.0.5 for open ports and services"})
	txt := m.tr("recon#1").plainText(true)
	if !strings.Contains(txt, "scan 10.0.0.5 for open ports and services") {
		t.Errorf("child pane should show the orchestrator's task prompt:\n%s", txt)
	}
}

// runningModel returns a model wired up as if an engagement had started: one
// window, Main pane focused.
func runningModel(t *testing.T) *model {
	t.Helper()
	m := testModel(t)
	m.w, m.h = 120, 40
	m.addAgent(core.Main)
	m.paneRoot = newLeaf(core.Main)
	m.focusPane = m.paneRoot
	m.windows = []*windowState{{root: m.paneRoot, focus: m.paneRoot, name: "1"}}
	m.winIdx = 0
	m.shells = map[core.AgentID]*shellState{}
	m.shellCh = make(chan shellMsg, 16)
	m.running = true
	m.resize()
	return m
}

func TestWindowsCreateSwitchKill(t *testing.T) {
	m := runningModel(t)
	win1Root := m.paneRoot

	m.newWindow()
	if len(m.windows) != 2 || m.winIdx != 1 {
		t.Fatalf("newWindow: windows=%d idx=%d", len(m.windows), m.winIdx)
	}
	if m.paneRoot == win1Root {
		t.Error("newWindow did not switch to a fresh pane tree")
	}

	m.selectWindow(0)
	if m.winIdx != 0 || m.paneRoot != win1Root {
		t.Fatalf("selectWindow(0): idx=%d root-restored=%v", m.winIdx, m.paneRoot == win1Root)
	}

	m.nextWindow()
	if m.winIdx != 1 {
		t.Fatalf("nextWindow: idx=%d", m.winIdx)
	}

	m.killWindow()
	if len(m.windows) != 1 || m.winIdx != 0 {
		t.Fatalf("killWindow: windows=%d idx=%d", len(m.windows), m.winIdx)
	}
	// The last window can't be closed.
	m.killWindow()
	if len(m.windows) != 1 {
		t.Errorf("killWindow removed the last window: %d", len(m.windows))
	}

	// The tab strip shows only with more than one window.
	if m.renderTabs() != "" {
		t.Error("tab strip should be empty with a single window")
	}
	m.newWindow()
	if !strings.Contains(ansi.Strip(m.renderTabs()), "2") {
		t.Errorf("tab strip missing window 2: %q", ansi.Strip(m.renderTabs()))
	}
}

func TestShellPane(t *testing.T) {
	m := runningModel(t)
	orderBefore := len(m.order)

	m.openShellPane()
	sh := m.focusShell()
	if sh == nil {
		t.Fatal("focused pane is not a shell after openShellPane")
	}
	if len(leaves(m.paneRoot)) != 2 {
		t.Errorf("openShellPane did not split: %d leaves", len(leaves(m.paneRoot)))
	}
	if len(m.order) != orderBefore {
		t.Errorf("shell pane leaked into the agent roster (order %d -> %d)", orderBefore, len(m.order))
	}
	if _, ok := m.states[sh.id]; ok {
		t.Error("shell pane should not appear in the states map")
	}

	id := sh.id
	m.applyShellMsg(shellMsg{id: id, chunk: "hello from shell\n"})
	m.applyShellMsg(shellMsg{id: id, chunk: "\n" + cwdMarker + "/opt/oline/engagements/work\n"})
	m.applyShellMsg(shellMsg{id: id, done: true, code: 0})

	txt := ansi.Strip(transcriptText(m.transcripts[id]))
	if !strings.Contains(txt, "hello from shell") {
		t.Errorf("shell output missing:\n%s", txt)
	}
	if strings.Contains(txt, cwdMarker) {
		t.Errorf("cwd marker was not stripped:\n%s", txt)
	}
	if sh.cwd != "/opt/oline/engagements/work" {
		t.Errorf("cwd not updated from marker: %q", sh.cwd)
	}
	if !strings.Contains(txt, "exit 0") {
		t.Errorf("missing exit status:\n%s", txt)
	}
	if sh.busy {
		t.Error("shell still marked busy after completion")
	}

	// Closing the shell pane reaps its state.
	m.closePaneCmd()
	if _, ok := m.shells[id]; ok {
		t.Error("closing the shell pane did not reap its state")
	}
}

// TestRosterBoxFillsHeight verifies the right-side pane renders exactly the pane
// height even when the help content overflows, so the bottom border is never
// clipped off.
func TestRosterBoxFillsHeight(t *testing.T) {
	m := runningModel(t)
	m.w, m.h = 120, 20 // short: the key-help list overflows the available height
	m.resize()
	m.addAgent("recon#1")
	m.addAgent("web#1")

	out := ansi.Strip(m.renderRoster(m.rightW, m.bodyH))
	got := len(strings.Split(out, "\n"))
	if got != m.bodyH {
		t.Fatalf("roster rendered %d lines, want %d (bottom border clipped)", got, m.bodyH)
	}
}

// TestRosterPaneToggle verifies the right Options pane shows by default and that
// toggling it off (Ctrl-b ?) hides the whole pane and gives the main area the full
// width.
func TestRosterPaneToggle(t *testing.T) {
	m := runningModel(t)
	if !m.showRoster {
		t.Fatal("the Options pane should be on by default")
	}
	// Visible: the main area leaves room for the roster, which renders the help.
	mainWithRoster := m.mainW
	out := ansi.Strip(m.renderRoster(m.rightW, m.bodyH))
	if !strings.Contains(out, "Ctrl-b Commands") {
		t.Error("the Options pane should include the command cheat sheet")
	}

	// Toggle off: resize() reclaims the width for the main panes.
	m.showRoster = false
	m.resize()
	if m.mainW <= mainWithRoster {
		t.Errorf("hiding the Options pane should widen the main area: %d -> %d", mainWithRoster, m.mainW)
	}
	if m.rightW != 0 {
		t.Errorf("hidden Options pane should have zero width, got %d", m.rightW)
	}
	if !strings.Contains(m.viewRunning(), "oline") {
		t.Error("running view should still render with the Options pane hidden")
	}
}

// TestToggleToolHitsClickedPane reproduces the cross-pane expand bug: the same
// agent shown in a wide and a narrow pane (which wrap text differently) must still
// toggle the tool block under the click in the pane that was actually clicked, not
// one positioned by the other pane's last render.
func TestToggleToolHitsClickedPane(t *testing.T) {
	m := runningModel(t)
	tr := m.tr(core.Main)
	tr.appendText(strings.Repeat("word ", 90)) // long text: wraps to different line counts by width
	tr.finalizeOpen()
	tr.startTool("⚙  bash", "⚙  bash full")
	tr.appendToolOut("some output")

	wide, narrow := newLeaf(core.Main), newLeaf(core.Main)
	root := &pane{dir: splitH, ratio: 0.5, a: wide, b: narrow}
	wide.parent, narrow.parent = root, root
	m.paneRoot, m.focusPane = root, wide
	wide.x, wide.y, wide.w, wide.h = 0, 1, 100, 30
	narrow.x, narrow.y, narrow.w, narrow.h = 100, 1, 24, 30

	var seg *segment
	for _, s := range tr.segs {
		if s.kind == segTool {
			seg = s
		}
	}
	if seg == nil {
		t.Fatal("no tool segment")
	}

	m.refreshLeaf(wide) // learn the tool's row in the WIDE pane
	wideStart := seg.startLine
	m.refreshLeaf(narrow) // narrow rendered last: shared startLine now reflects narrow
	if seg.startLine == wideStart {
		t.Skip("the two widths did not wrap differently; test inconclusive")
	}

	clickY := wide.y + 2 + wideStart - wide.vp.YOffset
	m.toggleToolAt(wide, wide.x+1, clickY)
	if !seg.expanded {
		t.Errorf("clicking the tool in the wide pane should expand it (wide row=%d, stale shared row=%d)", wideStart, seg.startLine)
	}
}
