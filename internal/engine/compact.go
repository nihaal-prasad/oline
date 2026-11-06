package engine

import (
	"context"
	"fmt"
	"strings"
	"unicode/utf8"

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
	est := a.tokensEst // O(1) cached estimate
	a.mu.Unlock()
	if est < ceil {
		return
	}
	a.compact(ctx)
}

// compact summarizes the agent's older conversation into one message and keeps a
// recent tail intact, bringing the context down to roughly compactKeepFraction of
// the ceiling. It no-ops when there is no ceiling or nothing old enough to
// summarize, and a failure leaves the history unchanged. compactIfNeeded calls it
// when the context crosses the ceiling; resume calls it directly to pre-shrink a
// restored transcript so a large prior conversation is not re-sent in full on every
// turn (the display transcript keeps the full history regardless).
func (a *Agent) compact(ctx context.Context) {
	ceil := a.cfg.MaxContextTokens
	if ceil <= 0 {
		return
	}
	a.mu.Lock()
	hist := append([]llm.Message(nil), a.history...)
	before := a.tokensEst
	a.mu.Unlock()

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

	// Tell the UI compaction is starting. It runs on this agent's goroutine with the
	// agent still marked Running, and the summarizer's output is NOT streamed to this
	// agent's transcript, so without this notice the pane shows no thoughts or tokens
	// for however long summarization takes and looks frozen. A matching notice is
	// emitted at the end (success or failure).
	a.s.notifyLive(event.Notice{Agent: a.id, Level: "info",
		Text: fmt.Sprintf("%s: compacting context to stay within the %d-token limit (summarizing ~%d tokens; no output until this finishes)…", a.id, ceil, before)})

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
	// returns nothing. A transcript larger than the summarizer's context window is
	// NOT a reason to fall back: summarize() covers the whole history by summarizing
	// it in window-sized chunks and then reducing the chunk summaries, so the oldest
	// turns are preserved instead of being truncated away (the point of using a
	// smaller, cheaper model to compact a larger model's context). Each attempt is
	// told its model's window via inChars, so no single request overflows it.
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
		a.s.notifyLive(event.Notice{Agent: a.id, Level: "warn",
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
	a.s.notifyLive(event.Notice{Agent: a.id, Level: "info",
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

// compactSystemPrompt steers every summarization request: the per-chunk map pass,
// the reduce pass, and the single-shot common case all share it.
const compactSystemPrompt = "You compact an AI agent's working transcript so it can keep going within a limited context window. Produce a concise but COMPLETE brief that preserves everything needed to continue: the goal, the current state, key facts and findings, decisions made, hosts/credentials/artifacts discovered, what was tried and what worked or failed, and the open threads and next steps. Be factual and dense; add no commentary."

// summarize produces a dense brief of older messages via the given model. The
// messages are flattened to plain text (so tool-call/result pairing never matters
// here) and handed to summarizeText, which keeps every request within inChars.
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
	return a.summarizeText(ctx, client, model, sb.String(), inChars, maxTok)
}

// summarizeText condenses transcript text to at most maxTok tokens using a model
// whose input window holds about inChars characters (inChars <= 0 means unbounded).
// When the text fits, it is summarized in a single call. When it does not, it is
// summarized hierarchically (map/reduce): the text is split into window-sized
// chunks, each chunk is summarized on its own, and the chunk summaries are joined
// and reduced — repeating until one brief remains. This covers the ENTIRE history
// even when the summarizer's window is far smaller than the transcript, instead of
// dropping the oldest turns, which is exactly the case when a large model's context
// is compacted by a smaller, cheaper summarizer model.
func (a *Agent) summarizeText(ctx context.Context, client LLM, model, text string, inChars, maxTok int) (string, error) {
	// maxReduceRounds bounds the reduce recursion so a pathological config can never
	// loop forever; each round roughly halves the text, so this covers ~2^N windows.
	const maxReduceRounds = 6
	lead := "Summarize this transcript:\n\n"
	for round := 0; ; round++ {
		if inChars <= 0 || len(text) <= inChars {
			return a.summarizeOnce(ctx, client, model, lead+text, maxTok)
		}
		// Cap each chunk's summary to half the window's token budget so the combined
		// output is strictly smaller than the input — this is what guarantees the loop
		// makes progress and terminates.
		chunkTok := (inChars / charsPerToken) / 2
		if chunkTok > maxTok {
			chunkTok = maxTok
		}
		if chunkTok < 256 {
			chunkTok = 256
		}
		chunks := chunkString(text, inChars)
		parts := make([]string, 0, len(chunks))
		for _, ch := range chunks {
			s, err := a.summarizeOnce(ctx, client, model, "Summarize this part of a longer transcript:\n\n"+ch, chunkTok)
			if err != nil {
				return "", err // let the caller fall back to the next model
			}
			if strings.TrimSpace(s) != "" {
				parts = append(parts, s)
			}
		}
		combined := strings.Join(parts, "\n\n")
		if strings.TrimSpace(combined) == "" {
			return "", nil
		}
		// Last-resort guard: if we have reduced too many times, or a round failed to
		// shrink the text, keep the most recent window and do one final pass rather
		// than loop. This should not trigger for realistic transcripts.
		if round >= maxReduceRounds || len(combined) >= len(text) {
			if inChars > 0 && len(combined) > inChars {
				combined = combined[len(combined)-inChars:]
			}
			return a.summarizeOnce(ctx, client, model, "Combine these transcript summaries into one dense brief:\n\n"+combined, maxTok)
		}
		// Reduce: summarize the joined chunk summaries (another round if still large).
		text = combined
		lead = "These are ordered summaries of consecutive parts of one transcript; combine them into one dense brief:\n\n"
	}
}

// summarizeOnce runs a single summarization request (the shared system prompt plus
// one user message) and returns the assembled text.
func (a *Agent) summarizeOnce(ctx context.Context, client LLM, model, userMsg string, maxTok int) (string, error) {
	req := llm.ChatReq{
		Model: model,
		Messages: []llm.Message{
			{Role: llm.RoleSystem, Content: compactSystemPrompt},
			{Role: llm.RoleUser, Content: userMsg},
		},
		MaxTokens:       maxTok,
		SessionAffinity: a.affinity, // keep the compaction prompt prefix warm on one replica
	}
	return a.runLLMText(ctx, client, req)
}

// chunkString splits s into pieces of at most max bytes, cutting at the last
// newline within the window when there is one (and otherwise backing off to a
// UTF-8 rune boundary) so a split never lands in the middle of a line or rune.
func chunkString(s string, max int) []string {
	if max <= 0 || len(s) <= max {
		return []string{s}
	}
	var out []string
	for len(s) > max {
		cut := strings.LastIndexByte(s[:max], '\n')
		if cut <= 0 { // no newline in the window: cut at a rune boundary
			cut = max
			for cut > 0 && !utf8.RuneStart(s[cut]) {
				cut--
			}
			if cut == 0 {
				cut = max
			}
		} else {
			cut++ // keep the newline with the chunk before it
		}
		out = append(out, s[:cut])
		s = s[cut:]
	}
	if len(s) > 0 {
		out = append(out, s)
	}
	return out
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
