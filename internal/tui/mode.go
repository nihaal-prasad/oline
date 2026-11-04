package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/x/ansi"

	"github.com/nihaal-prasad/oline/internal/core"
	"github.com/nihaal-prasad/oline/internal/engine"
)

// Operating-mode switching and the "share a pane with the AI" action.
//
// The engagement mode (automatic | planner | watcher) can change mid-run. Ctrl-b m
// cycles it; the change takes effect immediately (the engine reads the mode live,
// so watcher's execution block applies to the very next tool call) and the
// orchestrator is told so the models adapt. Watcher mode is "share on demand": the
// AI never ingests the terminals on its own, so Ctrl-b a pushes the focused pane's
// recent content to the orchestrator when the operator wants its read on it.

// cycleMode advances automatic -> planner -> watcher -> automatic (Ctrl-b m).
func (m *model) cycleMode() {
	if m.sess == nil {
		m.status = "no engagement yet"
		return
	}
	m.setRunMode((m.sess.Mode + 1) % 3)
}

// setRunMode switches the engagement mode and notifies the orchestrator so it (and,
// via its task prompts, its subagents) adapts. Enforcement is immediate regardless,
// since the engine checks the mode live on every tool call.
func (m *model) setRunMode(mode core.RunMode) {
	if m.sess == nil {
		return
	}
	prev := m.sess.Mode
	m.sess.Mode = mode
	m.status = "mode: " + mode.String()
	if m.router != nil {
		body := fmt.Sprintf("OPERATOR SWITCHED THE ENGAGEMENT MODE: %s -> %s. %s", prev, mode, modeBrief(mode))
		m.router.Send(engine.Envelope{From: "User", To: core.Main, Kind: engine.MsgSteer, Body: body})
		m.tr(core.Main).addStyled("── mode switched to "+mode.String()+" ──", styleAccent)
		m.refreshFor(core.Main)
	}
	if m.log != nil {
		m.log.userMsg(core.Main, "mode switched to "+mode.String())
	}
}

// modeBrief is the one-line contract the orchestrator is handed on a mode switch.
func modeBrief(mode core.RunMode) string {
	switch mode {
	case core.Watcher:
		return "WATCHER: you execute NOTHING (bash, bash_bg and revshell are hard-blocked by the harness); the operator runs every command themselves in the terminal panes. Watch, read and write files in the workspace, and ADVISE with concise concrete guidance. Do not spawn action subagents to run things (they are blocked too); use read-only/advisory help only. Tell any subagent you do spawn that it is in watcher mode. The operator shares terminal output with you when they want your read on it."
	case core.Planner:
		return "PLANNER: propose each step and get the operator's EXPLICIT approval before anything runs; assume a plan is unapproved until they clearly say yes. One approved step at a time. (You still carry out an approved step yourself.)"
	default:
		return "AUTOMATIC: run the engagement end-to-end yourself, within scope, pausing only for the usual gates (destructive actions, scope ambiguity, when you are unsure)."
	}
}

const sharePaneMaxChars = 6000 // cap on how much pane content is pushed to the AI

// sharePaneToAI pushes the focused pane's recent content to the orchestrator as an
// observation (Ctrl-b a). This is how the operator "shows" the AI what is on a
// terminal/shell/channel screen in watcher mode (and is handy in any mode).
func (m *model) sharePaneToAI() {
	if m.sess == nil || m.router == nil {
		m.status = "no engagement yet"
		return
	}
	if m.focusPane == nil {
		m.status = "no focused pane to share"
		return
	}
	label, content := m.paneText(m.focusPane)
	content = strings.TrimRight(content, "\n")
	if strings.TrimSpace(content) == "" {
		m.status = "nothing to share from this pane"
		return
	}
	if len(content) > sharePaneMaxChars { // keep the most recent tail
		content = content[len(content)-sharePaneMaxChars:]
	}
	body := fmt.Sprintf("[operator shared the %q pane for your read — this is what is on the operator's screen; observe and advise, do not act on it yourself]\n```\n%s\n```", label, content)
	m.router.Send(engine.Envelope{From: "User", To: core.Main, Kind: engine.MsgSteer, Body: body})
	m.log.userMsg(core.Main, "shared the "+label+" pane")
	m.tr(core.Main).addStyled("you shared the "+label+" pane with "+string(core.Main), styleAccent)
	m.refreshFor(core.Main)
	m.status = "shared the " + label + " pane with the orchestrator"
}

// paneText returns a short label and the current text content of a pane, for
// sharing with the AI: a terminal pane's rendered screen, or any other pane's
// transcript / live view text.
func (m *model) paneText(p *pane) (label, content string) {
	switch {
	case p.term:
		lbl := string(p.agent) + " (terminal)"
		if ts := m.terms[p.agent]; ts != nil && ts.emu != nil {
			return lbl, ansi.Strip(ts.emu.Render())
		}
		return lbl, ""
	case p.summary:
		return "Summary", m.summary
	case p.diagram:
		return "Diagram", m.diagram
	default:
		// agent, shell and reverse-shell panes all render from a transcript.
		return string(p.agent), m.tr(p.agent).plainText(m.showThoughts)
	}
}
