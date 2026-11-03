package engine

import (
	"context"
	"strings"
	"sync"

	"github.com/nihaal-prasad/oline/internal/event"
	"github.com/nihaal-prasad/oline/internal/sandbox"
)

// Command jobs (foreground bash + background bash_bg).
//
// Each command runs as ONE streaming sandbox Exec inside a goroutine whose context
// lives for the ENGAGEMENT (jobBaseCtx), not the agent's per-turn context. That is
// what keeps a command — and any children it spawns (a server and its subprocesses)
// — alive across the agent's turns: the Exec (and its wsl.exe relay / SSH channel)
// stays open until the command finishes, is explicitly stopped (StopCommands, or a
// pause of a foreground command), or the engagement ends. Cancelling the job's
// context stops the command together with its children. There is no detaching /
// setsid / pidfile machinery — that proved unreliable across backends.

// bgJob tracks one running command: the cancel that stops it (and its children),
// plus the flags that make the foreground->background promotion handoff race-free.
type bgJob struct {
	cancel context.CancelFunc

	mu       sync.Mutex
	promoted bool // the foreground gave up waiting; the goroutine posts MsgJobDone
	reported bool // the goroutine already delivered the result to the foreground
}

// cmdResult is a finished command's outcome.
type cmdResult struct {
	out      string
	code     int
	finished bool // true: ran to completion; false: cancelled / stopped
}

func (a *Agent) jobBaseCtx() context.Context {
	if a.s.baseCtx != nil {
		return a.s.baseCtx
	}
	return context.Background()
}

// execCommand runs cmd inside the sandbox as the agent's user, streaming its output
// (stdout+stderr merged) to the TUI/history as it arrives, and returns the captured
// output and exit code. A cancelled ctx (stop / pause / engagement-end) marks it as
// not finished; an infrastructure error is surfaced in the output rather than left
// silent.
func (a *Agent) execCommand(ctx context.Context, cmd string) cmdResult {
	w := &bgWriter{emit: func(chunk string) { a.s.emit(event.ToolOutput{Agent: a.id, Chunk: chunk}) }}
	st, err := a.s.Sandbox.Exec(ctx, a.id, sandbox.Cmd{Argv: []string{"bash", "-lc", cmd}, Policy: a.cfg.Policy}, w)
	out := w.String()
	if err != nil && ctx.Err() == nil {
		note := "[sandbox error: " + err.Error() + "]"
		a.s.emit(event.ToolOutput{Agent: a.id, Chunk: note})
		if strings.TrimSpace(out) != "" {
			out = strings.TrimRight(out, "\n") + "\n" + note
		} else {
			out = note
		}
	}
	return cmdResult{out: out, code: st.Code, finished: ctx.Err() == nil}
}

// forgetJob removes a backgrounded job from the agent's live set.
func (a *Agent) forgetJob(id int) {
	a.mu.Lock()
	delete(a.jobs, id)
	a.mu.Unlock()
}

// clearFg clears the foreground slot if it still holds job id.
func (a *Agent) clearFg(id int) {
	a.mu.Lock()
	if a.fgID == id {
		a.fgID, a.fgJob = 0, nil
	}
	a.mu.Unlock()
}
