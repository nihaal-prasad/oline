package tui

import (
	"time"

	"github.com/nihaal-prasad/oline/internal/core"
)

// The Subagents tab (enforced mode) auto-arranges the CURRENTLY LIVE subagents into
// a canonical grid, up to four panes:
//
//	1 pane  -> one pane, full tab
//	2 panes -> left | right
//	3 panes -> left column split top/bottom, right full
//	4 panes -> 2x2 (one full-width horizontal divider, one full-height vertical)
//
// `autoLeaves` is the source of truth: an ordered list (admission order = slot order)
// of the subagent leaf-panes holding a slot. A spawn or a reprompt (idle/paused ->
// running) ADMITS a subagent — a new pane opens only when fewer than four are open
// (never evicting a shown pane). When a subagent goes idle or is killed its pane is
// CLOSED, freeing a slot — this is gated by the `tui.closeIdleSubagentPanes` setting
// (on by default). After any change the whole tree is rebuilt canonically from
// `autoLeaves` (reconcileSubagentPanes), reusing the surviving leaf panes so their
// viewports/scroll are preserved; split ratios reset to 0.5. In customizable mode
// there is no Subagents tab, so all of this does nothing.
//
// Note: with more than four live subagents (only possible when maxConcurrency is
// raised above its default of 5) the extra ones run without a pane and a freed slot
// is not auto-refilled from a waiting runner until a fresh spawn/reprompt.

// customizablePanes reports whether the fully-customizable TUI fallback is on.
func (m *model) customizablePanes() bool {
	return m.store != nil && m.store.Effective().TUI.CustomizablePanes
}

// closeIdleSubagentPanes reports whether a subagent's auto pane is closed when it
// goes idle/killed (on by default; see the embedded config.yml).
func (m *model) closeIdleSubagentPanes() bool {
	return m.store != nil && m.store.Effective().TUI.CloseIdleSubagentPanes
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

// hNode / vNode build an internal split node over two children, wiring parents.
func hNode(a, b *pane) *pane { return linkSplit(&pane{dir: splitH, ratio: 0.5, a: a, b: b}) }
func vNode(a, b *pane) *pane { return linkSplit(&pane{dir: splitV, ratio: 0.5, a: a, b: b}) }
func linkSplit(p *pane) *pane {
	p.a.parent, p.b.parent = p, p
	return p
}

// autoLeafFor returns the slot leaf showing id, or nil.
func autoLeafFor(leaves []*pane, id core.AgentID) *pane {
	for _, l := range leaves {
		if l.agent == id {
			return l
		}
	}
	return nil
}

// removeLeaf returns leaves with p filtered out (order preserved).
func removeLeaf(leaves []*pane, p *pane) []*pane {
	out := leaves[:0]
	for _, l := range leaves {
		if l != p {
			out = append(out, l)
		}
	}
	return out
}

// arrangeAutoLeaves builds the canonical Subagents layout over 1..4 slot leaves,
// reusing the given leaf panes as cells:
//
//	1 -> s0
//	2 -> H(s0, s1)
//	3 -> H(V(s0, s2), s1)
//	4 -> H(V(s0, s2), V(s1, s3))
//
// (0 leaves is handled by the caller with an empty placeholder.)
func arrangeAutoLeaves(s []*pane) *pane {
	switch len(s) {
	case 1:
		s[0].parent = nil
		return s[0]
	case 2:
		return hNode(s[0], s[1])
	case 3:
		return hNode(vNode(s[0], s[2]), s[1])
	default: // 4 (callers cap autoLeaves at 4)
		return hNode(vNode(s[0], s[2]), vNode(s[1], s[3]))
	}
}

// pruneAutoLeaves drops tracked slot panes no longer present in the live tree.
func (m *model) pruneAutoLeaves(w *windowState, i int) {
	live := map[*pane]bool{}
	for _, l := range leaves(m.winRoot(i)) {
		live[l] = true
	}
	kept := w.autoLeaves[:0]
	for _, l := range w.autoLeaves {
		if live[l] {
			kept = append(kept, l)
		}
	}
	w.autoLeaves = kept
}

// reconcileSubagentPanes rebuilds window i's tree canonically from w.autoLeaves,
// reusing the surviving leaf panes (viewports preserved). Focus is kept on the
// focused leaf when it survives, otherwise the newest slot.
func (m *model) reconcileSubagentPanes(w *windowState, i int) {
	if len(w.autoLeaves) == 0 {
		empty := newEmptyLeaf()
		m.setWinRoot(i, empty, empty)
		return
	}
	root := arrangeAutoLeaves(w.autoLeaves)
	focus := w.focus
	if i == m.winIdx {
		focus = m.focusPane
	}
	if autoLeafFor(w.autoLeaves, focusAgentOf(focus)) != focus {
		focus = w.autoLeaves[len(w.autoLeaves)-1]
	}
	m.setWinRoot(i, root, focus)
}

// focusAgentOf returns a pane's agent id (empty for nil/non-leaf).
func focusAgentOf(p *pane) core.AgentID {
	if p == nil || !p.leaf {
		return ""
	}
	return p.agent
}

// autoAdmitSubagent opens a Subagents-tab pane for id (spawn or reprompt): only when
// fewer than four are open and id has no slot yet. Never evicts a shown pane.
func (m *model) autoAdmitSubagent(id core.AgentID) {
	if m.customizablePanes() {
		return
	}
	w, i := m.subagentsWindow()
	if w == nil {
		return
	}
	m.recordSpawn(id)
	m.pruneAutoLeaves(w, i)
	if autoLeafFor(w.autoLeaves, id) != nil {
		return // already shown
	}
	if len(w.autoLeaves) >= 4 {
		return // slots full (req: open a pane only when fewer than four are open)
	}
	w.autoLeaves = append(w.autoLeaves, newLeaf(id))
	m.reconcileSubagentPanes(w, i)
}

// autoRemoveSubagent closes id's Subagents-tab pane (idle/killed) and re-arranges the
// remaining panes canonically. The caller gates this on closeIdleSubagentPanes.
func (m *model) autoRemoveSubagent(id core.AgentID) {
	if m.customizablePanes() {
		return
	}
	w, i := m.subagentsWindow()
	if w == nil {
		return
	}
	m.pruneAutoLeaves(w, i)
	leaf := autoLeafFor(w.autoLeaves, id)
	if leaf == nil {
		return
	}
	w.autoLeaves = removeLeaf(w.autoLeaves, leaf)
	m.reconcileSubagentPanes(w, i)
}

// autoPlaceSpawn admits a newly spawned subagent into the Subagents tab (kept as the
// name the event.Spawned handler calls).
func (m *model) autoPlaceSpawn(child core.AgentID) { m.autoAdmitSubagent(child) }

// rebuildSubagentsTab repopulates the Subagents tab from the live subagents in spawn
// order (used on resume, where restored subagents fire no spawn/state events). It
// shows a snapshot, so a restored IDLE subagent still gets a pane (unlike a live
// idle transition, which closes one).
func (m *model) rebuildSubagentsTab() {
	w, i := m.subagentsWindow()
	if w == nil {
		return
	}
	w.autoLeaves = nil
	for _, id := range m.subagentsBySpawn() {
		if len(w.autoLeaves) >= 4 {
			break
		}
		w.autoLeaves = append(w.autoLeaves, newLeaf(id))
	}
	m.reconcileSubagentPanes(w, i)
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
