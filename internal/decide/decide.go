// Package decide is a minimal client for an OpenAI-compatible "decisions" API: a
// System-1 decision model that reads one document (`input`) and answers typed
// questions (predicate / choice / score), returning calibrated probabilities and
// generating NO output tokens. It is provider- and model-agnostic — the base URL,
// API key and model id come from oline's config, exactly like internal/llm. One
// Client per provider base URL + key + model. Cancelling the context aborts a call.
package decide

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"time"
)

// TransientError marks a failure that is likely temporary — a request timeout
// (including a cold model that did not warm up within the deadline), a network
// error, an HTTP 429, or a 5xx — as opposed to a definitive rejection (a 4xx, or a
// response that is not a decisions API at all). A caller probing the model at
// startup can use this to proceed optimistically on a transient failure (the model
// warms up on first real use) while still disabling the feature for a definitive one
// (a provider that does not actually serve /decisions).
type TransientError struct{ Err error }

func (e *TransientError) Error() string { return e.Err.Error() }
func (e *TransientError) Unwrap() error { return e.Err }

// IsTransient reports whether err (from any decide call) is a TransientError.
func IsTransient(err error) bool {
	var t *TransientError
	return errors.As(err, &t)
}

// sharedTransport pools connections across decision clients (mirrors internal/llm).
var sharedTransport = &http.Transport{
	Proxy:               http.ProxyFromEnvironment,
	ForceAttemptHTTP2:   true,
	MaxIdleConns:        100,
	MaxIdleConnsPerHost: 64,
	IdleConnTimeout:     90 * time.Second,
	TLSHandshakeTimeout: 10 * time.Second,
}

// Client talks to one decisions-API provider/model. The decisions endpoint is the
// provider's base URL plus "/decisions".
type Client struct {
	http    *http.Client
	baseURL string
	apiKey  string
	model   string

	maxAttempts int           // total connect attempts (1 = no retry)
	backoff     time.Duration // base retry delay
}

// New returns a decision client. baseURL is the provider's OpenAI-style base (e.g.
// ".../v1"); requests go to baseURL+"/decisions". There is no overall HTTP timeout —
// the caller bounds each call via the context.
func New(baseURL, apiKey, model string) *Client {
	return &Client{
		http:        &http.Client{Transport: sharedTransport},
		baseURL:     strings.TrimRight(baseURL, "/"),
		apiKey:      apiKey,
		model:       model,
		maxAttempts: 3,
		backoff:     300 * time.Millisecond,
	}
}

// Choice is one option of a choice question: a string value the model may pick, with
// an optional natural-language description to help it choose.
type Choice struct {
	Value       string
	Description string
}

// --- wire types ------------------------------------------------------------

type wireChoice struct {
	Value       string `json:"value"`
	Description string `json:"description,omitempty"`
}

type wireLevel struct {
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
}

type question struct {
	Type         string       `json:"type"`
	Name         string       `json:"name"`
	Instructions string       `json:"instructions"`
	Choices      []wireChoice `json:"choices,omitempty"`
	Levels       []wireLevel  `json:"levels,omitempty"`
}

type apiRequest struct {
	Model     string     `json:"model"`
	Input     string     `json:"input"`
	Questions []question `json:"questions"`
}

type apiAnswer struct {
	Type          string          `json:"type"`
	Name          string          `json:"name"`
	Probability   float64         `json:"probability"` // predicate
	Choice        json.RawMessage `json:"choice"`      // choice (string or bool)
	Score         float64         `json:"score"`       // score (probability-weighted level index)
	Confidence    float64         `json:"confidence"`
	Probabilities []struct {
		Value       json.RawMessage `json:"value"`
		Label       string          `json:"label"`
		Probability float64         `json:"probability"`
	} `json:"probabilities"`
}

type apiResponse struct {
	Answers []apiAnswer `json:"answers"`
}

// --- questions -------------------------------------------------------------

// Predicate asks a yes/no question about input and returns the probability the
// answer is true (0..1).
func (c *Client) Predicate(ctx context.Context, input, instructions string) (float64, error) {
	a, err := c.ask(ctx, input, question{Type: "predicate", Name: "q", Instructions: instructions})
	if err != nil {
		return 0, err
	}
	return a.Probability, nil
}

// Choice asks the model to pick one of choices for input. It returns the chosen
// value and the probability assigned to it (falling back to the answer confidence).
func (c *Client) Choice(ctx context.Context, input, instructions string, choices []Choice) (string, float64, error) {
	wc := make([]wireChoice, len(choices))
	for i, ch := range choices {
		wc[i] = wireChoice{Value: ch.Value, Description: ch.Description}
	}
	a, err := c.ask(ctx, input, question{Type: "choice", Name: "q", Instructions: instructions, Choices: wc})
	if err != nil {
		return "", 0, err
	}
	val := rawToString(a.Choice)
	prob := a.Confidence
	for _, p := range a.Probabilities {
		if rawToString(p.Value) == val {
			prob = p.Probability
			break
		}
	}
	return val, prob, nil
}

// Score asks a graded-scale question (levels lowest-first) about input and returns
// the most-likely level index and a 0..1 grade (the probability-weighted level
// index normalized by the top index).
func (c *Client) Score(ctx context.Context, input, instructions string, levels []string) (int, float64, error) {
	wl := make([]wireLevel, len(levels))
	for i, l := range levels {
		wl[i] = wireLevel{Label: l}
	}
	a, err := c.ask(ctx, input, question{Type: "score", Name: "q", Instructions: instructions, Levels: wl})
	if err != nil {
		return 0, 0, err
	}
	idx := int(math.Round(a.Score))
	if idx < 0 {
		idx = 0
	}
	if idx > len(levels)-1 {
		idx = len(levels) - 1
	}
	grade := 0.0
	if len(levels) > 1 {
		grade = a.Score / float64(len(levels)-1)
	}
	return idx, grade, nil
}

// Ping is a trivial decision call used to check the model is reachable at startup.
func (c *Client) Ping(ctx context.Context) error {
	_, err := c.Predicate(ctx, "ready check", "Answer true.")
	return err
}

// --- transport -------------------------------------------------------------

func (c *Client) ask(ctx context.Context, input string, q question) (apiAnswer, error) {
	raw, err := json.Marshal(apiRequest{Model: c.model, Input: input, Questions: []question{q}})
	if err != nil {
		return apiAnswer{}, fmt.Errorf("decide: marshal: %w", err)
	}
	var lastErr error
	for attempt := 1; attempt <= c.maxAttempts; attempt++ {
		if attempt > 1 {
			select {
			case <-time.After(c.backoff * time.Duration(attempt-1)):
			case <-ctx.Done():
				return apiAnswer{}, &TransientError{ctx.Err()}
			}
		}
		a, retryable, err := c.once(ctx, raw)
		if err == nil {
			return a, nil
		}
		lastErr = err
		if ctx.Err() != nil {
			return apiAnswer{}, &TransientError{ctx.Err()}
		}
		if !retryable {
			return apiAnswer{}, err // a definitive rejection (4xx / bad response shape)
		}
	}
	// Retries exhausted on a retryable (429 / 5xx / network) error: transient.
	return apiAnswer{}, &TransientError{lastErr}
}

func (c *Client) once(ctx context.Context, raw []byte) (apiAnswer, bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/decisions", bytes.NewReader(raw))
	if err != nil {
		return apiAnswer{}, false, fmt.Errorf("decide: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return apiAnswer{}, true, fmt.Errorf("decide: request failed: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		retryable := resp.StatusCode == 429 || resp.StatusCode >= 500
		return apiAnswer{}, retryable, fmt.Errorf("decide: provider returned %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	var r apiResponse
	if err := json.Unmarshal(body, &r); err != nil {
		return apiAnswer{}, false, fmt.Errorf("decide: decode response: %w", err)
	}
	if len(r.Answers) == 0 {
		return apiAnswer{}, false, fmt.Errorf("decide: response had no answers")
	}
	if r.Answers[0].Type == "refusal" {
		return apiAnswer{}, false, fmt.Errorf("decide: model refused to answer")
	}
	return r.Answers[0], false, nil
}

// rawToString decodes a JSON value that may be a quoted string or a bare literal
// (e.g. a boolean) into its string form.
func rawToString(r json.RawMessage) string {
	s := strings.TrimSpace(string(r))
	if s == "" {
		return ""
	}
	if s[0] == '"' {
		var str string
		if json.Unmarshal(r, &str) == nil {
			return str
		}
	}
	return s
}
