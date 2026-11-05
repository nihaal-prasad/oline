package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"path"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/nihaal-prasad/oline/internal/core"
	"github.com/nihaal-prasad/oline/internal/event"
	"github.com/nihaal-prasad/oline/internal/llm"
	"github.com/nihaal-prasad/oline/internal/sandbox"
)

// Reverse-shell channels.
//
// A channel is a named netcat listener running inside the sandbox that catches a
// shell connecting back from the target. The hard part a bare `nc` can't do on its
// own here is staying writable: the Exec contract streams output but has no stdin
// pipe, so we back each channel with a guest-side FIFO. A holder keeps the FIFO's
// write end open (so nc never sees EOF between commands) and nc reads its stdin
// from that FIFO; `send`/the operator's pane write command lines into the FIFO and
// nc forwards them to the connected shell, whose output nc appends to out.log.
//
//	<workspace>/channels/<name>/in       input FIFO (commands -> the shell)
//	<workspace>/channels/<name>/out.log  everything the shell has printed
//
// The listener runs as a persistent job on the ENGAGEMENT context (not any agent's
// per-turn context), tracked in Session.channels, so it survives the opening
// agent's turns and lives until `revshell close`, an explicit stop, or the
// engagement ends — the same reliable "hold the Exec open" mechanism bash_bg uses,
// rather than detaching with setsid (which proved unreliable across backends).

// Write arbitration: many subagents may share one channel, but only ONE may drive
// the connected shell at a time (two agents interleaving command lines is nonsense).
// Each channel has a single current WRITER. `send` is allowed only for the writer; an
// unheld channel is auto-claimed by the first sender. The orchestrator (Main)
// arbitrates: it alone can `grant` the write token to a chosen subagent (and can
// `acquire`/`release` it), so it decides who drives a shared channel and when.
// Independently, the actual FIFO write is serialized per channel by sendMu, so even a
// racing write can never interleave bytes on the wire.

// revChannel is one live reverse-shell channel.
type revChannel struct {
	name   string
	port   int
	dir    string // absolute guest path to the channel's working directory
	cancel context.CancelFunc

	writer core.AgentID // the agent currently allowed to send (empty = unheld); guarded by Session.chMu
	sendMu sync.Mutex   // serializes the actual FIFO write so bytes never interleave
}

const (
	workspaceFallback = "/opt/oline" // if the engagement workspace is unknown
	revReadDefault    = 4096         // default bytes returned by `read`
	revReadMax        = 1 << 16      // hard cap on bytes returned by read/send
	revExecTimeout    = 15 * time.Second
)

var chanNameRE = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

// chanRoot is the absolute guest directory that holds all channels for this run.
func (s *Session) chanRoot() string {
	ws := strings.TrimSpace(s.Workspace)
	if ws == "" {
		ws = workspaceFallback
	}
	return path.Join(ws, "channels")
}

func (s *Session) getChannel(name string) *revChannel {
	s.chMu.Lock()
	defer s.chMu.Unlock()
	return s.channels[name]
}

// CloseChannel stops a reverse-shell channel's listener and removes it. It is the
// operator-side kill path (the TUI's Ctrl-b K on a channel pane), so it uses
// emitAsync for the close event — the caller is the same goroutine that drains the
// event stream, and a blocking send would deadlock. Returns whether a channel was
// closed.
func (s *Session) CloseChannel(name string) bool {
	s.chMu.Lock()
	ch := s.channels[name]
	if ch != nil {
		delete(s.channels, name)
	}
	s.chMu.Unlock()
	if ch == nil {
		return false
	}
	ch.cancel() // stops nc, the FIFO holder, and the loop together
	s.emitAsync(event.Channel{Name: ch.name, Port: ch.port, Dir: ch.dir, Open: false})
	return true
}

// doRevshell implements the `revshell` tool (open/send/read/list/close). It is a
// sandbox-exec primitive gated by the per-agent allow-list (see dispatch); every
// action runs strictly inside the sandbox.
func (a *Agent) doRevshell(tc llm.ToolCall) (string, bool) {
	a.s.emit(event.ToolStarted{Agent: a.id, Tool: "revshell", ArgsPreview: preview(tc.Arguments), Args: tc.Arguments})
	reply := func(msg string, isErr bool) (string, bool) {
		a.s.emit(event.ToolOutput{Agent: a.id, Chunk: msg})
		return msg, isErr
	}
	var args struct {
		Action string `json:"action"`
		Name   string `json:"name"`
		Port   int    `json:"port"`
		Data   string `json:"data"`
		Bytes  int    `json:"bytes"`
		Agent  string `json:"agent"`
	}
	if err := json.Unmarshal([]byte(tc.Arguments), &args); err != nil {
		return reply("revshell: invalid arguments: "+err.Error(), true)
	}
	if a.s.Sandbox == nil {
		return reply("revshell: no sandbox attached", true)
	}
	action := strings.ToLower(strings.TrimSpace(args.Action))
	name := strings.TrimSpace(args.Name)

	switch action {
	case "open":
		a.s.auditCmd(string(a.id), "revshell.open", name, fmt.Sprintf("port %d", args.Port))
		return reply(a.revOpen(name, args.Port))
	case "send":
		a.s.auditCmd(string(a.id), "revshell.send", name, args.Data)
		return reply(a.revSend(name, args.Data))
	case "read":
		return reply(a.revRead(name, args.Bytes))
	case "list":
		return reply(a.revList())
	case "close":
		a.s.auditCmd(string(a.id), "revshell.close", name, "")
		return reply(a.revClose(name))
	case "acquire":
		return reply(a.revAcquire(name))
	case "release":
		return reply(a.revRelease(name))
	case "grant":
		return reply(a.revGrant(name, strings.TrimSpace(args.Agent)))
	case "":
		return reply("revshell: 'action' is required (open | send | read | list | close | acquire | release | grant)", true)
	default:
		return reply(fmt.Sprintf("revshell: unknown action %q (use open | send | read | list | close | acquire | release | grant)", action), true)
	}
}

// revOpen stands up a listener for name on port and tracks it as a persistent job.
func (a *Agent) revOpen(name string, port int) (string, bool) {
	if !chanNameRE.MatchString(name) {
		return "revshell open: 'name' must be 1-64 chars of letters, digits, '.', '_' or '-'", true
	}
	if port < 1 || port > 65535 {
		return "revshell open: 'port' must be between 1 and 65535", true
	}
	a.s.chMu.Lock()
	if a.s.channels == nil {
		a.s.channels = map[string]*revChannel{}
	}
	if existing := a.s.channels[name]; existing != nil {
		a.s.chMu.Unlock()
		return fmt.Sprintf("revshell open: channel %q is already open on port %d. Use a different name, or `close` it first.", name, existing.port), true
	}
	dir := path.Join(a.s.chanRoot(), name)
	ctx, cancel := context.WithCancel(a.jobBaseCtx())
	// The opener holds the write token initially (it will usually drive the shell
	// first); the orchestrator can reassign it with `grant`.
	ch := &revChannel{name: name, port: port, dir: dir, cancel: cancel, writer: a.id}
	a.s.channels[name] = ch
	a.s.chMu.Unlock()

	script := revListenScript(dir, name, port)
	policy := a.cfg.Policy

	// Hold the Exec open for the listener's whole life on the engagement context, so
	// it keeps catching shells across the opening agent's turns. Cancelling ctx
	// (close / engagement end) stops nc, the holder, and the loop together.
	a.s.router.wg.Add(1)
	go func() {
		defer a.s.router.wg.Done()
		w := &bgWriter{}
		_, _ = a.s.Sandbox.Exec(ctx, a.id, sandbox.Cmd{Argv: []string{"bash", "-lc", script}, Policy: policy}, w)
		// The listener has stopped (cancelled or failed). Drop it from the live set
		// if it is still the one we registered, so `open` can reuse the name.
		a.s.chMu.Lock()
		if cur := a.s.channels[name]; cur == ch {
			delete(a.s.channels, name)
		}
		a.s.chMu.Unlock()
	}()

	a.s.emit(event.Channel{Name: name, Port: port, Dir: dir, Open: true})
	return fmt.Sprintf("opened reverse-shell channel %q: listening on port %d inside the sandbox. Trigger the target's connect-back now; then `send` it commands and `read` its output. The operator can also open a pane for this channel and type into it.", name, port), false
}

// revSend writes one command line to the channel's shell and returns the output
// that follows shortly after. Only the channel's current writer may send; an unheld
// channel is auto-claimed by the caller. The actual FIFO write is serialized per
// channel so concurrent sends can never interleave bytes.
func (a *Agent) revSend(name, data string) (string, bool) {
	a.s.chMu.Lock()
	ch := a.s.channels[name]
	if ch == nil {
		a.s.chMu.Unlock()
		return fmt.Sprintf("revshell send: no open channel %q (use `list` to see channels, `open` to create one)", name), true
	}
	switch {
	case ch.writer == "":
		ch.writer = a.id // auto-claim an unheld channel
	case ch.writer == a.id:
		// already the writer
	default:
		holder := ch.writer
		a.s.chMu.Unlock()
		return fmt.Sprintf("revshell send: channel %q is currently held by %q, so you may not write to it. Ask the orchestrator (Main) to reassign it to you (revshell grant), or wait for %q to release it. Use `read` to watch its output meanwhile.", name, holder, holder), true
	}
	a.s.chMu.Unlock()

	if data == "" {
		return "revshell send: 'data' (the command line to send) is required", true
	}
	// Serialize the actual write per channel so two sends never interleave on the
	// wire (ownership already makes this rare; this is the clean hard guarantee).
	ch.sendMu.Lock()
	defer ch.sendMu.Unlock()
	// Write the line into the FIFO (the holder keeps a reader open, so this never
	// blocks), wait briefly, then return the tail of the output log.
	script := fmt.Sprintf(`printf '%%s\n' %s > %s/in; sleep 0.6; tail -c %d %s/out.log 2>/dev/null`,
		shSingleQuote(data), shSingleQuote(ch.dir), revReadDefault, shSingleQuote(ch.dir))
	out, err := a.revExec(script)
	if err != nil {
		return fmt.Sprintf("revshell send: %v", err), true
	}
	return fmt.Sprintf("sent to %q. Recent channel output:\n%s", name, strings.TrimRight(out, "\n")), false
}

// revAcquire claims the write token for the caller. It succeeds if the channel is
// unheld or already held by the caller; the orchestrator (Main) may take it from
// whoever holds it (it is the arbiter). Everyone else is refused and told who holds
// it, to go through Main.
func (a *Agent) revAcquire(name string) (string, bool) {
	a.s.chMu.Lock()
	defer a.s.chMu.Unlock()
	ch := a.s.channels[name]
	if ch == nil {
		return fmt.Sprintf("revshell acquire: no open channel %q", name), true
	}
	if ch.writer == "" || ch.writer == a.id {
		ch.writer = a.id
		return fmt.Sprintf("you now hold write access to channel %q.", name), false
	}
	if a.id == core.Main {
		prev := ch.writer
		ch.writer = core.Main
		return fmt.Sprintf("orchestrator took write access to channel %q (was held by %q).", name, prev), false
	}
	return fmt.Sprintf("revshell acquire: channel %q is held by %q. Only the orchestrator (Main) can reassign it (revshell grant); ask Main.", name, ch.writer), true
}

// revRelease frees the caller's write token (Main may free anyone's).
func (a *Agent) revRelease(name string) (string, bool) {
	a.s.chMu.Lock()
	defer a.s.chMu.Unlock()
	ch := a.s.channels[name]
	if ch == nil {
		return fmt.Sprintf("revshell release: no open channel %q", name), true
	}
	switch {
	case ch.writer == a.id:
		ch.writer = ""
		return fmt.Sprintf("released write access to channel %q; it is now free.", name), false
	case a.id == core.Main && ch.writer != "":
		prev := ch.writer
		ch.writer = ""
		return fmt.Sprintf("orchestrator released channel %q (was held by %q); it is now free.", name, prev), false
	default:
		return fmt.Sprintf("revshell release: you do not hold write access to channel %q (holder: %q).", name, writerLabel(ch.writer)), true
	}
}

// revGrant assigns the write token to a named agent. ORCHESTRATOR ONLY: Main is the
// arbiter of who may drive a shared channel.
func (a *Agent) revGrant(name, target string) (string, bool) {
	if a.id != core.Main {
		return "revshell grant: only the orchestrator (Main) can grant write access to a channel. Ask Main to arbitrate.", true
	}
	if target == "" {
		return "revshell grant: 'agent' (the subagent to grant write access to) is required.", true
	}
	a.s.chMu.Lock()
	ch := a.s.channels[name]
	if ch == nil {
		a.s.chMu.Unlock()
		return fmt.Sprintf("revshell grant: no open channel %q", name), true
	}
	prev := ch.writer
	ch.writer = core.AgentID(target)
	a.s.chMu.Unlock()
	// Tell the newly-granted agent it may now drive the channel.
	a.s.emit(event.Notice{Agent: core.AgentID(target), Level: "info",
		Text: fmt.Sprintf("orchestrator granted you write access to reverse-shell channel %q — you may now `revshell send` to it.", name)})
	return fmt.Sprintf("granted write access to channel %q to %q (was held by %q).", name, target, writerLabel(prev)), false
}

// writerLabel renders a writer id for messages, mapping the empty id to "nobody".
func writerLabel(id core.AgentID) string {
	if id == "" {
		return "nobody"
	}
	return string(id)
}

// revRead returns the channel's recent output.
func (a *Agent) revRead(name string, nbytes int) (string, bool) {
	if name == "" {
		return "revshell read: 'name' is required", true
	}
	if nbytes <= 0 {
		nbytes = revReadDefault
	}
	if nbytes > revReadMax {
		nbytes = revReadMax
	}
	ch := a.s.getChannel(name)
	dir := path.Join(a.s.chanRoot(), name)
	if ch != nil {
		dir = ch.dir
	}
	script := fmt.Sprintf(`if [ -f %s/out.log ]; then tail -c %d %s/out.log; else echo "[no such channel]"; fi`,
		shSingleQuote(dir), nbytes, shSingleQuote(dir))
	out, err := a.revExec(script)
	if err != nil {
		return fmt.Sprintf("revshell read: %v", err), true
	}
	status := "listener up"
	if ch == nil {
		status = "listener NOT running (channel closed or never opened)"
	}
	return fmt.Sprintf("channel %q (%s). Recent output:\n%s", name, status, strings.TrimRight(out, "\n")), false
}

// revList reports every live channel plus how much output each has captured.
func (a *Agent) revList() (string, bool) {
	a.s.chMu.Lock()
	type chInfo struct {
		name   string
		port   int
		writer core.AgentID
	}
	infos := make([]chInfo, 0, len(a.s.channels))
	for _, c := range a.s.channels {
		infos = append(infos, chInfo{c.name, c.port, c.writer})
	}
	a.s.chMu.Unlock()
	if len(infos) == 0 {
		return "no reverse-shell channels are open. Use `open` (name, port) to start one.", false
	}
	// One exec gathers the out.log sizes for all live channels.
	script := fmt.Sprintf(`for d in %s/*/; do [ -d "$d" ] || continue; n=$(basename "$d"); sz=$(wc -c < "$d/out.log" 2>/dev/null || echo 0); echo "$n $sz"; done`,
		shSingleQuote(a.s.chanRoot()))
	out, _ := a.revExec(script)
	sizes := map[string]string{}
	for _, ln := range strings.Split(out, "\n") {
		if f := strings.Fields(ln); len(f) == 2 {
			sizes[f[0]] = f[1]
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d reverse-shell channel(s):\n", len(infos))
	for _, c := range infos {
		sz := sizes[c.name]
		if sz == "" {
			sz = "0"
		}
		fmt.Fprintf(&b, "  %s — port %d, listening, %s bytes captured, writable by %s\n", c.name, c.port, sz, writerLabel(c.writer))
	}
	return strings.TrimRight(b.String(), "\n"), false
}

// revClose stops the listener and removes the channel. Closing is destructive for
// everyone sharing the channel, so only the current writer or the orchestrator may do
// it (an unheld channel may be closed by anyone).
func (a *Agent) revClose(name string) (string, bool) {
	a.s.chMu.Lock()
	ch := a.s.channels[name]
	if ch == nil {
		a.s.chMu.Unlock()
		return fmt.Sprintf("revshell close: no open channel %q", name), true
	}
	if ch.writer != "" && ch.writer != a.id && a.id != core.Main {
		holder := ch.writer
		a.s.chMu.Unlock()
		return fmt.Sprintf("revshell close: channel %q is held by %q; only its writer or the orchestrator (Main) may close it.", name, holder), true
	}
	delete(a.s.channels, name)
	a.s.chMu.Unlock()
	ch.cancel() // stops nc, the FIFO holder, and the loop together
	// Best-effort cleanup of the FIFO so a later `open` of the same name is clean.
	_, _ = a.revExec(fmt.Sprintf(`rm -f %s/in 2>/dev/null; true`, shSingleQuote(ch.dir)))
	a.s.emit(event.Channel{Name: name, Port: ch.port, Dir: ch.dir, Open: false})
	return fmt.Sprintf("closed reverse-shell channel %q (was on port %d). The listener is stopped; the output log is kept in the workspace.", name, ch.port), false
}

// revExec runs a short-lived helper command inside the sandbox and returns its
// combined output. It runs as the agent's egress user, bounded by revExecTimeout so
// a wedged FIFO can never hang the agent.
func (a *Agent) revExec(script string) (string, error) {
	ctx, cancel := context.WithTimeout(a.jobBaseCtx(), revExecTimeout)
	defer cancel()
	w := &bgWriter{}
	_, err := a.s.Sandbox.Exec(ctx, a.id, sandbox.Cmd{Argv: []string{"bash", "-lc", script}, Policy: a.cfg.Policy}, w)
	if err != nil && ctx.Err() == nil {
		return w.String(), err
	}
	return w.String(), nil
}

// revListenScript builds the listener loop for a channel. It creates the FIFO and
// output log, holds the FIFO's write end open on fd 3 so nc never sees EOF between
// commands (and so writes from `send`/the pane reach the shell), and loops nc so a
// client that disconnects can reconnect without restarting the whole channel. It
// prefers ncat when present and falls back to traditional netcat.
func revListenScript(dir, name string, port int) string {
	d := shSingleQuote(dir)
	n := shSingleQuote(name)
	return fmt.Sprintf(`set +e
mkdir -p %[1]s || exit 1
rm -f %[1]s/in
mkfifo %[1]s/in || exit 1
chmod 666 %[1]s/in 2>/dev/null
: > %[1]s/out.log
chmod 666 %[1]s/out.log 2>/dev/null
printf '[oline] channel %%s listening on port %[3]d\n' %[2]s >> %[1]s/out.log
exec 3<>%[1]s/in
while true; do
  if command -v ncat >/dev/null 2>&1; then
    ncat -lvn %[3]d <&3 >>%[1]s/out.log 2>&1
  else
    nc -lvnp %[3]d <&3 >>%[1]s/out.log 2>&1
  fi
  printf '\n[oline] listener on port %[3]d restarting\n' >> %[1]s/out.log
  sleep 1
done`, d, n, port)
}

// shSingleQuote single-quotes s for safe interpolation into a bash script.
func shSingleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
