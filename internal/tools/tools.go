// Package tools defines the tool interface, the registry, and the per-agent
// tool view that enforces each subagent's allow-list. The sandbox-facing tools
// (read, write, edit, find/grep/glob, web_search, web_fetch) are implemented here;
// bash's schema lives here too but its execution is engine-side (doBash), and the
// coordination primitives (task, wait, runtime, …) live in the engine, not here.
package tools

import (
	"context"
	"encoding/json"
	"sort"
	"sync"

	"github.com/nihaal-prasad/oline/internal/core"
	"github.com/nihaal-prasad/oline/internal/sandbox"
)

// FileLocks serializes file writes across agents: at most one agent may hold a
// given file at a time, so two subagents can't write the same file concurrently.
// A conflicting writer is rejected (not queued) with the holder's id, so the
// slower one gets a clear error. Safe for concurrent use.
type FileLocks struct {
	mu   sync.Mutex
	held map[string]core.AgentID
}

// NewFileLocks returns an empty lock set.
func NewFileLocks() *FileLocks { return &FileLocks{held: map[string]core.AgentID{}} }

// Acquire claims path for agent. It returns (agent, true) on success, or
// (holder, false) when a DIFFERENT agent already holds it. Re-acquiring a path
// you already hold succeeds (so edit's read+write is one hold).
func (f *FileLocks) Acquire(path string, agent core.AgentID) (core.AgentID, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if h, ok := f.held[path]; ok && h != agent {
		return h, false
	}
	f.held[path] = agent
	return agent, true
}

// Release frees path if agent holds it (a no-op otherwise).
func (f *FileLocks) Release(path string, agent core.AgentID) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.held[path] == agent {
		delete(f.held, path)
	}
}

// maxToolOutput caps how much output the sandboxed tools retain (bash output,
// file reads), so a runaway command can't exhaust memory. It also marks the
// point past which `edit` refuses to operate, since its read would be truncated.
const maxToolOutput = 1 << 20 // 1 MiB

// Schema is an OpenAI-style function definition advertised to the model.
type Schema struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"` // JSON Schema object
}

// Call is one tool invocation.
type Call struct {
	Agent core.AgentID    // who is calling (drives egress policy + workspace scoping)
	Args  json.RawMessage // raw JSON arguments from the model
	// Workspace is the engagement's guest workspace directory. File-tool relative
	// paths resolve against it, and write/edit are confined to it (the Privileged
	// installer excepted). Empty => the fallback root (tests / headless).
	Workspace string
	Sandbox   sandbox.Handle       // where sandboxed tools execute
	Policy    sandbox.EgressPolicy // the calling agent's egress policy (routes exec to its guest user)
	Emit      func(Result)         // stream partial output (e.g. bash stdout) to the TUI/history
	Locks     *FileLocks           // cross-agent file-write locks (nil => no locking, e.g. in tests)
}

// Result is a tool's output.
type Result struct {
	Content string
	IsError bool
}

// Tool is a single capability exposed to agents.
type Tool interface {
	Name() string
	Schema() Schema
	Execute(ctx context.Context, c Call) (Result, error)
}

// Registry holds every implemented tool. ViewFor intersects it with an agent's
// allow-list so the LLM request only ever advertises the tools that agent may use.
type Registry struct {
	all map[string]Tool
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry { return &Registry{all: map[string]Tool{}} }

// Register adds a tool (last write wins).
func (r *Registry) Register(t Tool) { r.all[t.Name()] = t }

// Get returns a tool by name.
func (r *Registry) Get(name string) (Tool, bool) {
	t, ok := r.all[name]
	return t, ok
}

// Names returns every registered tool name, sorted.
func (r *Registry) Names() []string {
	out := make([]string, 0, len(r.all))
	for n := range r.all {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// ViewFor returns the tools whose names appear in allow, in allow's order,
// skipping any the registry doesn't implement.
func (r *Registry) ViewFor(allow []string) []Tool {
	out := make([]Tool, 0, len(allow))
	for _, name := range allow {
		if t, ok := r.all[name]; ok {
			out = append(out, t)
		}
	}
	return out
}

// Schemas returns the function schemas for a tool view.
func (r *Registry) Schemas(view []Tool) []Schema {
	out := make([]Schema, 0, len(view))
	for _, t := range view {
		out = append(out, t.Schema())
	}
	return out
}
