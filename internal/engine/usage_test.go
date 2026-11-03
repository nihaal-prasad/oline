package engine_test

import (
	"context"
	"testing"
	"time"

	"github.com/nihaal-prasad/oline/internal/engine"
	"github.com/nihaal-prasad/oline/internal/event"
	"github.com/nihaal-prasad/oline/internal/llm"
	"github.com/nihaal-prasad/oline/internal/tools"
)

// usageLLM streams text and reports usage on SEVERAL chunks (as some providers do,
// carrying the running total), then stops with no tool call.
type usageLLM struct{}

func (usageLLM) Stream(ctx context.Context, _ llm.ChatReq, ch chan<- llm.Delta) error {
	defer close(ch)
	ch <- llm.Delta{TextDelta: "hello "}
	ch <- llm.Delta{Usage: &llm.Usage{PromptTokens: 1000, CompletionTokens: 10}}
	ch <- llm.Delta{TextDelta: "world"}
	ch <- llm.Delta{Usage: &llm.Usage{PromptTokens: 1000, CompletionTokens: 20}}
	ch <- llm.Delta{Usage: &llm.Usage{PromptTokens: 1000, CompletionTokens: 30}}
	ch <- llm.Delta{Finish: "stop"}
	return nil
}

func (c *collector) usages() []event.Usage {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []event.Usage
	for _, e := range c.ev {
		if u, ok := e.(event.Usage); ok {
			out = append(out, u)
		}
	}
	return out
}

// TestUsageBilledOncePerTurn is the regression test for cost over-counting: when a
// provider reports usage on every chunk, the engine must emit ONE Usage event per
// turn with the final totals, not one per chunk (which billed a single request many
// times over — the compaction-cost blow-up).
func TestUsageBilledOncePerTurn(t *testing.T) {
	llmFor := func(string) (engine.LLM, string, error) { return usageLLM{}, "m", nil }
	cfgFor := func(string) (engine.AgentConfig, bool) { return engine.AgentConfig{}, false }

	events := make(chan event.Event, 128)
	c := newCollector()
	go drain(events, c)

	s := engine.NewSession(tools.NewRegistry(), mockSB{}, llmFor, cfgFor, events, 5)
	main := engine.AgentConfig{Role: "Main", Model: "mock/main", IsOrchestrator: true}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s.Run(ctx, main, "go")
	close(events)
	<-c.done

	us := c.usages()
	if len(us) != 1 {
		t.Fatalf("want exactly 1 Usage event for the turn (not one per chunk), got %d: %+v", len(us), us)
	}
	if us[0].PromptTokens != 1000 || us[0].CompletionTokens != 30 {
		t.Errorf("Usage should carry the final totals, got prompt=%d completion=%d", us[0].PromptTokens, us[0].CompletionTokens)
	}
}
