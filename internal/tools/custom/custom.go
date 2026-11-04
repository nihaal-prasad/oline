// Package custom is the extension point for user-added tools.
//
// A tool is a piece of Go code the agents can call. To add one:
//
//  1. Create a file in this package with a type that implements tools.Tool
//     (Name, Schema, Execute). Execute receives a tools.Call carrying the calling
//     agent, the JSON arguments, the engagement workspace, the sandbox handle, and
//     the agent's egress policy — run work through c.Sandbox so it stays inside the
//     sandbox, exactly like the built-in tools.
//  2. Register it from an init() in that file: `func init() { custom.Register(myTool{}) }`.
//  3. Rebuild oline. The harness adds every registered tool to the tool registry.
//  4. Grant it to agents by listing its Name() in an agent's `tools:` frontmatter
//     (a built-in agent's .md, or your own custom agent file under
//     ~/oline/config/<mode>/agents/). Only agents that list it can call it.
//
// See docs/extending.md for a worked example.
package custom

import "github.com/nihaal-prasad/oline/internal/tools"

// registered holds every user-added tool, in registration order.
var registered []tools.Tool

// Register adds a custom tool so the harness includes it in the tool registry.
// Call it from an init() in your own file in this package. A later registration of
// the same Name() overrides an earlier one (last write wins), matching the registry.
func Register(t tools.Tool) { registered = append(registered, t) }

// All returns every registered custom tool. The harness calls this at startup and
// registers each one; it is also handy in tests.
func All() []tools.Tool {
	out := make([]tools.Tool, len(registered))
	copy(out, registered)
	return out
}
