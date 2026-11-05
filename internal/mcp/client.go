// Package mcp is a minimal Model Context Protocol (MCP) client. It lets oline
// consume external MCP tool servers: oline spawns a server (stdio transport),
// performs the MCP initialize handshake, discovers the server's tools, and exposes
// each one to agents as an ordinary oline tool (see tool.go) that proxies tools/call
// to the server.
//
// Trust boundary: an MCP server runs on the HOST, outside the sandbox and its
// per-agent egress firewall — like web_search/web_fetch. It is an operator-trusted
// integration, configured in config.yml and granted to agents by name. Only agents
// that are granted an MCP tool can call it.
//
// Transport: stdio only in this version (newline-delimited JSON-RPC 2.0, per the MCP
// spec). The client is safe for concurrent CallTool from multiple agents: writes are
// serialized and responses are routed to the waiting caller by request id.
package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"
)

// protocolVersion is the MCP protocol version oline advertises in initialize. A
// server that speaks a different version still replies with its own; we do not hard
// fail on a mismatch (tools/list + tools/call are stable across these versions).
const protocolVersion = "2025-06-18"

// rpcRequest / rpcResponse are JSON-RPC 2.0 messages. A notification omits ID.
type rpcRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      *int64 `json:"id,omitempty"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      *int64          `json:"id"`
	Result  json.RawMessage `json:"result"`
	Error   *rpcError       `json:"error"`
	Method  string          `json:"method"` // set when the server sends us a request/notification
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *rpcError) Error() string { return fmt.Sprintf("mcp rpc error %d: %s", e.Code, e.Message) }

// ToolDesc is one tool advertised by a server (from tools/list).
type ToolDesc struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

// Client is a connected MCP server over a stdio transport.
type Client struct {
	name string

	w   io.WriteCloser // the server's stdin
	enc *json.Encoder
	wmu sync.Mutex // serializes writes

	mu      sync.Mutex
	nextID  int64
	pending map[int64]chan rpcResponse

	tools []ToolDesc

	closeOnce sync.Once
	closed    chan struct{}
	readDone  chan struct{} // closed when the reader loop has returned (all reads finished)
	onClose   func()        // extra teardown (e.g. kill the process), set by Connect
}

// newClient wires a client to an already-open transport (w = server stdin, r =
// server stdout) and starts the reader loop. Connect uses it after spawning a
// process; tests use it with in-memory pipes.
func newClient(name string, w io.WriteCloser, r io.Reader) *Client {
	c := &Client{
		name:     name,
		w:        w,
		enc:      json.NewEncoder(w),
		pending:  map[int64]chan rpcResponse{},
		closed:   make(chan struct{}),
		readDone: make(chan struct{}),
	}
	go c.readLoop(r)
	return c
}

// Name is the server's configured name.
func (c *Client) Name() string { return c.name }

// Tools returns the tools discovered from the server (empty until initialized).
func (c *Client) Tools() []ToolDesc { return c.tools }

// readLoop reads newline-delimited JSON-RPC messages and routes each response to the
// caller waiting on its id. Server-initiated requests/notifications are ignored (we
// advertise no client capabilities that would trigger them). It ends at EOF.
func (c *Client) readLoop(r io.Reader) {
	defer close(c.readDone)
	br := bufio.NewReaderSize(r, 1<<20)
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			var resp rpcResponse
			if json.Unmarshal(line, &resp) == nil && resp.ID != nil && resp.Method == "" {
				c.mu.Lock()
				ch := c.pending[*resp.ID]
				delete(c.pending, *resp.ID)
				c.mu.Unlock()
				if ch != nil {
					ch <- resp
				}
			}
			// A message with a Method (server request/notification) or unparseable line
			// is ignored: v1 is a pure tool consumer.
		}
		if err != nil {
			c.fail()
			return
		}
	}
}

// fail unblocks every pending caller when the transport dies.
func (c *Client) fail() {
	c.closeOnce.Do(func() {
		close(c.closed)
		if c.onClose != nil {
			c.onClose()
		}
	})
	c.mu.Lock()
	for id, ch := range c.pending {
		close(ch)
		delete(c.pending, id)
	}
	c.mu.Unlock()
}

// call sends a request and waits for its response, bounded by ctx.
func (c *Client) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	c.mu.Lock()
	select {
	case <-c.closed:
		c.mu.Unlock()
		return nil, fmt.Errorf("mcp %q: connection closed", c.name)
	default:
	}
	c.nextID++
	id := c.nextID
	ch := make(chan rpcResponse, 1)
	c.pending[id] = ch
	c.mu.Unlock()

	if err := c.write(rpcRequest{JSONRPC: "2.0", ID: &id, Method: method, Params: params}); err != nil {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return nil, err
	}
	select {
	case <-ctx.Done():
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return nil, ctx.Err()
	case <-c.closed:
		return nil, fmt.Errorf("mcp %q: connection closed", c.name)
	case resp, ok := <-ch:
		if !ok {
			return nil, fmt.Errorf("mcp %q: connection closed", c.name)
		}
		if resp.Error != nil {
			return nil, resp.Error
		}
		return resp.Result, nil
	}
}

// notify sends a notification (no id, no reply expected).
func (c *Client) notify(method string, params any) error {
	return c.write(rpcRequest{JSONRPC: "2.0", Method: method, Params: params})
}

// write serializes one message to the transport as a single newline-delimited line.
func (c *Client) write(req rpcRequest) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	// json.Encoder.Encode appends a newline, which is exactly the stdio framing.
	if err := c.enc.Encode(req); err != nil {
		return fmt.Errorf("mcp %q: write %s: %w", c.name, req.Method, err)
	}
	return nil
}

// initialize performs the MCP handshake: initialize request, then the initialized
// notification. It records the server's reported protocol version.
func (c *Client) initialize(ctx context.Context) error {
	_, err := c.call(ctx, "initialize", map[string]any{
		"protocolVersion": protocolVersion,
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "oline", "version": "1"},
	})
	if err != nil {
		return err
	}
	return c.notify("notifications/initialized", map[string]any{})
}

// listTools fetches every tool the server advertises, following pagination cursors.
func (c *Client) listTools(ctx context.Context) error {
	var all []ToolDesc
	cursor := ""
	for {
		params := map[string]any{}
		if cursor != "" {
			params["cursor"] = cursor
		}
		raw, err := c.call(ctx, "tools/list", params)
		if err != nil {
			return err
		}
		var page struct {
			Tools      []ToolDesc `json:"tools"`
			NextCursor string     `json:"nextCursor"`
		}
		if err := json.Unmarshal(raw, &page); err != nil {
			return fmt.Errorf("mcp %q: bad tools/list response: %w", c.name, err)
		}
		all = append(all, page.Tools...)
		if page.NextCursor == "" || len(page.Tools) == 0 {
			break
		}
		cursor = page.NextCursor
	}
	c.tools = all
	return nil
}

// CallTool invokes a tool on the server and returns its textual content and whether
// the server flagged the result as an error. args is the raw JSON arguments object
// (may be nil/empty).
func (c *Client) CallTool(ctx context.Context, tool string, args json.RawMessage) (string, bool, error) {
	params := map[string]any{"name": tool}
	if len(strings.TrimSpace(string(args))) > 0 {
		params["arguments"] = json.RawMessage(args)
	} else {
		params["arguments"] = map[string]any{}
	}
	raw, err := c.call(ctx, "tools/call", params)
	if err != nil {
		return "", false, err
	}
	var res struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return "", false, fmt.Errorf("mcp %q: bad tools/call response: %w", c.name, err)
	}
	var b strings.Builder
	for _, part := range res.Content {
		if part.Type == "text" {
			b.WriteString(part.Text)
		} else if part.Text != "" {
			b.WriteString(part.Text)
		} else {
			fmt.Fprintf(&b, "[%s content omitted]", part.Type)
		}
	}
	return b.String(), res.IsError, nil
}

// Close shuts the client (and its process, via onClose) down.
func (c *Client) Close() error {
	c.closeOnce.Do(func() {
		close(c.closed)
		_ = c.w.Close()
		if c.onClose != nil {
			c.onClose()
		}
	})
	return nil
}
