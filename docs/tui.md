# The interface

oline is a full-screen terminal program. It opens with a setup wizard, then shows
the running engagement view. It works with the keyboard and the mouse.

## Splash screen

If your terminal supports Sixel graphics, oline shows the logo on a splash screen
before the wizard; otherwise it shows an ANSI version. Force it on or off with
`OLINE_SIXEL=1` or `OLINE_SIXEL=0`.

## Setup wizard

The wizard asks for the engagement settings in order; steps that do not apply are
skipped. Press Enter to advance.

1. **Mode**: pentester or exploiter.
2. **Models**: oline ships with no provider, so on the first run this is a form that
   defines one: the provider name, base URL, and API key, plus a model for each tier
   (`big`, `medium`, `small`) with its id, context window, max tokens, whether it is
   a reasoning model, and its prices. The fields are prefilled with an OpenAI
   example; the medium and small tiers can reuse the tier above. Your choices are
   saved, so later runs skip this step (or, when a provider already exists but the
   tiers are unset, show a short picker instead). Up/down or Tab move between fields,
   left/right or Space toggle, Enter saves.
3. **API key**: shown only if a chosen model's provider still has no key (for
   example you left the key blank). Saved to `secrets.yml`.
4. **Start**: a new engagement, or resume a previous one.
5. **Environment**: pick a sandbox. The wizard lists WSL distros and QEMU images
   and offers a "create new" option. See [sandbox.md](sandbox.md).
6. **Resume / import**: when resuming, pick an existing engagement in the sandbox
   or import one from a host archive, then choose whether to load its saved history.
7. **Scope**: the in-scope targets. Free text; IPs, CIDRs, and domains are parsed
   out (for example `10.0.0.0/24 and app.example.com`). IPs and CIDRs drive the
   firewall; domains are used for tool-layer checks and reporting.
8. **Goal**: a short description of what the engagement should achieve.
9. **VPN** (optional): point oline at an OpenVPN config so the sandbox connects
   through a VPN before agents start. WSL only.
10. **Auto / manual**: automatic runs without stopping for input; manual relays
    agent questions to you. You can switch while running.

## Running view

The default layout tiles panes like tmux:

```
+----------------------+------------------+-------------------+
|                      |  summary         |  Options pane     |
|  orchestrator        |  (live)          |  (roster + help)  |
|  transcript          +------------------+                   |
|                      |  diagram         |                   |
|                      |  (live)          |                   |
+----------------------+------------------+-------------------+
|  scope / status bar                                         |
|  command line                                               |
+-------------------------------------------------------------+
```

- The left pane follows the orchestrator.
- The center stacks two read-only panes:
  - **Summary** (top): the orchestrator's current state of play, in Markdown.
    `Ctrl-b s` asks it to refresh both this and the Diagram pane now.
  - **Diagram** (bottom): an ASCII map the orchestrator draws (a network map in
    pentester mode, a software-component map in exploiter mode). `Ctrl-b d` asks it
    to refresh just this pane now (`Ctrl-b s` refreshes both). It is shown verbatim
    in monospace; lines wider than the pane are clipped, so zoom the pane
    (`Ctrl-b z`) to see a large map.
  Both panes are read-only; focus an agent to type a message.
- The right **Options** pane (toggle with `Ctrl-b ?`) lists, in order: a **user
  panes** section with the shells you have opened; an **info panes** section with
  Main, Summary, and Diagram; then **active** and **idle subagents**. Each agent
  row shows its state, how much of its context window it has used, and, for a
  running agent with a time budget, a countdown of the time left.
- Click an info pane or a subagent, or arrow onto it, to show it in the focused
  pane. This is also how you reopen a Summary or Diagram pane you closed.
- A subagent's pane begins with the task prompt the orchestrator gave it; scroll to
  the top to read it.
- The status bar shows mode, scope, environment, automatic/manual, and the
  concurrency count.
- The command line sends a message to the focused agent. When a shell pane is
  focused it runs commands instead.

Markdown renders as it streams. Agent thoughts are hidden by default (`Ctrl-b t`
toggles). Click a tool call to expand it and see the full command and its output.

### Mouse

- Click a pane to focus it; click a tool call to expand or collapse it.
- Click a window tab in the header to switch to that window.
- Drag the border between two panes to resize them.
- Drag one pane onto another to swap what they show.
- The wheel scrolls the pane under the cursor; over the Options pane it scrolls the
  roster.

## Keybindings

Press `Ctrl-b`, then one of these keys:

| key | action |
|---|---|
| `%` / `\|` | split the focused pane across |
| `"` / `-` | split the focused pane down |
| arrows / `hjkl` | move between panes |
| `o` | cycle panes |
| `g` / `G` | jump the focused pane to the top / bottom |
| `z` | zoom the focused pane |
| `x` | close the focused pane |
| `$` | open a shell pane |
| `[` | copy mode |
| `c` | new window (tab) |
| `n` | next window |
| `1`-`9`, `0` | jump to window N (`0` is window 10) |
| `&` | close the current window |
| `p` / `P` | pause the focused agent / all agents |
| `r` / `R` | resume the focused agent / all agents |
| `K` | kill the focused agent |
| `S` | stop the commands the focused agent (or shell) is running |
| `s` | refresh the Summary and Diagram panes now |
| `d` | refresh the Diagram pane now |
| `t` | toggle agent thoughts |
| `b` | change the focused agent's time budget |
| `e` | open the model/provider editor |
| `?` | show or hide the Options pane |
| `q` | quit |

Without the prefix: up/down move the focused pane through the Options list, the
wheel scrolls the transcript, and typing plus Enter sends a message to the focused
agent (or runs a command in a focused shell pane).

### Windows (tabs)

`Ctrl-b c` opens a new window with its own panes, `Ctrl-b n` cycles to the next,
`Ctrl-b 1` to `Ctrl-b 9` jump by number, and `Ctrl-b &` closes the current one (not
the last). When more than one window exists, a tab strip appears in the header;
click a tab to switch to it.

### Shell panes

`Ctrl-b $` opens a shell pane that runs commands in the sandbox as root. Open
shells are listed under **user panes**. With a shell pane focused, the input line
shows a `$` prompt; type a command and press Enter, and the output appears in the
pane. The working directory is kept across commands. `Ctrl-b S` stops a running
command. The egress firewall restricts agents, not your own shell.

### Pause, resume, and kill

`Ctrl-b p` pauses the focused agent immediately, cancelling its model stream and
the command it is running; `Ctrl-b r` resumes it. `Ctrl-b P` pauses everything and
stops new agents from spawning; `Ctrl-b R` resumes everything. `Ctrl-b K` kills the
focused agent.

### Stop a command

`Ctrl-b S` stops whatever command the focused agent (or shell) is running, without
pausing or killing the agent.

### Copy mode

`Ctrl-b [` shows the focused agent's transcript as plain full-screen text so you
can select and copy it. Arrows, PgUp, PgDn, and the wheel scroll; Esc exits.

### Change scope while running

`Ctrl-b e`, then edit the scope item. Changing the in-scope targets while agents
run re-applies the firewall and tells the orchestrator the allowed scope changed.

### Edit a prompt

`Ctrl-b e` lists every agent plus the orchestrator's `system prompt`, `personality`,
and (pentester) `methodology`. Select one and press Enter to edit it in a box; `Tab`
moves between fields and the box, `alt+up` / `alt+down` resize it, and `Ctrl+S`
saves. See [configuration.md](configuration.md#the-in-app-editor).

### Change a time budget while running

Focus an agent, press `Ctrl-b b`, and type minutes (or `none` to remove the limit).
See [agents.md](agents.md).
