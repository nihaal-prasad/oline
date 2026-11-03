package engine

import (
	"context"
	"fmt"
	"strings"

	"github.com/nihaal-prasad/oline/internal/event"
	"github.com/nihaal-prasad/oline/internal/llm"
)

// Auto-compaction keeps an agent's conversation under its context ceiling: when
// the estimated token count crosses the ceiling, the older turns are summarized
// into one message and a recent tail is kept verbatim. The ceiling is per agent
// (see wire.go for the defaults and the user override).

const (
	// compactKeepFraction: after compacting, keep the most recent messages that fit
	// in roughly this fraction of the ceiling; summarize everything before them.
	compactKeepFraction = 0.30
	// charsPerToken approximates tokens from characters (~4 chars per token).
	charsPerToken = 4
	// maxSummarizeChars bounds the transcript text handed to the summarizer.
	maxSummarizeChars = 400_000
	// summaryMaxTokens caps the summary length.
	summaryMaxTokens = 4096
)

// msgTokens estimates one message's token cost (~4 chars/token plus a little
// per-message and per-tool-call overhead). estimateTokens sums it over a history;
// the agent also adds it incrementally as messages are appended.
func msgTokens(m llm.Message) int {
	n := len(m.Content)/charsPerToken + 8
	for _, c := range m.ToolCalls {
		n += (len(c.Name)+len(c.Arguments))/charsPerToken + 4
	}
	return n
}

// estimateTokens is a cheap heuristic for how many tokens a system prompt plus a
// message history occupy (~4 chars/token, with a little per-message overhead).
func estimateTokens(system string, msgs []llm.Message) int {
	n := len(system)/charsPerToken + 8
	for _, m := range msgs {
		n += msgTokens(m)
	}
	return n
}

// compactIfNeeded summarizes an agent's older conversation when the estimated
// context exceeds its ceiling, keeping a recent tail intact. It runs on the
// agent's own goroutine, so reads need no lock; the new history is written back
// under the lock. A failure leaves the history unchanged.
func (a *Agent) compactIfNeeded(ctx context.Context) {
	ceil := a.cfg.MaxContextTokens
	if ceil <= 0 {
		return
	}
	a.mu.Lock()
	hist := append([]llm.Message(nil), a.history...)
	before := a.tokensEst // O(1) cached estimate
	a.mu.Unlock()
	if before < ceil {
		return
	}

	// Keep the most recent messages up to compactKeepFraction of the ceiling;
	// summarize everything before that.
	keepBudget := int(float64(ceil) * compactKeepFraction)
	keepFrom, acc := len(hist), 0
	for i := len(hist) - 1; i >= 0; i-- {
		acc += estimateTokens("", hist[i:i+1])
		if acc > keepBudget {
			break
		}
		keepFrom = i
	}
	// Always keep at least the most recent message, even if it alone exceeds the
	// keep budget (otherwise a tiny ceiling would never compact).
	if keepFrom >= len(hist) {
		keepFrom = len(hist) - 1
	}
	// The kept tail must not begin with an orphan tool result (its assistant
	// tool-call would be in the summarized part), so push the boundary past any.
	for keepFrom < len(hist) && hist[keepFrom].Role == llm.RoleTool {
		keepFrom++
	}
	if keepFrom <= 0 || keepFrom >= len(hist) {
		return // nothing to summarize, or nothing would be left to keep
	}
	older, tail := hist[:keepFrom], hist[keepFrom:]

	// Bound the summarize LLM call so a hung stream can't stall the agent loop
	// indefinitely (the normal turn stream has the same backstop).
	sctx, cancel := context.WithTimeout(ctx, llmTurnTimeout)
	defer cancel()

	// Cap the summary length to a fraction of the ceiling so a small ceiling can't
	// produce a summary larger than the ceiling itself (which would re-trigger
	// compaction every turn — the thrash guard).
	maxTok := summaryMaxTokens
	if ceil/4 < maxTok {
		maxTok = ceil / 4
	}
	if maxTok < 256 {
		maxTok = 256
	}

	// Try the cheap summarizer model first, then fall back to the agent's own model
	// (which already handles this agent's context) if the summarizer one errors or
	// returns nothing — for example when the summarizer model's context window is
	// smaller than the transcript. Each attempt caps the transcript to what that
	// model's window can hold, so the model is never handed more than it accepts.
	type attempt struct {
		client  LLM
		model   string
		inChars int
	}
	var attempts []attempt
	if a.s.SummarizerLLM != nil && a.s.SummarizerModel != "" {
		attempts = append(attempts, attempt{a.s.SummarizerLLM, a.s.SummarizerModel, inputBudgetChars(a.s.SummarizerContextTokens, maxTok)})
	}
	attempts = append(attempts, attempt{a.llm, a.modelID, inputBudgetChars(ceil, maxTok)})

	var summary string
	var lastErr error
	for _, at := range attempts {
		summary, lastErr = a.summarize(sctx, at.client, at.model, older, at.inChars, maxTok)
		if lastErr == nil && strings.TrimSpace(summary) != "" {
			break
		}
	}
	if strings.TrimSpace(summary) == "" {
		reason := "the model returned an empty summary"
		if lastErr != nil {
			reason = lastErr.Error()
		}
		a.s.emit(event.Notice{Agent: a.id, Level: "warn",
			Text: fmt.Sprintf("%s: context compaction failed (%s); continuing without it", a.id, reason)})
		return
	}
	newHist := make([]llm.Message, 0, len(tail)+1)
	newHist = append(newHist, llm.Message{Role: llm.RoleUser, Content: "[Summary of the earlier conversation, auto-compacted to stay within the context limit]\n\n" + summary})
	newHist = append(newHist, tail...)
	after := estimateTokens(a.cfg.System, newHist)

	a.mu.Lock()
	a.history = newHist
	a.tokensEst = after // reset the running estimate to the compacted history
	a.mu.Unlock()
	a.s.emit(event.Notice{Agent: a.id, Level: "info",
		Text: fmt.Sprintf("%s: auto-compacted context (~%d → ~%d tokens, ceiling %d)", a.id, before, after, ceil)})
}

// inputBudgetChars is how many characters of transcript may be handed to a model
// with ctxTokens of context window, once room is left for the summary output
// (outTokens) and a little overhead. 0 (unknown window) means the fixed cap. The
// result is clamped to [4000, maxSummarizeChars] so there is always something to
// summarize but never more than the hard cap.
func inputBudgetChars(ctxTokens, outTokens int) int {
	if ctxTokens <= 0 {
		return maxSummarizeChars
	}
	budget := (ctxTokens - outTokens - 1024) * charsPerToken
	if budget < 4000 {
		budget = 4000
	}
	if budget > maxSummarizeChars {
		budget = maxSummarizeChars
	}
	return budget
}

// summarize produces a dense brief of older messages via the given model, capping
// the flattened transcript to inChars characters and the output to maxTok tokens.
// The transcript is flattened to plain text, so tool-call/result pairing never
// matters for the summarization request.
func (a *Agent) summarize(ctx context.Context, client LLM, model string, older []llm.Message, inChars, maxTok int) (string, error) {
	var sb strings.Builder
	for _, m := range older {
		switch m.Role {
		case llm.RoleUser:
			sb.WriteString("USER: " + m.Content + "\n")
		case llm.RoleAssistant:
			if strings.TrimSpace(m.Content) != "" {
				sb.WriteString("ASSISTANT: " + m.Content + "\n")
			}
			for _, c := range m.ToolCalls {
				sb.WriteString("ASSISTANT called " + c.Name + " " + c.Arguments + "\n")
			}
		case llm.RoleTool:
			sb.WriteString("TOOL RESULT: " + m.Content + "\n")
		}
	}
	text := sb.String()
	if inChars > 0 && len(text) > inChars { // keep the most recent part if too long
		text = text[len(text)-inChars:]
	}
	req := llm.ChatReq{
		Model: model,
		Messages: []llm.Message{
			{Role: llm.RoleSystem, Content: "You compact an AI agent's working transcript so it can keep going within a limited context window. Produce a concise but COMPLETE brief that preserves everything needed to continue: the goal, the current state, key facts and findings, decisions made, hosts/credentials/artifacts discovered, what was tried and what worked or failed, and the open threads and next steps. Be factual and dense; add no commentary."},
			{Role: llm.RoleUser, Content: "Summarize this transcript:\n\n" + text},
		},
		MaxTokens: maxTok,
	}
	return a.runLLMText(ctx, client, req)
}

// runLLMText runs one non-tool LLM request and returns the assembled text.
func (a *Agent) runLLMText(ctx context.Context, client LLM, req llm.ChatReq) (string, error) {
	ch := make(chan llm.Delta, 64)
	errc := make(chan error, 1)
	go func() { errc <- client.Stream(ctx, req, ch) }()
	var b strings.Builder
	var usage *llm.Usage
	for d := range ch {
		if d.TextDelta != "" {
			b.WriteString(d.TextDelta)
		}
		if d.Usage != nil {
			// A provider may report usage on every chunk (often the running total);
			// keep the latest and bill ONCE below, or a single compaction gets counted
			// many times over.
			usage = d.Usage
		}
	}
	if usage != nil {
		// Report compaction spend so the TUI counts it in the engagement total and
		// against the summarizer cost ceiling (it is otherwise invisible).
		a.s.emit(event.Usage{Agent: a.id, Model: req.Model,
			PromptTokens: usage.PromptTokens, CompletionTokens: usage.CompletionTokens,
			CachedPromptTokens: usage.CachedPromptTokens, Summarizer: true})
	}
	return b.String(), <-errc
}
