# The interface

oline is a full-screen terminal program. It opens with a setup wizard, then
switches to the running engagement view. The whole app is dark-themed and works
with the keyboard and the mouse.

## Splash screen

On launch oline checks whether your terminal supports Sixel graphics (Windows
Terminal 1.22 and newer, WezTerm, iTerm2, and others). If it does, it shows the
logo full-resolution on a splash screen before the wizard. If it does not, the
wizard shows an ANSI fallback version of the logo. Force this on or off with
`OLINE_SIXEL=1` or `OLINE_SIXEL=0`.

## Setup wizard

The wizard asks for the engagement settings in order. Steps that do not apply are
skipped.

1. **Mode** - pentester or exploiter.
2. **Models** - shown until all three model tiers are set. Pick a model for `big`,
   `medium`, and `small` from the providers in `models.yml` (up/down to choose a
   tier, left/right to change its model, Enter to confirm). There are no defaults;
   the choices are saved and the step is skipped on later runs. See
   [configuration.md](configuration.md).
3. **API key** - shown for any provider your chosen models use that has no key yet.
   The key is saved to `secrets.yml`.
4. **Start** - new engagement, or resume a previous one.
5. **Environment** - pick a sandbox. The wizard scans for WSL distros and QEMU
   images, and offers a "create new" option. See [sandbox.md](sandbox.md).
6. **Resume / import** - when resuming, pick an existing engagement in the sandbox
   or import one from a host archive. If the chosen engagement has saved
   conversation history, the wizard then asks whether to load that history or start
   from a clean slate. See [configuration.md](configuration.md).
7. **Scope** - the in-scope targets. Free text: IPs, CIDRs, and domains are parsed
   out of whatever you type (for example `10.0.0.0/24 and app.example.com`). IPs
   and CIDRs drive the firewall rules; domains are kept for tool-layer checks and
   reporting.
8. **Goal** - a short description of what the engagement should achieve.
9. **VPN** - optional. Point oline at an OpenVPN config so the sandbox connects
   through a VPN before agents start. Options are: none, a VPN that is already up,
   a `.ovpn` path inside the guest, or a `.ovpn` path on the host (copied in).
   VPN support is WSL only.
10. **Auto / manual** - automatic runs without stopping for input; manual relays
    agent questions to you. You can switch between them later while running.

Pressing Enter advances each step. The scope and goal become part of the
engagement record the orchestrator works from.

## Running view

The default layout is a tiling set of panes, like tmux:

```
+----------------------+------------------+-------------------+
|                      |  summary         |  Options pane     |
|  orchestrator        |  (live)          |  (roster + help)  |
|  transcript          +------------------+  active first,    |
|                      |  diagram         |  then idle        |
|                      |  (live)          |                   |
+----------------------+------------------+-------------------+
|  scope / status bar                                         |
|  command line                                               |
+-------------------------------------------------------------+
```

- The left pane follows the orchestrator.
- The center has two live panes, stacked:
  - The Summary pane (top) holds the orchestrator's current state-of-play (what is
    done, confirmed, in progress, and next). The orchestrator updates it as the
    engagement moves; `Ctrl-b s` asks it to refresh it now. It renders Markdown.
  - The Diagram pane (bottom) holds a live ASCII map the orchestrator draws of the
    engagement: in pentester mode a breadth-first network map (the hosts in play,
    which you have a foothold on versus root on, how hosts connect, and which run
    important services); in exploiter mode a breadth-first map of the target
    software's components and how they interact. The orchestrator keeps it legible
    (in a large environment it shows only the most important hosts/components and
    links, not everything) and updates it as the picture changes; `Ctrl-b d` asks it
    to refresh it now. It is shown verbatim in monospace so the art stays aligned;
    lines wider than the pane are clipped (not wrapped). To keep the map from being
    clipped, the orchestrator is told the pane's exact width and `update_diagram`
    rejects a map with any over-wide line (telling it the limit), re-checking the live
    width so a resize applies at once; so the map normally fits. Zoom the pane
    (`Ctrl-b z`) to give a large map more room.
  Both panes are read-only, so focus an agent (not an info pane) to type a message.
- The right pane is the Options pane (toggle it with `Ctrl-b ?`). It lists, in order:
  a **user panes** section with the shells you have opened (shown only once you have at
  least one shell open); an **info panes** section with the orchestrator (Main),
  Summary, and Diagram; then **active subagents** and **idle subagents**. Each agent
  row shows its state, how much of its context window it has used (for example `42%`),
  and, for a running agent with a time budget, a live countdown of the time left (for
  example `29:05`). The pane is kept as narrow as its content allows so the main panes
  get the width; long agent names are truncated with an ellipsis rather than wrapped.
- Clicking an info pane or a subagent, or moving through them with the up/down arrows,
  shows it in the focused pane. This is also how you reopen a Summary or Diagram pane
  you closed: arrow onto it (or click it) and it comes back in the focused pane.
- Open a shell with `Ctrl-b $`: it splits off a new shell pane in the sandbox rather
  than retargeting the focused pane. The shells you open are listed under **user
  panes**; click one there to jump to it (switching tabs if it is on another one). An
  engagement opens a second tab with one shell by default.
- You can split, close, and rearrange these panes like any tmux layout, and open a
  subagent's transcript in its own pane. A subagent's pane begins with the task
  prompt the orchestrator gave it, so scroll to the top to see what it was told to do.
- The status bar shows mode, scope, environment, automatic/manual, and the
  concurrency count (how many agents are running out of the cap).
- The command line sends a message to the agent whose transcript is focused. Type
  and press Enter to chat with any agent, including the orchestrator. When a shell
  pane is focused it runs commands instead (see below).

Assistant markdown renders live as it streams, not only when a reply finishes.
Agent thoughts are hidden by default; toggle them with `Ctrl-b` then `t`.

Click a tool call to expand it. The full command is shown, never truncated, along
with its output.

### Mouse

- Click a pane to focus it; click a tool call to expand or collapse it.
- Click a window tab in the header to switch to that window (the tab strip shows
  when more than one window is open).
- Drag the border between two panes to resize them (the split ratio follows the
  cursor; neither side collapses below a tenth of the space).
- Drag one pane onto another to swap what they show, so you can rearrange the layout
  without the keyboard. Focus follows the pane to where you drop it.
- The wheel scrolls the pane under the cursor. Over the Options pane on the right it
  scrolls that pane, so a long roster (many idle subagents plus the key-help list) is
  fully reachable; a `▲ ▼ more` hint shows when there is more to scroll. Elsewhere the
  wheel scrolls the focused transcript.

## Keybindings

oline uses a tmux-style prefix. Press `Ctrl-b`, then one of these keys:

| key | action |
|---|---|
| `%` / `\|` | split the focused pane across |
| `"` / `-` | split the focused pane down |
| arrows / `hjkl` | move between panes |
| `o` | cycle panes |
| `g` / `G` | jump the focused pane to the top / bottom |
| `z` | zoom the focused pane |
| `x` | close the focused pane |
| `$` | open a shell pane (see below) |
| `[` | copy mode (see below) |
| `c` | new window (tab) |
| `n` | next window |
| `1`–`9`, `0` | jump to window N (`0` is window 10) |
| `&` | close the current window |
| `p` / `P` | pause the focused agent / all agents |
| `r` / `R` | resume the focused agent / all agents |
| `K` | kill the focused agent |
| `S` | stop the commands the focused agent (or shell) is running |
| `s` | refresh the Summary pane now (ask the orchestrator to update it) |
| `d` | refresh the Diagram pane now (ask the orchestrator to update it) |
| `t` | toggle agent thoughts |
| `b` | change the focused agent's time budget |
| `e` | open the model/provider editor |
| `?` | show or hide the whole Options pane (the main panes take the freed width) |
| `q` | quit |

Without the prefix: the up and down arrows move the focused pane through the Options
list (info panes and subagents alike), the mouse wheel scrolls the transcript, and
typing plus Enter sends a message to the focused agent (or runs a command in a
focused shell pane).

### Windows (tabs)

A window is a full set of panes, like a tmux window. `Ctrl-b c` opens a new window
with its own layout, `Ctrl-b n` cycles to the next, and `Ctrl-b 1` through
`Ctrl-b 9` jump to a window by number. `Ctrl-b &` closes the current window (you
can't close the last one). When more than one window exists, a tab strip appears in
the header with the active window bracketed; click a tab there to switch to that
window.

### Shell panes

`Ctrl-b $` splits off a shell pane: an interactive shell that runs commands in the
sandbox as root. Each shell you open is then listed under **user panes** in the
Options pane, where you can click it to jump back to it. The egress firewall
restricts agents, not your own shell, so you have full access to the box to inspect
what the agents are doing.

With a shell pane focused, the input line shows a `$` prompt. Type a command and
press Enter; the output appears in the pane. The working directory is kept across
commands, so `cd` works as you would expect. `Ctrl-b S` stops a running command.
Closing the pane (`Ctrl-b x`) stops any command still running in it.

### Pause, resume, and kill

Pausing an agent is immediate. It cancels the in-flight model stream and kills the
command the agent is running in the sandbox. Resuming re-issues the step; the
aborted tool call is recorded as stopped by the operator. `P` pauses everything
and stops new agents from spawning; `R` resumes everything.

### Stop a command

`Ctrl-b` then `S` stops whatever command the focused agent is running, foreground
or background, without pausing or killing the agent. The agent gets the aborted
result and carries on. For a shell pane, the same key stops the command it is
running.

### Copy mode

`Ctrl-b` then `[` shows the focused agent's transcript as plain full-screen text,
with no other panes, borders, or styling, so you can drag-select and copy just
that pane's lines. The arrows, PgUp, PgDn, and the wheel scroll. Esc exits.

### Change scope while running

Open the editor with `Ctrl-b` then `e` and edit the scope item. Changing the
in-scope targets while agents are running re-applies the per-agent firewall on the
fly and tells the orchestrator that the allowed scope changed, so it stops
touching anything you removed and may engage anything you added.

### Edit a system prompt

The editor (`Ctrl-b` then `e`) lists every agent plus the orchestrator's prompt
bundle: a `system prompt` item, a `personality` item, and (pentester only) a
`methodology` item. Select one and press Enter to edit it in a multi-line box. `Tab`
moves between the fields and the prompt box; in the box, `Enter` inserts a newline,
`alt+↑` / `alt+↓` resize the box, and `Ctrl+S` saves. Each edit is written to its own
`.md` override under `~/oline/config/<mode>/` (`SYSTEM_TEMPLATE.md`, `PERSONALITY.md`,
`METHODOLOGY.md`, or `agents/<name>.md` with the subagent's frontmatter preserved).
A subagent change takes effect for agents spawned afterward; an orchestrator change
(any of the three files) takes effect on the next engagement. See
[configuration.md](configuration.md#the-in-app-editor) for details.

### Change a time budget while running

Focus an agent, press `Ctrl-b` then `b`, and type a number of minutes (or `none`
to remove the limit). The change counts from the moment you make it, and the
roster's time-left countdown updates to the new value right away. Removing a
running agent's limit sends it a message saying the limit was lifted. See
[agents.md](agents.md) for how budgets work.
