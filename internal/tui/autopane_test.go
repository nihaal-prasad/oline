package tui

import (
	"testing"
	"time"

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

func TestAutoReuseOldest(t *testing.T) {
	m := autoModel(t)
	base := time.Now()
	for i, id := range []string{"a1", "a2", "a3", "a4", "a5"} {
		m.spawnTime[core.AgentID(id)] = base.Add(time.Duration(i) * time.Second)
	}
	for _, id := range []string{"a1", "a2", "a3", "a4"} {
		m.spawn(id)
	}
	// 5th spawn replaces the oldest subagent's pane (a1, top-left slot).
	m.spawn("a5")
	if leafAgent(m.windows[1].root.a.a) != "a5" {
		t.Fatalf("5th spawn should replace the oldest (a1 top-left) with a5, got %q", leafAgent(m.windows[1].root.a.a))
	}
}

func TestAutoNoCancelOnEdit(t *testing.T) {
	m := autoModel(t)
	m.spawn("a1")
	m.spawn("a2")
	// The user switches to the Subagents tab and splits a pane: auto still holds.
	m.activateWindow(1)
	m.doSplit(splitV)
	if m.windows[1].kind != tabSubagents {
		t.Fatal("splitting must not change the tab's kind")
	}
	// A further spawn still auto-places (grows the left slot).
	m.spawn("a3")
	found := false
	for _, l := range leaves(m.windows[1].root) {
		if l.agent == "a3" {
			found = true
		}
	}
	if !found {
		t.Error("auto-layout should still place a3 after a user split")
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
