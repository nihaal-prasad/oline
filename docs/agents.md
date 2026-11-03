# Agents, modes, and tools

oline runs a fleet of AI agents. One agent is the orchestrator (called Main). It
does not run offensive tools itself; it delegates to specialized subagents,
collects their results, and steers them. The orchestrator and the subagents all
use the same engine.

## The two modes

### Pentester

A live, authorized penetration test. The orchestrator coordinates 17 specialized
subagents, plus a privileged installer agent that sets up tooling.

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

Turns a vulnerability report into working proof-of-concept exploits. The
orchestrator coordinates 9 subagents.

| subagent | role |
|---|---|
| intake | read and understand the vulnerability report |
| installer | install the target and tooling (runs as root) |
| analyst | analyze the target and the bug |
| exploiter | build the exploit |
| fuzzer | fuzz for crashes and inputs |
| judge | assess the exploit (internet access) |
| skeptic | challenge the findings (internet access) |
| summarizer | summarize progress |
| reporter | write the final report |

## Messaging a busy agent

A message you (or a peer) send reaches an agent's mailbox and wakes it: an agent
sleeping in `wait` or `heartbeat`, blocked on a running foreground command, or in
the middle of thinking (streaming a reply) is interrupted so it reads the message
right away rather than only at its next step. It is told it was woken early. If it
was thinking, that reply is cut short and the turn is re-issued with your message in
context; a foreground command it was running is moved to the background (it keeps
running) so the agent is free to respond.

## How orchestration works

Each agent is one goroutine with its own mailbox. A central router delivers
messages between agents and enforces a hard cap on how many agents run at once
(`maxConcurrency`, default 5). The cap counts the orchestrator too, so it reserves
one slot for itself and the subagents share the rest: at the default of 5, up to
4 subagents run concurrently alongside the orchestrator. Spawning past the cap
queues until a running subagent finishes.

The orchestrator drives the engagement with these tools:

- `task` - start a new subagent of a given role.
- `wait` - wait for a subagent or background job to return. A message that arrives
  while waiting interrupts the wait and hands control back, so the orchestrator
  stays responsive to you and to peers.
- `reprompt` - give a finished or idle subagent a new instruction while keeping
  its context. A subagent stays idle and registered after it finishes (releasing
  its concurrency slot), so it can be reprompted; if it is truly gone, a fresh one
  of its role starts.
- `inspect` - view any live subagent's current state, recent conversation history,
  and latest thoughts at any time, by id. Read-only, so the orchestrator can check
  on progress without waiting for the subagent to report.
- `send_message` - steer a peer or the orchestrator.
- `ask` - ask the user a question. In manual mode this reaches you in the TUI; in
  automatic mode it is recorded and the agent proceeds.
- `resources` - snapshot the sandbox's current load (CPU cores and load average,
  memory, workspace disk, and a per-agent-class breakdown of who is using what).
  Read-only. The orchestrator uses it to keep subagents from overloading the shared
  sandbox: it checks load before fanning out heavy subagents, staggers or holds off
  spawns when the box is saturated, and messages the heaviest subagents to ease off.

Subagents can message peers and the orchestrator and ask the user through the same
`send_message` and `ask` tools.

## Tools

Each subagent only sees the tools on its allow-list. The model request only
advertises those tools, and the engine re-checks the allow-list before running an
exec tool, so an agent cannot use a tool it was not granted.

Sandbox and file tools (run inside the sandbox, never on the host):

| tool | purpose |
|---|---|
| `bash` | run a shell command in the sandbox |
| `bash_bg` | run a shell command as a background job (granted with `bash`) |
| `read` `write` `edit` | read and change files in the workspace |
| `find` `grep` `glob` | search files in the workspace |
| `web_search` | search the web (this is the internet-access grant) |
| `web_fetch` | fetch and read one URL as text |

Research agents are granted both `web_search` (to find URLs) and `web_fetch` (to
read a specific page, such as an advisory or CVE entry) explicitly in their
frontmatter; unlike `bash`/`bash_bg`, the two are not auto-paired, so an agent can
have one without the other (the `installer`, for instance, has `web_search` only).
They use `web_fetch` instead of shelling out to curl or wget. Both run from the
host, outside the sandbox, and can never reach the engagement target. Agents whose
job is pure
research (for example the pentester `researcher`) have no `bash` at all.

Coordination tools (run in the engine, not the sandbox): `task`, `wait`,
`reprompt`, `inspect`, `send_message`, `ask`, `runtime`, `record_finding`,
`update_summary` and `update_diagram` (orchestrator only), and (exploiter)
`heartbeat`. `record_finding` logs a confirmed finding for the report (it is
surfaced in the recording agent's transcript); every agent has it. `update_summary`
sets the orchestrator's live Summary pane, and `update_diagram` sets its live
Diagram pane (an ASCII network/software map). The orchestrator is told the Diagram
pane's exact current width at the start, and `update_diagram` enforces it: a map with
any line wider than the pane is rejected (the panel is unchanged) with a message
naming the over-wide lines and the current limit, so the art is never silently
clipped. The width is read live on each call, so resizing the pane changes the limit
immediately and the tool reports the current width on every success and rejection.
`runtime`
reports how long the agent has been running and how much of
its time budget is left; agents are told to rely only on it (or oline's reminder
messages) to judge time. `heartbeat` is a bounded sleep the exploiter orchestrator
uses to pace its multi-pass loop.

File writes are serialized per file across agents: `write` and `edit` take a lock
on the target path for the duration of the write, so two subagents cannot write the
same file at once. The second one gets an error naming the agent that holds it, and
can retry.

## Running commands

A foreground `bash` command auto-backgrounds after 2 minutes. Instead of hanging
the agent on a long scan or a service that does not exit, the command keeps
running as a background job and the agent is freed to continue. It is notified
when the job finishes, or it can `wait` for it. Long work can also be started in
the background from the start with `bash_bg`.

Each background command runs as a single long-lived sandbox exec tied to the
engagement's lifetime (not a detached/setsid process), so the command and every
process it spawns (for example a server thread's subprocess) keep running across
the agent's turns until the command finishes on its own, it is stopped, or the
engagement ends. It is not killed when the launching call returns or the agent's
turn ends. Start servers and listeners with `bash_bg` rather than backgrounding
them with a trailing `&` inside a `bash` command. Stop a job explicitly with the
stop control (`Ctrl-b S`), which cancels that exec and kills the command and the
whole process tree it spawned.

## Time budgets

Each subagent runs under a time budget that the engine enforces, not the
orchestrator or any AI. When a subagent runs past its budget, the engine sends it
three escalating "wrap up" reminders. If it is still running 5 minutes after the
third, the engine terminates it and returns its partial output to the orchestrator
as a `[killed]` result. The orchestrator is never time-limited.

Time a subagent spends blocked in `wait` (or any sleep) counts against its budget,
so subagents are told to wait only as long as they actually need and to check how
much time they have left with the `runtime` tool (or oline's reminder messages)
rather than guessing. A subagent that finishes or times out is not a dead end: the
orchestrator can `reprompt` it (keeping its context) or spawn a fresh one to
continue, so subagents are told to return partial progress and next steps (and
always a brief summary) rather than overrun.

There is no turn cap. An agent keeps going until it finishes, is killed, the
engagement ends, or its time budget runs out. Time and lifecycle are the only
limits, not a count of tool-call turns.

## Model tiers

There are three model tiers: `big`, `medium`, and `small`. Every agent runs on one
of them, and you choose which model backs each tier (see
[configuration.md](configuration.md)). There are no defaults: you must set all three
the first time you run oline, and an engagement will not start until they are set to
a usable model.

- `big` is the strongest tier: the orchestrator, exploiters, thinkers, and critiques
  (for example `analyst`, `exploiter`, `judge`, `skeptic`, `binary-exploiter`,
  `web-exploiter`, `thinker`).
- `medium` is for balanced, in-between work (for example `recon`, `enumerator`,
  `installer`, `reporter`, `intake`).
- `small` is the fastest and cheapest: summarizers, and it is also what context
  compaction uses.

Each agent's tier is set in its frontmatter (`tier:`) and can be overridden in
`config.yml` (`agents.<name>.tier`) or the editor. The orchestrator is told which
agents are on which tier; if a lower-tier agent keeps struggling with something that
needs stronger reasoning, the orchestrator hands that piece to a higher-tier agent
rather than retrying the weaker one.

## Context limits and auto-compaction

Each agent has a context-token ceiling. When its conversation grows past that
ceiling, the engine auto-compacts it: the older turns are summarized into one
message and a recent tail is kept verbatim, so the agent can keep working without
overflowing the model's context window.

The ceiling defaults to a fraction of the model's context window, scaled by tier:
a big-tier agent keeps more of its window before compacting, a small-tier agent
compacts sooner, and medium sits between. Override it per agent with a
`max-context-tokens: <n>` line in the agent's frontmatter, an
`agents.<name>.max-context-tokens` entry in `config.yml`, or the max-context-tokens
field in the in-app editor (`Ctrl-b e`).

Set budgets three ways:

- **Default** for all subagents: `budget.defaultMinutes` in `config.yml`
  (0 means no default limit).
- **Per agent**: a `time-budget: <minutes>` line (or `none`) in the agent's
  frontmatter, or an `agents.<name>.time-budget` override in `config.yml`.
- **Live**: focus an agent, press `Ctrl-b` then `b`, and type minutes or `none`.
  A live change counts from the moment you make it (the new budget is "N minutes
  from now"), and the roster's countdown updates to it immediately.

## Customizing agents

Each agent's defaults (model tier, thinking level, tools, which roles it can spawn,
time budget, max context tokens) live in its prompt file's frontmatter under
`assets/<mode>/agents/<name>.md`. Override any of these per agent in
`config.yml`, or from the in-app editor. See [configuration.md](configuration.md).

Prompts and config resolve in layers: a file you place at
`~/oline/config/<mode>/...` overrides the embedded default, so you can change any
prompt or setting without rebuilding.
