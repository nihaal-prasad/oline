package tui

import (
	"strings"
	"testing"

	"github.com/nihaal-prasad/oline/internal/core"
	"github.com/nihaal-prasad/oline/internal/event"
)

func leafAgent(p *pane) core.AgentID {
	if p == nil || !p.leaf {
		return ""
	}
	return p.agent
}

// autoModel returns a running model with the enforced Info + Subagents tabs (Info
// active), as beginSession builds in the default (non-customizable) mode.
func autoModel(t *testing.T) *model {
	t.Helper()
	m := runningModel(t)
	info := m.windows[0]
	info.kind, info.name = tabInfo, "Info"
	sub := newEmptyLeaf()
	m.windows = append(m.windows, &windowState{root: sub, focus: sub, name: "Subagents", kind: tabSubagents})
	m.winIdx = 0
	return m
}

func (m *model) spawn(id string) {
	m.apply(event.Spawned{Parent: core.Main, Child: core.AgentID(id), Role: "r"})
}

func TestAutoPlaceScheme(t *testing.T) {
	m := autoModel(t)
	root := func() *pane { return m.windows[1].root }

	m.spawn("a1")
	if leafAgent(root()) != "a1" {
		t.Fatalf("after 1 spawn want single leaf a1, got %+v", root())
	}
	m.spawn("a2")
	if root().dir != splitH || leafAgent(root().a) != "a1" || leafAgent(root().b) != "a2" {
		t.Fatalf("after 2 spawns want H(a1,a2)")
	}
	m.spawn("a3")
	if leafAgent(root().a.a) != "a1" || leafAgent(root().a.b) != "a3" || leafAgent(root().b) != "a2" {
		t.Fatalf("after 3 spawns want left V(a1,a3), right a2")
	}
	m.spawn("a4")
	if leafAgent(root().a.b) != "a3" || leafAgent(root().b.a) != "a2" || leafAgent(root().b.b) != "a4" {
		t.Fatalf("after 4 spawns want V(a1,a3) | V(a2,a4)")
	}
}

// TestAutoNoOverflowBeyondFour: with four panes open, a further spawn opens no pane
// and evicts none (overflow only happens if maxConcurrency is raised above 5).
func TestAutoNoOverflowBeyondFour(t *testing.T) {
	m := autoModel(t)
	for _, id := range []string{"a1", "a2", "a3", "a4", "a5"} {
		m.spawn(id)
	}
	w := m.windows[1]
	if len(w.autoLeaves) != 4 {
		t.Fatalf("want 4 slots after 5 spawns, got %d", len(w.autoLeaves))
	}
	if autoLeafFor(w.autoLeaves, "a5") != nil {
		t.Error("the 5th spawn must not open a pane (slots full)")
	}
	if autoLeafFor(w.autoLeaves, "a1") == nil {
		t.Error("the oldest shown pane (a1) must not be evicted")
	}
	// Still the canonical 2x2.
	r := w.root
	if leafAgent(r.a.a) != "a1" || leafAgent(r.a.b) != "a3" || leafAgent(r.b.a) != "a2" || leafAgent(r.b.b) != "a4" {
		t.Errorf("want the 2x2 V(a1,a3)|V(a2,a4), got %+v", r)
	}
}

// TestNoSplitOnEnforcedTabs: in enforced mode the user can't split the Info or
// Subagents tabs; a Shell tab still splits.
func TestNoSplitOnEnforcedTabs(t *testing.T) {
	m := autoModel(t)

	// Info tab (active): split refused, tree unchanged, status explains.
	m.doSplit(splitV)
	if n := len(leaves(m.windows[0].root)); n != 1 {
		t.Errorf("Info tab must not split, got %d leaves", n)
	}
	if !strings.Contains(m.status, "can't split") {
		t.Errorf("expected a refusal status, got %q", m.status)
	}

	// Subagents tab: two panes, then a split is refused.
	m.spawn("a1")
	m.spawn("a2")
	m.activateWindow(1)
	m.doSplit(splitV)
	if n := len(leaves(m.windows[1].root)); n != 2 {
		t.Errorf("Subagents tab must not split, got %d leaves", n)
	}

	// A Shell tab still splits.
	shell := newLeaf(core.Main)
	m.windows = append(m.windows, &windowState{root: shell, focus: shell, name: "Shell", kind: tabShell})
	m.activateWindow(len(m.windows) - 1)
	m.doSplit(splitH)
	// doSplit mutates the live mirror (m.paneRoot) for the active window.
	if n := len(leaves(m.paneRoot)); n != 2 {
		t.Errorf("a Shell tab should split, got %d leaves", n)
	}
}

// TestCloseIdleFreesSlot: a subagent going idle closes its pane and the rest
// re-arrange canonically (default config has close-on-idle on).
func TestCloseIdleFreesSlot(t *testing.T) {
	m := autoModel(t)
	m.spawn("a1")
	m.spawn("a2")
	m.spawn("a3")
	m.apply(event.AgentState{Agent: "a2", State: core.Idle})
	w := m.windows[1]
	if len(w.autoLeaves) != 2 {
		t.Fatalf("idle should free a slot: want 2 panes, got %d", len(w.autoLeaves))
	}
	if autoLeafFor(w.autoLeaves, "a2") != nil {
		t.Error("the idle subagent's pane should be closed")
	}
	if leafAgent(w.root.a) != "a1" || leafAgent(w.root.b) != "a3" || w.root.dir != splitH {
		t.Errorf("want canonical H(a1,a3) after close, got %+v", w.root)
	}
}

// TestReopenOnReprompt: a reprompted (idle->running) subagent gets a pane again.
func TestReopenOnReprompt(t *testing.T) {
	m := autoModel(t)
	m.spawn("a1")
	m.spawn("a2")
	m.spawn("a3")
	m.apply(event.AgentState{Agent: "a2", State: core.Idle})    // closes a2's pane
	m.apply(event.AgentState{Agent: "a2", State: core.Running}) // reprompt
	w := m.windows[1]
	if len(w.autoLeaves) != 3 {
		t.Fatalf("reprompt should reopen a pane: want 3, got %d", len(w.autoLeaves))
	}
	if autoLeafFor(w.autoLeaves, "a2") == nil {
		t.Error("the reprompted subagent should have a pane again")
	}
}

// TestCloseIdleGatedByConfig: with the setting off, an idle subagent keeps its pane.
func TestCloseIdleGatedByConfig(t *testing.T) {
	m := autoModel(t)
	m.store.SetTUICloseIdleSubagentPanes(false)
	m.spawn("a1")
	m.spawn("a2")
	m.apply(event.AgentState{Agent: "a1", State: core.Idle})
	if n := len(m.windows[1].autoLeaves); n != 2 {
		t.Errorf("with close-on-idle off, the idle pane should persist: want 2, got %d", n)
	}
}

func TestViewEnforcement(t *testing.T) {
	m := autoModel(t)
	// Info tab active: only info views allowed.
	if !m.viewAllowedHere(core.Main) || !m.viewAllowedHere(summaryPaneID) {
		t.Error("Info tab should allow info views")
	}
	if m.viewAllowedHere(core.AgentID("recon#1")) {
		t.Error("Info tab should NOT allow a subagent view")
	}
	// Subagents tab active: only subagent views allowed.
	m.winIdx = 1
	if m.viewAllowedHere(core.Main) || m.viewAllowedHere(diagramPaneID) {
		t.Error("Subagents tab should NOT allow info views")
	}
	if !m.viewAllowedHere(core.AgentID("recon#1")) {
		t.Error("Subagents tab should allow a subagent view")
	}
}

func TestEnforcedTabsNotClosable(t *testing.T) {
	m := autoModel(t) // Info (0) + Subagents (1), enforced
	// Add a Shell tab so there are 3 and "last window" isn't the blocker.
	shell := newEmptyLeaf()
	m.windows = append(m.windows, &windowState{root: shell, focus: shell, name: "Shell", kind: tabShell})

	m.winIdx = 0 // Info
	m.killWindow()
	if len(m.windows) != 3 {
		t.Error("the Info tab must not be closable in enforced mode")
	}
	m.winIdx = 1 // Subagents
	m.killWindow()
	if len(m.windows) != 3 {
		t.Error("the Subagents tab must not be closable in enforced mode")
	}
	m.winIdx = 2 // Shell
	m.killWindow()
	if len(m.windows) != 2 {
		t.Error("a Shell tab should be closable in enforced mode")
	}
}
