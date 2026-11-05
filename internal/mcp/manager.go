package mcp

import (
	"context"
	"sync"

	"github.com/nihaal-prasad/oline/internal/tools"
)

// Manager owns the MCP server connections for an engagement. Close it at engagement
// end to terminate the server processes.
type Manager struct {
	mu      sync.Mutex
	clients []*Client
}

// ServerInfo describes a connected server for the operator-facing startup report.
type ServerInfo struct {
	Name  string
	Tools []string // oline tool names (<server>__<tool>)
}

// ConnectAll connects every configured server. It returns the tool adapters to
// register with oline's registry, a Manager that owns the connections, and a warning
// per server that could not be connected. A connection failure is NON-FATAL: that
// server's tools are simply unavailable and the engagement proceeds.
func ConnectAll(ctx context.Context, cfgs []ServerConfig) (*Manager, []tools.Tool, []string) {
	m := &Manager{}
	var out []tools.Tool
	var warns []string
	seen := map[string]bool{}
	for _, cfg := range cfgs {
		if cfg.Name == "" || cfg.Command == "" {
			continue // the config lint reports these
		}
		if seen[cfg.Name] {
			warns = append(warns, "mcp: server "+cfg.Name+" is defined more than once; the duplicate was skipped")
			continue
		}
		seen[cfg.Name] = true
		c, err := Connect(ctx, cfg)
		if err != nil {
			warns = append(warns, err.Error())
			continue
		}
		m.clients = append(m.clients, c)
		for _, td := range c.Tools() {
			out = append(out, Tool{
				client:   c,
				server:   cfg.Name,
				toolName: td.Name,
				fqName:   ToolName(cfg.Name, td.Name),
				desc:     td.Description,
				params:   td.InputSchema,
				timeout:  cfg.Timeout,
			})
		}
	}
	return m, out, warns
}

// ToolNames returns the oline tool names of every connected MCP tool, so the harness
// can mark them as execution tools (blocked in watcher mode) and validate grants.
func (m *Manager) ToolNames() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var names []string
	for _, c := range m.clients {
		for _, td := range c.Tools() {
			names = append(names, ToolName(c.name, td.Name))
		}
	}
	return names
}

// Servers returns a summary of each connected server and its tool names, for the
// operator-facing startup report.
func (m *Manager) Servers() []ServerInfo {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []ServerInfo
	for _, c := range m.clients {
		si := ServerInfo{Name: c.name}
		for _, td := range c.Tools() {
			si.Tools = append(si.Tools, ToolName(c.name, td.Name))
		}
		out = append(out, si)
	}
	return out
}

// Close terminates every connected server process. Safe to call more than once.
func (m *Manager) Close() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, c := range m.clients {
		_ = c.Close()
	}
	m.clients = nil
}
