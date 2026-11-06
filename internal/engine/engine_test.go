package engine_test

import (
	"context"
	"encoding/json"
	"io"
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

// --- scripted mock LLM ----------------------------------------------------

type step struct {
	text     string
	toolID   string
	toolName string
	toolArgs string
}

type mockLLM struct {
	mu    sync.Mutex
	steps []step
	i     int
}

func (m *mockLLM) Stream(ctx context.Context, _ llm.ChatReq, ch chan<- llm.Delta) error {
	defer close(ch)
	m.mu.Lock()
	var s step
	if m.i < len(m.steps) {
		s = m.steps[m.i]
		m.i++
	} else {
		s = step{text: "(no more script)"}
	}
	m.mu.Unlock()

	send := func(d llm.Delta) bool {
		select {
		case ch <- d:
			return true
		case <-ctx.Done():
			return false
		}
	}
	if s.text != "" && !send(llm.Delta{TextDelta: s.text}) {
		return ctx.Err()
	}
	if s.toolName != "" {
		if !send(llm.Delta{ToolCall: &llm.ToolCallDelta{Index: 0, ID: s.toolID, Name: s.toolName, ArgsFragment: s.toolArgs}}) {
			return ctx.Err()
		}
		send(llm.Delta{Finish: "tool_calls"})
	} else {
		send(llm.Delta{Finish: "stop"})
	}
	return nil
}

// --- mock sandbox ---------------------------------------------------------

type mockSB struct{ out string }

func (m mockSB) Exec(_ context.Context, _ core.AgentID, _ sandbox.Cmd, w io.Writer) (sandbox.ExitStatus, error) {
	io.WriteString(w, m.out)
	return sandbox.ExitStatus{Code: 0}, nil
}
func (mockSB) CopyIn(context.Context, string, string) error     { return nil }
func (mockSB) CopyOut(context.Context, string, string) error    { return nil }
func (mockSB) Backup(context.Context, string, string) error     { return nil }
func (mockSB) EgressController() sandbox.EgressController       { return nil }
func (mockSB) SetupEgress(context.Context, sandbox.Scope) error { return nil }
func (mockSB) Info() sandbox.EnvInfo                            { return sandbox.EnvInfo{} }

// --- event collector ------------------------------------------------------

type collector struct {
	mu   sync.Mutex
	ev   []event.Event
	done chan struct{}
}

func newCollector() *collector { return &collector{done: make(chan struct{})} }

func drain(ch <-chan event.Event, c *collector) {
	for e := range ch {
		c.mu.Lock()
		c.ev = append(c.ev, e)
		c.mu.Unlock()
	}
	close(c.done)
}

func (c *collector) sawSpawn(role string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, e := range c.ev {
		if s, ok := e.(event.Spawned); ok && s.Role == role {
			return true
		}
	}
	return false
}
func (c *collector) spawnCount(role string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, e := range c.ev {
		if s, ok := e.(event.Spawned); ok && s.Role == role {
			n++
		}
	}
	return n
}
func (c *collector) sawToolStart(tool string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, e := range c.ev {
		if s, ok := e.(event.ToolStarted); ok && s.Tool == tool {
			return true
		}
	}
	return false
}
func (c *collector) countUserQuestions() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, e := range c.ev {
		if _, ok := e.(event.UserQuestion); ok {
			n++
		}
	}
	return n
}

func (c *collector) sawMessage(to core.AgentID, bodySub string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, e := range c.ev {
		if m, ok := e.(event.Message); ok && m.To == to && containsStr(m.Body, bodySub) {
			return true
		}
	}
	return false
}

func (c *collector) sawToolOutput(sub string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, e := range c.ev {
		if o, ok := e.(event.ToolOutput); ok && containsStr(o.Chunk, sub) {
			return true
		}
	}
	return false
}

func (c *collector) sawText(sub string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, e := range c.ev {
		if t, ok := e.(event.TokenDelta); ok && containsStr(t.Text, sub) {
			return true
		}
	}
	return false
}

func containsStr(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && indexOf(s, sub) >= 0)
}
func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// --- tests ----------------------------------------------------------------

func waitText(t *testing.T, c *collector, sub string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if c.sawText(sub) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %q", sub)
}

// TestInteractiveOrchestratorContinues proves that an interactive orchestrator
// stays alive after finishing a turn: it goes idle, and a follow-up message
// makes it respond again instead of being ignored (the "messages to Main were
// ignored" bug).
func TestInteractiveOrchestratorContinues(t *testing.T) {
	mock := &mockLLM{steps: []step{{text: "first done"}, {text: "second done"}}}
	llmFor := func(string) (engine.LLM, string, error) { return mock, "m", nil }
	cfgFor := func(string) (engine.AgentConfig, bool) { return engine.AgentConfig{}, false }

	events := make(chan event.Event, 256)
	c := newCollector()
	go drain(events, c)

	s := engine.NewSession(tools.NewRegistry(), mockSB{}, llmFor, cfgFor, events, 5)
	s.Interactive = true
	orch := engine.AgentConfig{Role: "Main", Model: "m", IsOrchestrator: true, System: "x"}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan engine.Result, 1)
	go func() { done <- s.Run(ctx, orch, "hi") }()

	waitText(t, c, "first done") // first turn completes, orchestrator goes idle

	// A follow-up to the now-idle orchestrator must be delivered and acted on.
	if !s.Router().Send(engine.Envelope{From: "User", To: core.Main, Kind: engine.MsgSteer, Body: "again"}) {
		t.Fatal("send to idle interactive orchestrator failed (it exited?)")
	}
	waitText(t, c, "second done") // it continued the conversation

	cancel()
	close(events)
	<-done
}

func TestSpawnWaitResult(t *testing.T) {
	mainMock := &mockLLM{steps: []step{
		{toolID: "t1", toolName: "task", toolArgs: `{"agent":"probe","prompt":"run uname"}`},
		{toolID: "w1", toolName: "wait", toolArgs: `{}`},
		{text: "FINAL: probe completed"},
	}}
	probeMock := &mockLLM{steps: []step{
		{toolID: "b1", toolName: "bash", toolArgs: `{"cmd":"uname -a"}`},
		{text: "probe result: Linux mock box"},
	}}

	reg := tools.NewRegistry()
	reg.Register(tools.Bash{})

	llmFor := func(sel string) (engine.LLM, string, error) {
		switch sel {
		case "mock/main":
			return mainMock, "m", nil
		default:
			return probeMock, "p", nil
		}
	}
	cfgFor := func(role string) (engine.AgentConfig, bool) {
		if role == "probe" {
			return engine.AgentConfig{Role: "probe", Model: "mock/probe", Tools: []string{"bash"}}, true
		}
		return engine.AgentConfig{}, false
	}

	events := make(chan event.Event, 256)
	c := newCollector()
	go drain(events, c)

	s := engine.NewSession(reg, mockSB{out: "Linux mock box 6.6"}, llmFor, cfgFor, events, 5)
	mainCfg := engine.AgentConfig{Role: "Main", Model: "mock/main", Tools: []string{"task", "wait"}, IsOrchestrator: true}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	res := s.Run(ctx, mainCfg, "probe the box")
	close(events)
	<-c.done // deterministically wait for all events to be collected

	if res.Err != nil {
		t.Fatalf("Run error: %v", res.Err)
	}
	if res.Text != "FINAL: probe completed" {
		t.Errorf("final text = %q", res.Text)
	}
	if !c.sawSpawn("Probe") { // subagent names are presented capitalized
		t.Error("no Spawned event for Probe")
	}
	if !c.sawToolStart("bash") {
		t.Error("probe did not run the bash tool")
	}
	if !c.sawText("Linux mock box") {
		t.Error("probe's result text was not streamed")
	}
}

// sleepLLM always issues a single `sleeper` tool call, so the agent it drives
// parks in that tool (holding its concurrency slot) until its context is cancelled.
type sleepLLM struct{}

func (sleepLLM) Stream(ctx context.Context, _ llm.ChatReq, ch chan<- llm.Delta) error {
	defer close(ch)
	select {
	case ch <- llm.Delta{ToolCall: &llm.ToolCallDelta{Index: 0, ID: "s", Name: "sleeper", ArgsFragment: "{}"}}:
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case ch <- llm.Delta{Finish: "tool_calls"}:
	case <-ctx.Done():
	}
	return nil
}

// TestSpawnDoesNotBlockOrchestratorWhenPoolFull is the regression for the freeze
// where a saturated concurrency pool wedged the whole engagement. spawn used to
// acquire the slot in the CALLER's dispatch, so once every subagent slot was held
// the orchestrator blocked inside `task` — unable to drain its mailbox, read user
// messages, reprompt, or make any progress (agents stuck "running", nothing
// thinking). Spawning is now async: the child acquires its slot in its own
// goroutine, so the orchestrator keeps running even when the pool is full.
func TestSpawnDoesNotBlockOrchestratorWhenPoolFull(t *testing.T) {
	mainMock := &mockLLM{steps: []step{
		{toolID: "t1", toolName: "task", toolArgs: `{"agent":"probe","prompt":"A"}`},
		{toolID: "t2", toolName: "task", toolArgs: `{"agent":"probe","prompt":"B"}`},
		{text: "MAIN CONTINUED"},
	}}
	llmFor := func(sel string) (engine.LLM, string, error) {
		if sel == "mock/main" {
			return mainMock, "m", nil
		}
		return sleepLLM{}, "p", nil
	}
	cfgFor := func(role string) (engine.AgentConfig, bool) {
		if role == "probe" {
			return engine.AgentConfig{Role: "probe", Model: "mock/probe", Tools: []string{"sleeper"}}, true
		}
		return engine.AgentConfig{}, false
	}

	reg := tools.NewRegistry()
	reg.Register(sleeperTool{started: make(chan struct{}, 8)})

	events := make(chan event.Event, 256)
	c := newCollector()
	go drain(events, c)

	// maxConcurrency 2 => exactly ONE subagent slot, so the second spawn cannot get
	// a slot while the first probe is parked in the sleeper tool.
	s := engine.NewSession(reg, mockSB{}, llmFor, cfgFor, events, 2)
	mainCfg := engine.AgentConfig{Role: "Main", Model: "mock/main", Tools: []string{"task", "wait"}, IsOrchestrator: true}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	res := s.Run(ctx, mainCfg, "go")
	close(events)
	<-c.done

	if res.Err != nil {
		t.Fatalf("Run error: %v", res.Err)
	}
	// The orchestrator reached its third step even though the single subagent slot
	// was held by the first probe: the second `task` did not block it. Under the old
	// blocking spawn this hung until the 5s context deadline and never produced it.
	if res.Text != "MAIN CONTINUED" {
		t.Fatalf("orchestrator blocked on a full pool: final text = %q, want \"MAIN CONTINUED\"", res.Text)
	}
	if n := c.spawnCount("Probe"); n != 2 {
		t.Errorf("want 2 Probe spawns (both tasks issued without blocking), got %d", n)
	}
}

// TestRepromptSubagent checks the orchestrator can re-task a subagent that has
// already finished (idle): it stays registered, wakes on reprompt, runs a second
// cycle, and posts a second result.
func TestRepromptSubagent(t *testing.T) {
	mainMock := &mockLLM{steps: []step{
		{toolID: "t1", toolName: "task", toolArgs: `{"agent":"probe","prompt":"do A"}`},
		{toolID: "w1", toolName: "wait", toolArgs: `{}`},
		{toolID: "rp", toolName: "reprompt", toolArgs: `{"agent":"probe#1","prompt":"do B"}`},
		{toolID: "w2", toolName: "wait", toolArgs: `{}`},
		{text: "FINAL: both cycles done"},
	}}
	probeMock := &mockLLM{steps: []step{
		{text: "first cycle done"},
		{text: "second cycle done"},
	}}

	llmFor := func(sel string) (engine.LLM, string, error) {
		if sel == "mock/main" {
			return mainMock, "m", nil
		}
		return probeMock, "p", nil
	}
	cfgFor := func(role string) (engine.AgentConfig, bool) {
		if role == "probe" {
			return engine.AgentConfig{Role: "probe", Model: "mock/probe"}, true
		}
		return engine.AgentConfig{}, false
	}

	events := make(chan event.Event, 256)
	c := newCollector()
	go drain(events, c)

	s := engine.NewSession(tools.NewRegistry(), mockSB{}, llmFor, cfgFor, events, 5)
	mainCfg := engine.AgentConfig{Role: "Main", Model: "mock/main", Tools: []string{"task", "wait", "reprompt"}, IsOrchestrator: true}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	res := s.Run(ctx, mainCfg, "go")
	close(events)
	<-c.done

	if res.Err != nil {
		t.Fatalf("Run error: %v", res.Err)
	}
	if res.Text != "FINAL: both cycles done" {
		t.Errorf("final text = %q", res.Text)
	}
	if !c.sawText("first cycle done") || !c.sawText("second cycle done") {
		t.Error("reprompt did not drive a second cycle of the finished subagent")
	}
}

// sleeperTool blocks until its context is cancelled — used to test immediate pause.
type sleeperTool struct{ started chan struct{} }

func (sleeperTool) Name() string { return "sleeper" }
func (sleeperTool) Schema() tools.Schema {
	return tools.Schema{Name: "sleeper", Description: "sleeps", Parameters: json.RawMessage(`{"type":"object","properties":{},"required":[]}`)}
}
func (s sleeperTool) Execute(ctx context.Context, _ tools.Call) (tools.Result, error) {
	select {
	case s.started <- struct{}{}:
	default:
	}
	<-ctx.Done()
	return tools.Result{Content: "interrupted"}, nil
}

func TestPauseResumeKill(t *testing.T) {
	started := make(chan struct{}, 1)
	reg := tools.NewRegistry()
	reg.Register(sleeperTool{started: started})

	mainMock := &mockLLM{steps: []step{
		{toolID: "s1", toolName: "sleeper", toolArgs: `{}`},
		{text: "should not reach here before kill"},
	}}
	llmFor := func(string) (engine.LLM, string, error) { return mainMock, "m", nil }
	cfgFor := func(string) (engine.AgentConfig, bool) { return engine.AgentConfig{}, false }

	s := engine.NewSession(reg, mockSB{}, llmFor, cfgFor, nil, 5)
	mainCfg := engine.AgentConfig{Role: "Main", Model: "mock/main", Tools: []string{"sleeper"}, IsOrchestrator: true}

	done := make(chan engine.Result, 1)
	go func() { done <- s.Run(context.Background(), mainCfg, "go") }()

	// Wait until the sleeper tool is actually running.
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("sleeper never started")
	}

	// Pause should cease immediately: cancel the tool and block the loop.
	s.Router().Pause(core.Main)
	if !waitState(s, core.Main, core.Paused, 2*time.Second) {
		st, _ := s.Router().State(core.Main)
		t.Fatalf("agent did not reach Paused; state=%v", st)
	}

	// Kill from paused should terminate the run.
	s.Router().Kill(core.Main)
	select {
	case res := <-done:
		if !res.Killed {
			t.Errorf("expected Killed result, got %+v", res)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not return after Kill")
	}
}

func TestMessagingAndAsk(t *testing.T) {
	mock := &mockLLM{steps: []step{
		// send_message is plain agent↔agent coordination (no user-question relay).
		{toolID: "m1", toolName: "send_message", toolArgs: `{"to":"Main","body":"status: proceeding"}`},
		// ask puts a question to the user directly (one UserQuestion event).
		{toolID: "a1", toolName: "ask", toolArgs: `{"question":"Continue with the plan?"}`},
		{text: "done"},
	}}
	llmFor := func(string) (engine.LLM, string, error) { return mock, "m", nil }
	cfgFor := func(string) (engine.AgentConfig, bool) { return engine.AgentConfig{}, false }

	events := make(chan event.Event, 64)
	c := newCollector()
	go drain(events, c)

	s := engine.NewSession(tools.NewRegistry(), mockSB{}, llmFor, cfgFor, events, 5)
	// Automatic mode: ask must NOT block; it records the question and proceeds.
	s.Mode = core.Automatic
	mainCfg := engine.AgentConfig{Role: "Main", Model: "mock/main", IsOrchestrator: true,
		Tools: []string{"send_message", "ask"}}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	res := s.Run(ctx, mainCfg, "go")
	close(events)
	<-c.done

	if res.Err != nil {
		t.Fatalf("run error: %v", res.Err)
	}
	if res.Text != "done" {
		t.Errorf("final text = %q, want done (ask should not have blocked in auto mode)", res.Text)
	}
	if n := c.countUserQuestions(); n != 1 {
		t.Errorf("expected exactly 1 UserQuestion event (from `ask`; send_message no longer relays), got %d", n)
	}
}

// TestAskUnblockedBySteer proves that in planner mode, where `ask` BLOCKS for a
// user answer, a user STEER (not an answer) unblocks the ask so the agent acts on
// it — instead of absorbing it silently and leaving the agent hung waiting for an
// answer that never comes.
func TestAskUnblockedBySteer(t *testing.T) {
	mock := &mockLLM{steps: []step{
		{toolID: "a1", toolName: "ask", toolArgs: `{"question":"Continue with the plan?"}`},
		{text: "acting on the steer"},
	}}
	llmFor := func(string) (engine.LLM, string, error) { return mock, "m", nil }
	cfgFor := func(string) (engine.AgentConfig, bool) { return engine.AgentConfig{}, false }

	events := make(chan event.Event, 256)
	c := newCollector()
	go drain(events, c)

	s := engine.NewSession(tools.NewRegistry(), mockSB{}, llmFor, cfgFor, events, 5)
	s.Interactive = true
	s.Mode = core.Planner // planner mode: ask blocks for a user answer
	mainCfg := engine.AgentConfig{Role: "Main", Model: "mock/main", IsOrchestrator: true,
		Tools: []string{"ask"}, System: "x"}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan engine.Result, 1)
	go func() { done <- s.Run(ctx, mainCfg, "go") }()

	// Wait until the question has been asked (ask is now blocking).
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && c.countUserQuestions() == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	if c.countUserQuestions() == 0 {
		t.Fatal("ask did not emit a UserQuestion (it did not block as expected)")
	}

	if !s.Router().Send(engine.Envelope{From: "User", To: core.Main, Kind: engine.MsgSteer, Body: "do X instead"}) {
		t.Fatal("steer to the asking orchestrator failed")
	}
	waitText(t, c, "acting on the steer") // the steer unblocked the ask

	cancel()
	close(events)
	<-done
}

func waitState(s *engine.Session, id core.AgentID, want core.State, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if st, ok := s.Router().State(id); ok && st == want {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}
