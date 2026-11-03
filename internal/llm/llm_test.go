package llm_test

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/nihaal-prasad/oline/internal/llm"
)

func TestAssembleToolCalls(t *testing.T) {
	frags := []llm.ToolCallDelta{
		{Index: 0, ID: "call_a", Name: "bash"},
		{Index: 0, ArgsFragment: `{"cmd":"`},
		{Index: 0, ArgsFragment: `whoami"}`},
		{Index: 1, ID: "call_b", Name: "read", ArgsFragment: `{"path":"/etc/os-release"}`},
	}
	got := llm.AssembleToolCalls(frags)
	if len(got) != 2 {
		t.Fatalf("got %d calls, want 2", len(got))
	}
	if got[0].ID != "call_a" || got[0].Name != "bash" || got[0].Arguments != `{"cmd":"whoami"}` {
		t.Errorf("call 0 wrong: %+v", got[0])
	}
	if got[1].ID != "call_b" || got[1].Name != "read" || got[1].Arguments != `{"path":"/etc/os-release"}` {
		t.Errorf("call 1 wrong: %+v", got[1])
	}
}

// TestLiveStream hits the real provider. Skipped unless FIREWORKS_API_KEY is set.
// Run with: FIREWORKS_API_KEY=... go test ./internal/llm -run Live -v
func TestLiveStream(t *testing.T) {
	key := os.Getenv("FIREWORKS_API_KEY")
	if key == "" {
		t.Skip("FIREWORKS_API_KEY not set; skipping live provider test")
	}
	baseURL := envOr("OLINE_TEST_BASEURL", "https://api.fireworks.ai/inference/v1")
	model := envOr("OLINE_TEST_MODEL", "accounts/fireworks/models/glm-5p3-flash")

	c := llm.New(baseURL, key)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	ch := make(chan llm.Delta, 64)
	var text, reasoning strings.Builder
	var finish string
	var usageSeen bool
	errCh := make(chan error, 1)
	go func() {
		// Enough budget that a reasoning model still emits visible content.
		errCh <- c.Stream(ctx, llm.ChatReq{
			Model:           model,
			MaxTokens:       512,
			ReasoningEffort: "low",
			Messages: []llm.Message{
				{Role: llm.RoleUser, Content: "Reply with exactly the single word: pong"},
			},
		}, ch)
	}()
	for d := range ch {
		switch {
		case d.TextDelta != "":
			text.WriteString(d.TextDelta)
		case d.ReasoningDelta != "":
			reasoning.WriteString(d.ReasoningDelta)
		case d.Finish != "":
			finish = d.Finish
		case d.Usage != nil:
			usageSeen = true
		}
	}
	if err := <-errCh; err != nil {
		t.Fatalf("Stream error: %v", err)
	}
	t.Logf("text=%q reasoningLen=%d finish=%q usageSeen=%v", text.String(), reasoning.Len(), finish, usageSeen)
	if strings.TrimSpace(text.String()) == "" && reasoning.Len() == 0 {
		t.Error("no text or reasoning streamed from provider")
	}
	if finish == "" {
		t.Error("no finish_reason received")
	}
	if !usageSeen {
		t.Error("no usage received (stream_options include_usage not honored?)")
	}
}

// TestLiveToolCall checks a tool-call round-trip streams assembling fragments.
func TestLiveToolCall(t *testing.T) {
	key := os.Getenv("FIREWORKS_API_KEY")
	if key == "" {
		t.Skip("FIREWORKS_API_KEY not set; skipping live tool-call test")
	}
	baseURL := envOr("OLINE_TEST_BASEURL", "https://api.fireworks.ai/inference/v1")
	model := envOr("OLINE_TEST_MODEL", "accounts/fireworks/models/glm-5p3-flash")

	c := llm.New(baseURL, key)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	ch := make(chan llm.Delta, 128)
	var frags []llm.ToolCallDelta
	errCh := make(chan error, 1)
	go func() {
		errCh <- c.Stream(ctx, llm.ChatReq{
			Model:     model,
			MaxTokens: 128,
			Messages: []llm.Message{
				{Role: llm.RoleSystem, Content: "You run shell commands via the get_os tool. Always call it; never answer in prose."},
				{Role: llm.RoleUser, Content: "What OS is this machine running? Use the tool."},
			},
			Tools: []llm.FunctionDef{{
				Name:        "get_os",
				Description: "Return the operating system name of the current machine.",
				Parameters:  []byte(`{"type":"object","properties":{"verbose":{"type":"boolean"}},"required":[]}`),
			}},
		}, ch)
	}()
	for d := range ch {
		if d.ToolCall != nil {
			frags = append(frags, *d.ToolCall)
		}
	}
	if err := <-errCh; err != nil {
		t.Fatalf("Stream error: %v", err)
	}
	calls := llm.AssembleToolCalls(frags)
	t.Logf("assembled %d tool call(s): %+v", len(calls), calls)
	if len(calls) == 0 {
		t.Skip("model chose not to call the tool; skipping (not a client failure)")
	}
	if calls[0].Name != "get_os" {
		t.Errorf("tool call name = %q, want get_os", calls[0].Name)
	}
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
