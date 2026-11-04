package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"github.com/nihaal-prasad/oline/internal/core"
)

// TestUserPanesListsShells checks opened shells appear under a "command panes"
// section (above "info panes") and that clicking one focuses its pane.
func TestUserPanesListsShells(t *testing.T) {
	m := testModel(t)
	m.w, m.h = 120, 40
	m.paneRoot = newLeaf(core.Main)
	m.focusPane = m.paneRoot
	m.windows = []*windowState{{root: m.paneRoot, focus: m.paneRoot, name: "1"}}
	m.winIdx = 0
	m.running = true
	m.resize()

	m.openShellPane() // opens Shell#1 in the current window
	m.openShellPane() // opens Shell#2

	out := ansi.Strip(m.renderRoster(m.rightW, m.bodyH))
	up, ip := strings.Index(out, "command panes"), strings.Index(out, "info panes")
	if up < 0 || ip < 0 || up > ip {
		t.Fatalf("command panes must appear above info panes (up=%d ip=%d)\n%s", up, ip, out)
	}
	if !strings.Contains(out, "Shell#1") || !strings.Contains(out, "Shell#2") {
		t.Errorf("command panes should list the opened shells:\n%s", out)
	}
	// Clicking a listed shell focuses that shell's pane.
	var shellRow int
	for y, id := range m.rosterUserPaneRows {
		if id == core.AgentID("Shell#1") {
			shellRow = y
		}
	}
	if shellRow == 0 {
		t.Fatal("Shell#1 has no clickable roster row")
	}
	m.mousePress(m.rosterX+1, shellRow, true)
	if m.focusShell() == nil || m.focusPane.agent != core.AgentID("Shell#1") {
		t.Errorf("clicking the user-pane row should focus Shell#1, got %q", m.focusPane.agent)
	}
}

// TestClickTabSwitchesWindow checks a left-click on a window tab in the header
// switches to that window (mouse parity with Ctrl-b n), that clicking the active
// tab is a no-op, and that a header click off any tab does nothing.
func TestClickTabSwitchesWindow(t *testing.T) {
	m := testModel(t)
	m.w, m.h = 120, 40
	r1, r2, r3 := newLeaf(core.Main), newLeaf(core.Main), newLeaf(core.Main)
	m.paneRoot, m.focusPane = r1, r1
	m.windows = []*windowState{
		{root: r1, focus: r1, name: "1"},
		{root: r2, focus: r2, name: "2"},
		{root: r3, focus: r3, name: "3"},
	}
	m.winIdx = 0
	m.running = true
	m.resize()

	spans := m.tabSpans()
	if len(spans) != 3 {
		t.Fatalf("want 3 tab spans, got %d", len(spans))
	}
	hdr := m.bodyTop - 1 // the header row

	// Click the middle of window 3's tab.
	s := spans[2]
	m.mousePress((s.start+s.end)/2, hdr, false)
	if m.winIdx != 2 {
		t.Errorf("clicking tab 3 should activate window index 2, got %d", m.winIdx)
	}
	if m.paneRoot != r3 {
		t.Error("clicking tab 3 should load window 3's pane tree")
	}

	// Clicking the now-active tab keeps it put (no crash, no change).
	s = m.tabSpans()[2]
	m.mousePress((s.start+s.end)/2, hdr, false)
	if m.winIdx != 2 {
		t.Errorf("clicking the active tab should keep winIdx=2, got %d", m.winIdx)
	}

	// A header click to the left of the tab strip switches nothing.
	m.mousePress(0, hdr, false)
	if m.winIdx != 2 {
		t.Errorf("a non-tab header click should not switch windows, got %d", m.winIdx)
	}
}

// TestJumpTopBottomKeys checks Ctrl-b g / Ctrl-b G jump the focused pane to the
// top / bottom.
func TestJumpTopBottomKeys(t *testing.T) {
	m := testModel(t)
	m.w, m.h = 100, 30
	m.paneRoot = newLeaf(core.Main)
	m.focusPane = m.paneRoot
	m.running = true
	m.resize()

	var tm tea.Model = m
	tm, _ = tm.Update(tea.KeyMsg{Type: tea.KeyCtrlB})
	if !tm.(*model).prefix {
		t.Fatal("Ctrl-b should arm the prefix")
	}
	tm, _ = tm.Update(key("g"))
	if tm.(*model).status != "jumped to top" {
		t.Errorf("Ctrl-b g status = %q, want 'jumped to top'", tm.(*model).status)
	}
	tm, _ = tm.Update(tea.KeyMsg{Type: tea.KeyCtrlB})
	tm, _ = tm.Update(key("G"))
	if tm.(*model).status != "jumped to bottom" {
		t.Errorf("Ctrl-b G status = %q, want 'jumped to bottom'", tm.(*model).status)
	}
}

// TestBoundaryAtDetectsDivider checks the split divider is detected at the shared
// column, and not in the middle of a pane.
func TestBoundaryAtDetectsDivider(t *testing.T) {
	a := newLeaf(core.Main)
	b := newLeaf("recon#1")
	root := &pane{dir: splitH, ratio: 0.5, a: a, b: b}
	a.parent, b.parent = root, root
	computeRects(root, 0, 0, 80, 20)

	div := a.x + a.w
	if boundaryAt(root, div, 5) != root {
		t.Errorf("divider at x=%d not detected", div)
	}
	if boundaryAt(root, a.x+a.w/2, 5) != nil {
		t.Error("the middle of a pane must not be a boundary")
	}
	if boundaryAt(root, div, 100) != nil {
		t.Error("a y outside the node must not match the divider")
	}
}

// TestMouseResizeChangesRatio checks pressing a divider and dragging changes the
// split ratio, and releasing ends the resize.
func TestMouseResizeChangesRatio(t *testing.T) {
	m := testModel(t)
	a := newLeaf(core.Main)
	b := newLeaf("recon#1")
	root := &pane{dir: splitH, ratio: 0.5, a: a, b: b}
	a.parent, b.parent = root, root
	m.paneRoot, m.focusPane = root, a
	m.mainW, m.bodyTop, m.bodyH = 80, 0, 20
	computeRects(root, 0, 0, 80, 20)

	div := a.x + a.w
	m.mousePress(div, 5, false)
	if m.dragBoundary != root {
		t.Fatal("pressing the divider should start a resize")
	}
	m.mouseDrag(20, 5) // 20/80 => ratio ~0.25
	if root.ratio < 0.2 || root.ratio > 0.3 {
		t.Errorf("ratio after drag = %.3f, want ~0.25", root.ratio)
	}
	m.mouseDrag(4, 5) // clamp: 4/80 = 0.05 -> floored to 0.1
	if root.ratio < 0.09 || root.ratio > 0.11 {
		t.Errorf("ratio should clamp to ~0.1, got %.3f", root.ratio)
	}
	m.mouseRelease(4, 5)
	if m.dragBoundary != nil {
		t.Error("release should end the resize")
	}
}

// TestMouseMoveSwapsPanes checks that dragging one pane onto another swaps what the
// two panes display, while a press-release without motion is a plain click (no swap).
func TestMouseMoveSwapsPanes(t *testing.T) {
	m := testModel(t)
	a := newLeaf(core.Main)
	b := newLeaf("recon#1")
	root := &pane{dir: splitH, ratio: 0.5, a: a, b: b}
	a.parent, b.parent = root, root
	m.paneRoot, m.focusPane = root, a
	m.mainW, m.bodyTop, m.bodyH = 80, 0, 20
	computeRects(root, 0, 0, 80, 20)

	ax, bx := a.x+a.w/2, b.x+b.w/2
	m.mousePress(ax, 5, false)
	m.mouseDrag(bx, 5)
	m.mouseRelease(bx, 5)
	if a.agent != "recon#1" || b.agent != core.Main {
		t.Errorf("drag should swap views: a=%q b=%q", a.agent, b.agent)
	}
	if m.focusPane != b {
		t.Error("focus should follow the moved pane to its destination")
	}

	// A click (no motion) must NOT swap.
	m.mousePress(ax, 5, false)
	m.mouseRelease(ax, 5)
	if a.agent != "recon#1" {
		t.Error("a click without motion should not swap panes")
	}
}

// TestRosterScrollsWhenOverflowing checks a tall roster (many idle agents) in a
// short pane clamps the scroll offset and that wheel-down over the roster scrolls it.
func TestRosterScrollsWhenOverflowing(t *testing.T) {
	m := testModel(t)
	m.w, m.h = 120, 16 // short, so the content overflows
	m.addAgent(core.Main)
	for i := 1; i <= 30; i++ {
		id := core.AgentID("idle#" + string(rune('a'+i%26)) + strings.Repeat("x", i/26))
		m.addAgent(id)
		m.states[id] = core.Idle
	}
	m.running = true
	m.resize()

	_ = m.renderRoster(m.rightW, m.bodyH)
	if m.rosterScroll != 0 {
		t.Errorf("initial scroll should be 0, got %d", m.rosterScroll)
	}
	// Wheel down over the roster scrolls it; then a render clamps to the max.
	m.handleMouse(tea.MouseMsg{Button: tea.MouseButtonWheelDown, X: m.rosterX + 1, Y: m.bodyTop + 2})
	if m.rosterScroll <= 0 {
		t.Fatalf("wheel-down over the roster should increase the scroll offset, got %d", m.rosterScroll)
	}
	_ = m.renderRoster(m.rightW, m.bodyH)
	// A huge wheel-down then render must clamp, not run past the content.
	for i := 0; i < 50; i++ {
		m.handleMouse(tea.MouseMsg{Button: tea.MouseButtonWheelDown, X: m.rosterX + 1, Y: m.bodyTop + 2})
	}
	_ = m.renderRoster(m.rightW, m.bodyH)
	before := m.rosterScroll
	m.handleMouse(tea.MouseMsg{Button: tea.MouseButtonWheelDown, X: m.rosterX + 1, Y: m.bodyTop + 2})
	_ = m.renderRoster(m.rightW, m.bodyH)
	if m.rosterScroll != before {
		t.Errorf("scroll should be clamped at the bottom: %d -> %d", before, m.rosterScroll)
	}
}
