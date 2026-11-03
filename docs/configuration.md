# Configuration, secrets, backups, and logs

## The oline root

Everything oline keeps on the host lives under one root directory:

- `~/oline` by default, or
- `$OLINE_HOME` if that environment variable is set.

Layout:

```
<root>/
  config/
    secrets.yml              # API keys (mode 0600, git-ignored)
    pentester/                 # per-mode config + prompts (override the built-ins)
      config.yml               #   roles, concurrency, budgets, bash deny, agent overrides
      models.yml               #   providers, models, context window, max tokens, cost
      SYSTEM_TEMPLATE.md        #   orchestrator prompt
      PERSONALITY.md            #   orchestrator personality
      METHODOLOGY.md            #   (pentester) methodology
      agents/<name>.md          #   each subagent's prompt + frontmatter
    exploiter/
      ...                       #   same layout
  backups/<engagement>.tar.gz
  logs/<engagement>.log
  history/<engagement>.json  # saved conversation, reloaded when you resume
  images/                    # cached QEMU base images and user qcow2 files
```

Config is merged in layers, lowest precedence first:

1. Embedded defaults (shipped inside the binary).
2. On-disk files at `<root>/config/<mode>/`: a `config.yml`/`models.yml` whose
   keys you set, and any prompt file you drop in (`SYSTEM_TEMPLATE.md`,
   `PERSONALITY.md`, `METHODOLOGY.md`, `agents/<name>.md`) replaces the built-in.
3. In-session edits from the model editor, written back to layer 2.

A `config.yml`/`models.yml` only needs the keys you want to change; the rest fall
back to the embedded defaults.

## Scaffold the config to edit: `oline -init-config`

You do not have to write these files from scratch. `oline -init-config` writes the
selected mode's full default set (`config.yml`, `models.yml`, and every prompt
file) into `<root>/config/<mode>/` so you can edit them in place:

```bash
oline -mode pentester -init-config
```

```bash
oline -mode exploiter -init-config
```

It never overwrites files you have already edited; pass `-force` to replace them
with the shipped defaults. Edit any file and it loads automatically on the next run
(no rebuild).

## What you can configure, and where

| You want to change | Edit |
|---|---|
| The model for each tier (big/medium/small) | `config.yml` `modelRoles` (or `-edit`, or the first-run wizard) |
| Which tier an agent runs on, its thinking level | `config.yml` `agents.<name>.tier` (or `-edit`), or `tier:` in `agents/<name>.md` |
| A provider's base URL / API key | `models.yml` `baseUrl` + `secrets.yml` (or `-edit`) |
| Model list, context window, output `maxTokens` | `models.yml` under the provider's `models` |
| A tier's model token prices (input / output / cache-read / cache-write) | `-edit` (or `Ctrl-b e`): under that tier's `role` item; or `cost:` in `models.yml` under the model |
| Token ceiling before an agent auto-compacts | `config.yml` `agents.<name>.max-context-tokens`, or `max-context-tokens` in frontmatter |
| Time limits (default and per agent) | `config.yml` `budget.defaultMinutes`; per agent `agents.<name>.time-budget` or `time-budget` in frontmatter (or `Ctrl-b b` live) |
| An agent's allowed tools or which roles it may spawn | `config.yml` `agents.<name>.tools` / `.spawns`, or its frontmatter |
| An agent's system prompt / the orchestrator's system prompt | `-edit` (or `Ctrl-b e`), or the matching `.md` file under `config/<mode>/` (`agents/<name>.md` / `SYSTEM_TEMPLATE.md`) |
| The orchestrator's personality / methodology | `PERSONALITY.md` / `METHODOLOGY.md` under `config/<mode>/` |
| Max concurrent agents (orchestrator included) | `config.yml` `maxConcurrency` |
| Engagement spend cap (USD) | `config.yml` `budget.maxCostUSD` |
| Summarizer (compaction) spend ceiling | `config.yml` `budget.maxSummaryCostUSD` |
| Allow outbound IPv6 for agents | `config.yml` `network.allowIPv6` |
| Backups, host root | `config.yml` `backup.intervalMinutes`; `$OLINE_HOME` |
| Blocked shell patterns | `config.yml` `bash.patterns` |

## config.yml

```yaml
modelRoles:                  # NO defaults: you must set all three the first run
  big:    ""                 # strongest: exploiters, thinkers, critiques, orchestrator
  medium: ""                 # balanced, in-between work
  small:  ""                 # fastest/cheapest: summarizers, and context compaction

maxConcurrency: 5            # hard cap on agents running at once (orchestrator included)

backup:
  intervalMinutes: 10        # 0 disables backups

budget:
  defaultMinutes: 30         # per-subagent time budget; 0 = no default
  maxCostUSD: 100            # engagement spend cap in USD; 0 = no cap
  maxSummaryCostUSD: 25      # ceiling on the small-tier compaction spend; 0 = no cap

network:
  allowIPv6: false           # outbound non-loopback IPv6 for agents; off by default

bash:
  patterns:                  # commands matching these are blocked before the sandbox
    - { match: "*/mnt/c*",         approval: deny }
    - { match: "*cmd.exe*",        approval: deny }
    # ...

agents: {}                   # per-agent overrides, empty by default
```

- **modelRoles** maps the three model tiers to a `provider/model-id` selector.
  Every agent runs on one of these tiers (`big`, `medium`, or `small`), the
  orchestrator runs on `big`, and context compaction uses `small`. There are no
  defaults: all three must be set before an engagement can start, and oline refuses
  to start if any is missing, points at a model not in `models.yml`, or lacks a
  usable API key. The setup wizard prompts for them the first run (or use
  `oline -edit`), and your choices are saved here. Change which tier an agent uses
  with `agents.<name>.tier` (or `tier:` in its frontmatter).
- **maxConcurrency** is enforced by a real semaphore in the engine, not just a
  prompt instruction. It caps the total number of agents running at once, the
  orchestrator included: one slot is reserved for the orchestrator and the
  subagents share the rest, so at the default of 5 up to 4 subagents run
  concurrently. (Background `bash_bg` jobs a subagent starts are not counted.)
- **budget.maxCostUSD** is an engagement-wide spend cap in US dollars, summed
  across every agent from each model's `cost`. When the running total reaches it,
  oline pauses all agents and tells you; raise the value or resume with `Ctrl-b R`
  to continue. 0 disables the cap. The default is 100.
- **budget.maxSummaryCostUSD** is a separate ceiling on the `small`-tier model used
  for context compaction, so a misconfigured (expensive) small model can't quietly
  run up cost. That spend is tracked live and also counts toward `maxCostUSD`; when
  it reaches this ceiling, oline pauses the fleet and tells you to point `small` at a
  cheaper model or raise the value. 0 disables it. The default is 25.
- **network.allowIPv6** turns on outbound non-loopback IPv6 for the agent users.
  It is off by default: the per-agent firewall otherwise drops all IPv6 egress for
  the agents so they cannot bypass the (IPv4) scope rules over IPv6. Turn it on for
  a target that is only reachable over IPv6; the same scope/net/iso policy is then
  enforced over IPv6 too. See [sandbox.md](sandbox.md).
- **bash patterns** are a defense-in-depth deny list. See [sandbox.md](sandbox.md).
- **agents** holds per-agent overrides. Each entry can set `tier` (big/medium/small),
  `thinking-level`, `tools`, `spawns`, `time-budget`, and `max-context-tokens`
  (the context ceiling at which the agent auto-compacts). The in-app editor writes
  here.

## models.yml and providers

`models.yml` defines the model providers. Each provider is OpenAI-compatible:

```yaml
providers:
  fireworks:
    baseUrl: https://api.fireworks.ai/inference/v1
    api: openai
    apiKeyRef: env:FIREWORKS_API_KEY
    models:
      - id: accounts/fireworks/models/deepseek-v4p1-flash
        name: DeepSeek v4.1 Flash
        contextWindow: 163840
        maxTokens: 16384
        reasoning: true
        thinking: { mode: ..., efforts: [...], defaultLevel: ... }
        cost: { input: 0.0, output: 0.0, cacheRead: 0.0, cacheWrite: 0.0 }
```

The `cost` fields (per-token prices, including cache read and write) drive the
cost figures oline shows during a run.

## Prompts

Every prompt is a plain Markdown file you can override: the orchestrator's
`SYSTEM_TEMPLATE.md` and `PERSONALITY.md` (and `METHODOLOGY.md` for pentester), and
one `agents/<name>.md` per subagent. A file you place under `config/<mode>/`
replaces the built-in version entirely, so run `oline -init-config` to get the
defaults and edit from there. Each `agents/<name>.md` has YAML frontmatter
(`tier`, `thinking-level`, `tools`, `spawns`, `time-budget`, `max-context-tokens`)
followed by the prompt body; editing the frontmatter is an alternative to the
`config.yml` `agents` map for per-agent settings.

## Token usage and time limits

- **Output tokens** per model: `maxTokens` in `models.yml` (the ceiling on a single
  reply) and `contextWindow` (the model's total window).
- **Context auto-compaction**: each agent compacts its conversation when it crosses
  a token ceiling, derived from the model's `contextWindow` by role and overridable
  with `max-context-tokens` (per agent, in `config.yml` or frontmatter). The
  compaction summary is produced by the cheaper `small`-tier model (from
  `modelRoles`), not the agent's own model, to keep the cost of frequent compaction
  down. See
  [agents.md](agents.md).
- **Time budgets**: `budget.defaultMinutes` sets the default per-subagent limit;
  override per agent with `agents.<name>.time-budget` (or `time-budget` in
  frontmatter), or live with `Ctrl-b b`. The orchestrator is never time-limited.

## API keys and secrets

API keys are never written to `config.yml` or `models.yml`. Each provider carries
an `apiKeyRef` that is resolved at runtime:

- `env:NAME` reads the key from that environment variable.
- `secret:<provider>` reads it from `<root>/config/secrets.yml`.

A key you save for a provider in `secrets.yml` wins over the ambient reference.
The in-app editor writes keys only to `secrets.yml` (mode 0600). That file and the
user `config.yml` / `models.yml` overrides are git-ignored, so a fresh clone
contains no key.

## The in-app editor

Open it with `oline -edit`, or with `Ctrl-b` then `e` while running. It edits the
model for each tier (big/medium/small), **plus that model's token prices** (input,
output, cache-read, and cache-write, in USD per 1M tokens); per agent, the tier,
thinking level, and max context tokens; per provider, the base URL and API key; the
live engagement scope; the **system prompt of every agent**; and the
**orchestrator's system prompt, personality, and (pentester only) methodology**.

A tier's prices are edited on that tier's `role` item, right below its model selector
(the prices belong to whichever model the tier points at). They feed the live cost
readout and the spend caps, so a change takes effect immediately for agents spawned
afterward. A blank price counts as 0 (for example a model with no separate cache
rate). If you change the model itself, the price fields re-seed to the new model's
prices on save, so stale prices are never written onto it; edit and save again to
change the new model's prices.

If you point a tier at a model id the provider's catalog does not list yet, saving
adds it to that provider automatically (inheriting an existing model's context
window, output cap, and thinking settings, with a zero cost you then set), so the
engagement validates and runs instead of being rejected as "not in the model list".
This only works when the provider itself is known; a new provider must be defined in
`models.yml` first. Tune the auto-added model's context window, output `maxTokens`,
or thinking spec in `models.yml` if the inherited defaults do not fit it.

Prompts are edited in a multi-line box right in the editor. Select an agent, or one
of the orchestrator's prompt items (`system prompt`, `personality`, `methodology`),
and press Enter; `Tab` moves between fields and the prompt box. In the prompt box,
typing edits the text, `Enter` inserts a newline, `Ctrl+S` saves, and `alt+↑` /
`alt+↓` grow / shrink the box so you can give a long prompt more room. (On the
single-line fields, `Enter` still saves.)

Saving writes tier, agent, base-URL, and model-price changes to the user
`config.yml` / `models.yml`, keys to `secrets.yml`, and an edited prompt to a `.md`
override under
`~/oline/config/<mode>/`: `SYSTEM_TEMPLATE.md`, `PERSONALITY.md`, or `METHODOLOGY.md`
for the orchestrator, or `agents/<name>.md` for a subagent (its frontmatter is kept
verbatim, so tier/tools and the rest are untouched). An agent edit preserves that
agent's other overrides. A subagent prompt change applies to agents spawned after the
save; an orchestrator prompt change (any of its three files) applies to the next
engagement. The editor is a convenience; everything it sets can also be edited
directly in the files above.

## Backups

Engagement files live inside the sandbox, so the AI agents can never write to the
host. On a timer, and once when the engagement ends, the oline program itself
streams the engagement workspace out of the sandbox into a single compressed
archive at `<root>/backups/<engagement>.tar.gz`. There is only ever one backup
file per engagement; each run overwrites it atomically. Set the interval with
`backup.intervalMinutes` in `config.yml` (0 disables backups).

## Conversation history

oline saves each engagement's conversation to the host at
`<root>/history/<engagement>.json` on a timer and when the engagement ends. Like
the backups and the activity log, the program writes it; agents never touch it.

This lets you stop the program, turn it off, and later resume the same engagement
with its conversation intact. When you resume an engagement that has saved history,
the setup wizard asks whether to load the previous conversation or start from a
clean slate. Loading it restores the orchestrator's full context (so it continues
where it left off), rebuilds every agent's transcript so you see the prior
conversation, and restores your window and pane layout (splits, tabs, and which
view each pane showed); a clean slate ignores the saved history and starts fresh.
Shell panes are not restored, since their process is gone when the program exits.

## Activity log

For every engagement the oline program keeps a human-readable activity log at
`<root>/logs/<engagement>.log`. It records what each agent did: thinking,
assistant text, tool calls, tool output, state changes, spawns, user questions,
and notices, as timestamped entries you can `tail -f`. The program writes it
(agents never touch the host), it is append-only, and resuming an engagement
extends the same log. It is written mode 0600 with known API keys redacted.
