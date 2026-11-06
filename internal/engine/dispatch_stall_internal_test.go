package engine

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nihaal-prasad/oline/internal/core"
	"github.com/nihaal-prasad/oline/internal/event"
	"github.com/nihaal-prasad/oline/internal/llm"
	"github.com/nihaal-prasad/oline/internal/tools"
)

// dispatchLLM calls a tool on its first turn, then finishes on the second.
type dispatchLLM struct {
	mu sync.Mutex
	i  int
}

func (m *dispatchLLM) Stream(ctx context.Context, _ llm.ChatReq, ch chan<- llm.Delta) error {
	defer close(ch)
	m.mu.Lock()
	i := m.i
	m.i++
	m.mu.Unlock()
	send := func(d llm.Delta) bool {
		select {
		case ch <- d:
			return true
		case <-ctx.Done():
			return false
		}
	}
	if i == 0 {
		send(llm.Delta{ToolCall: &llm.ToolCallDelta{Index: 0, ID: "t1", Name: "slowtool", ArgsFragment: "{}"}})
		send(llm.Delta{Finish: "tool_calls"})
		return nil
	}
	send(llm.Delta{TextDelta: "FINAL: done"})
	send(llm.Delta{Finish: "stop"})
	return nil
}

// slowTool blocks long enough to trip the dispatch heartbeat.
type slowTool struct{ d time.Duration }

func (slowTool) Name() string { return "slowtool" }

func (slowTool) Schema() tools.Schema {
	return tools.Schema{Name: "slowtool", Description: "slow", Parameters: json.RawMessage(`{"type":"object","properties":{},"required":[]}`)}
}

func (s slowTool) Execute(ctx context.Context, _ tools.Call) (tools.Result, error) {
	select {
	case <-time.After(s.d):
	case <-ctx.Done():
		return tools.Result{}, ctx.Err()
	}
	return tools.Result{Content: "slow done"}, nil
}

// TestToolDispatchHeartbeat proves that a tool call taking longer than
// streamStallWarn posts a liveness notice naming the tool — so an agent that is
// Running but stuck in a slow/wedged tool (no thoughts, no tokens) no longer looks
// frozen. The stream-stall watchdog does not cover the tool-dispatch phase, which
// was the gap behind "agents freeze while marked active, with no watchdog message".
func TestToolDispatchHeartbeat(t *testing.T) {
	old := streamStallWarn
	streamStallWarn = 30 * time.Millisecond
	defer func() { streamStallWarn = old }()

	events := make(chan event.Event, 256)
	var mu sync.Mutex
	var notices []event.Notice
	drained := make(chan struct{})
	go func() {
		for e := range events {
			if n, ok := e.(event.Notice); ok {
				mu.Lock()
				notices = append(notices, n)
				mu.Unlock()
			}
		}
		close(drained)
	}()
	sawHeartbeat := func() bool {
		mu.Lock()
		defer mu.Unlock()
		for _, n := range notices {
			if n.Agent == core.Main && strings.Contains(n.Text, "still running") && strings.Contains(n.Text, "slowtool") {
				return true
			}
		}
		return false
	}

	mock := &dispatchLLM{}
	llmFor := func(string) (LLM, string, error) { return mock, "m", nil }
	cfgFor := func(string) (AgentConfig, bool) { return AgentConfig{}, false }
	reg := tools.NewRegistry()
	reg.Register(slowTool{d: 200 * time.Millisecond})
	s := NewSession(reg, nil, llmFor, cfgFor, events, 5)
	orch := AgentConfig{Role: "Main", Model: "m", IsOrchestrator: true, System: "x", Tools: []string{"slowtool"}}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan Result, 1)
	go func() { done <- s.Run(ctx, orch, "go") }()

	<-done // the (non-interactive) orchestrator returns after its final turn
	close(events)
	<-drained

	if !sawHeartbeat() {
		t.Error("no liveness heartbeat was emitted for a slow tool dispatch")
	}
}

// hangTool ignores its context and blocks until released — a stand-in for a
// misbehaving registry/MCP tool that never honors cancellation.
type hangTool struct{ release chan struct{} }

func (hangTool) Name() string { return "hang" }
func (hangTool) Schema() tools.Schema {
	return tools.Schema{Name: "hang", Description: "hangs", Parameters: json.RawMessage(`{"type":"object","properties":{},"required":[]}`)}
}
func (h hangTool) Execute(_ context.Context, _ tools.Call) (tools.Result, error) {
	<-h.release // deliberately ignores ctx
	return tools.Result{Content: "done"}, nil
}

// TestDispatchWatchedCtxEscape: a tool that ignores ctx must not wedge the agent. On
// kill/engagement-end, dispatchWatched returns via its ctx.Done escape so Run unwinds
// (and the subagent's slot is freed) rather than blocking forever on the stuck tool.
func TestDispatchWatchedCtxEscape(t *testing.T) {
	events := make(chan event.Event, 64)
	go func() {
		for range events {
		}
	}()
	rel := make(chan struct{})
	reg := tools.NewRegistry()
	reg.Register(hangTool{release: rel})

	mock := &scriptLLM{steps: []scriptStep{{tool: "hang", toolArg: "{}"}}}
	llmFor := func(string) (LLM, string, error) { return mock, "m", nil }
	cfgFor := func(string) (AgentConfig, bool) { return AgentConfig{}, false }
	s := NewSession(reg, nil, llmFor, cfgFor, events, 5)
	orch := AgentConfig{Role: "Main", Model: "m", IsOrchestrator: true, System: "x", Tools: []string{"hang"}}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan Result, 1)
	go func() { done <- s.Run(ctx, orch, "go") }()

	time.Sleep(150 * time.Millisecond) // let the agent reach the hanging tool
	cancel()                           // engagement ends while the tool is stuck
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		close(rel)
		t.Fatal("Run wedged on a ctx-ignoring tool — dispatchWatched escape regression")
	}
	close(rel) // let the orphaned tool goroutine finish
}
