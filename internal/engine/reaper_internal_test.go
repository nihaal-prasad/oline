package engine

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nihaal-prasad/oline/internal/event"
	"github.com/nihaal-prasad/oline/internal/llm"
	"github.com/nihaal-prasad/oline/internal/tools"
)

// scriptLLM plays a fixed sequence of turns (text, or one tool call), one per Stream.
type scriptLLM struct {
	mu    sync.Mutex
	steps []scriptStep
	i     int
}

type scriptStep struct {
	text    string
	tool    string
	toolArg string
}

func (m *scriptLLM) Stream(ctx context.Context, _ llm.ChatReq, ch chan<- llm.Delta) error {
	defer close(ch)
	m.mu.Lock()
	var s scriptStep
	if m.i < len(m.steps) {
		s = m.steps[m.i]
		m.i++
	} else {
		s = scriptStep{text: "idle"}
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
	if s.tool != "" {
		if !send(llm.Delta{ToolCall: &llm.ToolCallDelta{Index: 0, ID: "t", Name: s.tool, ArgsFragment: s.toolArg}}) {
			return ctx.Err()
		}
		send(llm.Delta{Finish: "tool_calls"})
		return nil
	}
	if !send(llm.Delta{TextDelta: s.text}) {
		return ctx.Err()
	}
	send(llm.Delta{Finish: "stop"})
	return nil
}

// TestSubagentReaped: a finished persistent subagent that is never reprompted is
// reaped after subagentIdleTTL (its goroutine ends), so a long engagement doesn't
// accumulate idle subagents. Main (interactive) is never reaped.
func TestSubagentReaped(t *testing.T) {
	oldTTL := subagentIdleTTL
	subagentIdleTTL = 60 * time.Millisecond
	defer func() { subagentIdleTTL = oldTTL }()

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
	sawReap := func() bool {
		mu.Lock()
		defer mu.Unlock()
		for _, n := range notices {
			if strings.Contains(n.Text, "reclaiming") && strings.Contains(string(n.Agent), "Probe") {
				return true
			}
		}
		return false
	}

	mainMock := &scriptLLM{steps: []scriptStep{{tool: "task", toolArg: `{"agent":"probe","prompt":"go"}`}}}
	probeMock := &scriptLLM{steps: []scriptStep{{text: "probe done"}}}
	llmFor := func(sel string) (LLM, string, error) {
		if sel == "mock/probe" {
			return probeMock, "p", nil
		}
		return mainMock, "m", nil
	}
	cfgFor := func(role string) (AgentConfig, bool) {
		if role == "probe" {
			return AgentConfig{Role: "probe", Model: "mock/probe"}, true
		}
		return AgentConfig{}, false
	}
	s := NewSession(tools.NewRegistry(), nil, llmFor, cfgFor, events, 5)
	s.Interactive = true // keep Main alive so only the subagent is reaped
	orch := AgentConfig{Role: "Main", Model: "mock/main", Tools: []string{"task", "wait"}, IsOrchestrator: true}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan Result, 1)
	go func() { done <- s.Run(ctx, orch, "go") }()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && !sawReap() {
		time.Sleep(10 * time.Millisecond)
	}
	if !sawReap() {
		t.Error("a finished subagent was not reaped after the idle TTL")
	}
	cancel()
	<-done
	close(events)
	<-drained
}
