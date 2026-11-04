package tui

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/nihaal-prasad/oline/internal/config"
	"github.com/nihaal-prasad/oline/internal/core"
	"github.com/nihaal-prasad/oline/internal/engine"
	"github.com/nihaal-prasad/oline/internal/llm"
)

// Conversation history is persisted on the host (not in the sandbox) so an
// engagement can be reloaded after the program is turned off and on. The program
// writes it, like the activity log; agents never touch it.

// engagementHistory is the on-disk form of a saved engagement conversation.
type engagementHistory struct {
	Engagement string               `json:"engagement"`
	SavedAt    time.Time            `json:"savedAt"`
	Agents     []engine.AgentRecord `json:"agents"`
	Windows    []savedWindow        `json:"windows,omitempty"` // TUI pane layout, restored on resume
	WinIdx     int                  `json:"winIdx,omitempty"`
}

// savedWindow / savedPane are the serializable form of the TUI's window and pane
// split tree, so a resumed engagement comes back to the same layout. Shell panes
// are not persisted (their process is gone); they are saved as a Main view.
type savedWindow struct {
	Name         string     `json:"name"`
	Zoom         bool       `json:"zoom,omitempty"`
	Root         *savedPane `json:"root"`
	FocusAgent   string     `json:"focusAgent,omitempty"`
	FocusSummary bool       `json:"focusSummary,omitempty"`
	FocusDiag    bool       `json:"focusDiagram,omitempty"`
}

type savedPane struct {
	Leaf    bool       `json:"leaf"`
	Agent   string     `json:"agent,omitempty"`
	Summary bool       `json:"summary,omitempty"`
	Diagram bool       `json:"diagram,omitempty"`
	Dir     int        `json:"dir,omitempty"`
	Ratio   float64    `json:"ratio,omitempty"`
	A       *savedPane `json:"a,omitempty"`
	B       *savedPane `json:"b,omitempty"`
}

func historyPath(engagement string) string {
	return filepath.Join(config.HistoryDir(), engagement+".json")
}

// historyExists reports whether a non-empty saved conversation exists for an
// engagement (so the wizard knows whether to offer "load vs clean slate").
func historyExists(engagement string) bool {
	if strings.TrimSpace(engagement) == "" {
		return false
	}
	info, err := os.Stat(historyPath(engagement))
	return err == nil && info.Size() > 0
}

// saveHistory writes the current engagement's conversation to the host atomically
// (unique temp + rename), mode 0600. A no-op without a live session/engagement.
// Serialized by histMu so the periodic save goroutine and the UI-goroutine saves
// (on finish / shutdown) can't interleave and corrupt the file.
func (m *model) saveHistory() {
	if m.sess == nil || m.engagement == "" {
		return
	}
	m.histMu.Lock()
	defer m.histMu.Unlock()
	data, err := json.MarshalIndent(engagementHistory{
		Engagement: m.engagement,
		SavedAt:    time.Now(),
		Agents:     m.sess.Snapshot(),
		Windows:    m.savedLayout, // last UI-goroutine layout snapshot (see refreshLayoutSnapshot)
		WinIdx:     m.savedWinIdx,
	}, "", "  ")
	if err != nil {
		return
	}
	dir := config.HistoryDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return
	}
	// A unique temp name per write means two concurrent saves never clobber each
	// other's temp before rename.
	tmp, err := os.CreateTemp(dir, ".history-*.tmp")
	if err != nil {
		return
	}
	tmpName := tmp.Name()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return
	}
	if err := os.Rename(tmpName, historyPath(m.engagement)); err != nil {
		_ = os.Remove(tmpName)
	}
}

// historyLoop persists the conversation periodically until the engagement context
// ends, then writes a final snapshot. Started once per engagement.
func (m *model) historyLoop(ctx context.Context) {
	t := time.NewTicker(15 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			m.saveHistory()
			return
		case <-t.C:
			m.saveHistory()
		}
	}
}

// loadHistory reads a saved engagement (conversations + pane layout).
func loadHistory(engagement string) (*engagementHistory, error) {
	data, err := os.ReadFile(historyPath(engagement))
	if err != nil {
		return nil, err
	}
	var eh engagementHistory
	if err := json.Unmarshal(data, &eh); err != nil {
		return nil, err
	}
	return &eh, nil
}

// refreshLayoutSnapshot captures the current window/pane layout into a histMu-guarded
// field. It reads live TUI state, so it must be called ONLY from the UI goroutine
// (the 1s tick, shutdown, and the end-of-turn save); saveHistory, which may run on
// the periodic-save goroutine, then only reads the snapshot under the lock.
func (m *model) refreshLayoutSnapshot() {
	if !m.running {
		return
	}
	m.saveWindow() // mirror the active window's live root/focus/zoom into m.windows
	wins := make([]savedWindow, 0, len(m.windows))
	for _, w := range m.windows {
		// Enforced tabs (Subagents / Shell) are transient and rebuilt deterministically
		// on resume, so they are not persisted as a frozen layout. (Enforced mode
		// doesn't restore a saved layout at all; this keeps a stray save from leaking
		// into a later customizable run.)
		if w.kind == tabSubagents || w.kind == tabShell {
			continue
		}
		// Don't persist a lone command/empty tab either: its content can't be restored
		// and would come back as a stray Main tab.
		if w.root != nil && w.root.leaf && (w.root.shell || w.root.rev || w.root.term || w.root.empty) {
			continue
		}
		sw := savedWindow{Name: w.name, Zoom: w.zoom, Root: toSavedPane(w.root)}
		if w.focus != nil {
			sw.FocusAgent = string(w.focus.agent)
			sw.FocusSummary = w.focus.summary
			sw.FocusDiag = w.focus.diagram
		}
		wins = append(wins, sw)
	}
	m.histMu.Lock()
	m.savedLayout = wins
	m.savedWinIdx = m.winIdx
	m.histMu.Unlock()
}

// toSavedPane serializes a pane subtree. A live shell pane can't be restored (its
// process is gone), so it is saved as a Main view.
func toSavedPane(p *pane) *savedPane {
	if p == nil {
		return nil
	}
	if p.leaf {
		sp := &savedPane{Leaf: true}
		switch {
		case p.summary:
			sp.Summary, sp.Agent = true, string(summaryPaneID)
		case p.diagram:
			sp.Diagram, sp.Agent = true, string(diagramPaneID)
		case p.shell, p.rev, p.term, p.empty:
			sp.Agent = string(core.Main)
		default:
			sp.Agent = string(p.agent)
		}
		return sp
	}
	return &savedPane{Dir: int(p.dir), Ratio: p.ratio, A: toSavedPane(p.a), B: toSavedPane(p.b)}
}

// fromSavedPane rebuilds a pane subtree, wiring up parent pointers. A malformed
// internal node (missing a child) collapses to whatever child exists, or a Main leaf.
func fromSavedPane(sp *savedPane) *pane {
	if sp == nil {
		return nil
	}
	if sp.Leaf {
		switch {
		case sp.Summary:
			return newSummaryLeaf()
		case sp.Diagram:
			return newDiagramLeaf()
		default:
			agent := core.AgentID(sp.Agent)
			if agent == "" {
				agent = core.Main
			}
			return newLeaf(agent)
		}
	}
	a, b := fromSavedPane(sp.A), fromSavedPane(sp.B)
	if a == nil || b == nil {
		if a != nil {
			return a
		}
		if b != nil {
			return b
		}
		return newLeaf(core.Main)
	}
	ratio := sp.Ratio
	if ratio <= 0 || ratio >= 1 {
		ratio = 0.5
	}
	p := &pane{dir: paneDir(sp.Dir), ratio: ratio, a: a, b: b}
	a.parent, b.parent = p, p
	return p
}

// restoreLayout rebuilds the window/pane layout from a saved engagement. It is a
// no-op (keeping the default layout) when there is nothing valid to restore.
func (m *model) restoreLayout(wins []savedWindow, winIdx int) {
	if len(wins) == 0 {
		return
	}
	built := make([]*windowState, 0, len(wins))
	for i, sw := range wins {
		root := fromSavedPane(sw.Root)
		if root == nil {
			root = newLeaf(core.Main)
		}
		// Pick the focused leaf: the one matching the saved focus, else the first leaf.
		ls := leaves(root)
		focus := ls[0]
		for _, l := range ls {
			if l.summary == sw.FocusSummary && l.diagram == sw.FocusDiag &&
				(sw.FocusSummary || sw.FocusDiag || string(l.agent) == sw.FocusAgent) {
				focus = l
				break
			}
		}
		name := sw.Name
		if name == "" {
			name = strconv.Itoa(i + 1)
		}
		built = append(built, &windowState{root: root, focus: focus, zoom: sw.Zoom, name: name})
	}
	m.windows = built
	if winIdx < 0 || winIdx >= len(built) {
		winIdx = 0
	}
	m.winIdx = winIdx
	m.activateWindow(winIdx) // loads root/focus/zoom and relays out
}

// restoreHistory seeds the orchestrator with its prior conversation and rebuilds
// every agent's transcript (and roster entry) from a saved engagement. Main
// continues live with its full context; the subagents are shown as historical
// (idle) so the user sees the prior conversation.
func (m *model) restoreHistory(recs []engine.AgentRecord) {
	for _, r := range recs {
		if r.ID == core.Main {
			// The LLM requires every assistant tool_call to be followed by its tool
			// results; a 15s snapshot can land mid-step, so trim to the longest valid
			// prefix before seeding. (The transcript rebuild below tolerates the full
			// history, so the display still shows everything.)
			m.sess.SeedHistory = sanitizeHistory(r.History)
			rebuildTranscript(m.tr(core.Main), r.History)
			continue
		}
		m.addAgent(r.ID)
		m.states[r.ID] = core.Idle // restored subagents are historical
		rebuildTranscript(m.tr(r.ID), r.History)
	}
}

// rebuildTranscript renders a saved message history into a transcript for display
// (approximate: tool results attach to the most recent tool call, which matches
// the common one-call-per-turn case). Thoughts are not persisted, so none show.
func rebuildTranscript(t *transcript, hist []llm.Message) {
	for _, msg := range hist {
		switch msg.Role {
		case llm.RoleUser:
			if s := strings.TrimSpace(msg.Content); s != "" {
				t.addStyled("in: "+s, styleAccent)
			}
		case llm.RoleAssistant:
			if s := strings.TrimSpace(msg.Content); s != "" {
				t.appendText(s)
				t.finalizeOpen()
			}
			for _, c := range msg.ToolCalls {
				header := "⚙  " + c.Name
				full := header
				if a := strings.TrimSpace(c.Arguments); a != "" {
					header += "  " + oneLineClip(a, 100)
					full += "  " + a
				}
				t.startTool(header, full)
			}
		case llm.RoleTool:
			if s := strings.TrimRight(msg.Content, "\n"); strings.TrimSpace(s) != "" {
				t.appendToolOut(s)
			}
		}
	}
	t.finalizeOpen()
}

// sanitizeHistory returns the longest prefix of h that is valid to resend to the
// model: every assistant tool_call message is followed by its tool results, and
// there is no dangling/partial tool exchange at the end (which a mid-step snapshot
// can produce).
func sanitizeHistory(h []llm.Message) []llm.Message {
	lastSafe, i := 0, 0
	for i < len(h) {
		m := h[i]
		switch m.Role {
		case llm.RoleUser, llm.RoleSystem:
			i++
			lastSafe = i
		case llm.RoleAssistant:
			if len(m.ToolCalls) == 0 {
				i++
				lastSafe = i
				continue
			}
			need, got, j := len(m.ToolCalls), 0, i+1
			for j < len(h) && h[j].Role == llm.RoleTool && got < need {
				j, got = j+1, got+1
			}
			if got < need { // incomplete tool exchange: drop it and everything after
				return h[:lastSafe]
			}
			i, lastSafe = j, j
		default: // an orphan tool result: skip it
			i++
		}
	}
	return h[:lastSafe]
}

// oneLineClip collapses whitespace and truncates to n runes (with an ellipsis).
func oneLineClip(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
