package engine

import (
	"context"
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

// revSB emulates the sandbox for reverse-shell tests. The listener's Exec (its
// script loops with `while true`) blocks until its context is cancelled, mirroring
// a persistent listener; every other (short-lived) Exec records its script and
// returns canned output immediately.
type revSB struct {
	mu        sync.Mutex
	scripts   []string
	listeners int
	canceled  int
	readOut   string
}

func (m *revSB) Exec(ctx context.Context, _ core.AgentID, cmd sandbox.Cmd, out io.Writer) (sandbox.ExitStatus, error) {
	script := ""
	if len(cmd.Argv) == 3 {
		script = cmd.Argv[2]
	}
	m.mu.Lock()
	m.scripts = append(m.scripts, script)
	isListener := strings.Contains(script, "while true")
	if isListener {
		m.listeners++
	}
	readOut := m.readOut
	m.mu.Unlock()

	if isListener {
		<-ctx.Done()
		m.mu.Lock()
		m.canceled++
		m.mu.Unlock()
		return sandbox.ExitStatus{Code: -1}, ctx.Err()
	}
	if readOut != "" {
		io.WriteString(out, readOut)
	}
	return sandbox.ExitStatus{Code: 0}, nil
}

func (m *revSB) ranScript(substr string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, s := range m.scripts {
		if strings.Contains(s, substr) {
			return true
		}
	}
	return false
}

func (*revSB) CopyIn(context.Context, string, string) error     { return nil }
func (*revSB) CopyOut(context.Context, string, string) error    { return nil }
func (*revSB) Backup(context.Context, string, string) error     { return nil }
func (*revSB) EgressController() sandbox.EgressController       { return nil }
func (*revSB) SetupEgress(context.Context, sandbox.Scope) error { return nil }
func (*revSB) Info() sandbox.EnvInfo                            { return sandbox.EnvInfo{} }

func newRevAgent(t *testing.T, sb sandbox.Handle) (*Agent, *Session, chan event.Event, context.CancelFunc) {
	t.Helper()
	events := make(chan event.Event, 256)
	s := NewSession(tools.NewRegistry(), sb, nil, func(string) (AgentConfig, bool) { return AgentConfig{}, false }, events, 5)
	s.Workspace = "/opt/oline/engagements/test"
	ctx, cancel := context.WithCancel(context.Background())
	s.baseCtx = ctx
	a := newAgent("recon#1", AgentConfig{Role: "recon", Tools: []string{"bash", "revshell"}}, nil, "m", s, "", "")
	s.router.register(a)
	return a, s, events, cancel
}

// drainChannelEvents collects the event.Channel values seen so far.
func drainChannelEvents(events chan event.Event) []event.Channel {
	var out []event.Channel
	for {
		select {
		case ev := <-events:
			if c, ok := ev.(event.Channel); ok {
				out = append(out, c)
			}
		default:
			return out
		}
	}
}

func TestRevshellOpenSendReadClose(t *testing.T) {
	sb := &revSB{readOut: "id\nuid=0(root)\n"}
	a, s, events, cancel := newRevAgent(t, sb)
	defer cancel()

	// open
	out, isErr := a.doRevshell(llm.ToolCall{Name: "revshell", Arguments: `{"action":"open","name":"foothold","port":4444}`})
	if isErr {
		t.Fatalf("open errored: %s", out)
	}
	// Give the listener goroutine a moment to launch its Exec.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		sb.mu.Lock()
		n := sb.listeners
		sb.mu.Unlock()
		if n >= 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if s.getChannel("foothold") == nil {
		t.Fatal("channel 'foothold' not registered after open")
	}
	if !sb.ranScript("nc -lvnp 4444") {
		t.Errorf("listener script did not reference the netcat listener on port 4444")
	}

	// duplicate open is rejected
	if _, isErr := a.doRevshell(llm.ToolCall{Name: "revshell", Arguments: `{"action":"open","name":"foothold","port":5555}`}); !isErr {
		t.Error("duplicate open should be rejected")
	}

	// send
	out, isErr = a.doRevshell(llm.ToolCall{Name: "revshell", Arguments: `{"action":"send","name":"foothold","data":"id"}`})
	if isErr {
		t.Fatalf("send errored: %s", out)
	}
	if !strings.Contains(out, "uid=0(root)") {
		t.Errorf("send should return the channel output, got: %s", out)
	}
	if !sb.ranScript("channels/foothold") || !sb.ranScript("/in") {
		t.Errorf("send did not write to the channel FIFO")
	}

	// read
	out, isErr = a.doRevshell(llm.ToolCall{Name: "revshell", Arguments: `{"action":"read","name":"foothold"}`})
	if isErr || !strings.Contains(out, "uid=0(root)") {
		t.Errorf("read should return channel output, got (isErr=%v): %s", isErr, out)
	}

	// list
	out, isErr = a.doRevshell(llm.ToolCall{Name: "revshell", Arguments: `{"action":"list"}`})
	if isErr || !strings.Contains(out, "foothold") || !strings.Contains(out, "port 4444") {
		t.Errorf("list should include the open channel, got: %s", out)
	}

	// close
	out, isErr = a.doRevshell(llm.ToolCall{Name: "revshell", Arguments: `{"action":"close","name":"foothold"}`})
	if isErr {
		t.Fatalf("close errored: %s", out)
	}
	if s.getChannel("foothold") != nil {
		t.Error("channel should be gone after close")
	}

	// We should have seen an open event and a close event.
	evs := drainChannelEvents(events)
	var sawOpen, sawClose bool
	for _, e := range evs {
		if e.Name == "foothold" && e.Open {
			sawOpen = true
		}
		if e.Name == "foothold" && !e.Open {
			sawClose = true
		}
	}
	if !sawOpen || !sawClose {
		t.Errorf("expected open+close channel events, got %+v", evs)
	}
}

func TestRevshellValidation(t *testing.T) {
	sb := &revSB{}
	a, _, _, cancel := newRevAgent(t, sb)
	defer cancel()

	cases := []string{
		`{"action":"open","name":"bad name","port":4444}`, // space in name
		`{"action":"open","name":"ok","port":0}`,          // bad port
		`{"action":"open","name":"ok","port":70000}`,      // bad port
		`{"action":"send","name":"nope","data":"id"}`,     // no such channel
		`{"action":"read"}`,                               // missing name
		`{"action":"bogus"}`,                              // unknown action
		`{}`,                                              // missing action
	}
	for _, args := range cases {
		if _, isErr := a.doRevshell(llm.ToolCall{Name: "revshell", Arguments: args}); !isErr {
			t.Errorf("expected error for args %s", args)
		}
	}
}

// TestOrchestratorRevshellArbitrateOnly: the orchestrator may arbitrate/observe
// reverse shells (grant/read/list/close) but may NOT create (`open`) or drive
// (`send`/`acquire`/`release`) them.
func TestOrchestratorRevshellArbitrateOnly(t *testing.T) {
	sb := &revSB{readOut: "ok\n"}
	_, s, _, cancel := newRevAgent(t, sb)
	defer cancel()
	orch := newAgent(core.Main, AgentConfig{Role: "Main", IsOrchestrator: true, Tools: []string{"revshell"}}, nil, "m", s, "", "")
	s.router.register(orch)

	for _, act := range []string{"open", "send", "acquire", "release"} {
		out, isErr := orch.doRevshell(llm.ToolCall{Arguments: `{"action":"` + act + `","name":"c","port":4444,"data":"x"}`})
		if !isErr || !strings.Contains(out, "only arbitrates") {
			t.Errorf("orchestrator %q must be refused (arbitrate-only): isErr=%v out=%q", act, isErr, out)
		}
	}
	// Observation is allowed for the orchestrator.
	if _, isErr := orch.doRevshell(llm.ToolCall{Arguments: `{"action":"list"}`}); isErr {
		t.Error("orchestrator should be allowed to `list` channels")
	}
}

// newRevPeer registers an extra agent sharing session s (for arbitration tests).
func newRevPeer(s *Session, id core.AgentID) *Agent {
	a := newAgent(id, AgentConfig{Role: "peer", Tools: []string{"bash", "revshell"}}, nil, "m", s, "", "")
	s.router.register(a)
	return a
}

func TestRevshellWriteArbitration(t *testing.T) {
	sb := &revSB{readOut: "ok\n"}
	a1, s, _, cancel := newRevAgent(t, sb) // recon#1
	defer cancel()
	a2 := newRevPeer(s, "recon#2")
	main := newRevPeer(s, core.Main)

	// recon#1 opens the channel and so holds the write token.
	if _, isErr := a1.doRevshell(llm.ToolCall{Arguments: `{"action":"open","name":"c","port":4444}`}); isErr {
		t.Fatal("open failed")
	}
	if got := s.getChannel("c").writer; got != "recon#1" {
		t.Fatalf("opener should hold the write token, got %q", got)
	}

	// recon#2 cannot send while recon#1 holds it.
	out, isErr := a2.doRevshell(llm.ToolCall{Arguments: `{"action":"send","name":"c","data":"id"}`})
	if !isErr || !strings.Contains(out, "held by") {
		t.Errorf("recon#2 send should be refused while recon#1 holds the token, got (isErr=%v): %s", isErr, out)
	}

	// A non-orchestrator cannot grant.
	if out, isErr := a2.doRevshell(llm.ToolCall{Arguments: `{"action":"grant","name":"c","agent":"recon#2"}`}); !isErr || !strings.Contains(out, "orchestrator") {
		t.Errorf("non-Main grant should be refused, got (isErr=%v): %s", isErr, out)
	}

	// The orchestrator grants the token to recon#2.
	if out, isErr := main.doRevshell(llm.ToolCall{Arguments: `{"action":"grant","name":"c","agent":"recon#2"}`}); isErr {
		t.Fatalf("Main grant failed: %s", out)
	}
	if got := s.getChannel("c").writer; got != "recon#2" {
		t.Fatalf("after grant, recon#2 should hold the token, got %q", got)
	}

	// Now recon#2 can send; recon#1 can no longer send.
	if _, isErr := a2.doRevshell(llm.ToolCall{Arguments: `{"action":"send","name":"c","data":"id"}`}); isErr {
		t.Error("recon#2 send should succeed after being granted the token")
	}
	if _, isErr := a1.doRevshell(llm.ToolCall{Arguments: `{"action":"send","name":"c","data":"id"}`}); !isErr {
		t.Error("recon#1 send should now be refused")
	}

	// recon#2 releases; the channel is free and recon#1 can re-acquire.
	if _, isErr := a2.doRevshell(llm.ToolCall{Arguments: `{"action":"release","name":"c"}`}); isErr {
		t.Error("release by the holder should succeed")
	}
	if got := s.getChannel("c").writer; got != "" {
		t.Errorf("after release the channel should be free, got %q", got)
	}
	if _, isErr := a1.doRevshell(llm.ToolCall{Arguments: `{"action":"acquire","name":"c"}`}); isErr {
		t.Error("recon#1 should be able to acquire a free channel")
	}

	// A non-holder, non-Main cannot close; the holder can.
	if _, isErr := a2.doRevshell(llm.ToolCall{Arguments: `{"action":"close","name":"c"}`}); !isErr {
		t.Error("non-holder close should be refused")
	}
	if _, isErr := a1.doRevshell(llm.ToolCall{Arguments: `{"action":"close","name":"c"}`}); isErr {
		t.Error("the holder should be able to close the channel")
	}
}

func TestSessionCloseChannel(t *testing.T) {
	sb := &revSB{}
	a, s, events, cancel := newRevAgent(t, sb)
	defer cancel()
	if _, isErr := a.doRevshell(llm.ToolCall{Arguments: `{"action":"open","name":"c","port":4444}`}); isErr {
		t.Fatal("open failed")
	}
	if !s.CloseChannel("c") {
		t.Fatal("CloseChannel should report it closed an open channel")
	}
	if s.getChannel("c") != nil {
		t.Error("channel should be gone after CloseChannel")
	}
	if s.CloseChannel("c") {
		t.Error("CloseChannel on a missing channel should report false")
	}
	var sawClose bool
	for _, e := range drainChannelEvents(events) {
		if e.Name == "c" && !e.Open {
			sawClose = true
		}
	}
	if !sawClose {
		t.Error("CloseChannel should emit a close event")
	}
}
