package engine_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/nihaal-prasad/oline/internal/core"
	"github.com/nihaal-prasad/oline/internal/engine"
	"github.com/nihaal-prasad/oline/internal/event"
	"github.com/nihaal-prasad/oline/internal/tools"
)

func (c *collector) sawNotice(sub string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, e := range c.ev {
		if n, ok := e.(event.Notice); ok && containsStr(n.Text, sub) {
			return true
		}
	}
	return false
}

func waitNotice(t *testing.T, c *collector, sub string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if c.sawNotice(sub) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for notice %q", sub)
}

// TestOrchestratorSurvivesProviderErrorAndResumes is the regression for the
// out-of-cash case: when the inference provider fails the orchestrator's turn
// (e.g. a 402 because the account ran out of credits), the interactive orchestrator
// must NOT end the engagement. It goes idle, and after the user reloads and sends any
// message, it re-issues the failed turn and continues from where it stopped.
func TestOrchestratorSurvivesProviderErrorAndResumes(t *testing.T) {
	payment := errors.New("llm: provider returned 402 Payment Required: insufficient credits")
	mock := &mockLLM{steps: []step{
		{err: payment},                 // turn 1: provider is out of cash
		{text: "resumed after reload"}, // turn 2 (after the user reprompts): continues
	}}
	llmFor := func(string) (engine.LLM, string, error) { return mock, "m", nil }
	cfgFor := func(string) (engine.AgentConfig, bool) { return engine.AgentConfig{}, false }

	events := make(chan event.Event, 256)
	c := newCollector()
	go drain(events, c)

	s := engine.NewSession(tools.NewRegistry(), mockSB{}, llmFor, cfgFor, events, 5)
	s.Interactive = true
	orch := engine.AgentConfig{Role: "Main", Model: "m", IsOrchestrator: true, System: "x"}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan engine.Result, 1)
	go func() { done <- s.Run(ctx, orch, "start the engagement") }()

	// The failed turn surfaces the provider error and the resume hint, and Main idles.
	waitNotice(t, c, "402 Payment Required")
	waitNotice(t, c, "send any message to resume")

	// Main must still be alive (did not return the error and tear the run down).
	select {
	case res := <-done:
		t.Fatalf("orchestrator exited on a provider error instead of waiting to resume: %+v", res)
	case <-time.After(100 * time.Millisecond):
	}

	// The user reloads credits and reprompts: the engagement resumes.
	if !s.Router().Send(engine.Envelope{From: "User", To: core.Main, Kind: engine.MsgSteer, Body: "reloaded, continue"}) {
		t.Fatal("send to the idle orchestrator failed (it exited?)")
	}
	waitText(t, c, "resumed after reload")

	cancel()
	close(events)
	<-done
}
