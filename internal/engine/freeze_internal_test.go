package engine

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/nihaal-prasad/oline/internal/core"
	"github.com/nihaal-prasad/oline/internal/event"
	"github.com/nihaal-prasad/oline/internal/llm"
	"github.com/nihaal-prasad/oline/internal/tools"
)

// holdLLM signals when its stream begins (so a test can confirm the agent is running
// and holding its concurrency slot), then stays silent until released.
type holdLLM struct {
	running chan struct{}
	release chan struct{}
	once    sync.Once
}

func (m *holdLLM) Stream(ctx context.Context, _ llm.ChatReq, ch chan<- llm.Delta) error {
	defer close(ch)
	m.once.Do(func() { close(m.running) })
	select {
	case <-m.release:
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case ch <- llm.Delta{TextDelta: "held done"}:
	case <-ctx.Done():
		return ctx.Err()
	}
	ch <- llm.Delta{Finish: "stop"}
	return nil
}

// signalLLM closes called the first time it streams, then finishes immediately — a
// probe for "did this agent ever actually get to run".
type signalLLM struct {
	called chan struct{}
	once   sync.Once
}

func (m *signalLLM) Stream(ctx context.Context, _ llm.ChatReq, ch chan<- llm.Delta) error {
	defer close(ch)
	m.once.Do(func() { close(m.called) })
	select {
	case ch <- llm.Delta{TextDelta: "ran"}:
	case <-ctx.Done():
		return ctx.Err()
	}
	ch <- llm.Delta{Finish: "stop"}
	return nil
}

// TestEngineSurvivesNonDrainingUI is the direct regression for the original freeze:
// a UI that stops draining the event channel. With a tiny buffer and NO consumer the
// channel fills at once, so every subsequent authoritative (must-deliver) emit is a
// blocking send. The guarantee is that those sends are bounded by the engagement
// context, so cancelling the engagement unwinds EVERY agent goroutine (orchestrator
// and a spawned subagent, both parked on a full channel) and Run returns. A permanent
// wedge here is exactly "agents marked active but frozen, UI never catches up".
func TestEngineSurvivesNonDrainingUI(t *testing.T) {
	events := make(chan event.Event, 1) // fills immediately; nothing ever drains it

	mainMock := &scriptLLM{steps: []scriptStep{{tool: "task", toolArg: `{"agent":"worker","prompt":"go"}`}}}
	workerMock := &scriptLLM{steps: []scriptStep{{text: "worker output"}}}
	llmFor := func(sel string) (LLM, string, error) {
		if sel == "mock/worker" {
			return workerMock, "w", nil
		}
		return mainMock, "m", nil
	}
	cfgFor := func(role string) (AgentConfig, bool) {
		if role == "worker" {
			return AgentConfig{Role: "worker", Model: "mock/worker"}, true
		}
		return AgentConfig{}, false
	}
	s := NewSession(tools.NewRegistry(), nil, llmFor, cfgFor, events, 5)
	s.Interactive = true // Main idles waiting for input after it spawns the worker
	orch := AgentConfig{Role: "Main", Model: "mock/main", Tools: []string{"task", "wait"}, IsOrchestrator: true, System: "x"}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan Result, 1)
	go func() { done <- s.Run(ctx, orch, "go") }()

	// Let the orchestrator spawn the worker and both pile lifecycle events onto the
	// full, undrained channel. Nothing should crash; agents just block on their
	// bounded emits.
	time.Sleep(300 * time.Millisecond)

	cancel() // engagement ends: every bounded emit/gate/await must now unwind
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run wedged against a non-draining UI — back-pressure freeze regression")
	}
}

// TestControlOpNeverBlocksUnderBackpressure proves a control operation issued from
// the TUI goroutine (Kill/Pause/Resume) never blocks, even when the event channel is
// full. The TUI goroutine is also the drainer, so a blocking lifecycle emit from a
// control op would deadlock the whole UI (it would be stuck in its own send and so
// never drain). Control ops therefore emit via emitAsync; this test confirms Kill
// returns promptly with a full, undrained channel.
func TestControlOpNeverBlocksUnderBackpressure(t *testing.T) {
	events := make(chan event.Event, 1) // fills on the first emit; never drained

	mock := &stallLLM{release: make(chan struct{})} // Main stays silently alive (running)
	llmFor := func(string) (LLM, string, error) { return mock, "m", nil }
	cfgFor := func(string) (AgentConfig, bool) { return AgentConfig{}, false }
	s := NewSession(tools.NewRegistry(), nil, llmFor, cfgFor, events, 5)
	s.Interactive = true
	orch := AgentConfig{Role: "Main", Model: "m", IsOrchestrator: true, System: "x"}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan Result, 1)
	go func() { done <- s.Run(ctx, orch, "go") }()

	// Wait until Main is registered and running (its first Running emit has filled the
	// cap-1 channel), so the next lifecycle emit would block a synchronous sender.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, ok := s.Router().get(core.Main); ok {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	// Kill from THIS goroutine (standing in for the TUI/drainer goroutine): it must
	// return without waiting on the full channel.
	returned := make(chan struct{})
	go func() {
		s.Router().Kill(core.Main) // the real TUI control path
		close(returned)
	}()
	select {
	case <-returned:
	case <-time.After(2 * time.Second):
		t.Fatal("a control op blocked on a full event channel — emitAsync regression")
	}

	close(mock.release)
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not unwind after Kill + cancel")
	}
}

// TestSubagentPoolHandoff proves the concurrency pool never strands the orchestrator:
// a queued subagent blocks on ITS OWN acquire (not the orchestrator), and the slot is
// handed to it the instant the holder finishes and releases. With a pool of one, a
// running subagent holds the only slot; a second subagent spawned behind it must not
// run until the first releases, and then must run promptly. A slot leaked on the busy
// agent's idle path (e.g. emitting Idle before releasing under back-pressure) would
// leave the second agent blocked forever — the orchestrator-wedge freeze.
func TestSubagentPoolHandoff(t *testing.T) {
	events := make(chan event.Event, 256)
	go func() {
		for range events { // drained normally; this test is about the pool, not back-pressure
		}
	}()

	hold := &holdLLM{running: make(chan struct{}), release: make(chan struct{})}
	sig := &signalLLM{called: make(chan struct{})}
	mainMock := &scriptLLM{steps: []scriptStep{{tool: "task", toolArg: `{"agent":"holder","prompt":"go"}`}}}
	llmFor := func(sel string) (LLM, string, error) {
		switch sel {
		case "mock/holder":
			return hold, "h", nil
		case "mock/second":
			return sig, "s", nil
		}
		return mainMock, "m", nil
	}
	cfgFor := func(role string) (AgentConfig, bool) {
		switch role {
		case "holder":
			return AgentConfig{Role: "holder", Model: "mock/holder"}, true
		case "second":
			return AgentConfig{Role: "second", Model: "mock/second"}, true
		}
		return AgentConfig{}, false
	}
	s := NewSession(tools.NewRegistry(), nil, llmFor, cfgFor, events, 1) // pool of ONE
	s.Interactive = true
	orch := AgentConfig{Role: "Main", Model: "mock/main", Tools: []string{"task", "wait"}, IsOrchestrator: true, System: "x"}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan Result, 1)
	go func() { done <- s.Run(ctx, orch, "go") }()

	// Wait until the holder is actually running (holds the only slot).
	select {
	case <-hold.running:
	case <-time.After(3 * time.Second):
		t.Fatal("the first subagent never started (never acquired the only slot)")
	}

	// Spawn a second subagent behind it. It must queue on acquireSem, not run.
	if _, err := s.Router().spawn(core.Main, "second", "go"); err != nil {
		t.Fatalf("spawn second: %v", err)
	}
	select {
	case <-sig.called:
		t.Fatal("the second subagent ran while the pool was full — slot accounting bug")
	case <-time.After(200 * time.Millisecond):
		// Correctly still queued.
	}

	// Release the holder: it finishes, releases the slot, and the queued agent must
	// pick it up and run promptly.
	close(hold.release)
	select {
	case <-sig.called:
	case <-time.After(3 * time.Second):
		t.Fatal("the queued subagent never got the slot after the holder released it — pool-handoff/slot-leak regression")
	}

	cancel()
	<-done
}
