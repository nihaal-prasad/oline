package engine

import (
	"context"
	"fmt"
	"strings"
	"sync"
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

// errMock is an LLM whose stream always fails (e.g. a provider 400), to exercise
// the compaction fallback and error-reporting paths.
type errMock struct{ err string }

func (m errMock) Stream(_ context.Context, _ llm.ChatReq, ch chan<- llm.Delta) error {
	close(ch)
	return fmt.Errorf("%s", m.err)
}

// noticeCollector drains events and returns a getter that, after the channel is
// closed, yields the warn/info notices that were emitted.
func noticeCollector(events chan event.Event) func() []event.Notice {
	var mu sync.Mutex
	var out []event.Notice
	done := make(chan struct{})
	go func() {
		for e := range events {
			if n, ok := e.(event.Notice); ok {
				mu.Lock()
				out = append(out, n)
				mu.Unlock()
			}
		}
		close(done)
	}()
	return func() []event.Notice {
		<-done
		mu.Lock()
		defer mu.Unlock()
		return out
	}
}

// countMock records how many summarization calls it received, so a test can tell a
// single-shot summary from a chunked (map/reduce) one.
type countMock struct {
	mu    sync.Mutex
	calls int
	text  string
}

func (m *countMock) Stream(ctx context.Context, _ llm.ChatReq, ch chan<- llm.Delta) error {
	defer close(ch)
	m.mu.Lock()
	m.calls++
	m.mu.Unlock()
	select {
	case ch <- llm.Delta{TextDelta: m.text}:
	case <-ctx.Done():
		return ctx.Err()
	}
	ch <- llm.Delta{Finish: "stop"}
	return nil
}

func (m *countMock) count() int { m.mu.Lock(); defer m.mu.Unlock(); return m.calls }

func bigHistory(a *Agent, pairs int) {
	big := strings.Repeat("x", 2000)
	for i := 0; i < pairs; i++ {
		a.history = append(a.history,
			llm.Message{Role: llm.RoleUser, Content: big},
			llm.Message{Role: llm.RoleAssistant, Content: big})
	}
	a.tokensEst = estimateTokens(a.cfg.System, a.history)
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

// TestCompactFallsBackToOwnModel checks that when the summarizer (small) model
// fails, compaction retries with the agent's own model and still succeeds.
func TestCompactFallsBackToOwnModel(t *testing.T) {
	events := make(chan event.Event, 64)
	go func() {
		for range events {
		}
	}()
	s := NewSession(tools.NewRegistry(), nil, nil, func(string) (AgentConfig, bool) { return AgentConfig{}, false }, events, 5)
	s.SummarizerLLM = errMock{err: "llm: provider returned 400 Bad Request: context length exceeded"}
	s.SummarizerModel = "small"
	// The agent's OWN model works and returns a summary.
	a := newAgent("recon#1", AgentConfig{Role: "recon", MaxContextTokens: 100}, sumMock{text: "OWN-MODEL SUMMARY"}, "m", s, "", "")
	bigHistory(a, 10)
	before := len(a.history)

	a.compactIfNeeded(context.Background())

	if len(a.history) >= before {
		t.Fatalf("history was not compacted after fallback: %d -> %d", before, len(a.history))
	}
	if !strings.Contains(a.history[0].Content, "OWN-MODEL SUMMARY") {
		t.Errorf("summary should come from the fallback model, got %q", a.history[0].Content)
	}
}

// TestCompactFailureReportsReason checks that when every model fails, the warning
// names the underlying reason instead of a bare "failed".
func TestCompactFailureReportsReason(t *testing.T) {
	events := make(chan event.Event, 64)
	notices := noticeCollector(events)
	s := NewSession(tools.NewRegistry(), nil, nil, func(string) (AgentConfig, bool) { return AgentConfig{}, false }, events, 5)
	// Both the summarizer model and the agent's own model fail.
	s.SummarizerLLM = errMock{err: "llm: provider returned 400 Bad Request: context length exceeded"}
	s.SummarizerModel = "small"
	a := newAgent("recon#1", AgentConfig{Role: "recon", MaxContextTokens: 100}, errMock{err: "llm: provider returned 500"}, "m", s, "", "")
	bigHistory(a, 10)
	before := len(a.history)

	a.compactIfNeeded(context.Background())
	close(events)

	if len(a.history) != before {
		t.Errorf("history must be left unchanged when compaction fails: %d -> %d", before, len(a.history))
	}
	var warn string
	for _, n := range notices() {
		if n.Level == "warn" && strings.Contains(n.Text, "compaction failed") {
			warn = n.Text
		}
	}
	if warn == "" {
		t.Fatal("expected a compaction-failed warning")
	}
	if !strings.Contains(warn, "context length exceeded") && !strings.Contains(warn, "500") {
		t.Errorf("warning should name the underlying reason, got %q", warn)
	}
}

// TestCompactChunksLargeHistory checks that when the whole older transcript is far
// larger than the summarizer model's context window, compaction summarizes it in
// multiple chunks (map/reduce) rather than truncating to the most recent window —
// so the oldest turns are still represented in the final summary.
func TestCompactChunksLargeHistory(t *testing.T) {
	events := make(chan event.Event, 256)
	go func() {
		for range events {
		}
	}()
	s := NewSession(tools.NewRegistry(), nil, nil, func(string) (AgentConfig, bool) { return AgentConfig{}, false }, events, 5)
	mock := &countMock{text: "S"}
	s.SummarizerLLM = mock
	s.SummarizerModel = "small"
	s.SummarizerContextTokens = 6000 // a small window relative to the transcript

	// Large ceiling with far more history than the summarizer window can hold at once.
	a := newAgent("recon#1", AgentConfig{Role: "recon", MaxContextTokens: 8000}, sumMock{text: "OWN"}, "m", s, "", "")
	bigHistory(a, 20)
	before := len(a.history)

	a.compactIfNeeded(context.Background())

	if len(a.history) >= before {
		t.Fatalf("history was not compacted: %d -> %d", before, len(a.history))
	}
	if n := mock.count(); n <= 1 {
		t.Errorf("expected multiple summarizer calls (chunked map/reduce), got %d", n)
	}
	if !strings.Contains(a.history[0].Content, "S") {
		t.Errorf("summary should come from the chunked summarizer, got %q", a.history[0].Content)
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
