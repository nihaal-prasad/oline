# Extending oline: custom agents and tools

You can add your own subagents and tools without touching the core code. Agents are
pure config; tools can be pure config (a shell command) or Go (for richer logic).

## Custom agents (config files, no rebuild)

Drop a Markdown file at `~/oline/config/<mode>/agents/<name>.md` (`<mode>` is
`pentester`, `exploiter`, or `programmer`). oline discovers it at startup and adds it as a spawnable
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
in that agent's `tools:` frontmatter (a built-in agent or your own). There are three
ways to add one:

- a **command tool**: a shell command, declared in config, that runs inside the
  sandbox (no rebuild);
- a **Go tool**: the `tools.Tool` interface implemented in the extension package
  (rebuild), for logic beyond a single command;
- an **MCP tool server**: an external [Model Context Protocol](https://modelcontextprotocol.io)
  server, declared in config, whose tools oline connects to and exposes (no rebuild).

The first two run **inside the sandbox** under the calling agent's egress policy. MCP
servers run **on the host**, outside the sandbox (see the trust note below).

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

### MCP tool servers (config, no rebuild)

oline is a [Model Context Protocol](https://modelcontextprotocol.io) **client**: it
can connect to external MCP tool servers and expose their tools to agents. This plugs
oline into the whole MCP ecosystem (filesystem, git, HTTP, ticketing, and your
organization's own internal MCP servers) without writing any code.

Declare servers in `~/oline/config/<mode>/config.yml` under `mcpServers:`. oline
spawns each server at engagement start (stdio transport), runs the MCP handshake,
discovers its tools, and registers each one as an oline tool named
**`<server-name>__<tool-name>`**.

```yaml
mcpServers:
  - name: fs                       # namespace for this server's tools
    command: npx
    args: ["-y", "@modelcontextprotocol/server-filesystem", "/data/engagement"]
  - name: jira
    command: /opt/mcp/jira-server  # a binary, uvx, docker run, …
    env:
      JIRA_BASE_URL: https://example.atlassian.net
      JIRA_TOKEN: ${env:JIRA_TOKEN}   # keep secrets in the environment, not here
    tools: [search_issues, create_issue]   # optional: expose only these tools
    timeoutSeconds: 60                      # optional: per-call timeout
```

Fields: `name` (required, the tool namespace), `command` (required, the server
executable), `args`, `env` (merged over oline's environment), `dir` (working
directory), `tools` (optional allow-list of server-side tool names to expose; omit
for all), and `timeoutSeconds` (per tool call; default 120).

At engagement start, oline prints each connected server and the exact tool names it
exposes (for example `fs__read_file`, `jira__search_issues`) to the orchestrator's
transcript. **Grant a tool to an agent by listing that `<server>__<tool>` name in the
agent's `tools:` frontmatter**: only agents that list it can call it. A server that
fails to connect is reported and skipped; the engagement proceeds without it. The
server processes are shut down when the engagement ends.

> **Trust boundary (read this).** Unlike command and Go tools, an MCP server runs on
> the **host**, outside the sandbox and its per-agent egress firewall (the same place
> `web_search`/`web_fetch` run). A granted MCP tool can therefore reach whatever the
> server can reach (the host filesystem, internal networks, third-party APIs),
> regardless of an agent's scope restrictions. Treat an MCP server as an
> operator-trusted integration: only configure servers you trust, and grant their
> tools only to the agents that genuinely need them. In **watcher mode** MCP tools are
> blocked along with the other execution tools.

Only **stdio** servers are supported in this version (remote servers over Streamable
HTTP are a planned addition). Whole-server grants (listing just `jira` to grant all of
its tools) are planned too; for now, grant the individual `<server>__<tool>` names.

## Notes

- **Watcher mode** executes nothing, so custom tools (command, Go, and MCP) are
  blocked there alongside `bash`, just like any other execution.
- A custom tool's name must be unique; a custom tool with a built-in's name overrides
  the built-in.
- **Command and Go tools** run inside the sandbox under the calling agent's egress
  policy; the same scope firewall that confines the built-in tools confines yours.
- **MCP tools run on the host**, outside the sandbox and the egress firewall, so grant
  them deliberately (see the trust note above). `oline -print` lists configured MCP
  servers but does not connect to them; tool names are discovered at engagement start.
