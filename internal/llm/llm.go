// Package llm is an OpenAI-compatible, streaming chat-completions client. One
// Client per provider (baseURL + key). Cancelling the context aborts the request
// mid-stream, which is the engine's pause/abort primitive.
package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// sharedTransport pools connections across all provider clients. The default
// transport caps idle connections per host at 2, which churns TLS connections
// when several agents stream from the same provider concurrently; raise it.
var sharedTransport = &http.Transport{
	Proxy:                 http.ProxyFromEnvironment,
	ForceAttemptHTTP2:     true,
	MaxIdleConns:          100,
	MaxIdleConnsPerHost:   64,
	IdleConnTimeout:       90 * time.Second,
	TLSHandshakeTimeout:   10 * time.Second,
	ExpectContinueTimeout: time.Second,
}

// Role is a chat message role.
type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

// Message is one chat message.
type Message struct {
	Role       Role
	Content    string
	ToolCalls  []ToolCall // assistant messages that call tools
	ToolCallID string     // for RoleTool: which call this result answers
	Name       string     // optional tool/function name
}

// ToolCall is an assistant's request to call a function.
type ToolCall struct {
	ID        string
	Name      string
	Arguments string // JSON
}

// FunctionDef advertises a callable tool to the model.
type FunctionDef struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

// ChatReq is a streaming chat-completions request.
type ChatReq struct {
	Model           string
	Messages        []Message
	Tools           []FunctionDef
	ReasoningEffort string // low|medium|high -> reasoning_effort (when the model supports it)
	MaxTokens       int
	// SessionAffinity, when set, is sent as the `x-session-affinity` request header.
	// On multi-replica serverless providers (e.g. Fireworks) prompt caches are
	// per-replica, so a stable per-agent affinity key pins an agent's successive
	// turns to the same replica and lets its growing, mostly-unchanged prompt prefix
	// hit the (far cheaper) prompt cache instead of being reprocessed in full. It is
	// only a routing hint and never changes the model's output.
	SessionAffinity string
}

// Delta is one normalized streamed event. Exactly one of its fields is
// meaningful per value.
type Delta struct {
	TextDelta      string
	ReasoningDelta string
	ToolCall       *ToolCallDelta
	Finish         string // "stop" | "tool_calls" | "length" | ...
	Usage          *Usage
}

// ToolCallDelta is a streamed fragment of a tool call (OpenAI streams them by index).
type ToolCallDelta struct {
	Index        int
	ID           string
	Name         string
	ArgsFragment string
}

// Usage is token accounting from the final chunk. CachedPromptTokens is the
// subset of PromptTokens that were served from the provider's prompt cache (and
// are billed at the cheaper cache-read rate).
type Usage struct {
	PromptTokens       int
	CompletionTokens   int
	CachedPromptTokens int
}

// Client talks to one OpenAI-compatible provider.
type Client struct {
	http    *http.Client
	baseURL string
	apiKey  string

	// maxAttempts is the total number of connect attempts (1 = no retry) for a
	// request that fails before any output has streamed; backoffBase is the first
	// retry delay (doubled, with jitter, each further attempt). Defaults are set by
	// New; tests override them.
	maxAttempts int
	backoffBase time.Duration
}

// New returns a client for baseURL using apiKey. There is no overall HTTP
// timeout; callers control lifetime via the context passed to Stream.
func New(baseURL, apiKey string) *Client {
	return &Client{
		http:        &http.Client{Transport: sharedTransport},
		baseURL:     strings.TrimRight(baseURL, "/"),
		apiKey:      apiKey,
		maxAttempts: 4,
		backoffBase: 500 * time.Millisecond,
	}
}

// --- wire types -----------------------------------------------------------

type apiReq struct {
	Model           string         `json:"model"`
	Messages        []apiMsg       `json:"messages"`
	Stream          bool           `json:"stream"`
	StreamOptions   *apiStreamOpts `json:"stream_options,omitempty"`
	Tools           []apiTool      `json:"tools,omitempty"`
	ToolChoice      string         `json:"tool_choice,omitempty"`
	MaxTokens       int            `json:"max_tokens,omitempty"`
	ReasoningEffort string         `json:"reasoning_effort,omitempty"`
}

type apiStreamOpts struct {
	IncludeUsage bool `json:"include_usage"`
}

type apiMsg struct {
	Role       string        `json:"role"`
	Content    *string       `json:"content"` // ALWAYS present (null for a tool-call assistant with no text); never omitted — providers reject a missing content field
	ToolCalls  []apiToolCall `json:"tool_calls,omitempty"`
	ToolCallID string        `json:"tool_call_id,omitempty"`
	Name       string        `json:"name,omitempty"`
}

type apiTool struct {
	Type     string      `json:"type"`
	Function FunctionDef `json:"function"`
}

type apiToolCall struct {
	ID       string      `json:"id"`
	Type     string      `json:"type"`
	Function apiFuncCall `json:"function"`
}

type apiFuncCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type apiChunk struct {
	Choices []struct {
		Delta struct {
			Content          string `json:"content"`
			ReasoningContent string `json:"reasoning_content"`
			Reasoning        string `json:"reasoning"`
			ToolCalls        []struct {
				Index    int    `json:"index"`
				ID       string `json:"id"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens        int `json:"prompt_tokens"`
		CompletionTokens    int `json:"completion_tokens"`
		PromptTokensDetails *struct {
			CachedTokens int `json:"cached_tokens"`
		} `json:"prompt_tokens_details"`
	} `json:"usage"`
}

// --- request building ------------------------------------------------------

func toAPIMessages(ms []Message) []apiMsg {
	out := make([]apiMsg, len(ms))
	for i, m := range ms {
		am := apiMsg{Role: string(m.Role), ToolCallID: m.ToolCallID, Name: m.Name}
		for _, tc := range m.ToolCalls {
			am.ToolCalls = append(am.ToolCalls, apiToolCall{
				ID:       tc.ID,
				Type:     "function",
				Function: apiFuncCall{Name: tc.Name, Arguments: tc.Arguments},
			})
		}
		// The `content` field must always be serialized. An assistant message that
		// only makes tool calls uses null; everything else (including an empty tool
		// result) uses the string, so it is never dropped.
		if m.Role == RoleAssistant && len(am.ToolCalls) > 0 && m.Content == "" {
			am.Content = nil
		} else {
			c := m.Content
			am.Content = &c
		}
		out[i] = am
	}
	return out
}

func (c *Client) buildBody(req ChatReq) ([]byte, error) {
	body := apiReq{
		Model:           req.Model,
		Messages:        toAPIMessages(req.Messages),
		Stream:          true,
		StreamOptions:   &apiStreamOpts{IncludeUsage: true},
		MaxTokens:       req.MaxTokens,
		ReasoningEffort: req.ReasoningEffort,
	}
	for _, t := range req.Tools {
		body.Tools = append(body.Tools, apiTool{Type: "function", Function: t})
	}
	if len(body.Tools) > 0 {
		body.ToolChoice = "auto"
	}
	return json.Marshal(body)
}

// Stream POSTs a streaming chat-completions request and emits normalized deltas
// on ch, closing ch when the stream ends. Cancelling ctx aborts the request
// immediately (the HTTP body read unblocks and the method returns ctx.Err()).
func (c *Client) Stream(ctx context.Context, req ChatReq, ch chan<- Delta) error {
	defer close(ch)

	raw, err := c.buildBody(req)
	if err != nil {
		return fmt.Errorf("llm: marshal request: %w", err)
	}

	// Connect with bounded retry+backoff: a transient failure before any output has
	// streamed (connection error, 429, or 5xx) is retried, which rides out a brief
	// provider blip or rate-limit instead of killing the agent's turn. Once the body
	// is being read and deltas have been emitted, a mid-stream failure is NOT retried
	// (the partial output already went out) — it returns an error as before.
	resp, err := c.connect(ctx, raw, req.SessionAffinity)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	emit := func(d Delta) error {
		select {
		case ch <- d:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	// Inline <think>…</think> chain-of-thought is split out of content into the
	// reasoning channel so the TUI can treat it as a (toggleable) thought.
	sp := &thinkSplitter{}
	emitTxt := func(s string) error { return emit(Delta{TextDelta: s}) }
	emitThink := func(s string) error { return emit(Delta{ReasoningDelta: s}) }

	r := bufio.NewReaderSize(resp.Body, 1<<20)
	for {
		line, err := readLine(r, maxSSELineBytes)
		if len(line) > 0 {
			if derr := dispatchLine(line, emit, sp, emitTxt, emitThink); derr != nil {
				return derr
			}
		}
		if err != nil {
			if err == io.EOF {
				return sp.flush(emitTxt, emitThink)
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("llm: read stream: %w", err)
		}
	}
}

// connect POSTs the request and returns a live 200 response, retrying a transient
// connect-phase failure (network error, 429, or 5xx) up to c.maxAttempts with
// exponential backoff. It never retries once a 200 body is in hand, so no streamed
// output is ever duplicated. Cancelling ctx aborts immediately.
func (c *Client) connect(ctx context.Context, raw []byte, affinity string) (*http.Response, error) {
	attempts := c.maxAttempts
	if attempts < 1 {
		attempts = 1
	}
	var lastErr error
	var nextDelay time.Duration // delay to wait before the NEXT attempt (0 => exponential)
	for attempt := 1; attempt <= attempts; attempt++ {
		if attempt > 1 {
			if err := c.backoff(ctx, attempt-1, nextDelay); err != nil {
				return nil, err
			}
			nextDelay = 0
		}
		httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/chat/completions", bytes.NewReader(raw))
		if err != nil {
			return nil, fmt.Errorf("llm: build request: %w", err)
		}
		httpReq.Header.Set("Content-Type", "application/json")
		httpReq.Header.Set("Accept", "text/event-stream")
		if c.apiKey != "" {
			httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)
		}
		// Prompt-cache stickiness: route this agent's turns to the same serverless
		// replica so its cached prompt prefix is reused (ignored by providers that
		// don't honor the header). Harmless when empty.
		if affinity != "" {
			httpReq.Header.Set("x-session-affinity", affinity)
		}

		resp, err := c.http.Do(httpReq)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			lastErr = fmt.Errorf("llm: request failed: %w", err)
			continue // network error: retry
		}
		if resp.StatusCode == http.StatusOK {
			return resp, nil
		}
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		retryAfter := parseRetryAfter(resp.Header.Get("Retry-After"))
		resp.Body.Close()
		lastErr = fmt.Errorf("llm: provider returned %s: %s", resp.Status, strings.TrimSpace(string(snippet)))
		if attempt < attempts && retryableStatus(resp.StatusCode) {
			// Wait once before retrying: a server-advertised Retry-After (common on
			// 429) if present, otherwise the exponential backoff. The single sleep
			// happens at the top of the next iteration.
			nextDelay = retryAfter
			continue
		}
		return nil, lastErr
	}
	return nil, lastErr
}

// backoff sleeps before a retry: either the explicit duration d, or an exponential
// delay (backoffBase * 2^(n-1), capped at 8s) with jitter. Returns ctx.Err() if the
// context ends first.
func (c *Client) backoff(ctx context.Context, n int, d time.Duration) error {
	if d <= 0 {
		base := c.backoffBase
		if base <= 0 {
			base = 500 * time.Millisecond
		}
		// n = attempt-1 (>=1 for the first retry). Shift by n-1 so the first retry waits
		// exactly backoffBase, matching the doc (backoffBase * 2^(n-1)); the guard keeps
		// the shift non-negative if ever called with n==0.
		shift := n - 1
		if shift < 0 {
			shift = 0
		}
		d = base << uint(shift)
		if d > 8*time.Second {
			d = 8 * time.Second
		}
		d += time.Duration(rand.Int63n(int64(base))) // jitter up to one base interval
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// retryableStatus reports whether an HTTP status is worth retrying (rate limit or a
// transient server error). A 4xx other than 408/425/429 is a client error and is not
// retried.
func retryableStatus(code int) bool {
	switch code {
	case http.StatusRequestTimeout, // 408
		http.StatusTooEarly,            // 425
		http.StatusTooManyRequests,     // 429
		http.StatusInternalServerError, // 500
		http.StatusBadGateway,          // 502
		http.StatusServiceUnavailable,  // 503
		http.StatusGatewayTimeout:      // 504
		return true
	}
	return false
}

// parseRetryAfter reads a Retry-After header given as a number of seconds (the
// HTTP-date form is ignored). Returns 0 when absent/unparseable. Capped at 30s so a
// hostile value can't stall an agent indefinitely.
func parseRetryAfter(h string) time.Duration {
	h = strings.TrimSpace(h)
	if h == "" {
		return 0
	}
	secs, err := strconv.Atoi(h)
	if err != nil || secs <= 0 {
		return 0
	}
	if secs > 30 {
		secs = 30
	}
	return time.Duration(secs) * time.Second
}

// maxSSELineBytes caps the length of a single SSE line. A legitimate line (even
// one carrying a large streamed tool-call fragment) stays well under this; the
// cap stops a buggy or hostile provider from streaming an endless line with no
// newline and exhausting memory.
const maxSSELineBytes = 32 << 20 // 32 MiB

// readLine reads one '\n'-terminated line (the newline is included when present)
// but fails once a single line exceeds max bytes, so an unterminated stream
// can't grow without bound.
func readLine(r *bufio.Reader, max int) (string, error) {
	chunk, err := r.ReadSlice('\n')
	if err != bufio.ErrBufferFull {
		return string(chunk), err // common case: whole line fit in the buffer
	}
	buf := append([]byte(nil), chunk...)
	for {
		if len(buf) > max {
			return "", fmt.Errorf("llm: SSE line exceeds %d bytes", max)
		}
		chunk, err = r.ReadSlice('\n')
		buf = append(buf, chunk...)
		if err != bufio.ErrBufferFull {
			return string(buf), err
		}
	}
}

// dispatchLine parses one SSE line and emits any resulting deltas. Content is
// routed through sp so inline <think> spans become reasoning. It returns a
// sentinel error only on context cancellation.
func dispatchLine(line string, emit func(Delta) error, sp *thinkSplitter, emitTxt, emitThink func(string) error) error {
	line = strings.TrimRight(line, "\r\n")
	data, ok := strings.CutPrefix(line, "data:")
	if !ok {
		return nil // comments, blank lines, event: fields
	}
	data = strings.TrimSpace(data)
	if data == "" || data == "[DONE]" {
		return nil
	}
	var chunk apiChunk
	if err := json.Unmarshal([]byte(data), &chunk); err != nil {
		return nil // tolerate keep-alive / non-JSON lines
	}

	for _, choice := range chunk.Choices {
		d := choice.Delta
		if d.Content != "" {
			if err := sp.feed(d.Content, emitTxt, emitThink); err != nil {
				return err
			}
		}
		if rc := d.ReasoningContent; rc != "" {
			if err := emit(Delta{ReasoningDelta: rc}); err != nil {
				return err
			}
		} else if d.Reasoning != "" {
			if err := emit(Delta{ReasoningDelta: d.Reasoning}); err != nil {
				return err
			}
		}
		for _, tc := range d.ToolCalls {
			if err := emit(Delta{ToolCall: &ToolCallDelta{
				Index:        tc.Index,
				ID:           tc.ID,
				Name:         tc.Function.Name,
				ArgsFragment: tc.Function.Arguments,
			}}); err != nil {
				return err
			}
		}
		if choice.FinishReason != "" {
			if err := emit(Delta{Finish: choice.FinishReason}); err != nil {
				return err
			}
		}
	}
	if chunk.Usage != nil {
		u := &Usage{
			PromptTokens:     chunk.Usage.PromptTokens,
			CompletionTokens: chunk.Usage.CompletionTokens,
		}
		if d := chunk.Usage.PromptTokensDetails; d != nil {
			u.CachedPromptTokens = d.CachedTokens
		}
		if err := emit(Delta{Usage: u}); err != nil {
			return err
		}
	}
	return nil
}

// AssembleToolCalls folds a sequence of ToolCallDeltas (as received during a
// stream) into complete ToolCalls, keyed by index. Helper for the engine.
func AssembleToolCalls(frags []ToolCallDelta) []ToolCall {
	var out []ToolCall
	byIndex := map[int]int{} // stream index -> position in out of the call currently open at that index
	for _, f := range frags {
		pos, open := byIndex[f.Index]
		// A fragment carrying a NEW id at an index that already holds a call with a
		// DIFFERENT id starts a separate tool call. Some stricter OpenAI-compatible
		// providers reuse an index across sequential calls this way; keying on index
		// alone would concatenate two calls' arguments into one broken blob.
		if open && f.ID != "" && out[pos].ID != "" && out[pos].ID != f.ID {
			open = false
		}
		if !open {
			out = append(out, ToolCall{})
			pos = len(out) - 1
			byIndex[f.Index] = pos
		}
		if f.ID != "" {
			out[pos].ID = f.ID
		}
		if f.Name != "" {
			out[pos].Name = f.Name
		}
		out[pos].Arguments += f.ArgsFragment
	}
	return out
}
