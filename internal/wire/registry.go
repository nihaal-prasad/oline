package wire

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/nihaal-prasad/oline/internal/agents"
	"github.com/nihaal-prasad/oline/internal/config"
	"github.com/nihaal-prasad/oline/internal/engine"
	"github.com/nihaal-prasad/oline/internal/tools"
	"github.com/nihaal-prasad/oline/internal/tools/custom"
)

// BuildRegistry registers every tool oline implements, plus the user's custom tools:
// the config-declared command tools (no recompile) and any Go tools added via the
// tools/custom extension point. Per-agent scoping decides which each agent sees (an
// agent uses a tool by listing its name in its `tools:` frontmatter). This is the one
// place the tool registry is built, shared by the TUI and `-print`.
func BuildRegistry(store *config.Store) *tools.Registry {
	reg := tools.NewRegistry()
	reg.Register(tools.Bash{})
	reg.Register(tools.Read{})
	reg.Register(tools.Write{})
	reg.Register(tools.Edit{})
	reg.Register(tools.Find{})
	reg.Register(tools.Grep{})
	reg.Register(tools.Glob{})
	reg.Register(tools.WebSearch{})
	reg.Register(tools.WebFetch{})
	for _, t := range configCommandTools(store) {
		reg.Register(t)
	}
	for _, t := range custom.All() { // Go extension tools (see internal/tools/custom)
		reg.Register(t)
	}
	return reg
}

// configCommandTools turns the config's `tools:` command-tool specs into runnable
// tools. A spec missing a name or command is skipped (the lint flags it).
func configCommandTools(store *config.Store) []tools.Tool {
	var out []tools.Tool
	for _, spec := range store.Effective().Tools {
		if strings.TrimSpace(spec.Name) == "" || strings.TrimSpace(spec.Command) == "" {
			continue
		}
		var params []byte
		if len(spec.Parameters) > 0 {
			params, _ = json.Marshal(spec.Parameters)
		}
		out = append(out, tools.CommandTool{
			ToolName:    spec.Name,
			Desc:        spec.Description,
			Params:      params,
			CommandTmpl: spec.Command,
			Dir:         spec.Dir,
		})
	}
	return out
}

// CustomToolNames lists every custom tool's name (config command tools + Go
// extension tools), so watcher mode can block them as execution tools.
func CustomToolNames(store *config.Store) map[string]bool {
	names := map[string]bool{}
	for _, t := range configCommandTools(store) {
		names[t.Name()] = true
	}
	for _, t := range custom.All() {
		names[t.Name()] = true
	}
	return names
}

// Lint checks the roster and config for mistakes that otherwise fail silently or
// surprise the operator mid-run, and returns human-readable warnings (empty if all
// is well). It is shown by `-print` and at engagement start.
func Lint(store *config.Store, roster *agents.Roster, reg *tools.Registry) []string {
	known := map[string]bool{}
	for _, n := range reg.Names() {
		known[n] = true
	}
	for _, n := range engine.PrimitiveNames() {
		known[n] = true
	}
	agentSet := map[string]bool{}
	for _, n := range roster.Names() {
		agentSet[strings.ToLower(n)] = true
	}

	var warns []string
	for _, n := range roster.Names() {
		d := roster.Agents[n]
		if t := strings.TrimSpace(d.Tier); t != "" && t != "big" && t != "medium" && t != "small" {
			warns = append(warns, fmt.Sprintf("agent %q: unknown tier %q (want big, medium, or small)", n, t))
		}
		if strings.TrimSpace(d.Description) == "" {
			warns = append(warns, fmt.Sprintf("agent %q: no description — the orchestrator won't know when to use it", n))
		}
		for _, tl := range d.Tools {
			if !known[tl] {
				warns = append(warns, fmt.Sprintf("agent %q: tool %q is not registered and will be ignored", n, tl))
			}
		}
		for _, sp := range d.Spawns {
			if !agentSet[strings.ToLower(sp)] {
				warns = append(warns, fmt.Sprintf("agent %q: spawns %q, which is not a known agent", n, sp))
			}
		}
	}

	seen := map[string]bool{}
	for _, ct := range store.Effective().Tools {
		name := strings.TrimSpace(ct.Name)
		if name == "" {
			warns = append(warns, "config tool: an entry under `tools:` has no name")
			continue
		}
		if strings.TrimSpace(ct.Command) == "" {
			warns = append(warns, fmt.Sprintf("config tool %q: no command, so it will be ignored", name))
		}
		if seen[name] {
			warns = append(warns, fmt.Sprintf("config tool %q: defined more than once", name))
		}
		seen[name] = true
	}
	return warns
}
