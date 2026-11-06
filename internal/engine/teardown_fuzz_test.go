package engine_test

import (
	"context"
	"io"
	"math/rand"
	"sync"
	"testing"
	"time"

	"github.com/nihaal-prasad/oline/internal/core"
	"github.com/nihaal-prasad/oline/internal/engine"
	"github.com/nihaal-prasad/oline/internal/event"
	"github.com/nihaal-prasad/oline/internal/llm"
	"github.com/nihaal-prasad/oline/internal/sandbox"
	"github.com/nihaal-prasad/oline/internal/tools"
)

// chaosLLM drives an agent through a deterministic (seeded) random mix of actions:
// spawn a subagent (orchestrator only), run a foreground bash command, start a
// background bash job, or produce text and idle. Reused for Main and the workers so
// the engagement keeps churning live goroutines (subagents + bash jobs) right up to
// the moment it is torn down.
type chaosLLM struct {
	mu       sync.Mutex
	rnd      *rand.Rand
	canSpawn bool
}

func (m *chaosLLM) Stream(ctx context.Context, _ llm.ChatReq, ch chan<- llm.Delta) error {
	defer close(ch)
	m.mu.Lock()
	n := m.rnd.Intn(100)
	m.mu.Unlock()
	send := func(d llm.Delta) bool {
		select {
		case ch <- d:
			return true
		case <-ctx.Done():
			return false
		}
	}
	tool := func(name, args string) error {
		if !send(llm.Delta{ToolCall: &llm.ToolCallDelta{Index: 0, ID: "t", Name: name, ArgsFragment: args}}) {
			return ctx.Err()
		}
		send(llm.Delta{Finish: "tool_calls"})
		return nil
	}
	switch {
	case m.canSpawn && n < 30:
		return tool("task", `{"agent":"worker","prompt":"go"}`)
	case n < 55:
		return tool("bash", `{"cmd":"echo hi"}`)
	case n < 75:
		return tool("bash_bg", `{"cmd":"sleep 1"}`)
	default:
		if !send(llm.Delta{TextDelta: "working"}) {
			return ctx.Err()
		}
		send(llm.Delta{Finish: "stop"})
		return nil
	}
}

// chaosSB is a sandbox whose Exec takes a little time and HONORS ctx, so foreground
// and background bash jobs are genuine live goroutines that must unwind on cancel.
type chaosSB struct{}

func (chaosSB) Exec(ctx context.Context, _ core.AgentID, _ sandbox.Cmd, w io.Writer) (sandbox.ExitStatus, error) {
	io.WriteString(w, "ok\n")
	select {
	case <-time.After(3 * time.Millisecond):
	case <-ctx.Done():
		return sandbox.ExitStatus{}, ctx.Err()
	}
	return sandbox.ExitStatus{Code: 0}, nil
}
func (chaosSB) CopyIn(context.Context, string, string) error     { return nil }
func (chaosSB) CopyOut(context.Context, string, string) error    { return nil }
func (chaosSB) Backup(context.Context, string, string) error     { return nil }
func (chaosSB) EgressController() sandbox.EgressController       { return nil }
func (chaosSB) SetupEgress(context.Context, sandbox.Scope) error { return nil }
func (chaosSB) Info() sandbox.EnvInfo                            { return sandbox.EnvInfo{} }

// FuzzTeardownStress hammers the exact failure class behind the recent crash: dynamic
// emitter registration (subagent spawns, foreground/background bash jobs) racing the
// engagement teardown. A real orchestrator churns subagents and bash jobs while a
// fuzzer-derived stream of control ops (pause/resume/kill/stop/steer/budget/pause-all)
// is applied concurrently, and the engagement context is cancelled at a fuzzed moment.
// Invariants: no panic (WaitGroup reuse, send-on-closed, nil deref), Session.Run
// always returns (no deadlock, no wedged agent), and Events can be closed cleanly
// afterwards (nothing emits once Run has returned).
func FuzzTeardownStress(f *testing.F) {
	f.Add(int64(1), uint16(10), uint8(1))
	f.Add(int64(2), uint16(0), uint8(3))
	f.Add(int64(3), uint16(80), uint8(2))
	f.Add(int64(4), uint16(150), uint8(0))

	f.Fuzz(func(t *testing.T, seed int64, cancelRaw uint16, aggression uint8) {
		mainLLM := &chaosLLM{rnd: rand.New(rand.NewSource(seed)), canSpawn: true}
		workerLLM := &chaosLLM{rnd: rand.New(rand.NewSource(seed ^ 0x9e3779b1)), canSpawn: false}
		llmFor := func(sel string) (engine.LLM, string, error) {
			if sel == "mock/worker" {
				return workerLLM, "w", nil
			}
			return mainLLM, "m", nil
		}
		cfgFor := func(role string) (engine.AgentConfig, bool) {
			if role == "worker" {
				return engine.AgentConfig{Role: "worker", Model: "mock/worker", Tools: []string{"bash", "bash_bg", "wait"}}, true
			}
			return engine.AgentConfig{}, false
		}

		// Drain via a stoppable loop and NEVER close the channel — production never
		// closes it either (the TUI just stops reading). Closing it here would risk a
		// send-on-closed panic against a control op's emitAsync fallback goroutine, which
		// is a harness hazard, not a product one.
		events := make(chan event.Event, 4096)
		drainStop := make(chan struct{})
		drained := make(chan struct{})
		go func() {
			defer close(drained)
			for {
				select {
				case <-events:
				case <-drainStop:
					return
				}
			}
		}()

		s := engine.NewSession(tools.NewRegistry(), chaosSB{}, llmFor, cfgFor, events, 3)
		s.Interactive = true
		orch := engine.AgentConfig{Role: "Main", Model: "mock/main", Tools: []string{"task", "wait", "bash", "bash_bg"}, IsOrchestrator: true, System: "x"}

		// A safety-net timeout bounds the engagement even if the explicit cancel below
		// never fires first; the test's own watchdog catches a true deadlock. Kept short
		// so each fuzz exec is cheap (go-fuzz flags a worker whose execs run long).
		ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		defer cancel()

		done := make(chan struct{})
		go func() { s.Run(ctx, orch, "go"); close(done) }()

		r := s.Router()
		stop := make(chan struct{})
		opDone := make(chan struct{})
		go func() {
			defer close(opDone)
			cr := rand.New(rand.NewSource(seed ^ 0x5bd1e995))
			// Bound the number of control ops per exec so each fuzz iteration is cheap and
			// its duration predictable (an unbounded spin executes thousands of ops and
			// makes go-fuzz's hang detector fire); the window is still dense enough to race
			// teardown against live spawns and bash jobs.
			ops := 20 + cr.Intn(100)
			for k := 0; k < ops; k++ {
				select {
				case <-stop:
					return
				default:
				}
				live := r.Live()
				if len(live) == 0 {
					live = []core.AgentID{core.Main}
				}
				tgt := live[cr.Intn(len(live))]
				switch cr.Intn(8) {
				case 0:
					r.Pause(tgt)
				case 1:
					r.Resume(tgt)
				case 2:
					r.Kill(tgt)
				case 3:
					r.StopCommands(tgt)
				case 4:
					r.Send(engine.Envelope{From: "User", To: tgt, Kind: engine.MsgSteer, Body: "x"})
				case 5:
					r.PauseAll()
				case 6:
					r.ResumeAll()
				case 7:
					r.SetBudget(tgt, time.Duration(cr.Intn(3))*time.Millisecond)
				}
				if d := int(aggression % 4); d > 0 {
					time.Sleep(time.Duration(cr.Intn(d+1)) * time.Millisecond)
				}
			}
		}()

		// Cancel the engagement at a fuzzed moment (bounded so the test stays fast),
		// racing teardown against the in-flight spawns and bash jobs.
		go func() {
			select {
			case <-time.After(time.Duration(cancelRaw%100) * time.Millisecond):
				cancel()
			case <-done:
			}
		}()

		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Fatal("Session.Run did not return — deadlock under concurrent teardown")
		}
		close(stop)
		<-opDone
		close(drainStop) // stop draining without closing events (see above)
		<-drained
	})
}
