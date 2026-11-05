package engine

import (
	"strings"
	"testing"

	"github.com/nihaal-prasad/oline/internal/event"
	"github.com/nihaal-prasad/oline/internal/llm"
	"github.com/nihaal-prasad/oline/internal/tools"
)

// TestTerminalToolReadsOperatorPanes checks that content the TUI mirrors for the
// operator's shell and terminal panes is readable by an agent through the `terminal`
// tool (list + read), the way revshell read exposes a reverse shell.
func TestTerminalToolReadsOperatorPanes(t *testing.T) {
	events := make(chan event.Event, 64)
	go func() {
		for range events {
		}
	}()
	s := NewSession(tools.NewRegistry(), nil, nil, func(string) (AgentConfig, bool) { return AgentConfig{}, false }, events, 5)
	a := newAgent("recon#1", AgentConfig{Role: "recon", Tools: []string{"terminal"}}, nil, "m", s, "", "")

	// Empty to begin with.
	if out, isErr := a.doTerminal(llm.ToolCall{Arguments: `{"action":"list"}`}); isErr || !strings.Contains(out, "no operator terminals") {
		t.Fatalf("empty list should say so, got isErr=%v %q", isErr, out)
	}

	// The TUI mirrors a line shell's command/output and a terminal's screen.
	s.RecordOperatorLine("Shell#1", "$ whoami")
	s.RecordOperatorLine("Shell#1", "root")
	s.SetOperatorScreen("Term#1", "top - 10:00:00 up 1 day\nPID USER")

	out, isErr := a.doTerminal(llm.ToolCall{Arguments: `{"action":"list"}`})
	if isErr || !strings.Contains(out, "Shell#1") || !strings.Contains(out, "Term#1") {
		t.Fatalf("list should show both terminals, got isErr=%v %q", isErr, out)
	}
	if !strings.Contains(out, "[shell]") || !strings.Contains(out, "[terminal]") {
		t.Errorf("list should report each terminal's kind, got %q", out)
	}

	out, isErr = a.doTerminal(llm.ToolCall{Arguments: `{"action":"read","name":"Shell#1"}`})
	if isErr || !strings.Contains(out, "whoami") || !strings.Contains(out, "root") {
		t.Errorf("read Shell#1 should return its command history, got isErr=%v %q", isErr, out)
	}

	out, isErr = a.doTerminal(llm.ToolCall{Arguments: `{"action":"read","name":"Term#1"}`})
	if isErr || !strings.Contains(out, "PID USER") {
		t.Errorf("read Term#1 should return its screen, got isErr=%v %q", isErr, out)
	}

	// A full-screen terminal keeps only its latest screen (replace, not append).
	s.SetOperatorScreen("Term#1", "SECOND SCREEN")
	if out, _ := a.doTerminal(llm.ToolCall{Arguments: `{"action":"read","name":"Term#1"}`}); strings.Contains(out, "PID USER") || !strings.Contains(out, "SECOND SCREEN") {
		t.Errorf("terminal screen should be replaced, got %q", out)
	}

	// Unknown terminal and closed terminal are reported / gone.
	if out, isErr := a.doTerminal(llm.ToolCall{Arguments: `{"action":"read","name":"Nope#9"}`}); !isErr || !strings.Contains(out, "no open operator terminal") {
		t.Errorf("reading an unknown terminal should error, got isErr=%v %q", isErr, out)
	}
	s.ForgetOperator("Shell#1")
	if out, isErr := a.doTerminal(llm.ToolCall{Arguments: `{"action":"read","name":"Shell#1"}`}); !isErr || !strings.Contains(out, "no open operator terminal") {
		t.Errorf("a closed terminal should no longer be readable, got isErr=%v %q", isErr, out)
	}
}

// TestTerminalToolAdvertised checks the terminal tool is actually advertised to an
// agent in its function-call schema (the mechanism by which the model knows a tool
// exists), not merely dispatchable — i.e. it is in primitiveSchemas and surfaces
// through toolDefs when granted.
func TestTerminalToolAdvertised(t *testing.T) {
	events := make(chan event.Event, 8)
	go func() {
		for range events {
		}
	}()
	s := NewSession(tools.NewRegistry(), nil, nil, func(string) (AgentConfig, bool) { return AgentConfig{}, false }, events, 5)
	a := newAgent("recon#1", AgentConfig{Role: "recon", Tools: []string{"terminal"}}, nil, "m", s, "", "")
	found := false
	for _, d := range a.toolDefs() {
		if d.Name == "terminal" {
			found = true
		}
	}
	if !found {
		t.Error("the terminal tool must be advertised in the agent's function-call schema when granted")
	}
}

// TestTerminalShellTrimsToCap checks a chatty operator shell's mirror is bounded
// (kept to the most recent output) rather than growing without limit.
func TestTerminalShellTrimsToCap(t *testing.T) {
	events := make(chan event.Event, 8)
	go func() {
		for range events {
		}
	}()
	s := NewSession(tools.NewRegistry(), nil, nil, func(string) (AgentConfig, bool) { return AgentConfig{}, false }, events, 5)
	line := strings.Repeat("x", 1000)
	for i := 0; i < 200; i++ { // ~200 KB of output into a 64 KB cap
		s.RecordOperatorLine("Shell#1", line)
	}
	content, ok := s.readOperatorTerminal("Shell#1")
	if !ok {
		t.Fatal("shell should exist")
	}
	if len(content) > opTermMaxBytes {
		t.Errorf("shell mirror not trimmed to cap: %d > %d", len(content), opTermMaxBytes)
	}
}
