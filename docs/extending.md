# Extending oline: custom agents and tools

You can add your own subagents and tools without touching the core code. Agents are
pure config; tools can be pure config (a shell command) or Go (for richer logic).

## Custom agents (config files, no rebuild)

Drop a Markdown file at `~/oline/config/<mode>/agents/<name>.md` (`<mode>` is
`pentester` or `exploiter`). oline discovers it at startup and adds it as a spawnable
subagent. The file is frontmatter plus a system prompt body, exactly like the
built-in agents:

```markdown
---
name: screenshotter
description: Captures a screenshot of an in-scope web page and saves it to the workspace.
tier: medium
thinking-level: low
tools: bash, read, write
---

You are the SCREENSHOTTER subagent in an authorized engagement. Capture the page you
are asked to, save it under the workspace, and report the path. (the rest of the
system prompt)
```

Frontmatter keys:

- `name`: the agent id the orchestrator spawns.
- `description`: one line shown to the orchestrator so it knows when to use the
  agent. Give a good one, or the orchestrator will not know what your agent is for.
- `tier`: `big`, `medium`, or `small` (which model it runs on).
- `thinking-level`: `low`, `medium`, or `high`.
- `tools`: comma-separated tool names the agent may call (built-in or custom).
- `spawns`: roles this agent may spawn (optional).
- `time-budget`, `max-context-tokens`: optional limits.

The orchestrator sees the new agent in its roster (name and description) and can
`task` it. In enforced mode it shows up on the Subagents tab like any other. Edit it
later with `-edit` or `Ctrl-b e`.

## Custom tools

A tool is a capability an agent calls. Grant a tool to an agent by listing its name
in that agent's `tools:` frontmatter (a built-in agent or your own). There are two
ways to add one.

### Command tools (config files, no rebuild)

Declare a tool in `~/oline/config/<mode>/config.yml` under `tools:`. Each runs a
shell command inside the sandbox; a `{{arg}}` placeholder is replaced with the
shell-quoted value of that argument (so an argument value can never break out of its
quotes into the command).

```yaml
tools:
  - name: nmap_quick
    description: Fast top-1000 TCP port scan of an in-scope host.
    parameters:
      type: object
      properties:
        host:
          type: string
          description: the target host or IP
      required: [host]
    command: nmap -T4 --top-ports 1000 {{host}}
```

The command runs as the calling agent's user and egress policy (so an in-scope agent
can reach the target), inside the sandbox, with output streamed like `bash`. Install
whatever the command needs in the sandbox first (the `installer` agent can). Then
grant it: an agent whose frontmatter lists `nmap_quick` under `tools:` can call it.
`parameters` is a JSON Schema object written as YAML; omit it for a tool that takes
no arguments.

### Go tools (extension point, rebuild)

For logic beyond a single shell command, implement the `tools.Tool` interface in the
`internal/tools/custom` package and register it, then rebuild oline:

```go
package custom

import (
	"context"
	"encoding/json"

	"github.com/nihaal-prasad/oline/internal/tools"
)

type reverse struct{}

func (reverse) Name() string { return "reverse" }

func (reverse) Schema() tools.Schema {
	return tools.Schema{
		Name:        "reverse",
		Description: "Reverse a string.",
		Parameters:  json.RawMessage(`{"type":"object","properties":{"s":{"type":"string"}},"required":["s"]}`),
	}
}

func (reverse) Execute(ctx context.Context, c tools.Call) (tools.Result, error) {
	var a struct{ S string `json:"s"` }
	_ = json.Unmarshal(c.Args, &a)
	r := []rune(a.S)
	for i, j := 0, len(r)-1; i < j; i, j = i+1, j-1 {
		r[i], r[j] = r[j], r[i]
	}
	return tools.Result{Content: string(r)}, nil
}

func init() { Register(reverse{}) }
```

`Execute` receives the calling agent, the JSON arguments, the engagement workspace,
the sandbox handle, and the agent's egress policy. Run any command work through
`c.Sandbox` so it stays inside the sandbox, exactly like the built-in tools. Rebuild
(`make` or `pwsh ./build.ps1`), then grant the tool to agents by name.

## Notes

- **Watcher mode** executes nothing, so custom tools (command and Go) are blocked
  there alongside `bash`, just like any other execution.
- A custom tool's name must be unique; a custom tool with a built-in's name overrides
  the built-in.
- Custom tools run inside the sandbox under the calling agent's egress policy; the
  same scope firewall that confines the built-in tools confines yours.
