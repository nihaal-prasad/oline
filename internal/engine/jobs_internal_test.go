package engine

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nihaal-prasad/oline/internal/core"
	"github.com/nihaal-prasad/oline/internal/event"
	"github.com/nihaal-prasad/oline/internal/llm"
	"github.com/nihaal-prasad/oline/internal/sandbox"
	"github.com/nihaal-prasad/oline/internal/tools"
)

// jobSB emulates a running command: its single Exec blocks until the command is
// told to finish (m.finish) or its context is cancelled (stop / engagement end),
// counting cancellations so tests can assert a command was reaped.
type jobSB struct {
	mu         sync.Mutex
	canceled   int
	finish     chan struct{}
	failLaunch bool // when set, Exec returns an infrastructure error immediately
}

func (m *jobSB) Exec(ctx context.Context, _ core.AgentID, cmd sandbox.Cmd, out io.Writer) (sandbox.ExitStatus, error) {
	if m.failLaunch {
		return sandbox.ExitStatus{Code: -1}, errors.New("exec failed: cannot start command")
	}
	select {
	case <-ctx.Done():
		m.mu.Lock()
		m.canceled++
		m.mu.Unlock()
		return sandbox.ExitStatus{Code: -1}, ctx.Err()
	case <-m.finish:
		io.WriteString(out, "job output\n")
		return sandbox.ExitStatus{Code: 0}, nil
	}
}

func (m *jobSB) killCount() int { m.mu.Lock(); defer m.mu.Unlock(); return m.canceled }

func (*jobSB) CopyIn(context.Context, string, string) error     { return nil }
func (*jobSB) CopyOut(context.Context, string, string) error    { return nil }
func (*jobSB) Backup(context.Context, string, string) error     { return nil }
func (*jobSB) EgressController() sandbox.EgressController       { return nil }
func (*jobSB) SetupEgress(context.Context, sandbox.Scope) error { return nil }
func (*jobSB) Info() sandbox.EnvInfo                            { return sandbox.EnvInfo{} }

func newJobAgent(t *testing.T, sb sandbox.Handle) (*Agent, *Session, context.CancelFunc) {
	t.Helper()
	events := make(chan event.Event, 256)
	go func() {
		for range events {
		}
	}()
	s := NewSession(tools.NewRegistry(), sb, nil, func(string) (AgentConfig, bool) { return AgentConfig{}, false }, events, 5)
	ctx, cancel := context.WithCancel(context.Background())
	s.setBase(ctx)
	a := newAgent("recon#1", AgentConfig{Role: "recon", Tools: []string{"bash", "bash_bg"}}, nil, "m", s, "", "")
	s.router.register(a)
	return a, s, cancel
}

// TestBashBgDetachedStopKills verifies a background job runs detached (registered,
// not killed on its own) and is reaped by an explicit StopCommands via an in-guest
// process-group kill.
func TestBashBgDetachedStopKills(t *testing.T) {
	sb := &jobSB{finish: make(chan struct{})}
	a, _, cancel := newJobAgent(t, sb)
	defer cancel()

	out, isErr := a.doBashBg(llm.ToolCall{Name: "bash_bg", Arguments: `{"cmd":"python server.py"}`})
	if isErr {
		t.Fatalf("bash_bg errored: %s", out)
	}
	a.mu.Lock()
	n := len(a.jobs)
	a.mu.Unlock()
	if n != 1 {
		t.Fatalf("expected 1 running detached job, got %d", n)
	}

	if got := a.StopCommands(); got != 1 {
		t.Errorf("StopCommands returned %d, want 1", got)
	}
	// The streamer sees the tail cancelled and group-kills the detached tree.
	waitFor(t, 2*time.Second, func() bool { return sb.killCount() > 0 })
}

// TestBashBgNaturalFinish verifies a background job that completes on its own posts
// a MsgJobDone and is not group-killed.
func TestBashBgNaturalFinish(t *testing.T) {
	sb := &jobSB{finish: make(chan struct{})}
	a, _, cancel := newJobAgent(t, sb)
	defer cancel()

	if _, isErr := a.doBashBg(llm.ToolCall{Name: "bash_bg", Arguments: `{"cmd":"echo hi"}`}); isErr {
		t.Fatal("bash_bg errored")
	}
	close(sb.finish) // job completes

	// MsgJobDone is delivered to the agent's own mailbox.
	deadline := time.After(2 * time.Second)
	for {
		select {
		case env := <-a.box:
			if env.Kind == MsgJobDone {
				if sb.killCount() != 0 {
					t.Error("a naturally-finished job should not be group-killed")
				}
				return
			}
		case <-deadline:
			t.Fatal("no MsgJobDone after the job finished")
		}
	}
}

// TestForegroundBashWakesOnMessage verifies a user message woke an agent that was
// blocked on a foreground command: the command is backgrounded (not killed) and
// the agent is freed with a "woken early" note so it can read the message.
func TestForegroundBashWakesOnMessage(t *testing.T) {
	sb := &jobSB{finish: make(chan struct{})}
	a, _, cancel := newJobAgent(t, sb)
	defer cancel()

	type res struct {
		out   string
		isErr bool
	}
	done := make(chan res, 1)
	go func() {
		out, isErr := a.doBash(llm.ToolCall{Name: "bash", Arguments: `{"cmd":"python server.py"}`})
		done <- res{out, isErr}
	}()

	// Wait until the foreground command is registered.
	waitFor(t, 2*time.Second, func() bool { a.mu.Lock(); defer a.mu.Unlock(); return a.fgJob != nil })

	a.s.router.Send(Envelope{From: "User", To: a.id, Kind: MsgSteer, Body: "stop and look at this"})

	select {
	case r := <-done:
		if r.isErr {
			t.Errorf("woke with an error result: %s", r.out)
		}
		if !strings.Contains(r.out, "woken early") {
			t.Errorf("expected a woken-early note, got: %s", r.out)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("foreground command did not wake on the user message")
	}

	a.mu.Lock()
	njobs, nfg := len(a.jobs), a.fgJob
	a.mu.Unlock()
	if njobs != 1 || nfg != nil {
		t.Errorf("woken command should be moved to the background (jobs=%d fg=%v)", njobs, nfg)
	}
	if sb.killCount() != 0 {
		t.Error("waking on a message must not kill the command")
	}
}

// TestBashExecErrorSurfaced checks that an infrastructure error from the sandbox is
// surfaced in the result (and isn't a silent empty output).
func TestBashExecErrorSurfaced(t *testing.T) {
	sb := &jobSB{finish: make(chan struct{}), failLaunch: true}
	a, _, cancel := newJobAgent(t, sb)
	defer cancel()

	out, isErr := a.doBash(llm.ToolCall{Name: "bash", Arguments: `{"cmd":"echo hi"}`})
	if !isErr {
		t.Fatal("a sandbox exec error should return an error result")
	}
	if !strings.Contains(out, "sandbox error") || !strings.Contains(out, "cannot start command") {
		t.Errorf("error should surface the sandbox failure, got: %q", out)
	}
}

func waitFor(t *testing.T, d time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.After(d)
	for {
		if cond() {
			return
		}
		select {
		case <-deadline:
			t.Fatal("condition not met in time")
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}
}
