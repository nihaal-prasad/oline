package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"io"

	"github.com/nihaal-prasad/oline/internal/sandbox"
)

// Bash runs a shell command INSIDE the sandbox (never on the host). Output
// streams to the caller via Call.Emit and is also returned.
type Bash struct{}

func (Bash) Name() string { return "bash" }

func (Bash) Schema() Schema {
	return Schema{
		Name:        "bash",
		Description: "Run a shell command inside the sandboxed environment and return its combined output and exit code. All execution is confined to the sandbox.",
		Parameters:  json.RawMessage(`{"type":"object","properties":{"cmd":{"type":"string","description":"the shell command to run"}},"required":["cmd"]}`),
	}
}

func (Bash) Execute(ctx context.Context, c Call) (Result, error) {
	var args struct {
		Cmd string `json:"cmd"`
	}
	if err := json.Unmarshal(c.Args, &args); err != nil {
		return Result{Content: "bash: invalid arguments: " + err.Error(), IsError: true}, nil
	}
	if args.Cmd == "" {
		return Result{Content: "bash: 'cmd' is required", IsError: true}, nil
	}
	if c.Sandbox == nil {
		return Result{Content: "bash: no sandbox attached", IsError: true}, nil
	}

	w := &emitWriter{emit: c.Emit}
	st, err := c.Sandbox.Exec(ctx, c.Agent, sandbox.Cmd{Argv: []string{"bash", "-lc", args.Cmd}, Policy: c.Policy}, w)
	if err != nil {
		return Result{Content: w.buf.String() + "\n" + err.Error(), IsError: true}, nil
	}
	content := fmt.Sprintf("%s\n[exit %d]", w.buf.String(), st.Code)
	return Result{Content: content, IsError: st.Code != 0}, nil
}

// emitWriter accumulates output and forwards chunks to the TUI as they arrive.
type emitWriter struct {
	emit func(Result)
	buf  limitedBuilder
}

func (w *emitWriter) Write(p []byte) (int, error) {
	w.buf.Write(p)
	if w.emit != nil {
		w.emit(Result{Content: string(p)})
	}
	return len(p), nil
}

var _ io.Writer = (*emitWriter)(nil)

// limitedBuilder caps retained output so a runaway command can't exhaust memory.
type limitedBuilder struct {
	b   []byte
	max int
}

func (l *limitedBuilder) Write(p []byte) (int, error) {
	if l.max == 0 {
		l.max = maxToolOutput
	}
	if len(l.b) < l.max {
		room := l.max - len(l.b)
		if room >= len(p) {
			l.b = append(l.b, p...)
		} else {
			l.b = append(l.b, p[:room]...)
			l.b = append(l.b, "\n[...output truncated...]"...)
		}
	}
	return len(p), nil
}

func (l *limitedBuilder) String() string { return string(l.b) }
