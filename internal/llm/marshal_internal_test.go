package llm

import (
	"encoding/json"
	"testing"
)

// TestContentAlwaysPresent guards against the 400 "missing field content":
// every message must serialize a "content" field. An assistant message that
// only makes tool calls uses null; an empty tool result uses "".
func TestContentAlwaysPresent(t *testing.T) {
	c := New("http://example", "k")
	raw, err := c.buildBody(ChatReq{
		Model: "m",
		Messages: []Message{
			{Role: RoleUser, Content: "hi"},
			{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "1", Name: "find", Arguments: "{}"}}}, // no text
			{Role: RoleTool, ToolCallID: "1", Name: "find", Content: ""},                           // empty result
			{Role: RoleAssistant, Content: "done"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	var body struct {
		Messages []map[string]json.RawMessage `json:"messages"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Messages) != 4 {
		t.Fatalf("got %d messages", len(body.Messages))
	}
	for i, m := range body.Messages {
		if _, ok := m["content"]; !ok {
			t.Errorf("message %d is missing the content field: %v", i, m)
		}
	}
	if s := string(body.Messages[1]["content"]); s != "null" {
		t.Errorf("assistant tool-call content = %s, want null", s)
	}
	if s := string(body.Messages[2]["content"]); s != `""` {
		t.Errorf("empty tool-result content = %s, want \"\"", s)
	}
	if s := string(body.Messages[3]["content"]); s != `"done"` {
		t.Errorf("assistant text content = %s, want \"done\"", s)
	}
}
