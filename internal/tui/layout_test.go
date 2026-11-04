package tui

import (
	"testing"

	"github.com/nihaal-prasad/oline/internal/core"
)

// centerLayout builds the default-style tree: splitH(Main, splitV(summary, diagram)).
func centerLayout(ratio float64) (*pane, *pane) {
	main := newLeaf(core.Main)
	center := &pane{dir: splitV, ratio: 0.5, a: newSummaryLeaf(), b: newDiagramLeaf()}
	center.a.parent, center.b.parent = center, center
	root := &pane{dir: splitH, ratio: ratio, a: main, b: center}
	main.parent, center.parent = root, root
	return root, main
}

// TestAnyShellPane checks the shell-pane detector used to decide whether to add the
// default shell tab.
func TestAnyShellPane(t *testing.T) {
	m := testModel(t)
	m.paneRoot = newLeaf(core.Main)
	m.windows = []*windowState{{root: m.paneRoot, focus: m.paneRoot, name: "1"}}
	if m.anyShellPane() {
		t.Error("no shell pane exists yet")
	}
	sh := newShellLeaf("Shell#1")
	m.windows = append(m.windows, &windowState{root: sh, focus: sh, name: "2"})
	if !m.anyShellPane() {
		t.Error("a shell window should be detected")
	}
}

// TestLayoutSnapshotSkipsLoneShell checks a lone shell tab is not persisted (so it
// doesn't come back as a stray Main tab and pile up across resumes).
func TestLayoutSnapshotSkipsLoneShell(t *testing.T) {
	m := testModel(t)
	m.w, m.h = 120, 40
	m.paneRoot = newLeaf(core.Main)
	m.focusPane = m.paneRoot
	sh := newShellLeaf("Shell#1")
	m.windows = []*windowState{
		{root: m.paneRoot, focus: m.paneRoot, name: "1"},
		{root: sh, focus: sh, name: "2"},
	}
	m.winIdx = 0
	m.running = true
	m.resize()

	m.refreshLayoutSnapshot()
	if len(m.savedLayout) != 1 {
		t.Errorf("a lone shell tab should be skipped in the saved layout, got %d windows", len(m.savedLayout))
	}
}

// TestSavedPaneRoundTrip checks a pane tree serializes and rebuilds with the same
// structure, ratios, and wired parent pointers.
func TestSavedPaneRoundTrip(t *testing.T) {
	root, _ := centerLayout(0.4)
	back := fromSavedPane(toSavedPane(root))

	if back.leaf || back.dir != splitH || back.ratio != 0.4 {
		t.Fatalf("root should be splitH @0.4, got leaf=%v dir=%v ratio=%v", back.leaf, back.dir, back.ratio)
	}
	if !back.a.leaf || back.a.agent != core.Main {
		t.Errorf("left should be the Main leaf, got %+v", back.a)
	}
	if back.b.leaf || back.b.dir != splitV {
		t.Fatalf("right should be a splitV node, got %+v", back.b)
	}
	if !back.b.a.summary || !back.b.b.diagram {
		t.Errorf("center should hold summary then diagram, got a.summary=%v b.diagram=%v", back.b.a.summary, back.b.b.diagram)
	}
	if back.a.parent != back || back.b.parent != back || back.b.a.parent != back.b {
		t.Error("parent pointers were not re-wired on restore")
	}
}

// TestShellPaneSavesAsMain checks a live shell pane (which can't be restored) is
// persisted as a Main view rather than a dangling shell.
func TestShellPaneSavesAsMain(t *testing.T) {
	sp := toSavedPane(newShellLeaf("Shell#1"))
	if !sp.Leaf || sp.Summary || sp.Diagram || sp.Agent != string(core.Main) {
		t.Errorf("shell pane should save as a Main leaf, got %+v", sp)
	}
}

// TestRefreshAndRestoreLayout checks the UI snapshot captures the layout and that
// restoreLayout rebuilds it (three panes: Main + Summary + Diagram).
func TestRefreshAndRestoreLayout(t *testing.T) {
	m := testModel(t)
	m.w, m.h = 120, 40
	root, main := centerLayout(0.5)
	m.paneRoot, m.focusPane = root, main
	m.windows = []*windowState{{root: root, focus: main, name: "1"}}
	m.winIdx = 0
	m.running = true
	m.resize()

	m.refreshLayoutSnapshot()
	if len(m.savedLayout) != 1 || m.savedLayout[0].Root == nil {
		t.Fatalf("layout snapshot not captured: %+v", m.savedLayout)
	}

	m.windows = nil // wipe, then restore from the snapshot
	m.restoreLayout(m.savedLayout, m.savedWinIdx)
	if got := len(leaves(m.paneRoot)); got != 3 {
		t.Errorf("restored layout has %d panes, want 3", got)
	}
	var sawSummary, sawDiagram, sawMain bool
	for _, l := range leaves(m.paneRoot) {
		switch {
		case l.summary:
			sawSummary = true
		case l.diagram:
			sawDiagram = true
		case l.agent == core.Main:
			sawMain = true
		}
	}
	if !sawMain || !sawSummary || !sawDiagram {
		t.Errorf("restored panes main=%v summary=%v diagram=%v", sawMain, sawSummary, sawDiagram)
	}
}
