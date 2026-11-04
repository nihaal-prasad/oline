package tui

import (
	"time"

	"github.com/nihaal-prasad/oline/internal/core"
)

// The Subagents tab (enforced mode) auto-arranges spawned subagents, always on:
//
//	1st subagent  -> one pane
//	2nd           -> split left/right (1st left, 2nd right)
//	3rd           -> the left pane splits top/bottom (1st top, 3rd bottom)
//	4th           -> the right pane splits top/bottom (2nd top, 4th bottom)
//	5th+          -> the pane showing the OLDEST subagent is retargeted to the new one
//
// The user may still resize panes, split new ones, and retarget a pane to a
// different subagent; none of that cancels the layout. It grows by splitting the
// tracked slot panes in place (so resizes and other panes are preserved), and once
// the four slots exist, new spawns reuse the oldest. In customizable mode there is
// no Subagents tab, so this does nothing.

// customizablePanes reports whether the fully-customizable TUI fallback is on.
func (m *model) customizablePanes() bool {
	return m.store != nil && m.store.Effective().TUI.CustomizablePanes
}

// subagentsWindow returns the Subagents tab and its index, or (nil, -1).
func (m *model) subagentsWindow() (*windowState, int) {
	for i, w := range m.windows {
		if w.kind == tabSubagents {
			return w, i
		}
	}
	return nil, -1
}

// recordSpawn stamps a subagent's spawn time (for oldest-pane reuse).
func (m *model) recordSpawn(id core.AgentID) {
	if m.spawnTime == nil {
		m.spawnTime = map[core.AgentID]time.Time{}
	}
	if _, ok := m.spawnTime[id]; !ok {
		m.spawnTime[id] = time.Now()
	}
}

// winRoot returns the live root of window i (the active window's mirrored root, or
// the parked state).
func (m *model) winRoot(i int) *pane {
	if i == m.winIdx {
		return m.paneRoot
	}
	return m.windows[i].root
}

// setWinRoot writes a window's root/focus back, updating the live view and relaying
// when it is the active window.
func (m *model) setWinRoot(i int, root, focus *pane) {
	w := m.windows[i]
	w.root, w.focus, w.zoom = root, focus, false
	if i == m.winIdx {
		m.paneRoot, m.focusPane, m.zoom = root, focus, false
		m.relayout()
	}
}

// autoPlaceSpawn places a newly spawned subagent into the Subagents tab.
func (m *model) autoPlaceSpawn(child core.AgentID) {
	if m.customizablePanes() {
		return
	}
	w, i := m.subagentsWindow()
	if w == nil {
		return
	}
	m.recordSpawn(child)
	root := m.winRoot(i)

	// Drop tracked slot panes that are no longer in the tree (user closed them).
	live := map[*pane]bool{}
	for _, l := range leaves(root) {
		live[l] = true
	}
	kept := w.autoLeaves[:0]
	for _, l := range w.autoLeaves {
		if live[l] {
			kept = append(kept, l)
		}
	}
	w.autoLeaves = kept

	switch len(w.autoLeaves) {
	case 0: // empty tab: a single pane
		leaf := newLeaf(child)
		w.autoLeaves = []*pane{leaf}
		m.setWinRoot(i, leaf, leaf)
	case 1: // split left/right: existing left, new right
		nl := newLeaf(child)
		nr, _ := splitPaneWith(root, w.autoLeaves[0], splitH, nl)
		w.autoLeaves = append(w.autoLeaves, nl)
		m.setWinRoot(i, nr, nl)
	case 2: // split the left pane top/bottom
		nl := newLeaf(child)
		nr, _ := splitPaneWith(root, w.autoLeaves[0], splitV, nl)
		w.autoLeaves = append(w.autoLeaves, nl)
		m.setWinRoot(i, nr, nl)
	case 3: // split the right pane top/bottom
		nl := newLeaf(child)
		nr, _ := splitPaneWith(root, w.autoLeaves[1], splitV, nl)
		w.autoLeaves = append(w.autoLeaves, nl)
		m.setWinRoot(i, nr, nl)
	default: // 4 slots full: retarget the oldest subagent's pane
		oldest := w.autoLeaves[0]
		for _, l := range w.autoLeaves[1:] {
			if m.spawnTime[l.agent].Before(m.spawnTime[oldest.agent]) {
				oldest = l
			}
		}
		oldest.agent = child
		oldest.shell, oldest.rev, oldest.term, oldest.empty = false, false, false, false
		m.refreshLeaf(oldest)
		m.setWinRoot(i, root, oldest)
	}
}

// rebuildSubagentsTab resets the Subagents tab and re-places every live subagent in
// spawn order (used on resume, where restored subagents fire no spawn events).
func (m *model) rebuildSubagentsTab() {
	w, i := m.subagentsWindow()
	if w == nil {
		return
	}
	w.autoLeaves = nil
	empty := newEmptyLeaf()
	m.setWinRoot(i, empty, empty)
	for _, id := range m.subagentsBySpawn() {
		m.autoPlaceSpawn(id)
	}
}

// subagentsBySpawn lists the live (non-killed) subagents in spawn order.
func (m *model) subagentsBySpawn() []core.AgentID {
	var out []core.AgentID
	for _, id := range m.order {
		if id == core.Main || m.states[id] == core.Killed {
			continue
		}
		out = append(out, id)
	}
	return out
}
