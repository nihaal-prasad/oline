package engine

import (
	"context"
	"strings"
	"testing"

	"github.com/nihaal-prasad/oline/internal/event"
	"github.com/nihaal-prasad/oline/internal/llm"
	"github.com/nihaal-prasad/oline/internal/tools"
)

// sumMock is a minimal LLM that returns a fixed summary for the compaction call.
type sumMock struct{ text string }

func (m sumMock) Stream(ctx context.Context, _ llm.ChatReq, ch chan<- llm.Delta) error {
	defer close(ch)
	select {
	case ch <- llm.Delta{TextDelta: m.text}:
	case <-ctx.Done():
		return ctx.Err()
	}
	ch <- llm.Delta{Finish: "stop"}
	return nil
}

func TestCompactIfNeeded(t *testing.T) {
	events := make(chan event.Event, 64)
	go func() {
		for range events {
		}
	}()
	s := NewSession(tools.NewRegistry(), nil, nil, func(string) (AgentConfig, bool) { return AgentConfig{}, false }, events, 5)
	a := newAgent("recon#1", AgentConfig{Role: "recon", MaxContextTokens: 100}, sumMock{text: "DENSE SUMMARY"}, "m", s, "", "")

	big := strings.Repeat("x", 2000) // ~500 tokens each, well over the 100 ceiling
	for i := 0; i < 10; i++ {
		a.history = append(a.history,
			llm.Message{Role: llm.RoleUser, Content: big},
			llm.Message{Role: llm.RoleAssistant, Content: big})
	}
	before := len(a.history)
	a.tokensEst = estimateTokens(a.cfg.System, a.history) // sync the cache after building history directly

	a.compactIfNeeded(context.Background())

	if len(a.history) >= before {
		t.Fatalf("history was not compacted: %d -> %d", before, len(a.history))
	}
	if a.history[0].Role != llm.RoleUser || !strings.Contains(a.history[0].Content, "DENSE SUMMARY") {
		t.Errorf("first message should be the summary, got %q / %q", a.history[0].Role, a.history[0].Content)
	}
}

func TestContextUsage(t *testing.T) {
	events := make(chan event.Event, 16)
	go func() {
		for range events {
		}
	}()
	s := NewSession(tools.NewRegistry(), nil, nil, func(string) (AgentConfig, bool) { return AgentConfig{}, false }, events, 5)

	a := newAgent("recon#1", AgentConfig{Role: "recon", MaxContextTokens: 1000}, sumMock{}, "m", s, "", "")
	a.history = []llm.Message{{Role: llm.RoleUser, Content: strings.Repeat("x", 2000)}} // ~500 tokens of 1000
	a.tokensEst = estimateTokens(a.cfg.System, a.history)                               // sync the cache
	s.router.register(a)
	pct, ok := s.router.ContextUsage("recon#1")
	if !ok {
		t.Fatal("expected a usage percentage for a live agent with a ceiling")
	}
	if pct < 40 || pct > 70 {
		t.Errorf("context usage = %d%%, want ~50%%", pct)
	}

	if _, ok := s.router.ContextUsage("ghost#9"); ok {
		t.Error("an unknown agent should report no usage")
	}

	b := newAgent("b#1", AgentConfig{Role: "b"}, sumMock{}, "m", s, "", "") // no ceiling
	s.router.register(b)
	if _, ok := s.router.ContextUsage("b#1"); ok {
		t.Error("an agent with no ceiling should report no usage")
	}
}

func TestCompactNoopUnderCeiling(t *testing.T) {
	events := make(chan event.Event, 16)
	go func() {
		for range events {
		}
	}()
	s := NewSession(tools.NewRegistry(), nil, nil, func(string) (AgentConfig, bool) { return AgentConfig{}, false }, events, 5)
	a := newAgent("recon#1", AgentConfig{Role: "recon", MaxContextTokens: 100000}, sumMock{text: "x"}, "m", s, "", "")
	a.history = []llm.Message{{Role: llm.RoleUser, Content: "small"}}
	a.compactIfNeeded(context.Background())
	if len(a.history) != 1 {
		t.Errorf("history under the ceiling must not be compacted: %d", len(a.history))
	}
}
