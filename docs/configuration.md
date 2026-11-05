# Configuration, secrets, backups, and logs

## The oline root

Everything oline keeps on the host lives under one root directory: `~/oline`, or
`$OLINE_HOME` if that environment variable is set.

```
<root>/
  config/
    secrets.yml              # API keys (mode 0600, git-ignored)
    models.yml               # providers and model tier roles, shared by every mode
    pentester/               # per-mode config and prompts
      config.yml             #   concurrency, budgets, bash deny, agent overrides
      models.yml             #   optional per-mode provider/model overrides
      SYSTEM_TEMPLATE.md      #   orchestrator prompt
      PERSONALITY.md          #   orchestrator personality
      agents/<name>.md        #   each subagent's prompt and frontmatter
    exploiter/
      ...                     #   same layout
  backups/<engagement>.tar.gz
  logs/<engagement>.log
  audit/<engagement>.audit.jsonl  # tamper-evident, hash-chained command log
  history/<engagement>.json   # saved conversation, reloaded when you resume
  images/                     # cached QEMU base images and user qcow2 files
```

Config loads in layers: the defaults built into the binary, then any per-mode files
you place under `<root>/config/<mode>/`, then the shared `<root>/config/models.yml`,
then edits you make in the app. A `config.yml` or `models.yml` only needs the keys
you want to change; the rest use the defaults.

Your providers and your big/medium/small tier selections are the same whatever mode
you run, so they live in the shared `<root>/config/models.yml` and apply to every
mode. The wizard and the in-app editor write them there, so setting them up once in
one mode configures them for all of them. Everything else (budgets, concurrency,
bash rules, per-agent overrides, and prompts) stays per-mode.

## Scaffold the config: `oline -init-config`

`oline -init-config` writes the mode's full default set (`config.yml`,
`models.yml`, and every prompt file) into `<root>/config/<mode>/` to edit in place:

```bash
oline -mode pentester -init-config
oline -mode exploiter -init-config
```

It does not overwrite files you have already edited; pass `-force` to replace them
with the defaults. Edited files load on the next run.

## What you can configure, and where

| You want to change | Edit |
|---|---|
| The model for each tier (big/medium/small) | shared `models.yml` `modelRoles` (or `-edit`, or the wizard); applies to every mode |
| Which tier an agent runs on, its thinking level | `config.yml` `agents.<name>.tier` (or `-edit`), or `tier:` in `agents/<name>.md` |
| The orchestrator's tier, thinking level, context ceiling | `config.yml` `orchestrator` (or the editor's **Main** section) |
| A provider's base URL / API key | `models.yml` `baseUrl` and `secrets.yml` (or `-edit`) |
| Model list, context window, output `maxTokens` | `models.yml` under the provider's `models` |
| A tier's model token prices (input / output / cache-read / cache-write) | `-edit` under that tier's `role` item, or `cost:` in `models.yml` |
| Token ceiling before an agent auto-compacts | `config.yml` `agents.<name>.max-context-tokens`, or frontmatter |
| Time limits | `config.yml` `budget.defaultMinutes`; per agent `agents.<name>.time-budget` or frontmatter (or `Ctrl-b b` live) |
| An agent's allowed tools or which roles it may spawn | `config.yml` `agents.<name>.tools` / `.spawns`, or frontmatter |
| An agent or the orchestrator's system prompt | `-edit`, or the `.md` file under `config/<mode>/` |
| The orchestrator's personality | `PERSONALITY.md` under `config/<mode>/` |
| Max concurrent agents (orchestrator included) | `config.yml` `maxConcurrency` |
| Engagement spend cap (USD) | `config.yml` `budget.maxCostUSD` |
| Compaction spend ceiling | `config.yml` `budget.maxSummaryCostUSD` |
| Allow outbound IPv6 for agents | `config.yml` `network.allowIPv6` |
| Backup interval, host root | `config.yml` `backup.intervalMinutes`; `$OLINE_HOME` |
| Blocked shell patterns | `config.yml` `bash.patterns` |
| Fully customizable panes/tabs (off by default) | `config.yml` `tui.customizablePanes` (or `-edit` under the `tui` item) |
| Auto-close a subagent's pane when it goes idle | `config.yml` `tui.closeIdleSubagentPanes` (on by default) |
| Make QEMU VMs throwaway (they persist/resume by default) | `config.yml` `qemu.disposable` |
| Host tap + nftables boundary for QEMU (Linux) | `config.yml` `qemu.hostFirewall` |

## config.yml

```yaml
modelRoles:                  # no defaults: set the first three before the first run
  big:    ""                 # strongest: exploiters, thinkers, critiques
  medium: ""                 # balanced work (and the orchestrator, by default)
  small:  ""                 # summarizers and context compaction
  decision: ""               # OPTIONAL System-1 decision model; blank = disabled

maxConcurrency: 5            # cap on agents running at once (orchestrator included)

orchestrator:                # the orchestrator (Main) runs on its own tier
  tier: medium               # which tier backs it (default medium)
  thinkingLevel: high        # its reasoning effort (default high)
  maxContextTokens: 0        # compaction ceiling (0 = auto, a fraction of the window)

backup:
  intervalMinutes: 10        # 0 disables backups

budget:
  defaultMinutes: 30         # per-subagent time budget; 0 = no default
  maxCostUSD: 100            # engagement spend cap in USD; 0 = no cap
  maxSummaryCostUSD: 25      # cap on small-tier compaction spend; 0 = no cap

network:
  allowIPv6: false           # outbound non-loopback IPv6 for agents

bash:
  patterns:                  # commands matching these are blocked before the sandbox
    - { match: "*/mnt/c*",  approval: deny }
    - { match: "*cmd.exe*", approval: deny }

agents: {}                   # per-agent overrides

qemu:                        # QEMU backend only (ignored for WSL)
  disposable: false          # VMs persist/resume by default; true = throwaway each run
  hostFirewall: false        # Linux host: host-owned tap + nftables cap (needs sudo)

tui:                         # interface preferences (not engagement behavior)
  customizablePanes: false   # true = free-form numeric tabs (drops the fixed tabs/auto-layout)
  closeIdleSubagentPanes: true  # auto-close a subagent's pane when it goes idle/killed
```

- **modelRoles** maps each tier to a `provider/model-id` selector. The first three
  (`big`/`medium`/`small`) must be set before an engagement can start, and the model
  must exist in `models.yml` with a usable API key, or oline refuses to start. The
  wizard prompts for them on the first run, or use `oline -edit`. **`decision`** is
  optional (blank = disabled): a System-1 decision model that enables finding
  triage/dedup, judge pre-screening, a subagent progress detector, and task routing —
  see [AGENTS.md](AGENTS.md#decision-model-optional). It is validated and probed at
  engagement start; if it is unset or unreachable the features stay off and oline runs
  exactly as before.
- **maxConcurrency** caps how many agents run at once, the orchestrator included,
  so at the default of 5 up to 4 subagents run alongside it. Background `bash_bg`
  jobs are not counted.
- **orchestrator** sets the tier, thinking level, and context ceiling for the
  orchestrator (Main). It defaults to the `medium` tier at `high` thinking —
  strong planning at a lower cost than `big`. `maxContextTokens: 0` auto-sizes the
  compaction ceiling to a fraction of the model's window.
- **budget.maxCostUSD** caps engagement spend in US dollars. At the cap, oline
  pauses all agents; raise the value or resume with `Ctrl-b R`. 0 disables it.
- **budget.maxSummaryCostUSD** caps the spend of the `small`-tier model used for
  compaction. It counts toward `maxCostUSD`. 0 disables it.
- **network.allowIPv6** enables outbound non-loopback IPv6 for agents. Off by
  default. See [sandbox.md](sandbox.md).
- **qemu.disposable / qemu.hostFirewall**: QEMU VMs persist for resume by default
  (`disposable: true` makes them throwaway); on a Linux host `hostFirewall` adds a
  host-owned tap + nftables boundary. See [sandbox.md](sandbox.md).
- **bash.patterns** is a deny list for shell commands. See [sandbox.md](sandbox.md).
- **agents** holds per-agent overrides: `tier`, `thinking-level`, `tools`,
  `spawns`, `time-budget`, and `max-context-tokens`.

## models.yml and providers

oline ships with no provider. The first-run wizard writes the one you configure to
the shared `<root>/config/models.yml`; you can also add or edit providers there
directly, and every mode picks them up. `models.yml` defines the OpenAI-compatible
providers and their models, and holds the big/medium/small tier selections under
`modelRoles`:

```yaml
providers:
  openai:                                 # any name; it prefixes the model selector
    baseUrl: https://api.openai.com/v1
    api: openai
    apiKeyRef: secret:openai              # or env:NAME to read a key from the environment
    models:
      - id: gpt-4o
        name: GPT-4o
        contextWindow: 128000
        maxTokens: 16384
        reasoning: false
        thinking: { mode: ..., efforts: [...], defaultLevel: ... }   # reasoning models only
        cost: { input: 0.0, output: 0.0, cacheRead: 0.0, cacheWrite: 0.0 }
```

The `cost` fields set the per-token prices used for the cost figures shown during
a run.

## Prompts

Every prompt is a Markdown file you can override: the orchestrator's
`SYSTEM_TEMPLATE.md` and `PERSONALITY.md`, and one `agents/<name>.md` per subagent.
A file under `config/<mode>/` replaces the
default, so run `oline -init-config` and edit from there. Each `agents/<name>.md`
has YAML frontmatter (`tier`, `thinking-level`, `tools`, `spawns`, `time-budget`,
`max-context-tokens`) followed by the prompt body.

## API keys and secrets

Keys are never written to `config.yml` or `models.yml`. Each provider's
`apiKeyRef` is resolved at runtime:

- `env:NAME` reads the key from that environment variable.
- `secret:<provider>` reads it from `<root>/config/secrets.yml`.

A key saved in `secrets.yml` takes precedence. The setup wizard and the in-app
editor write keys only to `secrets.yml` (mode 0600). That file and the user
`config.yml` / `models.yml` are git-ignored.

## The in-app editor

Open it with `oline -edit`, or `Ctrl-b e` while running. It edits:

- the model for each tier, and that model's token prices (input, output,
  cache-read, cache-write, in USD per 1M tokens),
- per agent: the tier, thinking level, and max context tokens,
- per provider: the base URL and API key,
- the live engagement scope,
- the TUI settings (Customizable Panes),
- each agent's system prompt, and the orchestrator's system prompt and personality.

A tier's prices are edited on that tier's `role` item, below its model selector. A
blank price is 0. If you point a tier at a model id not in the provider's catalog,
saving adds it to that provider (inheriting another model's context window, output
cap, and thinking settings, with a zero cost you then set). A new provider must be
defined in `models.yml` first.

Prompts are edited in a multi-line box: select an agent or an orchestrator prompt
item and press Enter. `Tab` moves between fields and the box; in the box `Enter`
inserts a newline, `Ctrl+S` saves, and `alt+up` / `alt+down` resize it. Saved
changes go to the user `config.yml` / `models.yml`, `secrets.yml`, or the matching
`.md` override. A subagent change applies to agents spawned afterward; an
orchestrator prompt change applies to the next engagement.

## Backups

On a timer, and when the engagement ends, oline copies the engagement workspace
out of the sandbox into a single archive at `<root>/backups/<engagement>.tar.gz`,
overwriting the previous one. Set the interval with `backup.intervalMinutes` in
`config.yml` (0 disables).

## Conversation history

oline saves each engagement's conversation to `<root>/history/<engagement>.json` on
a timer and at the end, so you can stop and later resume it. On resume, the wizard
asks whether to load that history. Loading restores the orchestrator's context and
every agent's transcript. In customizable mode it also restores the window/pane
layout; the fixed Info / Subagents / Shell tabs and shell panes are rebuilt instead.

## Activity log

Each engagement writes a human-readable log at `<root>/logs/<engagement>.log`: every
agent's thinking, text, tool calls and output, state changes, spawns, questions, and
notices, as timestamped entries you can `tail -f`. Append-only, mode 0600, with known
API keys redacted.

## Audit log

Alongside the activity log, oline keeps a tamper-evident command log at
`<root>/audit/<engagement>.audit.jsonl`. Every command run during the engagement —
by an agent (`bash`, `bash_bg`, `revshell`) or by you in your own shell/terminal
panes — is appended as one JSON record carrying who ran it, when, and what. Each
record's SHA-256 hash covers the previous record's hash, forming a chain: altering,
inserting, removing, or reordering any entry breaks the chain from that point on. The
log lives on the host (never inside the sandbox), so a compromised target cannot
reach it, and a resumed engagement extends the same chain.

Verify an engagement's chain at any time:

```bash
oline -audit-verify "<engagement>"
```

It reports whether the log is intact or names the first broken entry, then exits. (As
with any local append-only log, someone with write access to the host file could
rewrite the whole chain; an off-host copy of the head hash defends against that.)
