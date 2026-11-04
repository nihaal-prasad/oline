package tui

import (
	"context"
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/nihaal-prasad/oline/internal/core"
	"github.com/nihaal-prasad/oline/internal/sandbox"
)

// A shell pane lets the user run commands directly in the sandbox, instead of
// only watching an agent. It runs as root (Privileged): the egress rules
// constrain AGENTS, not the operator's own shell. Each command is a fresh
// `bash -lc`, so the working directory is carried across commands explicitly via
// a trailing marker the output handler strips (see runShell / addShellLine).

const (
	shellHome     = "/opt/oline/engagements" // starting working directory
	cwdMarker     = "__OLINE_CWD__"          // sentinel prefixing the trailing PWD line
	shellMaxBytes = 1 << 20                  // soft cap on retained output per command
)

// shellState is one shell pane's live state, keyed by a synthetic agent id.
type shellState struct {
	id        core.AgentID
	cwd       string
	cancel    context.CancelFunc
	busy      bool
	truncated bool
	pend      strings.Builder // partial (not-yet-newline-terminated) output
	bytes     int             // output bytes retained for the current command
}

func (s *shellState) running() bool { return s != nil && s.busy }

// stop cancels the running command, if any. Returns whether one was running.
func (s *shellState) stop() bool {
	if s != nil && s.busy && s.cancel != nil {
		s.cancel()
		return true
	}
	return false
}

// shellMsg carries one chunk of a shell command's output, or its completion,
// back to the Bubble Tea update loop (model mutation must happen there, not in
// the exec goroutine).
type shellMsg struct {
	id       core.AgentID
	chunk    string
	done     bool
	code     int
	err      error
	canceled bool
}

// listenShell waits for the next shell output message and delivers it as a tea.Msg.
func listenShell(ch chan shellMsg) tea.Cmd {
	return func() tea.Msg { return <-ch }
}

// shellWriter forwards exec output to the shell channel as it streams.
type shellWriter struct {
	ch chan shellMsg
	id core.AgentID
}

func (w *shellWriter) Write(p []byte) (int, error) {
	w.ch <- shellMsg{id: w.id, chunk: string(p)}
	return len(p), nil
}

// focusShell returns the shell state of the focused pane, or nil if the focused
// pane is an agent view.
func (m *model) focusShell() *shellState {
	if m.focusPane == nil || !m.focusPane.shell {
		return nil
	}
	return m.shells[m.focusPane.agent]
}

// anyShellPane reports whether any window currently contains a shell pane (so the
// default shell tab isn't added when one already exists, e.g. via a restored layout).
func (m *model) anyShellPane() bool {
	for _, w := range m.windows {
		for _, l := range leaves(w.root) {
			if l.shell {
				return true
			}
		}
	}
	// Also check the active window's live tree, in case it hasn't been mirrored yet.
	for _, l := range leaves(m.paneRoot) {
		if l.shell {
			return true
		}
	}
	return false
}

// shellIDsSorted returns the live shell ids in the order they were opened (Shell#1,
// Shell#2, …), skipping any that have been closed.
func (m *model) shellIDsSorted() []core.AgentID {
	var out []core.AgentID
	for i := 1; i <= m.shellSeq; i++ {
		id := core.AgentID(fmt.Sprintf("Shell#%d", i))
		if _, ok := m.shells[id]; ok {
			out = append(out, id)
		}
	}
	return out
}

// focusShellByID jumps focus to the pane showing shell id, switching to its window
// (tab) if it lives in another one. A shell is tied to its leaf, so this focuses the
// existing pane rather than retargeting the current one.
func (m *model) focusShellByID(id core.AgentID) {
	m.saveWindow() // mirror the active window's live state before searching all windows
	for wi, w := range m.windows {
		for _, l := range leaves(w.root) {
			if l.shell && l.agent == id {
				m.activateWindow(wi)
				m.focusPane = l
				m.saveWindow()
				m.relayout()
				m.status = "focused " + string(id)
				return
			}
		}
	}
}

// shellTranscript returns a shell pane's transcript WITHOUT registering it as an
// agent (so it never shows up in the roster or the states map).
func (m *model) shellTranscript(id core.AgentID) *transcript {
	t := m.transcripts[id]
	if t == nil {
		t = &transcript{}
		m.transcripts[id] = t
	}
	return t
}

// createShellLeaf allocates a new shell (state + seeded transcript) and returns a
// leaf showing it, WITHOUT placing it in the pane tree. Callers decide where it
// goes (a split via openShellPane, or a new window's root for the default tab).
func (m *model) createShellLeaf() *pane {
	if m.shells == nil {
		m.shells = map[core.AgentID]*shellState{}
	}
	m.shellSeq++
	id := core.AgentID(fmt.Sprintf("Shell#%d", m.shellSeq))
	sh := &shellState{id: id, cwd: shellHome}
	m.shells[id] = sh

	t := m.shellTranscript(id)
	t.addStyled("interactive shell in the sandbox (runs as root). type a command, Enter runs it; Ctrl-b S stops a running one.", styleMuted)
	t.addRich(styleMuted.Render("cwd: " + sh.cwd))
	return newShellLeaf(id)
}

// openShellPane splits the focused pane and makes the new pane an interactive
// shell. Bound to Ctrl-b $.
func (m *model) openShellPane() {
	if m.paneRoot == nil || m.focusPane == nil {
		return
	}
	m.zoom = false
	leaf := m.createShellLeaf()
	m.paneRoot, m.focusPane = splitPaneWith(m.paneRoot, m.focusPane, splitV, leaf)
	m.relayout()
	m.status = "opened shell pane (" + string(leaf.agent) + ")"
}

// runShell executes cmd in the shell pane sh. It runs as root in the sandbox and
// preserves the working directory across commands.
func (m *model) runShell(sh *shellState, cmd string) {
	t := m.shellTranscript(sh.id)
	if sh.busy {
		t.addStyled("[a command is still running — Ctrl-b S stops it]", styleWarn)
		m.refreshFor(sh.id)
		return
	}
	if m.sess == nil || m.sess.Sandbox == nil {
		t.addStyled("[no sandbox attached]", styleErr)
		m.refreshFor(sh.id)
		return
	}
	t.addStyled("$ "+cmd, styleAccent)

	sh.busy = true
	sh.bytes = 0
	sh.truncated = false
	ctx, cancel := context.WithCancel(context.Background())
	sh.cancel = cancel

	// cd into the tracked dir (non-fatal if it vanished), run the command, then
	// print the resulting PWD on its own marked line so the next command resumes
	// there. The marker line is stripped from the display.
	script := fmt.Sprintf("cd %s 2>/dev/null; { %s\n }; __rc=$?; printf '\\n%s%%s\\n' \"$PWD\"; exit $__rc",
		shQuote(sh.cwd), cmd, cwdMarker)

	id, sb, ch := sh.id, m.sess.Sandbox, m.shellCh
	go func() {
		w := &shellWriter{ch: ch, id: id}
		st, err := sb.Exec(ctx, core.AgentID("User"), sandbox.Cmd{
			Argv:   []string{"bash", "-lc", script},
			Dir:    shellHome,
			Policy: sandbox.Privileged,
		}, w)
		canceled := ctx.Err() != nil
		cancel()
		ch <- shellMsg{id: id, done: true, code: st.Code, err: err, canceled: canceled}
	}()
	m.refreshFor(sh.id)
}

// applyShellMsg folds one shell output/completion message into the transcript.
// Reverse-shell panes share shellCh, so route their messages first.
func (m *model) applyShellMsg(msg shellMsg) {
	if rs := m.revs[msg.id]; rs != nil {
		m.applyRevMsg(rs, msg)
		return
	}
	sh := m.shells[msg.id]
	if sh == nil {
		return
	}
	t := m.shellTranscript(msg.id)
	if msg.chunk != "" {
		sh.pend.WriteString(msg.chunk)
		m.drainShellLines(sh, t, false)
	}
	if msg.done {
		m.drainShellLines(sh, t, true)
		sh.busy = false
		sh.cancel = nil
		switch {
		case msg.canceled:
			t.addStyled("[stopped]", styleWarn)
		case msg.err != nil:
			t.addStyled(fmt.Sprintf("[error: %v]", msg.err), styleErr)
		default:
			t.addRich(styleMuted.Render(fmt.Sprintf("[exit %d]", msg.code)))
		}
		t.addRich(styleMuted.Render("cwd: " + sh.cwd))
	}
	m.refreshFor(msg.id)
}

// drainShellLines emits every complete line buffered in sh.pend. When flush is
// true (command finished) it also emits any trailing partial line.
func (m *model) drainShellLines(sh *shellState, t *transcript, flush bool) {
	s := sh.pend.String()
	sh.pend.Reset()
	for {
		i := strings.IndexByte(s, '\n')
		if i < 0 {
			break
		}
		m.addShellLine(sh, t, s[:i])
		s = s[i+1:]
	}
	if flush {
		if strings.TrimRight(s, "\r") != "" {
			m.addShellLine(sh, t, s)
		}
		return
	}
	sh.pend.WriteString(s) // keep the unterminated remainder for the next chunk
}

// addShellLine appends one output line, stripping the cwd marker (updating the
// tracked directory) and enforcing the per-command output cap.
func (m *model) addShellLine(sh *shellState, t *transcript, line string) {
	line = strings.TrimRight(line, "\r")
	if strings.HasPrefix(line, cwdMarker) {
		if cwd := strings.TrimPrefix(line, cwdMarker); cwd != "" {
			sh.cwd = cwd
		}
		return
	}
	if sh.truncated {
		return
	}
	sh.bytes += len(line) + 1
	if sh.bytes > shellMaxBytes {
		sh.truncated = true
		t.addStyled("[output truncated]", styleWarn)
		return
	}
	t.addRich(line)
}

// shQuote single-quotes s for safe interpolation into a bash script.
func shQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
