package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nihaal-prasad/oline/internal/tools"
)

// blockWriter is a server stdin that blocks every Write until the connection is
// closed — simulating an MCP server that stopped reading its stdin.
type blockWriter struct{ closed chan struct{} }

func (w *blockWriter) Write(p []byte) (int, error) {
	<-w.closed
	return 0, errors.New("closed")
}
func (w *blockWriter) Close() error {
	close(w.closed)
	return nil
}

// TestClientWriteDoesNotHangOnStuckServer verifies the bounded write: if the server
// stops reading its stdin (Encode blocks forever in the writer goroutine), callers
// still return on their own context instead of wedging the whole fleet.
func TestClientWriteDoesNotHangOnStuckServer(t *testing.T) {
	bw := &blockWriter{closed: make(chan struct{})}
	bR, bW := io.Pipe() // server -> client; nothing is ever sent
	defer bW.Close()
	c := newClient("stuck", bw, bR)
	defer c.Close()

	var wg sync.WaitGroup
	for i := 0; i < 3; i++ { // several concurrent callers: none may block the others
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
			defer cancel()
			if _, _, err := c.CallTool(ctx, "echo", nil); err == nil {
				t.Error("expected a context/timeout error from a stuck server write, got nil")
			}
		}()
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("CallTool hung on a stuck server write — bounded-write regression")
	}
}

// TestHelperMCPServer is not a real test: when re-executed as a subprocess (with
// OLINE_MCP_HELPER=1) it behaves as a stdio MCP server, so TestConnectSpawnsProcess
// can exercise the real process spawn/handshake/reap path.
func TestHelperMCPServer(t *testing.T) {
	if os.Getenv("OLINE_MCP_HELPER") != "1" {
		return
	}
	fakeServer(os.Stdin, os.Stdout, []map[string]any{
		{"name": "ping", "description": "ping", "inputSchema": map[string]any{"type": "object"}},
	})
	os.Exit(0)
}

func TestConnectSpawnsProcess(t *testing.T) {
	c, err := Connect(context.Background(), ServerConfig{
		Name:    "h",
		Command: os.Args[0],
		Args:    []string{"-test.run=TestHelperMCPServer"},
		Env:     map[string]string{"OLINE_MCP_HELPER": "1"},
	})
	if err != nil {
		t.Fatalf("Connect to a spawned stdio server: %v", err)
	}
	defer c.Close()
	if len(c.Tools()) != 1 || c.Tools()[0].Name != "ping" {
		t.Fatalf("discovered tools = %+v, want [ping]", c.Tools())
	}
	out, isErr, err := c.CallTool(context.Background(), "ping", nil)
	if err != nil || isErr || out != "called ping" {
		t.Errorf("CallTool over a real process = %q isErr=%v err=%v", out, isErr, err)
	}
}

// fakeServer plays a minimal MCP server over the pipes: it answers initialize,
// tools/list, and tools/call, and ignores the initialized notification.
func fakeServer(in io.Reader, out io.Writer, toolList []map[string]any) {
	br := bufio.NewReader(in)
	enc := json.NewEncoder(out)
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			var req map[string]any
			if json.Unmarshal(line, &req) == nil {
				method, _ := req["method"].(string)
				id := req["id"]
				switch method {
				case "initialize":
					_ = enc.Encode(map[string]any{"jsonrpc": "2.0", "id": id, "result": map[string]any{
						"protocolVersion": protocolVersion,
						"capabilities":    map[string]any{},
						"serverInfo":      map[string]any{"name": "fake", "version": "1"},
					}})
				case "notifications/initialized":
					// notification: no response
				case "tools/list":
					_ = enc.Encode(map[string]any{"jsonrpc": "2.0", "id": id, "result": map[string]any{"tools": toolList}})
				case "tools/call":
					params, _ := req["params"].(map[string]any)
					name, _ := params["name"].(string)
					_ = enc.Encode(map[string]any{"jsonrpc": "2.0", "id": id, "result": map[string]any{
						"content": []map[string]any{{"type": "text", "text": "called " + name}},
						"isError": false,
					}})
				}
			}
		}
		if err != nil {
			return
		}
	}
}

// newTestClient wires a client to an in-memory fake server and runs the handshake.
func newTestClient(t *testing.T, toolList []map[string]any) *Client {
	t.Helper()
	aR, aW := io.Pipe() // client -> server
	bR, bW := io.Pipe() // server -> client
	go fakeServer(aR, bW, toolList)
	c := newClient("srv", aW, bR)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.initialize(ctx); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	if err := c.listTools(ctx); err != nil {
		t.Fatalf("listTools: %v", err)
	}
	return c
}

func TestClientHandshakeListAndCall(t *testing.T) {
	c := newTestClient(t, []map[string]any{
		{"name": "echo", "description": "echoes input", "inputSchema": map[string]any{"type": "object"}},
		{"name": "add", "description": "adds numbers", "inputSchema": map[string]any{"type": "object"}},
	})
	defer c.Close()

	if got := len(c.Tools()); got != 2 {
		t.Fatalf("expected 2 tools discovered, got %d", got)
	}
	out, isErr, err := c.CallTool(context.Background(), "echo", json.RawMessage(`{"x":1}`))
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if isErr || out != "called echo" {
		t.Errorf("CallTool result = %q isErr=%v, want \"called echo\"", out, isErr)
	}
}

func TestClientCallAfterCloseFails(t *testing.T) {
	c := newTestClient(t, nil)
	c.Close()
	if _, _, err := c.CallTool(context.Background(), "echo", nil); err == nil {
		t.Error("a call on a closed client should fail, not hang or panic")
	}
}

func TestToolAdapter(t *testing.T) {
	c := newTestClient(t, []map[string]any{
		{"name": "echo", "description": "echoes", "inputSchema": map[string]any{"type": "object"}},
	})
	defer c.Close()
	tool := Tool{client: c, server: "srv", toolName: "echo", fqName: ToolName("srv", "echo"), params: json.RawMessage(`{"type":"object"}`)}
	if tool.Name() != "srv__echo" {
		t.Errorf("tool name = %q, want srv__echo", tool.Name())
	}
	if sc := tool.Schema(); !strings.Contains(sc.Description, "outside the sandbox") {
		t.Errorf("schema description should flag the host-side nature, got %q", sc.Description)
	}
	res, err := tool.Execute(context.Background(), tools.Call{Args: json.RawMessage(`{}`)})
	if err != nil || res.IsError || !strings.Contains(res.Content, "called echo") {
		t.Errorf("Execute = %+v err=%v, want the server's content", res, err)
	}
}

func TestToolNameSanitizes(t *testing.T) {
	if got := ToolName("ji ra", "create/issue"); got != "ji_ra__create_issue" {
		t.Errorf("ToolName sanitization = %q, want ji_ra__create_issue", got)
	}
}

func TestFilterTools(t *testing.T) {
	in := []ToolDesc{{Name: "a"}, {Name: "b"}, {Name: "c"}}
	got := filterTools(in, []string{"a", "c"})
	if len(got) != 2 || got[0].Name != "a" || got[1].Name != "c" {
		t.Errorf("filterTools = %+v, want a and c", got)
	}
}
