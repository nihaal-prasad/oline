# Agents, modes, and tools

oline runs a fleet of AI agents. One is the orchestrator (Main). It does not run
offensive tools itself; it delegates to specialized subagents, collects their
results, and steers them.

## The two modes

### Pentester

A penetration test. The orchestrator coordinates 17 subagents plus a privileged
installer.

| subagent | role |
|---|---|
| recon | discover hosts, ports, and services |
| researcher | look up CVEs and public information (internet access) |
| web-exploiter | web application attacks |
| binary-exploiter | memory-corruption and binary attacks |
| domain-exploiter | Active Directory / domain attacks |
| db-exploiter | database attacks |
| ai-exploiter | attacks against AI/LLM components |
| enumerator | deeper enumeration of a foothold |
| privesc | privilege escalation |
| pivot | move through the network |
| implanter | establish access on a host |
| evader | work around defenses |
| cracker | password and hash cracking |
| programmer | write custom code and exploits |
| thinker | reason through hard problems |
| reporter | write the engagement report |
| cleanup | revert artifacts left on target hosts |
| installer | install tooling in the sandbox (runs as root) |

### Exploiter

Develops proof-of-concept exploits for a target. It discovers the vulnerabilities
itself (there is no pre-supplied report), then weaponizes them. The orchestrator
coordinates 8 subagents.

The "scope" here is the application to analyze: a local path, or a URL/name for the
installer to fetch. The target runs locally, so agents are network-isolated and
never attack a remote host; only the installer (download) and judge/skeptic
(research) reach the internet.

| subagent | role |
|---|---|
| installer | install the target and tooling (runs as root) |
| analyst | analyze the target and the bug |
| exploiter | build the exploit |
| fuzzer | fuzz for crashes and inputs |
| judge | assess the exploit (internet access) |
| skeptic | challenge the findings (internet access) |
| summarizer | summarize progress |
| reporter | write the final report |

## Orchestration

`maxConcurrency` (default 5) caps how many agents run at once, the orchestrator
included, so at the default up to 4 subagents run alongside it. Spawning past the
cap queues until a subagent finishes.

The orchestrator uses these coordination tools:

- `task`: start a subagent of a given role.
- `wait`: wait for a subagent or background job. An arriving message interrupts the
  wait.
- `reprompt`: give a finished or idle subagent a new instruction, keeping its
  context.
- `inspect`: view a live subagent's state, recent history, and latest thoughts.
- `send_message`: steer a peer or the orchestrator.
- `ask`: ask the user a question. In planner and watcher mode it reaches you in the
  TUI and the agent waits for your answer; in automatic mode it is recorded and the
  agent proceeds.
- `resources`: snapshot the sandbox's CPU, memory, and disk load.

Subagents can also use `send_message` and `ask`.

## Tools

Each subagent only sees the tools on its allow-list. You can add your own agents and
tools via config; see [extending.md](extending.md).

Sandbox and file tools (run inside the sandbox):

| tool | purpose |
|---|---|
| `bash` | run a shell command |
| `bash_bg` | run a shell command as a background job (granted with `bash`) |
| `revshell` | set up and drive a reverse-shell listener channel (granted with `bash`) |
| `read` `write` `edit` | read and change files in the workspace |
| `find` `grep` `glob` | search files in the workspace |
| `web_search` | search the web (the internet-access grant) |
| `web_fetch` | fetch and read one URL as text |

`web_search` and `web_fetch` run from the host, outside the sandbox, and cannot
reach the target. They are granted separately in an agent's frontmatter.

Coordination tools (run in the engine): `task`, `wait`, `reprompt`, `inspect`,
`send_message`, `ask`, `runtime`, `record_finding`, `update_summary` and
`update_diagram` (orchestrator only), and `heartbeat` (exploiter). `record_finding`
logs a confirmed finding for the report; every agent has it. `update_summary` sets
the Summary pane and `update_diagram` sets the Diagram pane. `runtime` reports the
agent's remaining time budget.

`write` and `edit` can only write inside the engagement workspace
(`/opt/oline/engagements/<name>`): a relative path is taken from that workspace, and a
write anywhere else on the guest is refused. The `installer`, which runs as root to
install tooling, is the one exception.

File writes are serialized per file: two subagents cannot write the same file at
once, and the second gets an error naming the one that holds it.

## Running commands

A foreground `bash` command auto-backgrounds after 2 minutes and keeps running; the
agent is notified when it finishes, or can `wait`. Use `bash_bg` for long-running
work (scans, servers, listeners) from the start: it and its child processes run until
the job finishes, is stopped, or the engagement ends. `Ctrl-b S` stops a job and its
whole process tree.

## Reverse-shell channels

The `revshell` tool (granted with `bash`) runs a named netcat listener in the sandbox
that catches a reverse shell and stays writable so the agent can drive it. Actions:
`open` (start a listener on a port), `send` (write a command line and read what
follows), `read` (recent output), `list` (channels, state, and who holds write
access), `close`, and the write-arbitration actions below. Each channel is backed by
an input FIFO and output log under `<workspace>/channels/<name>/`, and its listener
persists across the opening agent's turns until closed or the engagement ends.

A channel can be shared, but only ONE agent may `send` at a time via a single write
token: the opener holds it first, an unheld channel is auto-claimed by the first
sender, and an agent can `acquire` a free channel or `release` its own. The
**orchestrator (Main) is the arbiter**: only Main can `grant` the token to a named
subagent. Writes are serialized per channel, so bytes never interleave.

Every open channel appears in the operator's **command panes** list, where you can
watch it and type to its shell yourself (see [the interface](tui.md)); your typing is
a human override and doesn't take the agents' token. It all stays inside the sandbox.

## Model tiers

There are three tiers: `big`, `medium`, and `small`. Every agent runs on one, and
you choose which model backs each tier (see [configuration.md](configuration.md)).
All three must be set before an engagement starts.

- `big`: the orchestrator, exploiters, thinkers, and critiques.
- `medium`: balanced work (for example recon, enumerator, installer, reporter).
- `small`: summarizers, and context compaction.

Set an agent's tier in its frontmatter (`tier:`), in `config.yml`
(`agents.<name>.tier`), or in the editor.

## Context auto-compaction

When an agent's conversation grows past its context-token ceiling, oline compacts
it: older turns are summarized and a recent tail is kept. The ceiling defaults to a
fraction of the model's context window by tier; override it per agent with
`max-context-tokens` in frontmatter, in `config.yml`, or in the editor.

## Time budgets

Each subagent runs under a time budget the engine enforces. Past its budget, the
engine sends three escalating reminders and, if it is still running 5 minutes
later, terminates it and returns its partial output as a `[killed]` result. The
orchestrator is never time-limited. Time spent in `wait` counts against the budget.

Set budgets three ways:

- **Default** for all subagents: `budget.defaultMinutes` in `config.yml` (0 = no
  default).
- **Per agent**: `time-budget: <minutes>` (or `none`) in frontmatter, or
  `agents.<name>.time-budget` in `config.yml`.
- **Live**: focus an agent, press `Ctrl-b b`, and type minutes or `none`.
