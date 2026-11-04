package tools

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestRenderCommand(t *testing.T) {
	// Each {{arg}} is replaced with the SHELL-QUOTED value, so a malicious value
	// can't break out of its quotes into the command.
	got := renderCommand("nmap -T4 {{host}} -p {{ports}}", map[string]any{
		"host":  "10.0.0.5; rm -rf /",
		"ports": 80,
	})
	want := "nmap -T4 '10.0.0.5; rm -rf /' -p '80'"
	if got != want {
		t.Errorf("renderCommand = %q, want %q", got, want)
	}
	// A missing argument becomes empty (quoted), never an unquoted blank that could
	// shift the command's meaning.
	got = renderCommand("echo {{missing}}x", map[string]any{})
	if got != "echo ''x" {
		t.Errorf("missing arg render = %q", got)
	}
	// A value containing a single quote is escaped, not terminated.
	got = renderCommand("f {{v}}", map[string]any{"v": "a'b"})
	if got != `f 'a'\''b'` {
		t.Errorf("quote-escape render = %q", got)
	}
}

func TestCommandToolSchema(t *testing.T) {
	ct := CommandTool{ToolName: "scan", Desc: "scan a host", Params: json.RawMessage(`{"type":"object","properties":{"host":{"type":"string"}}}`)}
	if ct.Name() != "scan" {
		t.Errorf("Name = %q", ct.Name())
	}
	s := ct.Schema()
	if s.Name != "scan" || s.Description != "scan a host" || !strings.Contains(string(s.Parameters), "host") {
		t.Errorf("Schema = %+v", s)
	}
	// No params => a valid empty object schema, not empty bytes.
	empty := CommandTool{ToolName: "ping"}.Schema()
	if !strings.Contains(string(empty.Parameters), "object") {
		t.Errorf("empty params schema = %q", empty.Parameters)
	}
}
