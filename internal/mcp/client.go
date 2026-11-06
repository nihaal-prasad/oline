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

	w       io.WriteCloser // the server's stdin
	enc     *json.Encoder
	writeCh chan writeReq // owned by writeLoop, which is the only writer to enc

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
		writeCh:  make(chan writeReq),
		pending:  map[int64]chan rpcResponse{},
		closed:   make(chan struct{}),
		readDone: make(chan struct{}),
	}
	go c.readLoop(r)
	go c.writeLoop()
	return c
}

// writeReq is one message to serialize; writeLoop replies on done with the encode
// error (nil on success).
type writeReq struct {
	req  rpcRequest
	done chan error
}

// writeLoop is the ONLY goroutine that touches enc, so writes are serialized without
// a lock. If the server stops reading its stdin, Encode blocks here (and only here);
// callers in write() are not stuck, because they bound their enqueue/wait on ctx. A
// blocked Encode unblocks when the transport is closed (fail/Close close c.w), after
// which this loop sees c.closed and returns.
func (c *Client) writeLoop() {
	for {
		select {
		case <-c.closed:
			return
		case wr := <-c.writeCh:
			wr.done <- c.enc.Encode(wr.req)
		}
	}
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
		_ = c.w.Close() // unblock a writeLoop stuck in Encode on a server that stopped reading
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

	if err := c.write(ctx, rpcRequest{JSONRPC: "2.0", ID: &id, Method: method, Params: params}); err != nil {
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

// notify sends a notification (no id, no reply expected), bounded by ctx.
func (c *Client) notify(ctx context.Context, method string, params any) error {
	return c.write(ctx, rpcRequest{JSONRPC: "2.0", Method: method, Params: params})
}

// write hands one message to writeLoop and waits for it to be encoded, bounded by
// ctx and the connection's lifetime. It never blocks indefinitely on a server that
// has stopped reading its stdin: a stuck Encode is confined to writeLoop, and this
// caller returns ctx.Err() instead of wedging (and so never blocks other callers).
func (c *Client) write(ctx context.Context, req rpcRequest) error {
	done := make(chan error, 1)
	select {
	case c.writeCh <- writeReq{req: req, done: done}:
	case <-ctx.Done():
		return ctx.Err()
	case <-c.closed:
		return fmt.Errorf("mcp %q: connection closed", c.name)
	}
	select {
	case err := <-done:
		if err != nil {
			return fmt.Errorf("mcp %q: write %s: %w", c.name, req.Method, err)
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-c.closed:
		return fmt.Errorf("mcp %q: connection closed", c.name)
	}
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
	return c.notify(ctx, "notifications/initialized", map[string]any{})
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
		if part.Text != "" { // covers type=="text" and any part that carries text
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
