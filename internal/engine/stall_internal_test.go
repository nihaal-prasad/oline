package engine

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nihaal-prasad/oline/internal/core"
	"github.com/nihaal-prasad/oline/internal/event"
	"github.com/nihaal-prasad/oline/internal/llm"
	"github.com/nihaal-prasad/oline/internal/tools"
)

// stallLLM is an LLM whose stream stays silent until released, then emits one
// token and finishes — exercising the stall watchdog (a turn with no output).
type stallLLM struct{ release chan struct{} }

func (m *stallLLM) Stream(ctx context.Context, _ llm.ChatReq, ch chan<- llm.Delta) error {
	defer close(ch)
	select {
	case <-m.release:
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case ch <- llm.Delta{TextDelta: "late done"}:
	case <-ctx.Done():
		return ctx.Err()
	}
	return nil
}

// TestStreamStallWarns proves a turn that produces no output for streamStallWarn
// posts a "still waiting" notice, so a slow/stalled provider doesn't look like a
// frozen-but-"running" agent.
func TestStreamStallWarns(t *testing.T) {
	old := streamStallWarn
	streamStallWarn = 40 * time.Millisecond
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
	sawStall := func() bool {
		mu.Lock()
		defer mu.Unlock()
		for _, n := range notices {
			if n.Agent == core.Main && strings.Contains(n.Text, "still waiting for the model") {
				return true
			}
		}
		return false
	}

	mock := &stallLLM{release: make(chan struct{})}
	llmFor := func(string) (LLM, string, error) { return mock, "m", nil }
	cfgFor := func(string) (AgentConfig, bool) { return AgentConfig{}, false }
	s := NewSession(tools.NewRegistry(), nil, llmFor, cfgFor, events, 5)
	s.Interactive = true
	orch := AgentConfig{Role: "Main", Model: "m", IsOrchestrator: true, System: "x"}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan Result, 1)
	go func() { done <- s.Run(ctx, orch, "go") }()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && !sawStall() {
		time.Sleep(10 * time.Millisecond)
	}
	if !sawStall() {
		t.Error("no stall notice was emitted for a silent model turn")
	}

	close(mock.release) // let the turn finish, then wind the orchestrator down
	cancel()
	<-done
	close(events)
	<-drained
}
