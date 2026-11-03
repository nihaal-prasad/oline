package engine_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/nihaal-prasad/oline/internal/engine"
	"github.com/nihaal-prasad/oline/internal/event"
	"github.com/nihaal-prasad/oline/internal/tools"
)

// countToolStarts counts ToolStarted events for a given tool.
func (c *collector) countToolStarts(tool string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, e := range c.ev {
		if s, ok := e.(event.ToolStarted); ok && s.Tool == tool {
			n++
		}
	}
	return n
}

// TestNoTurnCap verifies there is no per-agent turn cap: an agent that makes far
// more tool calls than the old limit (60) still runs to its natural end rather
// than being cut off with a "max turns" result. The only limits are time and
// lifecycle.
func TestNoTurnCap(t *testing.T) {
	const n = 150 // well past the retired 60-turn cap
	var steps []step
	for i := 0; i < n; i++ {
		steps = append(steps, step{toolID: "b", toolName: "bash", toolArgs: `{"cmd":"true"}`})
	}
	steps = append(steps, step{text: "FINAL: done"})

	reg := tools.NewRegistry()
	reg.Register(tools.Bash{})

	mock := &mockLLM{steps: steps}
	llmFor := func(string) (engine.LLM, string, error) { return mock, "m", nil }
	cfgFor := func(string) (engine.AgentConfig, bool) { return engine.AgentConfig{}, false }

	events := make(chan event.Event, 4096)
	c := newCollector()
	go drain(events, c)

	s := engine.NewSession(reg, mockSB{out: "ok"}, llmFor, cfgFor, events, 5)
	// A directly-run, non-interactive agent (no parent, not the orchestrator) with
	// the bash tool.
	cfg := engine.AgentConfig{Role: "runner", Model: "mock/runner", Tools: []string{"bash"}}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	res := s.Run(ctx, cfg, "loop a lot")
	close(events)
	<-c.done

	if res.Text != "FINAL: done" {
		t.Fatalf("agent did not finish naturally after %d tool-call turns; got %q", n, res.Text)
	}
	if strings.Contains(res.Text, "max turns") {
		t.Errorf("agent was capped by a turn limit: %q", res.Text)
	}
	if got := c.countToolStarts("bash"); got != n {
		t.Errorf("ran %d bash turns; want %d (loop was truncated)", got, n)
	}
}
