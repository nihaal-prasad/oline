package engine

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/nihaal-prasad/oline/internal/event"
	"github.com/nihaal-prasad/oline/internal/llm"
)

// Operator terminals: the engine-side mirror of what the human operator is doing
// in their own interactive panes in the TUI — the line-oriented shell panes
// (Ctrl-b $) and the full-screen PTY terminal panes (Ctrl-b T / the reverse-shell
// catcher). The TUI pushes each pane's content here as it changes, and the
// `terminal` tool lets agents read it, the same way `revshell read` lets them read
// a reverse-shell channel. It is READ-ONLY: agents observe, they never type into
// the operator's terminals.

// opTermMaxBytes caps the retained content for one line-oriented shell, so a very
// chatty operator shell can't grow the mirror without bound (a full-screen terminal
// keeps only its latest screen, which is naturally bounded).
const opTermMaxBytes = 64 << 10

// operatorTerminal is one mirrored operator pane.
type operatorTerminal struct {
	kind    string // "shell" (line-oriented history) or "terminal" (latest full screen)
	content string
	updated time.Time
}

// recordOperator folds new content for an operator terminal into the mirror. For a
// line shell (replace=false) text is appended and trimmed to the most recent
// opTermMaxBytes; for a full terminal (replace=true) it replaces the latest screen.
func (s *Session) recordOperator(id, kind, text string, replace bool) {
	if id == "" {
		return
	}
	s.opTermMu.Lock()
	defer s.opTermMu.Unlock()
	if s.opTerms == nil {
		s.opTerms = map[string]*operatorTerminal{}
	}
	ot := s.opTerms[id]
	if ot == nil {
		ot = &operatorTerminal{}
		s.opTerms[id] = ot
		s.opTermOrd = append(s.opTermOrd, id)
	}
	ot.kind = kind
	if replace {
		ot.content = text
	} else {
		ot.content += text
		if len(ot.content) > opTermMaxBytes {
			ot.content = ot.content[len(ot.content)-opTermMaxBytes:]
			// Drop the now-partial leading line so the trim never starts mid-line.
			if i := strings.IndexByte(ot.content, '\n'); i >= 0 && i+1 <= len(ot.content) {
				ot.content = ot.content[i+1:]
			}
		}
	}
	ot.updated = time.Now()
}

// RecordOperatorLine appends one line to a line-oriented operator shell's mirror.
func (s *Session) RecordOperatorLine(id, line string) {
	s.recordOperator(id, "shell", line+"\n", false)
}

// SetOperatorScreen replaces a full-screen operator terminal's mirror with its
// current screen contents (plain text).
func (s *Session) SetOperatorScreen(id, screen string) {
	s.recordOperator(id, "terminal", screen, true)
}

// ForgetOperator drops a terminal from the mirror when its pane closes, so it no
// longer appears in `terminal list`.
func (s *Session) ForgetOperator(id string) {
	s.opTermMu.Lock()
	defer s.opTermMu.Unlock()
	if _, ok := s.opTerms[id]; !ok {
		return
	}
	delete(s.opTerms, id)
	for i, n := range s.opTermOrd {
		if n == id {
			s.opTermOrd = append(s.opTermOrd[:i], s.opTermOrd[i+1:]...)
			break
		}
	}
}

// readOperatorTerminal returns a terminal's mirrored content (and whether it exists).
func (s *Session) readOperatorTerminal(id string) (string, bool) {
	s.opTermMu.Lock()
	defer s.opTermMu.Unlock()
	ot := s.opTerms[id]
	if ot == nil {
		return "", false
	}
	return ot.content, true
}

// listOperatorTerminals renders the open operator terminals for the `terminal list`
// action: id, kind, retained size, and how long since each last changed.
func (s *Session) listOperatorTerminals() string {
	s.opTermMu.Lock()
	defer s.opTermMu.Unlock()
	if len(s.opTermOrd) == 0 {
		return "no operator terminals are open. The user opens a line shell with Ctrl-b $ and a full terminal with Ctrl-b T; there is nothing to read until they do."
	}
	var b strings.Builder
	b.WriteString("operator terminals (what the user is doing in the UI; read-only):\n")
	for _, id := range s.opTermOrd {
		ot := s.opTerms[id]
		if ot == nil {
			continue
		}
		fmt.Fprintf(&b, "  %s  [%s]  %d bytes, last activity %s ago\n", id, ot.kind, len(ot.content), time.Since(ot.updated).Round(time.Second))
	}
	b.WriteString("Use `terminal read <id>` to see a terminal's contents.")
	return b.String()
}

// doTerminal implements the read-only `terminal` tool (list | read): it lets an
// agent see what the operator is doing in their own shell/terminal panes, mirroring
// the `revshell read`/`list` actions for reverse-shell channels.
func (a *Agent) doTerminal(tc llm.ToolCall) (string, bool) {
	a.s.emit(event.ToolStarted{Agent: a.id, Tool: "terminal", ArgsPreview: preview(tc.Arguments), Args: tc.Arguments})
	var args struct {
		Action string `json:"action"`
		Name   string `json:"name"`
		Bytes  int    `json:"bytes"`
	}
	if err := json.Unmarshal([]byte(tc.Arguments), &args); err != nil {
		return "terminal: invalid arguments: " + err.Error(), true
	}
	switch strings.ToLower(strings.TrimSpace(args.Action)) {
	case "list", "":
		return a.s.listOperatorTerminals(), false
	case "read":
		name := strings.TrimSpace(args.Name)
		if name == "" {
			return "terminal read: 'name' is required (an id from `terminal list`, e.g. Shell#1 or Term#1)", true
		}
		content, ok := a.s.readOperatorTerminal(name)
		if !ok {
			return fmt.Sprintf("terminal read: no open operator terminal %q (use `list` to see them)", name), true
		}
		if n := args.Bytes; n > 0 && len(content) > n {
			content = content[len(content)-n:]
		}
		if strings.TrimSpace(content) == "" {
			return fmt.Sprintf("operator terminal %q is empty so far (nothing shown yet).", name), false
		}
		return content, false
	default:
		return fmt.Sprintf("terminal: unknown action %q (use list | read)", args.Action), true
	}
}
