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

Develops proof-of-concept exploits for a target. It can start from a vulnerability
report, or discover the vulnerabilities itself when there is none. The orchestrator
coordinates 9 subagents.

| subagent | role |
|---|---|
| intake | read and understand a vulnerability report, when one is provided |
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
- `ask`: ask the user a question. In manual and watcher mode it reaches you in the
  TUI and the agent waits for your answer; in automatic mode it is recorded and the
  agent proceeds.
- `resources`: snapshot the sandbox's CPU, memory, and disk load.

Subagents can also use `send_message` and `ask`.

## Tools

Each subagent only sees the tools on its allow-list.

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

A foreground `bash` command moves to the background after 2 minutes and keeps
running; the agent is notified when it finishes, or can `wait` for it. Start
long-running work with `bash_bg` from the start. A background job and the processes
it spawns keep running until the job finishes, it is stopped, or the engagement
ends. Start servers and listeners with `bash_bg`. `Ctrl-b S` stops a job and the
whole process tree it spawned.

## Reverse-shell channels

The `revshell` tool (granted to any agent with `bash`) sets up a named netcat
listener inside the sandbox that catches a shell connecting back from the target,
and keeps it writable so the agent can drive it. Its actions are `open` (start a
listener on a port), `send` (write one command line to the connected shell and read
back what follows), `read` (return recent output), `list` (show every channel, its
state, and who holds write access), `close` (stop the listener), and the
write-arbitration actions below. Each channel is backed by an input FIFO and an
output log in the engagement workspace (`<workspace>/channels/<name>/`), and its
listener runs as a persistent job that survives the opening agent's turns until it is
closed or the engagement ends.

A channel can be shared by several subagents, but only ONE may `send` to it at a
time: each channel has a single write token. The opener holds it first, an unheld
channel is auto-claimed by the first sender, and a subagent can `acquire` a free
channel or `release` its own token. The **orchestrator (Main) is the arbiter**: only
Main can `grant` the token to a named subagent, so it decides who drives a shared
channel and when. Concurrent sends can never interleave bytes on the wire regardless,
because each channel serializes its writes.

Every open channel shows up in the operator's **command panes** list, where you can
open a pane to watch it and type to its shell yourself (see [the interface](tui.md));
the operator's own typing is a human override and does not take the agents' write
token. All of it stays inside the sandbox.

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
