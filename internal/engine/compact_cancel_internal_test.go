package engine

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/nihaal-prasad/oline/internal/event"
	"github.com/nihaal-prasad/oline/internal/llm"
	"github.com/nihaal-prasad/oline/internal/tools"
)

// TestCompactKillDuringSummarize models the real pause/kill-during-compaction path:
// compaction runs on the agent goroutine under the step context, and Kill cancels
// that context (cancelStep). A summarizer stalled mid-call must then unwind promptly,
// compact must return, and the history must be left UNCHANGED (no partial/empty
// summary written, no orphaned tool pairing) so the agent can stop cleanly.
func TestCompactKillDuringSummarize(t *testing.T) {
	events := make(chan event.Event, 64)
	go func() {
		for range events {
		}
	}()
	s := NewSession(tools.NewRegistry(), nil, nil, func(string) (AgentConfig, bool) { return AgentConfig{}, false }, events, 5)
	mock := &stallLLM{release: make(chan struct{})} // summarizer stays blocked until released/cancelled
	a := newAgent("recon#1", AgentConfig{Role: "recon", MaxContextTokens: 100}, mock, "m", s, "", "")
	bigHistory(a, 10)
	wantLen := len(a.history)

	// Mirror Run: the step context is cancellable and registered via setCancel, so
	// Kill's cancelStep tears it down exactly as in the live loop.
	stepCtx, cancel := context.WithCancel(context.Background())
	a.setCancel(cancel)

	done := make(chan struct{})
	go func() { a.compact(stepCtx); close(done) }()

	time.Sleep(50 * time.Millisecond) // let compaction reach the (blocked) summarizer
	a.Kill()                          // sets Killed + cancelStep(), as the TUI control op does

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		close(mock.release)
		t.Fatal("compact did not return after Kill cancelled the step — summarizer cancellation regression")
	}

	if len(a.history) != wantLen {
		t.Errorf("a cancelled compaction changed the history: %d -> %d (must be left intact)", wantLen, len(a.history))
	}
	if strings.Contains(a.history[0].Content, "late done") {
		t.Error("a cancelled compaction wrote a summary into the history")
	}
	close(mock.release)
}

// TestCompactEngagementCancelDuringSummarize is the engagement-end variant: the base
// (engagement) context is cancelled while the summarizer is stalled. compact must
// return, leave the history intact, and report the failure (so the pane does not look
// frozen mid-summary).
func TestCompactEngagementCancelDuringSummarize(t *testing.T) {
	events := make(chan event.Event, 64)
	getNotices := noticeCollector(events)
	s := NewSession(tools.NewRegistry(), nil, nil, func(string) (AgentConfig, bool) { return AgentConfig{}, false }, events, 5)
	mock := &stallLLM{release: make(chan struct{})}
	a := newAgent("recon#1", AgentConfig{Role: "recon", MaxContextTokens: 100}, mock, "m", s, "", "")
	bigHistory(a, 10)
	wantLen := len(a.history)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { a.compact(ctx); close(done) }()
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		close(mock.release)
		t.Fatal("compact did not return after the engagement context was cancelled")
	}
	close(mock.release)
	close(events)

	if len(a.history) != wantLen {
		t.Errorf("history changed on a cancelled compaction: %d -> %d", wantLen, len(a.history))
	}
	var warned bool
	for _, n := range getNotices() {
		if n.Level == "warn" && strings.Contains(n.Text, "compaction failed") {
			warned = true
		}
	}
	if !warned {
		t.Error("a cancelled compaction emitted no failure notice; the pane would look frozen mid-summary")
	}
}

// TestCompactTailNeverStartsWithToolResult asserts the structural invariant compact
// must preserve regardless of where the keep boundary falls: the kept tail (and thus
// the compacted history after the summary message) must never BEGIN with a tool
// result, whose matching assistant tool_calls turn would have been summarized away —
// an OpenAI-style provider 400s on that. Built so the natural boundary lands amid a
// tool-result run.
func TestCompactTailNeverStartsWithToolResult(t *testing.T) {
	events := make(chan event.Event, 64)
	go func() {
		for range events {
		}
	}()
	s := NewSession(tools.NewRegistry(), nil, nil, func(string) (AgentConfig, bool) { return AgentConfig{}, false }, events, 5)
	a := newAgent("recon#1", AgentConfig{Role: "recon", MaxContextTokens: 400}, sumMock{text: "SUMMARY"}, "m", s, "", "")

	// Filler to push well over the ceiling, then a run of tool results (with their
	// assistant tool_calls turn) and a trailing assistant message, so the keep boundary
	// can fall on a tool result and must be pushed forward.
	big := strings.Repeat("y", 1200)
	tc := []llm.ToolCall{{ID: "t1", Name: "bash", Arguments: `{"cmd":"x"}`}}
	for i := 0; i < 8; i++ {
		a.history = append(a.history,
			llm.Message{Role: llm.RoleUser, Content: big},
			llm.Message{Role: llm.RoleAssistant, Content: big})
	}
	a.history = append(a.history,
		llm.Message{Role: llm.RoleAssistant, ToolCalls: tc},
		llm.Message{Role: llm.RoleTool, Content: "r1"},
		llm.Message{Role: llm.RoleTool, Content: "r2"},
		llm.Message{Role: llm.RoleAssistant, Content: "done"})
	a.tokensEst = estimateTokens(a.cfg.System, a.history)

	a.compact(context.Background())

	// If compaction happened, index 0 is the summary and index 1 is the tail start.
	if len(a.history) >= 2 && a.history[1].Role == llm.RoleTool {
		t.Errorf("compacted tail begins with an orphan tool result: %+v", a.history[1])
	}
	// And no tool result anywhere in the result may lack a preceding assistant turn.
	for i, m := range a.history {
		if m.Role == llm.RoleTool && (i == 0 || a.history[i-1].Role == llm.RoleUser) {
			t.Errorf("tool result at %d has no assistant turn before it (orphaned)", i)
		}
	}
}
