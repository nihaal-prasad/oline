package engine_test

import (
	"context"
	"testing"
	"time"

	"github.com/nihaal-prasad/oline/internal/engine"
	"github.com/nihaal-prasad/oline/internal/event"
	"github.com/nihaal-prasad/oline/internal/tools"
)

// TestSendMessageIsAToolCall verifies that agent-to-agent send_message surfaces
// as a tool call in the sender's transcript (a ToolStarted with the full args,
// plus a ToolOutput with the delivery result), the same way other tools do.
func TestSendMessageIsAToolCall(t *testing.T) {
	mainMock := &mockLLM{steps: []step{
		{toolID: "t1", toolName: "task", toolArgs: `{"agent":"probe","prompt":"go"}`},
		{toolID: "m1", toolName: "send_message", toolArgs: `{"to":"probe#1","body":"what's your status?"}`},
		{text: "FINAL: done"},
	}}
	probeMock := &mockLLM{steps: []step{{text: "probe idle"}}}

	llmFor := func(sel string) (engine.LLM, string, error) {
		if sel == "mock/main" {
			return mainMock, "m", nil
		}
		return probeMock, "p", nil
	}
	cfgFor := func(role string) (engine.AgentConfig, bool) {
		if role == "probe" {
			return engine.AgentConfig{Role: "probe", Model: "mock/probe"}, true
		}
		return engine.AgentConfig{}, false
	}

	events := make(chan event.Event, 256)
	c := newCollector()
	go drain(events, c)

	s := engine.NewSession(tools.NewRegistry(), mockSB{}, llmFor, cfgFor, events, 5)
	mainCfg := engine.AgentConfig{Role: "Main", Model: "mock/main", Tools: []string{"task", "wait", "send_message"}, IsOrchestrator: true}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	res := s.Run(ctx, mainCfg, "go")
	close(events)
	<-c.done

	if res.Err != nil {
		t.Fatalf("Run error: %v", res.Err)
	}
	if !c.sawToolStart("send_message") {
		t.Error("send_message did not emit a ToolStarted event (not shown as a tool call)")
	}
	if !c.sawToolOutput("message delivered") {
		t.Error("send_message did not emit its delivery result as tool output")
	}
	// The recipient must also get a Message event so the message is visible in ITS
	// pane — otherwise a delivered message looks like it was never received.
	// The recipient id is canonicalized ("probe" -> "Probe#1"); the Message event
	// must carry that canonical id so it lands in the right pane.
	if !c.sawMessage("Probe#1", "what's your status?") {
		t.Error("send_message did not emit an event.Message to the recipient (inbound message not surfaced in its pane)")
	}
}
