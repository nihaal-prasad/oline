package engine_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/nihaal-prasad/oline/internal/engine"
	"github.com/nihaal-prasad/oline/internal/event"
	"github.com/nihaal-prasad/oline/internal/llm"
	"github.com/nihaal-prasad/oline/internal/tools"
)

// TestInspectTool checks the orchestrator can view a live subagent's state and
// history at any time via the inspect tool.
func TestInspectTool(t *testing.T) {
	mainMock := &mockLLM{steps: []step{
		{toolID: "t1", toolName: "task", toolArgs: `{"agent":"probe","prompt":"scan"}`},
		{toolID: "w1", toolName: "wait", toolArgs: `{}`},
		{toolID: "i1", toolName: "inspect", toolArgs: `{"agent":"probe#1"}`},
		{text: "FINAL"},
	}}
	probeMock := &mockLLM{steps: []step{{text: "scanned 3 hosts"}}}

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
	mainCfg := engine.AgentConfig{Role: "Main", Model: "mock/main", Tools: []string{"task", "wait", "inspect"}, IsOrchestrator: true}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	res := s.Run(ctx, mainCfg, "go")
	close(events)
	<-c.done

	if res.Err != nil {
		t.Fatalf("Run error: %v", res.Err)
	}
	if !c.sawToolStart("inspect") {
		t.Error("inspect did not show as a tool call")
	}
	if !c.sawToolOutput("role Probe") { // names are presented capitalized
		t.Error("inspect output did not report the subagent's role/state")
	}
	if !c.sawToolOutput("scanned 3 hosts") {
		t.Error("inspect output did not include the subagent's conversation history")
	}
}

// captureMock records the messages of its first Stream call so a test can check
// what history the model was given.
type captureMock struct {
	mu   sync.Mutex
	msgs []llm.Message
}

func (c *captureMock) Stream(ctx context.Context, req llm.ChatReq, ch chan<- llm.Delta) error {
	c.mu.Lock()
	if c.msgs == nil {
		c.msgs = append([]llm.Message(nil), req.Messages...)
	}
	c.mu.Unlock()
	defer close(ch)
	select {
	case ch <- llm.Delta{TextDelta: "ok"}:
	case <-ctx.Done():
		return ctx.Err()
	}
	ch <- llm.Delta{Finish: "stop"}
	return nil
}

// TestSeedHistoryIsPrepended checks a resumed engagement restores the prior
// conversation: Session.SeedHistory is prepended to the orchestrator's history
// ahead of the new resume prompt.
func TestSeedHistoryIsPrepended(t *testing.T) {
	cap := &captureMock{}
	llmFor := func(string) (engine.LLM, string, error) { return cap, "m", nil }
	cfgFor := func(string) (engine.AgentConfig, bool) { return engine.AgentConfig{}, false }

	events := make(chan event.Event, 64)
	c := newCollector()
	go drain(events, c)

	s := engine.NewSession(tools.NewRegistry(), mockSB{}, llmFor, cfgFor, events, 5)
	s.SeedHistory = []llm.Message{{Role: llm.RoleUser, Content: "SEED-CONTEXT-123"}}
	mainCfg := engine.AgentConfig{Role: "Main", Model: "mock/main", IsOrchestrator: true}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s.Run(ctx, mainCfg, "do the thing")
	close(events)
	<-c.done

	cap.mu.Lock()
	msgs := cap.msgs
	cap.mu.Unlock()

	seedAt, promptAt := -1, -1
	for i, m := range msgs {
		if m.Content == "SEED-CONTEXT-123" {
			seedAt = i
		}
		if m.Content == "do the thing" {
			promptAt = i
		}
	}
	if seedAt < 0 {
		t.Fatal("restored seed history was not given to the model")
	}
	if promptAt < 0 {
		t.Fatal("resume prompt was not given to the model")
	}
	if seedAt > promptAt {
		t.Errorf("seed history should come before the resume prompt (seed=%d prompt=%d)", seedAt, promptAt)
	}
}
