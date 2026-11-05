package mcp

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"
	"time"

	"github.com/nihaal-prasad/oline/internal/tools"
)

var nameSanitizer = regexp.MustCompile(`[^A-Za-z0-9_-]`)

// ToolName is the oline tool name for a server tool: "<server>__<tool>", sanitized
// to the characters function-calling names allow and capped to 64 characters. Agents
// grant and call MCP tools by this name.
func ToolName(server, tool string) string {
	clean := func(s string) string { return nameSanitizer.ReplaceAllString(s, "_") }
	name := clean(server) + "__" + clean(tool)
	if len(name) > 64 {
		name = name[:64]
	}
	return name
}

// Tool adapts one MCP server tool to oline's tools.Tool interface. Execute proxies
// to the server's tools/call; it runs host-side (outside the sandbox), so it ignores
// c.Sandbox and c.Policy — the egress firewall does not apply to it.
type Tool struct {
	client   *Client
	server   string
	toolName string // the server-side tool name
	fqName   string // the oline tool name agents use
	desc     string
	params   json.RawMessage
	timeout  time.Duration
}

func (t Tool) Name() string { return t.fqName }

func (t Tool) Schema() tools.Schema {
	params := t.params
	if len(strings.TrimSpace(string(params))) == 0 {
		params = json.RawMessage(`{"type":"object","properties":{}}`)
	}
	desc := strings.TrimSpace(t.desc)
	if desc == "" {
		desc = "MCP tool " + t.toolName + "."
	}
	// Make the host-side nature explicit to the model.
	desc += " (External MCP tool via server \"" + t.server + "\"; it runs on the host, outside the sandbox and its egress rules.)"
	return tools.Schema{Name: t.fqName, Description: desc, Parameters: params}
}

func (t Tool) Execute(ctx context.Context, c tools.Call) (tools.Result, error) {
	to := t.timeout
	if to <= 0 {
		to = DefaultCallTimeout
	}
	cctx, cancel := context.WithTimeout(ctx, to)
	defer cancel()
	out, isErr, err := t.client.CallTool(cctx, t.toolName, c.Args)
	if err != nil {
		return tools.Result{Content: t.fqName + ": " + err.Error(), IsError: true}, nil
	}
	if strings.TrimSpace(out) == "" && !isErr {
		out = "(the MCP tool returned no content)"
	}
	return tools.Result{Content: out, IsError: isErr}, nil
}
