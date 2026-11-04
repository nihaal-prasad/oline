package tui

import "github.com/nihaal-prasad/oline/internal/core"

// Per-tab enforcement (Customizable Panes off).
//
// Each tab has a fixed identity that restricts which views its panes may show and
// what the Options sidebar offers: the Info tab holds only the info views (Main,
// Summary, Diagram), the Subagents tab only subagent views, and a Shell tab only
// command panes (shell / terminal / reverse-shell). Resizing and splitting panes
// work on every tab; only the view TYPE is constrained. In customizable mode every
// tab is tabFree and nothing is enforced.

// activeKind is the kind of the active tab, or tabFree in customizable mode.
func (m *model) activeKind() tabKind {
	if m.customizablePanes() {
		return tabFree
	}
	if m.winIdx >= 0 && m.winIdx < len(m.windows) {
		return m.windows[m.winIdx].kind
	}
	return tabFree
}

// isInfoView reports whether id is one of the info views (Main / Summary / Diagram).
func isInfoView(id core.AgentID) bool {
	return id == core.Main || id == summaryPaneID || id == diagramPaneID
}

// viewAllowedHere reports whether a pane on the active tab may show view id. In
// customizable mode anything is allowed; otherwise the tab's kind decides.
func (m *model) viewAllowedHere(id core.AgentID) bool {
	switch m.activeKind() {
	case tabInfo:
		return isInfoView(id)
	case tabSubagents:
		return !isInfoView(id) && id != emptyPaneID // a subagent view
	case tabShell:
		return false // command panes are opened with Ctrl-b $/T/L, not retargeted
	default:
		return true
	}
}
