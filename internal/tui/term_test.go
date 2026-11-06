package tui

import (
	"bytes"
	"context"
	"io"
	"strings"
	"sync"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"github.com/nihaal-prasad/oline/internal/core"
	"github.com/nihaal-prasad/oline/internal/engine"
	"github.com/nihaal-prasad/oline/internal/sandbox"
)

// fakePTY records what the operator typed and whether it was closed/resized.
type fakePTY struct {
	mu        sync.Mutex
	in        bytes.Buffer
	closed    bool
	resizedTo [2]int
}

func (p *fakePTY) Write(b []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.in.Write(b)
}
func (p *fakePTY) Resize(rows, cols int) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.resizedTo = [2]int{rows, cols}
	return nil
}
func (p *fakePTY) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.closed = true
	return nil
}
func (p *fakePTY) typed() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.in.String()
}

// fakeInteractiveHandle is a sandbox.Handle that also implements sandbox.Interactive.
type fakeInteractiveHandle struct{ pty *fakePTY }

func (h *fakeInteractiveHandle) OpenTerminal(ctx context.Context, agent core.AgentID, cmd sandbox.Cmd, rows, cols int, out io.Writer) (sandbox.PTYSession, error) {
	return h.pty, nil
}
func (h *fakeInteractiveHandle) Exec(context.Context, core.AgentID, sandbox.Cmd, io.Writer) (sandbox.ExitStatus, error) {
	return sandbox.ExitStatus{}, nil
}
func (*fakeInteractiveHandle) CopyIn(context.Context, string, string) error     { return nil }
func (*fakeInteractiveHandle) CopyOut(context.Context, string, string) error    { return nil }
func (*fakeInteractiveHandle) Backup(context.Context, string, string) error     { return nil }
func (*fakeInteractiveHandle) EgressController() sandbox.EgressController       { return nil }
func (*fakeInteractiveHandle) SetupEgress(context.Context, sandbox.Scope) error { return nil }
func (*fakeInteractiveHandle) Info() sandbox.EnvInfo                            { return sandbox.EnvInfo{} }

func termModel(t *testing.T) (*model, *fakePTY) {
	t.Helper()
	m := runningModel(t)
	pty := &fakePTY{}
	m.sess = &engine.Session{Sandbox: &fakeInteractiveHandle{pty: pty}}
	return m, pty
}

func TestKeyToBytes(t *testing.T) {
	cases := []struct {
		name string
		msg  tea.KeyMsg
		want string
	}{
		{"rune", key("a"), "a"},
		{"enter", tea.KeyMsg{Type: tea.KeyEnter}, "\r"},
		{"tab", tea.KeyMsg{Type: tea.KeyTab}, "\t"},
		{"backspace", tea.KeyMsg{Type: tea.KeyBackspace}, "\x7f"},
		{"esc", tea.KeyMsg{Type: tea.KeyEsc}, "\x1b"},
		{"up", tea.KeyMsg{Type: tea.KeyUp}, "\x1b[A"},
		{"down", tea.KeyMsg{Type: tea.KeyDown}, "\x1b[B"},
		{"right", tea.KeyMsg{Type: tea.KeyRight}, "\x1b[C"},
		{"left", tea.KeyMsg{Type: tea.KeyLeft}, "\x1b[D"},
		{"ctrlc", tea.KeyMsg{Type: tea.KeyCtrlC}, "\x03"},
		{"ctrld", tea.KeyMsg{Type: tea.KeyCtrlD}, "\x04"},
		{"altx", tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("x"), Alt: true}, "\x1bx"},
	}
	for _, c := range cases {
		if got := string(keyToBytes(c.msg)); got != c.want {
			t.Errorf("%s: keyToBytes = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestTerminalPaneRendersOutput(t *testing.T) {
	m, _ := termModel(t)
	m.openTermPane()
	ts := m.focusTerm()
	if ts == nil {
		t.Fatal("no focused terminal pane after openTermPane")
	}
	// Simulate the child writing to the terminal.
	ts.emu.Write([]byte("hello from the pty"))
	out := ansi.Strip(m.renderTerm(m.focusPane, 40, 10, true))
	if !strings.Contains(out, "hello from the pty") {
		t.Errorf("terminal pane did not render child output:\n%s", out)
	}
}

func TestTerminalForwardsKeystrokes(t *testing.T) {
	m, pty := termModel(t)
	m.openTermPane()
	if m.focusTerm() == nil {
		t.Fatal("no focused terminal pane")
	}
	// A focused terminal consumes keys and forwards their bytes to the PTY.
	m.handleRunningKey(key("l"))
	m.handleRunningKey(key("s"))
	m.handleRunningKey(tea.KeyMsg{Type: tea.KeyEnter})
	m.handleRunningKey(tea.KeyMsg{Type: tea.KeyCtrlC})
	if got := pty.typed(); got != "ls\r\x03" {
		t.Errorf("keystrokes forwarded = %q, want %q", got, "ls\r\x03")
	}
	// Ctrl-b is held back as oline's prefix, not sent to the child.
	before := pty.typed()
	m.handleRunningKey(tea.KeyMsg{Type: tea.KeyCtrlB})
	if !m.prefix {
		t.Error("Ctrl-b in a terminal pane should arm the oline prefix")
	}
	if pty.typed() != before {
		t.Error("Ctrl-b should not be forwarded to the child")
	}
	// Ctrl-b Ctrl-b sends a literal Ctrl-b (0x02) to the child.
	m.handleRunningKey(tea.KeyMsg{Type: tea.KeyCtrlB})
	if got := pty.typed(); got != before+"\x02" {
		t.Errorf("Ctrl-b Ctrl-b should send a literal 0x02, got %q", got)
	}
}

func TestTerminalReapClosesSession(t *testing.T) {
	if raceDetectorOn {
		t.Skip("skipped under -race: known benign data race in charmbracelet/x/vt@v0.1.0 on terminal teardown (see raceflag_on_test.go)")
	}
	m, pty := termModel(t)
	m.openTermPane()
	id := m.focusPane.agent
	if _, ok := m.terms[id]; !ok {
		t.Fatal("terminal not tracked")
	}
	m.closePaneCmd()
	if _, ok := m.terms[id]; ok {
		t.Error("closing the terminal pane did not reap it")
	}
	if !pty.closed {
		t.Error("closing the terminal pane did not close its PTY session")
	}
}

func TestTerminalListedUnderCommandPanes(t *testing.T) {
	m, _ := termModel(t)
	m.openTermPane()
	out := ansi.Strip(m.renderRoster(m.rightW, m.bodyH))
	if !strings.Contains(out, "command panes") || !strings.Contains(out, "Term#1") {
		t.Errorf("terminal should be listed under command panes:\n%s", out)
	}
}

func TestListenPortValidation(t *testing.T) {
	m, _ := termModel(t)
	m.startListenInput()
	if !m.listenInput {
		t.Fatal("startListenInput did not enter port-entry mode")
	}
	m.input.SetValue("not-a-port")
	m.applyListenInput()
	if m.listenInput {
		t.Error("applyListenInput should exit port-entry mode even on bad input")
	}
	if len(m.terms) != 0 {
		t.Error("a bad port must not open a terminal")
	}
	// A valid port opens a catcher terminal.
	m.startListenInput()
	m.input.SetValue("4444")
	m.applyListenInput()
	if len(m.terms) != 1 {
		t.Errorf("a valid port should open one catcher terminal, got %d", len(m.terms))
	}
}

func ctrlb() tea.KeyMsg { return tea.KeyMsg{Type: tea.KeyCtrlB} }

func TestKillTerminalPane(t *testing.T) {
	if raceDetectorOn {
		t.Skip("skipped under -race: known benign data race in charmbracelet/x/vt@v0.1.0 on terminal teardown (see raceflag_on_test.go)")
	}
	m, pty := termModel(t)
	m.openTermPane()
	id := m.focusPane.agent
	// Ctrl-b K kills the terminal's PTY session and closes the pane.
	m.handleRunningKey(ctrlb())
	m.handleRunningKey(key("K"))
	if _, ok := m.terms[id]; ok {
		t.Error("Ctrl-b K should have reaped the terminal pane")
	}
	if !pty.closed {
		t.Error("Ctrl-b K should have closed the terminal's PTY session")
	}
}

func TestKillReverseShellPane(t *testing.T) {
	m, _ := termModel(t)
	m.applyChannel("c1", 9001, "/opt/oline/engagements/x/channels/c1", true)
	m.openRevPane("c1")
	id := m.focusPane.agent
	if m.focusRev() == nil {
		t.Fatal("expected a focused reverse-shell pane")
	}
	// Ctrl-b K kills the channel and closes the operator pane.
	m.handleRunningKey(ctrlb())
	m.handleRunningKey(key("K"))
	if _, ok := m.revs[id]; ok {
		t.Error("Ctrl-b K should have reaped the reverse-shell pane")
	}
}

// TestSocatEnsureScript: the auto-install snippet short-circuits when socat is
// present, tries a package manager otherwise, and ends with a status marker the
// ensureSocat switch keys off.
func TestSocatEnsureScript(t *testing.T) {
	for _, want := range []string{
		"command -v socat",
		"apt-get install -y socat",
		"socat: already present",
		"socat: installed",
		"socat: install failed",
	} {
		if !strings.Contains(socatEnsureScript, want) {
			t.Errorf("socatEnsureScript missing %q", want)
		}
	}
}
