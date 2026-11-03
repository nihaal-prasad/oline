package tui

import (
	"strings"
	"testing"

	"github.com/nihaal-prasad/oline/internal/llm"
)

func TestSanitizeHistoryDropsDanglingToolCall(t *testing.T) {
	h := []llm.Message{
		{Role: llm.RoleUser, Content: "start"},
		{Role: llm.RoleAssistant, Content: "", ToolCalls: []llm.ToolCall{{ID: "a", Name: "bash", Arguments: "{}"}}},
		{Role: llm.RoleTool, ToolCallID: "a", Content: "output"},
		{Role: llm.RoleAssistant, Content: "", ToolCalls: []llm.ToolCall{{ID: "b", Name: "bash", Arguments: "{}"}}},
		// snapshot landed before the tool result for "b" arrived -> dangling
	}
	got := sanitizeHistory(h)
	if len(got) != 3 {
		t.Fatalf("want 3 valid messages, got %d: %+v", len(got), got)
	}
	if got[len(got)-1].Role != llm.RoleTool {
		t.Errorf("sanitized history should end at the completed tool exchange, got %q", got[len(got)-1].Role)
	}
}

func TestSanitizeHistoryKeepsCompleteHistory(t *testing.T) {
	h := []llm.Message{
		{Role: llm.RoleUser, Content: "start"},
		{Role: llm.RoleAssistant, Content: "done"},
	}
	if got := sanitizeHistory(h); len(got) != 2 {
		t.Errorf("complete history should be kept whole: got %d", len(got))
	}
}

func TestHistoryExistsEmptyName(t *testing.T) {
	if historyExists("") {
		t.Error("empty engagement name should not report existing history")
	}
}

func TestRebuildTranscriptRendersToolCall(t *testing.T) {
	tr := &transcript{}
	rebuildTranscript(tr, []llm.Message{
		{Role: llm.RoleUser, Content: "do a scan"},
		{Role: llm.RoleAssistant, Content: "running nmap", ToolCalls: []llm.ToolCall{{ID: "1", Name: "bash", Arguments: `{"cmd":"nmap -sV 10.0.0.5"}`}}},
		{Role: llm.RoleTool, ToolCallID: "1", Content: "22/tcp open ssh"},
	})
	txt := tr.plainText(true)
	for _, want := range []string{"do a scan", "running nmap", "nmap -sV 10.0.0.5", "22/tcp open ssh"} {
		if !strings.Contains(txt, want) {
			t.Errorf("rebuilt transcript missing %q:\n%s", want, txt)
		}
	}
}
