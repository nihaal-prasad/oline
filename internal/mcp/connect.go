package mcp

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	defaultConnectTimeout = 30 * time.Second
	// DefaultCallTimeout bounds a single tools/call when the server config sets none.
	DefaultCallTimeout = 120 * time.Second
)

// ServerConfig describes one MCP server oline should spawn and connect to.
type ServerConfig struct {
	Name       string            // namespace for the server's tools (e.g. "jira")
	Command    string            // the executable to run (stdio transport)
	Args       []string          // its arguments
	Env        map[string]string // extra environment (merged over the host environment)
	Dir        string            // working directory ("" => inherit)
	AllowTools []string          // optional allow-list: only expose these tool names
	Timeout    time.Duration     // per-call timeout (0 => DefaultCallTimeout)
}

// ringBuffer keeps only the last maxBytes written, for capturing a failed server's
// most recent stderr without unbounded growth.
type ringBuffer struct {
	mu       sync.Mutex
	buf      []byte
	maxBytes int
}

func (b *ringBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf = append(b.buf, p...)
	if b.maxBytes > 0 && len(b.buf) > b.maxBytes {
		b.buf = b.buf[len(b.buf)-b.maxBytes:]
	}
	return len(p), nil
}

func (b *ringBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return strings.TrimSpace(string(b.buf))
}

// Connect spawns the server process (stdio transport), performs the MCP handshake,
// and discovers its tools. On any failure it tears the process down and returns an
// error that includes the server's most recent stderr for diagnosis.
func Connect(ctx context.Context, cfg ServerConfig) (*Client, error) {
	if strings.TrimSpace(cfg.Command) == "" {
		return nil, fmt.Errorf("mcp %q: no command", cfg.Name)
	}
	cmd := exec.Command(cfg.Command, cfg.Args...)
	cmd.Dir = cfg.Dir
	cmd.Env = append(os.Environ(), envList(cfg.Env)...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("mcp %q: stdin: %w", cfg.Name, err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("mcp %q: stdout: %w", cfg.Name, err)
	}
	errBuf := &ringBuffer{maxBytes: 4096}
	cmd.Stderr = errBuf
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("mcp %q: start %q: %w", cfg.Name, cfg.Command, err)
	}

	c := newClient(cfg.Name, stdin, stdout)
	c.onClose = func() { _ = cmd.Process.Kill() }
	// Reap the process once the reader has finished (Wait must follow all pipe reads).
	go func() {
		<-c.closed
		_ = cmd.Process.Kill()
		<-c.readDone
		_ = cmd.Wait()
	}()

	ictx, cancel := context.WithTimeout(ctx, defaultConnectTimeout)
	defer cancel()
	if err := c.initialize(ictx); err != nil {
		_ = c.Close()
		return nil, fmt.Errorf("mcp %q: initialize: %w%s", cfg.Name, err, stderrSuffix(errBuf))
	}
	if err := c.listTools(ictx); err != nil {
		_ = c.Close()
		return nil, fmt.Errorf("mcp %q: tools/list: %w%s", cfg.Name, err, stderrSuffix(errBuf))
	}
	if len(cfg.AllowTools) > 0 {
		c.tools = filterTools(c.tools, cfg.AllowTools)
	}
	return c, nil
}

func stderrSuffix(b *ringBuffer) string {
	if s := b.String(); s != "" {
		return " (server stderr: " + s + ")"
	}
	return ""
}

func envList(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k, v := range m {
		out = append(out, k+"="+v)
	}
	sort.Strings(out)
	return out
}

func filterTools(tools []ToolDesc, allow []string) []ToolDesc {
	keep := map[string]bool{}
	for _, a := range allow {
		keep[a] = true
	}
	out := tools[:0]
	for _, t := range tools {
		if keep[t.Name] {
			out = append(out, t)
		}
	}
	return out
}
