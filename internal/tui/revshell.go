package tui

import (
	"context"
	"fmt"
	"strings"

	"github.com/nihaal-prasad/oline/internal/core"
	"github.com/nihaal-prasad/oline/internal/sandbox"
)

// Reverse-shell channel panes.
//
// Agents set up reverse-shell channels with the `revshell` tool: a named netcat
// listener inside the sandbox that catches a shell connecting back from the target,
// backed by a guest-side input FIFO and an output log (see internal/engine). The
// engine announces each open/close with an event.Channel, which the TUI tracks so
// the channels appear under "command panes" in the Options sidebar.
//
// A reverse-shell pane is the operator's window onto one channel. It is the mirror
// image of a shell pane (shell.go): instead of running fresh `bash -lc` commands,
// it streams the channel's out.log with `tail -f` and, when focused, sends each
// typed line into the channel's input FIFO — straight to the connected shell. It is
// never open by default; the user opens one by clicking the channel's row.

// channelInfo is the TUI's record of one reverse-shell channel the engine reported.
type channelInfo struct {
	name string
	port int
	dir  string // absolute guest path to the channel's working directory
	open bool   // the listener is up (false after a close event)
}

// revState is one open reverse-shell pane's live state, keyed by its synthetic
// pane id (rev#<name>).
type revState struct {
	id        core.AgentID
	name      string
	dir       string
	port      int
	cancel    context.CancelFunc // stops the tail stream
	pend      strings.Builder    // partial (not-yet-newline-terminated) output
	bytes     int                // output bytes retained
	truncated bool
}

// revID is the synthetic pane/transcript id for a channel's reverse-shell pane.
func revID(name string) core.AgentID { return core.AgentID("rev#" + name) }

// focusRev returns the reverse-shell state of the focused pane, or nil.
func (m *model) focusRev() *revState {
	if m.focusPane == nil || !m.focusPane.rev {
		return nil
	}
	return m.revs[m.focusPane.agent]
}

// focusCommandPane reports whether the focused pane is a command pane (a user shell
// or a reverse-shell channel) rather than an agent view.
func (m *model) focusCommandPane() bool {
	return m.focusPane != nil && (m.focusPane.shell || m.focusPane.rev || m.focusPane.term)
}

// channelsSorted returns the known channels in the order they were opened.
func (m *model) channelsSorted() []*channelInfo {
	var out []*channelInfo
	for _, n := range m.channelOrd {
		if c := m.channels[n]; c != nil {
			out = append(out, c)
		}
	}
	return out
}

// applyChannel folds an engine channel open/close event into the model.
func (m *model) applyChannel(name string, port int, dir string, open bool) {
	if m.channels == nil {
		m.channels = map[string]*channelInfo{}
	}
	if open {
		if c := m.channels[name]; c != nil {
			c.port, c.dir, c.open = port, dir, true
		} else {
			m.channels[name] = &channelInfo{name: name, port: port, dir: dir, open: true}
			m.channelOrd = append(m.channelOrd, name)
		}
		m.status = fmt.Sprintf("reverse-shell channel %q opened on port %d", name, port)
		return
	}
	// Closed: note it in any open pane, then drop it from the list.
	if c := m.channels[name]; c != nil {
		c.open = false
	}
	if _, ok := m.revs[revID(name)]; ok {
		t := m.shellTranscript(revID(name))
		t.addStyled("[channel closed — the listener has stopped]", styleWarn)
		m.refreshFor(revID(name))
	}
	delete(m.channels, name)
	for i, n := range m.channelOrd {
		if n == name {
			m.channelOrd = append(m.channelOrd[:i], m.channelOrd[i+1:]...)
			break
		}
	}
	m.status = fmt.Sprintf("reverse-shell channel %q closed", name)
}

// openRevPane opens (or focuses) a reverse-shell pane for the named channel. A
// channel's pane is tied to its leaf, so if one already exists this jumps to it;
// otherwise it splits the focused pane and starts streaming the channel.
func (m *model) openRevPane(name string) {
	c := m.channels[name]
	if c == nil {
		m.status = "no such reverse-shell channel: " + name
		return
	}
	id := revID(name)
	// Already open somewhere: focus that pane (switching tabs if needed).
	if _, ok := m.revs[id]; ok {
		m.focusRevByID(id)
		return
	}
	if m.paneRoot == nil || m.focusPane == nil {
		return
	}
	if m.revs == nil {
		m.revs = map[core.AgentID]*revState{}
	}
	rs := &revState{id: id, name: name, dir: c.dir, port: c.port}
	m.revs[id] = rs

	t := m.shellTranscript(id)
	t.addStyled(fmt.Sprintf("reverse-shell channel %q (port %d). Live output below; type a command and Enter to send it to the connected shell.", name, c.port), styleMuted)

	m.zoom = false
	leaf := newRevLeaf(id)
	m.paneRoot, m.focusPane = splitPaneWith(m.paneRoot, m.focusPane, splitV, leaf)
	m.startRevTail(rs)
	m.relayout()
	m.status = "opened reverse-shell pane (" + name + ")"
}

// focusRevByID jumps focus to the pane showing reverse-shell id, switching window
// (tab) if it lives in another one.
func (m *model) focusRevByID(id core.AgentID) {
	m.saveWindow()
	for wi, w := range m.windows {
		for _, l := range leaves(w.root) {
			if l.rev && l.agent == id {
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

// startRevTail streams the channel's output log into the pane with `tail -f`, until
// the pane is closed (cancel) or the engagement ends. It runs as root so it can read
// the channel's log regardless of which agent owns it.
func (m *model) startRevTail(rs *revState) {
	if m.sess == nil || m.sess.Sandbox == nil {
		m.shellTranscript(rs.id).addStyled("[no sandbox attached]", styleErr)
		m.refreshFor(rs.id)
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	rs.cancel = cancel
	// Print the whole log so far, then follow it. `-F` (not `-f`) keeps following
	// across truncation/recreation of out.log, so a reopened channel still streams.
	script := fmt.Sprintf(`exec tail -n +1 -F %s/out.log 2>/dev/null`, shQuote(rs.dir))
	id, sb, ch := rs.id, m.sess.Sandbox, m.shellCh
	go func() {
		w := &shellWriter{ch: ch, id: id}
		_, _ = sb.Exec(ctx, core.AgentID("User"), sandbox.Cmd{
			Argv:   []string{"bash", "-lc", script},
			Dir:    shellHome,
			Policy: sandbox.Privileged,
		}, w)
		cancel()
		ch <- shellMsg{id: id, done: true, canceled: true}
	}()
	m.refreshFor(rs.id)
}

// sendRev writes one command line into the channel's input FIFO, delivering it to
// the connected shell. The response arrives via the tail stream, so this only echoes
// the line locally and fires the write off; it does not block.
func (m *model) sendRev(rs *revState, cmd string) {
	t := m.shellTranscript(rs.id)
	t.addStyled("$ "+cmd, styleAccent)
	m.auditUser("user.revshell.send", rs.name, cmd) // record the operator's command (host audit log)
	if m.sess == nil || m.sess.Sandbox == nil {
		t.addStyled("[no sandbox attached]", styleErr)
		m.refreshFor(rs.id)
		return
	}
	// The listener holds the FIFO's read end open, so this write never blocks; if the
	// channel was closed, the write simply fails and is ignored.
	script := fmt.Sprintf(`printf '%%s\n' %s > %s/in 2>/dev/null`, shQuote(cmd), shQuote(rs.dir))
	sb := m.sess.Sandbox
	go func() {
		_, _ = sb.Exec(context.Background(), core.AgentID("User"), sandbox.Cmd{
			Argv:   []string{"bash", "-lc", script},
			Dir:    shellHome,
			Policy: sandbox.Privileged,
		}, discardWriter{})
	}()
	m.refreshFor(rs.id)
}

// applyRevMsg folds one streamed output/termination message into a reverse-shell
// pane's transcript.
func (m *model) applyRevMsg(rs *revState, msg shellMsg) {
	t := m.shellTranscript(msg.id)
	if msg.chunk != "" {
		rs.pend.WriteString(msg.chunk)
		m.drainRevLines(rs, t, false)
	}
	if msg.done {
		m.drainRevLines(rs, t, true)
		rs.cancel = nil
		// A cancelled stream is the normal result of closing the pane; only note an
		// unexpected end (the tail process died on its own) so the user isn't left
		// staring at a frozen pane.
		if !msg.canceled {
			t.addStyled("[output stream ended]", styleWarn)
		}
	}
	m.refreshFor(msg.id)
}

// drainRevLines emits every complete line buffered in rs.pend (mirrors the shell
// pane's drain), enforcing a soft output cap.
func (m *model) drainRevLines(rs *revState, t *transcript, flush bool) {
	s := rs.pend.String()
	rs.pend.Reset()
	for {
		i := strings.IndexByte(s, '\n')
		if i < 0 {
			break
		}
		m.addRevLine(rs, t, s[:i])
		s = s[i+1:]
	}
	if flush {
		if strings.TrimRight(s, "\r") != "" {
			m.addRevLine(rs, t, s)
		}
		return
	}
	rs.pend.WriteString(s)
}

func (m *model) addRevLine(rs *revState, t *transcript, line string) {
	line = strings.TrimRight(line, "\r")
	if rs.truncated {
		return
	}
	rs.bytes += len(line) + 1
	if rs.bytes > shellMaxBytes {
		rs.truncated = true
		t.addStyled("[output truncated — close and reopen the pane to see the latest]", styleWarn)
		return
	}
	t.addRich(line)
}

// reapRev stops the tail stream backing a reverse-shell pane and forgets its
// bookkeeping, so closing the pane or its window leaves nothing streaming. The
// channel itself (the listener) stays up until an agent runs `revshell close`.
func (m *model) reapRev(p *pane) {
	if p == nil || !p.rev {
		return
	}
	if rs := m.revs[p.agent]; rs != nil {
		if rs.cancel != nil {
			rs.cancel()
		}
		delete(m.revs, p.agent)
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

// discardWriter swallows exec output (used for fire-and-forget FIFO writes).
type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }
