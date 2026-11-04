package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"path"
	"regexp"
	"strings"
	"time"

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

// revChannel is one live reverse-shell channel.
type revChannel struct {
	name   string
	port   int
	dir    string // absolute guest path to the channel's working directory
	cancel context.CancelFunc
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
		return reply(a.revOpen(name, args.Port))
	case "send":
		return reply(a.revSend(name, args.Data))
	case "read":
		return reply(a.revRead(name, args.Bytes))
	case "list":
		return reply(a.revList())
	case "close":
		return reply(a.revClose(name))
	case "":
		return reply("revshell: 'action' is required (open | send | read | list | close)", true)
	default:
		return reply(fmt.Sprintf("revshell: unknown action %q (use open | send | read | list | close)", action), true)
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
	ch := &revChannel{name: name, port: port, dir: dir, cancel: cancel}
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
// that follows shortly after.
func (a *Agent) revSend(name, data string) (string, bool) {
	ch := a.s.getChannel(name)
	if ch == nil {
		return fmt.Sprintf("revshell send: no open channel %q (use `list` to see channels, `open` to create one)", name), true
	}
	if data == "" {
		return "revshell send: 'data' (the command line to send) is required", true
	}
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
	chans := make([]*revChannel, 0, len(a.s.channels))
	for _, c := range a.s.channels {
		chans = append(chans, c)
	}
	a.s.chMu.Unlock()
	if len(chans) == 0 {
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
	fmt.Fprintf(&b, "%d reverse-shell channel(s):\n", len(chans))
	for _, c := range chans {
		sz := sizes[c.name]
		if sz == "" {
			sz = "0"
		}
		fmt.Fprintf(&b, "  %s — port %d, listening, %s bytes captured\n", c.name, c.port, sz)
	}
	return strings.TrimRight(b.String(), "\n"), false
}

// revClose stops the listener and removes the channel.
func (a *Agent) revClose(name string) (string, bool) {
	a.s.chMu.Lock()
	ch := a.s.channels[name]
	if ch != nil {
		delete(a.s.channels, name)
	}
	a.s.chMu.Unlock()
	if ch == nil {
		return fmt.Sprintf("revshell close: no open channel %q", name), true
	}
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
