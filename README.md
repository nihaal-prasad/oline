# O-Line (`oline`)

<p align="center">
  <img src="docs/logo.png" alt="O-Line" width="560">
</p>

**Offensive Line**: a standalone, single-binary Go harness that drives a fleet of
offensive-security AI agents toward an authorized target, inside an isolated sandbox.

Two modes:

- **Pentester**: a live, authorized penetration test: an orchestrator coordinating
  17 specialized subagents (recon, researcher, web/binary/domain/db/ai exploiters,
  enumerator, privesc, pivot, implanter, evader, cracker, programmer, thinker,
  reporter, cleanup).
- **Exploiter**: turns a vulnerability report into working proof-of-concept exploits:
  an orchestrator plus 9 subagents (intake, installer, analyst, exploiter, fuzzer,
  judge, summarizer, skeptic, reporter).

It is a self-contained Go harness with its own agent engine, prompts, personalities,
and per-agent configuration.

> **Authorized use only.** Run this exclusively against systems you own or have
> explicit written permission to test. The orchestrator records scope and keeps all
> execution inside the sandbox.

## Highlights

- **Tmux-style TUI**: a setup wizard, focusable per-agent transcripts, an agent
  roster/hub, a scope/status bar, and a command line. Mouse + prefix keybinds.
- **Native agent engine**: an actor-model fleet (one goroutine + mailbox per agent),
  spawn / wait / steer / kill, a concurrency cap (default 5 agents at once, the orchestrator included), and **immediate pause/resume**
  (cancels the in-flight LLM stream *and* the running sandbox command). The
  orchestrator can **inspect** any subagent's state, history, and thoughts at any time.
- **Resumable engagements**: the conversation is saved to the host; stop the program
  and later reload an engagement's history, or start it from a clean slate.
- **Sandboxed execution**: all agent code runs inside WSL or QEMU. Commands are pinned
  to a WSL-native working directory; the host `/mnt` is never used.
- **Un-bypassable per-agent egress**: each agent class runs as a distinct **no-sudo**
  guest user governed by **root-owned firewall rules it cannot change**. In-scope-only
  agents reach only the target; the internet agents (`researcher` in pentester mode,
  `judge`/`skeptic` in exploiter mode) reach the internet but never the target. The
  `installer` is the one exception: it runs as **root** (privileged and unrestricted)
  so it can set up tooling.
- **Per-agent tool scoping**: each subagent only ever sees its allow-listed tools.
  File writes are serialized per file across agents (the slower of two writers gets
  an error naming the other).
- **Auto-compaction**: each agent has a per-role context-token ceiling (editable);
  when its conversation grows past it, older turns are summarized so it keeps going
  within the model's context window.
- **In-app editor**: edit each agent's model tier and thinking level, the system
  prompt of every agent and of the orchestrator (in a resizable multi-line box), and
  each provider's baseURL and API key, and save — keys go to a git-ignored secrets file.
- **Single cross-compiled binary** for Linux and Windows.

## Documentation

Full docs are in [`docs/`](docs/README.md):

- [Getting started](docs/getting-started.md) — build, API key, running, and flags.
- [The interface](docs/tui.md) — the setup wizard, the running view, and keybindings.
- [Agents, modes, and tools](docs/agents.md) — the rosters, orchestration, tools, and time budgets.
- [Configuration, secrets, backups, and logs](docs/configuration.md) — config files, model settings, backups, and logs.
- [Sandbox and egress](docs/sandbox.md) — the WSL and QEMU backends and the egress model.

## Build

Requires Go 1.27+.

```bash
go build ./cmd/oline      # or: make build
go test ./...             # unit tests (live tests skip without env — see below)
```

Cross-compiled release binaries into `dist/`:

```bash
make dist                 # or: pwsh ./build.ps1
```

## Usage

```bash
oline -mode pentester     # launch the TUI with the setup wizard
oline -mode exploiter
oline -edit               # open the model/provider editor and exit
oline -init-config        # write the mode's default config + prompts to ~/oline/config/<mode>/ to edit
oline -print              # print the resolved roster (no TUI) and exit
oline -version
```

Direct start (skip the wizard — handy for scripting/CI):

```bash
oline -mode pentester -env <wsl-distro> -scope "10.0.0.5, 10.0.0.0/24" -goal "prove RCE" [-manual]
```

### Keybindings (running view)

`Ctrl-b` then (the tmux default prefix):

| key | action |
|---|---|
| `%` / `\|` · `"` / `-` | split the focused pane across / down |
| `←↑↓→` / `hjkl` · `o` | move between panes · cycle panes |
| `g` / `G` | jump the focused pane to top / bottom |
| `z` / `x` | zoom / close the focused pane |
| `$` | open a shell pane — see below |
| `[` | copy mode — see below |
| `c` · `n` · `1`–`9`/`0` · `&` | new window · next window · window N (`0` = 10) · close window |
| `p` / `P` · `r` / `R` | pause / resume focused agent (capital = all) |
| `K` · `S` | kill focused agent · stop its running commands |
| `s` · `d` · `t` · `b` · `e` | summary · diagram · toggle thoughts · time budget · editor |
| `?` | show / hide the whole right-side Options pane (reflows the main panes) |
| `q` | quit |

`↑`/`↓` move the focused pane through the Options list · mouse wheel scrolls the
transcript · type + `Enter` sends a message to the focused agent. The Options pane
lists your open shells under **user panes** (only once you have a shell open), then
the **info panes** (Main, Summary, Diagram), then **active subagents**, then
**idle** ones; click or arrow onto an info pane or subagent to show it in the focused
pane (this is also how you reopen a Summary or Diagram pane you closed), and click a
user pane to jump to that shell. Open a shell with `Ctrl-b $`. Agent **thoughts are hidden by default** (`Ctrl-b` `t`
toggles). Click a tool call to expand it — the **full command** is shown (never
truncated), with its output. The Summary pane renders Markdown; the Diagram pane is
shown verbatim in monospace.

**Mouse:** drag the border between panes to **resize** them, and drag one pane onto
another to **swap** them (rearrange the layout without the keyboard). The wheel
scrolls the pane under the cursor — over the Options pane it scrolls the roster, so a
long list of idle subagents plus the key-help is always reachable.

An engagement opens with three panes: the **orchestrator** on the left, and in the
center a live **Summary** pane (top) the orchestrator keeps current with the state of
play (`Ctrl-b` `s` refreshes it on demand) and a live **Diagram** pane (bottom) in
which it draws an ASCII map of the engagement — a breadth-first **network map** in
pentester mode (hosts, which you have a foothold on vs. root on, how they connect, and
their key services) or a **software-component map** in exploiter mode — kept legible
and refreshed as the picture changes (`Ctrl-b` `d` refreshes it on demand). Confirmed
findings are still recorded for the final report (via `record_finding`), surfaced in
the recording agent's transcript. The right-side **Options** pane (roster +
help, toggled with `Ctrl-b` `?`) sits alongside them. Split, close, and rearrange panes
like any tmux layout.

**Windows (tabs)** work like tmux: `Ctrl-b` `c` opens a new window with its own pane
layout, `Ctrl-b` `n` cycles to the next, `Ctrl-b` `1`–`9` (and `0` for window 10) jumps to window N, and
`Ctrl-b` `&` closes the current one. The header shows a tab strip once more than one
window exists.

**Shell panes** let you run commands yourself. An engagement opens a second tab with one
shell by default, and `Ctrl-b` `$` splits off another; every open shell is listed under
**user panes** in the Options pane. A shell runs in the sandbox (as root). With it focused,
the input line becomes a `$` prompt: type a command, press `Enter`, and the output appears in
that pane. The working directory carries across commands. `Ctrl-b` `S` stops a running command.

**Change scope live** (`Ctrl-b` `e` → the "scope" item): editing the in-scope targets while
agents are running re-applies the per-agent firewall on the fly and tells the orchestrator the
allowed scope changed (so it stops touching anything removed and may engage anything added).

**Copy mode** (`Ctrl-b` `[`): shows the focused agent's transcript as plain, full-screen
text (no other panes, no styling/borders) so you can drag-select and copy just that pane's
lines. `↑`/`↓`/`PgUp`/`PgDn` and the wheel scroll; `Esc` exits.

Assistant markdown renders **live as it streams** (throttled), not only once a reply finishes.

### Running commands & re-tasking

- A **foreground `bash` command auto-backgrounds** after 2 minutes: instead of hanging the
  agent on a long scan / a service / an input-waiting command, the command keeps running as a
  background job (the agent is notified when it finishes, or can `wait`) and the agent is freed
  to continue. Long work can also be started in the background up front with `bash_bg`.
- **Stop a command** the focused agent is running — foreground or background — with `Ctrl-b`
  `S`, without pausing or killing the agent (it gets the aborted result and carries on). The
  same key stops a shell pane's running command.
- The orchestrator can **re-task a finished or idle subagent** with the `reprompt` tool
  (giving it a new instruction while keeping its context); a subagent now stays idle and
  registered after finishing (releasing its concurrency slot) so it can be repromted, and if
  it's truly gone a fresh one of its role is started. `task` still starts brand-new subagents.

## Configuration & secrets

- The host **oline root** is `~/oline` by default, or `$OLINE_HOME` if set — config,
  secrets, backups, logs, and history all live under it. Per mode, `<root>/config/<mode>/`
  holds `config.yml`, `models.yml`, and the prompt files (`SYSTEM_TEMPLATE.md`,
  `PERSONALITY.md`, `agents/<name>.md`, …), each overriding the embedded default.
- **Everything is configurable in files.** Run `oline -init-config` to write the mode's
  defaults to `<root>/config/<mode>/`, then edit any of them — models, context window and
  output tokens, per-token cost, context auto-compaction ceilings, time budgets, the
  engagement spend cap (`budget.maxCostUSD`, default $100) and a separate summarizer
  spend ceiling (`budget.maxSummaryCostUSD`, default $25), outbound IPv6 for agents
  (`network.allowIPv6`, off by default), an agent's tier/tools/spawns, and every prompt and
  personality. Files load automatically next run. See [docs/configuration.md](docs/configuration.md).
- **Three model tiers, no defaults.** `big`, `medium`, and `small` (in `config.yml`
  `modelRoles`); every agent runs on one tier, the orchestrator on `big`, and context
  compaction on `small`. You must choose a model for each tier the first run (the setup
  wizard prompts you) along with the provider key — the engagement will not start until
  all three resolve to a usable model. Your choices are then saved.
- **API keys are never committed.** Each provider in `models.yml` carries an `apiKeyRef`:
  - `env:FIREWORKS_API_KEY` — read from that environment variable, or
  - `secret:<provider>` — read from `~/oline/config/secrets.yml` (mode `0600`, git-ignored).
  The in-app editor writes keys only to `secrets.yml`.

## Backups

Engagement files live **inside the sandbox** — the AI agents can never write to the host.
On a timer (and once when the engagement ends), the **oline program itself** streams the
engagement workspace out of the sandbox into a single compressed archive on the host at
`$OLINE_HOME/backups/<engagement>.tar.gz` (default `~/oline/backups/...`). There is only
ever **one backup file per engagement** — each run overwrites it atomically. Backups work
for any sandbox backend (WSL today; QEMU uses the same mechanism once its lifecycle lands).

Configure in `config.yml`:

```yaml
backup:
  intervalMinutes: 10   # 0 disables backups entirely
```

and relocate the host root with the `OLINE_HOME` environment variable.

## Conversation history

oline saves each engagement's conversation to the host at
`$OLINE_HOME/history/<engagement>.json` (default `~/oline/history/...`) on a timer and
when the engagement ends. The **program** writes it, like backups and the activity log;
agents never touch it. You can stop oline, turn it off, turn it back on, and resume the
engagement with its conversation intact. When you resume an engagement that has saved
history, the setup wizard asks whether to **load the previous conversation** (the
orchestrator continues with full context and every agent's transcript is restored) or
**start from a clean slate**.

## Time budgets

Each subagent runs under a **time budget that oline's engine enforces** — not the orchestrator or any
AI. When a subagent runs past its budget, the engine sends it three escalating "wrap up" reminders and,
if it is still running **5 minutes after the third**, terminates it (its partial output comes back to
the orchestrator as a `[killed]` result). The orchestrator is never time-limited.

There is **no turn cap**. An agent runs until it finishes, is killed, the engagement ends, or its
time budget terminates it. Time and lifecycle are the only limits, never a count of tool-call turns.

Budgets are configurable:

- **Default** for all subagents: `budget.defaultMinutes` in `config.yml` (0 = no default limit).
- **Per agent**: a `time-budget: <minutes>` (or `none`) line in the agent's frontmatter, or an
  `agents.<name>.time-budget` override in `config.yml`.
- **Live, while running**: focus an agent and press **Ctrl-b then b**, then type minutes (or `none` to
  remove the limit). A live change counts from that moment, and the Options pane shows a running
  agent's time left as a live `m:ss` countdown. Removing a running agent's limit sends it a message
  telling it the limit was lifted.
- Agents check their own remaining time with the **`runtime`** tool (or oline's reminder messages),
  so they aren't guessing.

## Activity log

For every engagement, the **oline program** keeps a running, human-readable activity log
on the host at `$OLINE_HOME/logs/<engagement>.log` (default `~/oline/logs/...`). It records
what each agent was doing — its **thinking**, assistant **text**, tool **calls**, tool
**output**, state changes, spawns, user questions, and notices — as timestamped entries you
can `tail -f` live. Like backups, the program writes it (agents never touch the host), and
it is **append-only**, so resuming an engagement extends the same log.

## Sandbox & egress model

- **WSL** (Windows): agents run as unprivileged users (`oline-scope` / `oline-net` /
  `oline-iso`) sharing a workspace, governed by root-owned iptables owner-match rules
  (IPv4, with IPv6 egress blocked by default and the same scope policy enforced over
  IPv6 when `network.allowIPv6` is on). An unprivileged agent cannot flush or
  alter them (no root, no sudo), so the boundary holds even though agents have a shell.
  The lone exception is the Privileged `installer` (runs as root to install tooling): on
  WSL it *could* change in-guest rules, so treat WSL's boundary as in-guest and use QEMU
  when you need it to hold against a root agent. Egress setup is **mandatory** — oline
  refuses to start an engagement if it can't be applied, rather than running unconfined.
- **QEMU** (cross-platform): the strongest isolation — a separate kernel, so there is no
  shared-kernel escape and the host `/mnt` drives aren't present at all. oline boots a
  disposable copy-on-write overlay of a base image, talks to it over SSH, and tears the VM
  down when the engagement ends. Per-agent egress is enforced the same way as WSL (root-owned
  in-guest `iptables` owner rules, applied over SSH); on a Linux host you can additionally pin
  the guest to a tap and filter with host `nftables` (`BuildNftRules`) for a boundary the guest
  cannot touch even as root. See **QEMU backend** below.
- **Tool-layer defense-in-depth** (on top of the network boundary): `bash`/`bash_bg`
  commands matching the configured deny patterns (the host `/mnt` drive mounts and Windows
  interop binaries like `cmd.exe`/`powershell.exe`) are blocked before they reach the
  sandbox, and the structured file tools reject host `/mnt` paths. The per-engagement
  activity log on the host is written `0600` with known API keys redacted.

### QEMU backend

Install QEMU (`qemu-system-x86_64` + `qemu-img`; oline also finds the Windows installer's
`C:\Program Files\qemu`). Then in the setup wizard's environment step you get, in addition to
any WSL distros:

- **Debian 13 VM (download on first use)** — on start, oline downloads the official Debian 13
  generic-cloud qcow2 (verified against the published `SHA512SUMS`), generates a throwaway SSH
  key and a cloud-init seed to inject it, boots the VM headless on a disposable overlay, waits
  for SSH, and provisions the per-agent users + egress. The base image is cached under
  `~/oline/images` and reused.
- **A qcow2 you supply** — drop any cloud image (with cloud-init) at `~/oline/images/<name>.qcow2`
  and it appears as an environment to open.

VMs are disposable: the overlay and seed are discarded on exit (the base image is kept), so
QEMU engagements don't support "resume". A live end-to-end test is gated behind `OLINE_TEST_QEMU`.

### Live tests

Some tests exercise the real provider and sandbox and are skipped unless enabled:

```bash
FIREWORKS_API_KEY=...  OLINE_TEST_WSL=<distro>  go test ./...   # live WSL egress/recon
OLINE_TEST_QEMU=1  go test ./internal/sandbox/ -run TestLiveQEMU -timeout 20m   # boots a real VM
```

They are benign (they inspect the local sandbox and never attack a target) and reversible
(egress tests restore firewall state and remove the users they create; the QEMU test tears
its VM down).

## Status

Core complete: embedded rosters + config/secrets, the streaming LLM client, the agent
engine (spawn/wait/steer, pause/resume/kill), **both sandboxes — the WSL distro and the QEMU
VM — with un-bypassable per-agent egress**, the full tmux-style TUI, and the in-app editor.

## License

MIT (planned).
