package tui

import (
	"time"

	"github.com/nihaal-prasad/oline/internal/core"
)

// Auto-layout tab.
//
// The second tab starts empty and arranges spawned subagents on its own, in a fixed
// 2x2 slot order:
//
//	slot 0 = top-left   slot 1 = right / top-right
//	slot 2 = bottom-left slot 3 = bottom-right
//
//	1 subagent  -> one pane (slot 0)
//	2 subagents -> split vertically: slot 0 left, slot 1 right
//	3 subagents -> the left pane splits horizontally: slot 0 top, slot 2 bottom
//	4 subagents -> the right pane splits horizontally: slot 1 top, slot 3 bottom
//	5th+        -> reuse a pane whose subagent is no longer running, else the pane
//	               with the longest-running subagent
//
// Any user edit to the tab (splitting, closing, moving, or retargeting a pane)
// cancels the auto-layout for that tab; Ctrl-b A re-enables it for the focused tab.
// Only one tab is auto at a time.

// autoWindow returns the auto-layout window and its index, or (nil, -1).
func (m *model) autoWindow() (*windowState, int) {
	for i, w := range m.windows {
		if w.auto {
			return w, i
		}
	}
	return nil, -1
}

// recordSpawn stamps a subagent's spawn time for longest-running reuse.
func (m *model) recordSpawn(id core.AgentID) {
	if m.spawnTime == nil {
		m.spawnTime = map[core.AgentID]time.Time{}
	}
	if _, ok := m.spawnTime[id]; !ok {
		m.spawnTime[id] = time.Now()
	}
}

// autoPlaceSpawn places a newly spawned subagent into the auto tab (if any).
func (m *model) autoPlaceSpawn(child core.AgentID) {
	w, i := m.autoWindow()
	if w == nil {
		return
	}
	m.recordSpawn(child)
	m.placeInAuto(w, child)
	m.applyAutoTree(i, child)
}

// placeInAuto updates a window's slot assignment for a newly arriving subagent:
// fill the next empty slot (up to 4), otherwise reuse a finished pane, otherwise the
// longest-running one.
func (m *model) placeInAuto(w *windowState, child core.AgentID) {
	if len(w.autoSlots) < 4 {
		w.autoSlots = append(w.autoSlots, child)
		return
	}
	idx := -1
	for k, id := range w.autoSlots { // a pane whose subagent is no longer running
		if s, ok := m.states[id]; !ok || s == core.Idle || s == core.Killed {
			idx = k
			break
		}
	}
	if idx < 0 { // all still running: take the longest-running (earliest spawn)
		idx = 0
		for k := 1; k < len(w.autoSlots); k++ {
			if m.spawnTime[w.autoSlots[k]].Before(m.spawnTime[w.autoSlots[idx]]) {
				idx = k
			}
		}
	}
	w.autoSlots[idx] = child
}

// applyAutoTree rebuilds window i's pane tree from its slots and focuses the pane
// showing focusOn, updating the live view when it is the active window.
func (m *model) applyAutoTree(i int, focusOn core.AgentID) {
	w := m.windows[i]
	root, focus := buildAutoTree(w.autoSlots, focusOn)
	w.root, w.focus = root, focus
	w.zoom = false
	if i == m.winIdx {
		m.paneRoot, m.focusPane, m.zoom = root, focus, false
		m.relayout()
	}
}

// buildAutoTree constructs the 2x2 slot layout for the given slots (0..4 of them)
// and returns the root plus the leaf showing focusOn (or the last leaf).
func buildAutoTree(slots []core.AgentID, focusOn core.AgentID) (root, focus *pane) {
	switch len(slots) {
	case 0:
		r := newEmptyLeaf()
		return r, r
	case 1:
		r := newLeaf(slots[0])
		return r, r
	case 2:
		a, b := newLeaf(slots[0]), newLeaf(slots[1])
		return internalNode(splitH, a, b), b
	case 3:
		a, c, b := newLeaf(slots[0]), newLeaf(slots[2]), newLeaf(slots[1])
		return internalNode(splitH, internalNode(splitV, a, c), b), c
	default: // 4
		a, c := newLeaf(slots[0]), newLeaf(slots[2])
		b, d := newLeaf(slots[1]), newLeaf(slots[3])
		r := internalNode(splitH, internalNode(splitV, a, c), internalNode(splitV, b, d))
		f := r
		for _, l := range leaves(r) {
			if l.agent == focusOn {
				f = l
				break
			}
		}
		return r, f
	}
}

// internalNode makes a split node wiring up parent pointers.
func internalNode(dir paneDir, a, b *pane) *pane {
	n := &pane{dir: dir, ratio: 0.5, a: a, b: b}
	a.parent, b.parent = n, n
	return n
}

// enableAutoHere turns the focused tab into THE auto-layout tab (Ctrl-b A): it
// disables auto on every other tab and rebuilds this one from the live subagents in
// spawn order. A user edit will cancel it again.
func (m *model) enableAutoHere() {
	m.ensureWindows()
	for i, w := range m.windows {
		if i != m.winIdx {
			w.auto = false
		}
	}
	w := m.windows[m.winIdx]
	w.auto = true
	w.autoSlots = nil
	for _, id := range m.subagentsBySpawn() {
		m.placeInAuto(w, id)
	}
	focus := core.AgentID("")
	if len(w.autoSlots) > 0 {
		focus = w.autoSlots[len(w.autoSlots)-1]
	}
	m.applyAutoTree(m.winIdx, focus)
	m.status = "auto-layout enabled for tab " + w.name + " — any edit to it cancels it"
}

// subagentsBySpawn lists the live (non-killed) subagents in spawn order.
func (m *model) subagentsBySpawn() []core.AgentID {
	var out []core.AgentID
	for _, id := range m.order {
		if id == core.Main {
			continue
		}
		if m.states[id] == core.Killed {
			continue
		}
		out = append(out, id)
	}
	return out
}

// cancelAutoActive turns off auto-layout for the active tab when the user edits it
// (split, close, move, retarget, or open a pane). A no-op if the active tab isn't an
// auto tab, so it is safe to call from every mutation handler.
func (m *model) cancelAutoActive(reason string) {
	m.ensureWindows()
	if m.winIdx < 0 || m.winIdx >= len(m.windows) {
		return
	}
	w := m.windows[m.winIdx]
	if !w.auto {
		return
	}
	w.auto = false
	w.autoSlots = nil
	m.status = "auto-layout canceled for tab " + w.name + " (" + reason + ") — Ctrl-b A re-enables it"
}
