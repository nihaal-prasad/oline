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

// autoModel returns a running model with the main tab plus an empty auto tab
// (window index 1); the main tab stays active, like a real start.
func autoModel(t *testing.T) *model {
	t.Helper()
	m := runningModel(t)
	auto := newEmptyLeaf()
	m.windows = append(m.windows, &windowState{root: auto, focus: auto, name: "2", auto: true})
	return m
}

func (m *model) spawn(id string) {
	m.apply(event.Spawned{Parent: core.Main, Child: core.AgentID(id), Role: "r"})
}

func TestBuildAutoTree(t *testing.T) {
	a, b, c, d := core.AgentID("a"), core.AgentID("b"), core.AgentID("c"), core.AgentID("d")

	r, _ := buildAutoTree([]core.AgentID{a}, a)
	if !r.leaf || leafAgent(r) != a {
		t.Fatalf("1 slot: want single leaf a, got %+v", r)
	}

	r, _ = buildAutoTree([]core.AgentID{a, b}, b)
	if r.dir != splitH || leafAgent(r.a) != a || leafAgent(r.b) != b {
		t.Fatalf("2 slots: want H(a,b) left/right")
	}

	r, _ = buildAutoTree([]core.AgentID{a, b, c}, c)
	if r.dir != splitH || r.a.dir != splitV || leafAgent(r.a.a) != a || leafAgent(r.a.b) != c || leafAgent(r.b) != b {
		t.Fatalf("3 slots: want H(V(a,c), b)")
	}

	r, _ = buildAutoTree([]core.AgentID{a, b, c, d}, d)
	if r.dir != splitH || r.a.dir != splitV || r.b.dir != splitV {
		t.Fatalf("4 slots: want H(V,V)")
	}
	if leafAgent(r.a.a) != a || leafAgent(r.a.b) != c || leafAgent(r.b.a) != b || leafAgent(r.b.b) != d {
		t.Fatalf("4 slots: positions wrong (want a/c left, b/d right)")
	}
}

func TestAutoPlaceScheme(t *testing.T) {
	m := autoModel(t)
	root := func() *pane { return m.windows[1].root }

	m.spawn("a1")
	if leafAgent(root()) != "a1" {
		t.Fatalf("after 1 spawn want single leaf a1")
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

func TestAutoReuse(t *testing.T) {
	m := autoModel(t)
	w := m.windows[1]
	base := time.Now()
	for i, id := range []core.AgentID{"a1", "a2", "a3", "a4"} {
		w.autoSlots = append(w.autoSlots, id)
		m.states[id] = core.Running
		m.spawnTime[id] = base.Add(time.Duration(i) * time.Second)
	}
	// a2 (slot 1) is finished -> the 5th reuses its slot.
	m.states["a2"] = core.Idle
	m.spawnTime["a5"] = base.Add(10 * time.Second)
	m.placeInAuto(w, "a5")
	if w.autoSlots[1] != "a5" {
		t.Fatalf("5th should reuse the finished slot (a2 -> a5), got %v", w.autoSlots)
	}
	// Now all four running -> the 6th replaces the longest-running (a1, earliest).
	m.states["a5"] = core.Running
	m.spawnTime["a6"] = base.Add(20 * time.Second)
	m.placeInAuto(w, "a6")
	if w.autoSlots[0] != "a6" {
		t.Fatalf("6th should replace the longest-running (a1 -> a6), got %v", w.autoSlots)
	}
}

func TestAutoCancelOnEdit(t *testing.T) {
	m := autoModel(t)
	m.spawn("a1")
	m.spawn("a2")
	// Switch to the auto tab and split a pane: a user edit cancels auto.
	m.activateWindow(1)
	m.doSplit(splitV)
	if m.windows[1].auto {
		t.Fatal("splitting a pane on the auto tab should cancel auto-layout")
	}
	// A further spawn must NOT rearrange the (now manual) tab.
	before := len(leaves(m.windows[1].root))
	m.spawn("a3")
	if got := len(leaves(m.windows[1].root)); got != before {
		t.Errorf("auto placement should stop after cancel (leaves %d -> %d)", before, got)
	}
}

func TestEnableAutoHere(t *testing.T) {
	m := autoModel(t)
	m.spawn("a1")
	m.spawn("a2")
	m.activateWindow(1)
	m.doSplit(splitV) // cancels auto
	if m.windows[1].auto {
		t.Fatal("precondition: auto should be canceled")
	}
	m.enableAutoHere() // re-enable on the active (second) tab
	if !m.windows[1].auto {
		t.Fatal("Ctrl-b A should re-enable auto on the focused tab")
	}
	// Rebuilt from the two live subagents: H(a1, a2).
	r := m.windows[1].root
	if r.dir != splitH || leafAgent(r.a) != "a1" || leafAgent(r.b) != "a2" {
		t.Fatalf("re-enabled auto should rebuild H(a1,a2), got %+v", r)
	}
}
