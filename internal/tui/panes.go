package tui

import (
	"github.com/charmbracelet/bubbles/viewport"

	"github.com/nihaal-prasad/oline/internal/core"
)

// windowState is one window (tab): its own split tree, focused leaf, and zoom.
// The active window's state is mirrored into the model's paneRoot/focusPane/zoom
// so all existing pane code keeps operating on "the current window"; the inactive
// windows are parked here and swapped in on switch.
type windowState struct {
	root  *pane
	focus *pane
	zoom  bool
	name  string
}

// pane is a node in the tmux-style split tree. A leaf shows one agent's
// transcript; an internal node splits its area between two children.
type paneDir int

const (
	splitH paneDir = iota // children side by side
	splitV                // children stacked
)

// Synthetic pane ids for the two live info panes (Summary, Diagram). They are not
// agents, so they never appear in the roster and never get a transcript.
const (
	summaryPaneID = core.AgentID("\x00summary")
	diagramPaneID = core.AgentID("\x00diagram")
)

type pane struct {
	agent   core.AgentID
	vp      viewport.Model
	leaf    bool
	shell   bool // true: an interactive shell the user types into (not an agent view)
	summary bool // true: the live engagement-summary pane (content from m.summary)
	diagram bool // true: the live ASCII map pane (content from m.diagram)

	dir          paneDir
	ratio        float64
	a, b, parent *pane

	// computed each layout pass (for rendering + mouse hit-testing)
	x, y, w, h int
}

func newLeaf(agent core.AgentID) *pane {
	return &pane{agent: agent, leaf: true, ratio: 0.5, vp: viewport.New(10, 10)}
}

// newShellLeaf is a leaf whose content is a user-driven shell, keyed by a
// synthetic id so it reuses the transcript/render machinery without appearing in
// the agent roster.
func newShellLeaf(id core.AgentID) *pane {
	return &pane{agent: id, leaf: true, shell: true, ratio: 0.5, vp: viewport.New(10, 10)}
}

// newSummaryLeaf / newDiagramLeaf are the two live info panes. They reuse the
// transcript/viewport machinery (for scrolling) but their content is rendered from
// the model's summary / diagram state, not an agent transcript.
func newSummaryLeaf() *pane {
	return &pane{agent: summaryPaneID, leaf: true, summary: true, ratio: 0.5, vp: viewport.New(10, 10)}
}
func newDiagramLeaf() *pane {
	return &pane{agent: diagramPaneID, leaf: true, diagram: true, ratio: 0.5, vp: viewport.New(10, 10)}
}

func leaves(p *pane) []*pane {
	if p == nil {
		return nil
	}
	if p.leaf {
		return []*pane{p}
	}
	return append(leaves(p.a), leaves(p.b)...)
}

// splitPane splits focused, adding a new leaf for agent. Returns the (possibly
// new) root and the new focused leaf.
func splitPane(root, focused *pane, d paneDir, agent core.AgentID) (*pane, *pane) {
	return splitPaneWith(root, focused, d, newLeaf(agent))
}

// splitPaneWith splits focused, inserting the given leaf nl beside it. Returns
// the (possibly new) root and nl as the new focused leaf.
func splitPaneWith(root, focused *pane, d paneDir, nl *pane) (*pane, *pane) {
	internal := &pane{dir: d, ratio: 0.5, a: focused, b: nl, parent: focused.parent}
	pp := focused.parent
	focused.parent = internal
	nl.parent = internal
	switch {
	case pp == nil:
		root = internal
	case pp.a == focused:
		pp.a = internal
	default:
		pp.b = internal
	}
	return root, nl
}

// closePane removes focused. Returns the new root and new focused leaf (the
// original is returned unchanged when it is the only pane).
func closePane(root, focused *pane) (*pane, *pane) {
	if focused.parent == nil {
		return root, focused
	}
	parent := focused.parent
	sib := parent.b
	if parent.a != focused {
		sib = parent.a
	}
	gp := parent.parent
	sib.parent = gp
	switch {
	case gp == nil:
		root = sib
	case gp.a == parent:
		gp.a = sib
	default:
		gp.b = sib
	}
	nf := sib
	for !nf.leaf {
		nf = nf.a
	}
	return root, nf
}

// computeRects assigns each node its rectangle within (x,y,w,h).
func computeRects(p *pane, x, y, w, h int) {
	p.x, p.y, p.w, p.h = x, y, w, h
	if p.leaf {
		return
	}
	if p.dir == splitH {
		aw := clampInt(int(float64(w)*p.ratio), 1, w-1)
		computeRects(p.a, x, y, aw, h)
		computeRects(p.b, x+aw, y, w-aw, h)
	} else {
		ah := clampInt(int(float64(h)*p.ratio), 1, h-1)
		computeRects(p.a, x, y, w, ah)
		computeRects(p.b, x, y+ah, w, h-ah)
	}
}

// navigate returns the nearest leaf in a direction ('h','j','k','l') from focused.
func navigate(root, focused *pane, dir byte) *pane {
	fx, fy := focused.x+focused.w/2, focused.y+focused.h/2
	var best *pane
	bestDist := 1 << 30
	for _, l := range leaves(root) {
		if l == focused {
			continue
		}
		cx, cy := l.x+l.w/2, l.y+l.h/2
		ok := false
		switch dir {
		case 'l':
			ok = cx > fx
		case 'h':
			ok = cx < fx
		case 'j':
			ok = cy > fy
		case 'k':
			ok = cy < fy
		}
		if !ok {
			continue
		}
		if d := absInt(cx-fx) + absInt(cy-fy); d < bestDist {
			bestDist, best = d, l
		}
	}
	return best
}

// boundaryAt returns the internal split node whose divider line (x,y) sits on, or
// nil. The divider between two horizontally-split children is the column shared by
// the left child's right border and the right child's left border; for a vertical
// split it is the shared row. A 1-cell tolerance makes the thin border easy to grab.
func boundaryAt(root *pane, x, y int) *pane {
	var found *pane
	var walk func(p *pane)
	walk = func(p *pane) {
		if p == nil || p.leaf {
			return
		}
		if p.dir == splitH {
			div := p.a.x + p.a.w // first column of the right child
			if (x == div || x == div-1) && y >= p.y && y < p.y+p.h {
				found = p
			}
		} else {
			div := p.a.y + p.a.h // first row of the bottom child
			if (y == div || y == div-1) && x >= p.x && x < p.x+p.w {
				found = p
			}
		}
		walk(p.a)
		walk(p.b)
	}
	walk(root)
	return found
}

// paneAt returns the leaf whose rectangle contains (x,y), or nil.
func paneAt(root *pane, x, y int) *pane {
	for _, l := range leaves(root) {
		if x >= l.x && x < l.x+l.w && y >= l.y && y < l.y+l.h {
			return l
		}
	}
	return nil
}

func clampInt(v, lo, hi int) int {
	if hi < lo {
		hi = lo
	}
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func absInt(n int) int {
	if n < 0 {
		return -n
	}
	return n
}
