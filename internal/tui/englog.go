package tui

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/nihaal-prasad/oline/internal/config"
	"github.com/nihaal-prasad/oline/internal/core"
	"github.com/nihaal-prasad/oline/internal/event"
)

// englog is a per-engagement activity log written to the HOST filesystem (the
// program writes it — agents never touch the host). It records what every agent
// was doing: its thinking, assistant text, tool calls, tool output, state
// changes, spawns, user questions, and notices. It is append-only (so resuming
// an engagement extends the same log) and can be tailed live.
//
// A nil *englog is fully usable — every method no-ops — so a logging failure
// (e.g. the host dir isn't writable) never disrupts the engagement.
type englog struct {
	mu     sync.Mutex
	f      *os.File
	path   string
	pend   map[core.AgentID]*pendEntry // coalesced streaming text/think/output
	redact []string                    // literal secret values to scrub from entries
}

type pendEntry struct {
	kind string
	b    strings.Builder
}

// openEngLog opens (creating as needed) <HostRoot>/logs/<engagement>.log in
// append mode. Returns nil on any failure.
func openEngLog(engagement string) *englog {
	if strings.TrimSpace(engagement) == "" {
		return nil
	}
	dir := config.LogDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil
	}
	path := filepath.Join(dir, safeFileName(engagement)+".log")
	// 0600: the activity log can contain whatever agents print, so keep it as
	// private as the secrets file rather than world-readable.
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil
	}
	return &englog{f: f, path: path, pend: map[core.AgentID]*pendEntry{}}
}

// setRedact registers literal secret values (e.g. resolved API keys) to scrub
// from log entries, in case an agent echoes one. Nil-safe.
func (l *englog) setRedact(secrets []string) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.redact = secrets
}

// scrub replaces any registered secret value with a placeholder. Caller holds mu.
func (l *englog) scrub(s string) string {
	for _, sec := range l.redact {
		if len(sec) >= 8 {
			s = strings.ReplaceAll(s, sec, "«redacted»")
		}
	}
	return s
}

// Path returns the host log file path (empty for a nil logger).
func (l *englog) Path() string {
	if l == nil {
		return ""
	}
	return l.path
}

// banner writes a session delimiter with a free-form context line.
func (l *englog) banner(line string) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	fmt.Fprintf(l.f, "\n========== %s  %s ==========\n", nowStamp(), line)
}

// log records one engine event (coalescing streamed text/thinking/output).
func (l *englog) log(ev event.Event) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	switch e := ev.(type) {
	case event.TokenDelta:
		if e.Text != "" {
			l.stream(e.Agent, "text", e.Text)
		}
		if e.Reasoning != "" {
			l.stream(e.Agent, "think", e.Reasoning)
		}
	case event.ToolStarted:
		l.flush(e.Agent)
		l.writeEntry(e.Agent, "call", strings.TrimSpace(e.Tool+"  "+e.ArgsPreview))
	case event.ToolOutput:
		l.stream(e.Agent, "output", e.Chunk)
	case event.AgentState:
		l.flush(e.Agent)
		l.writeEntry(e.Agent, "state", e.State.String())
	case event.Spawned:
		l.flush(e.Parent)
		l.writeEntry(e.Parent, "spawn", string(e.Child)+" (role "+e.Role+")")
	case event.UserQuestion:
		l.flush(e.From)
		l.writeEntry(e.From, "ask-user", e.Text)
	case event.Notice:
		who := e.Agent
		if who == "" {
			who = core.Main
		}
		l.writeEntry(who, "notice:"+e.Level, e.Text)
	case event.Finding:
		l.flush(e.Agent)
		detail := e.Title
		if e.Target != "" {
			detail += " — " + e.Target
		}
		if e.Detail != "" {
			detail += " — " + e.Detail
		}
		l.writeEntry(e.Agent, "finding:"+e.Severity, detail)
	case event.Summary:
		l.flush(core.Main)
		l.writeEntry(core.Main, "summary", e.Text)
	}
}

// userMsg records a message the user sent to an agent.
func (l *englog) userMsg(to core.AgentID, body string) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.flush(to)
	l.writeEntry(to, "user→", body)
}

// close flushes any pending streams and closes the file.
func (l *englog) close() {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	for a := range l.pend {
		l.flush(a)
	}
	_ = l.f.Close()
}

// stream appends to an agent's coalesced buffer, flushing first if the kind
// changed (e.g. thinking -> text, or text -> output).
func (l *englog) stream(agent core.AgentID, kind, s string) {
	p := l.pend[agent]
	if p == nil {
		p = &pendEntry{}
		l.pend[agent] = p
	}
	if p.b.Len() > 0 && p.kind != kind {
		l.flush(agent)
		p = l.pend[agent]
	}
	p.kind = kind
	p.b.WriteString(s)
}

// flush writes an agent's pending coalesced buffer as one entry. Caller holds mu.
func (l *englog) flush(agent core.AgentID) {
	p := l.pend[agent]
	if p == nil || p.b.Len() == 0 {
		return
	}
	l.writeEntry(agent, p.kind, p.b.String())
	p.b.Reset()
	p.kind = ""
}

// writeEntry writes one "[ts] agent kind: body" record (multi-line bodies are
// indented under a header line). Caller holds mu.
func (l *englog) writeEntry(agent core.AgentID, kind, body string) {
	body = l.scrub(strings.TrimRight(body, "\n"))
	ag := string(agent)
	if ag == "" {
		ag = "-"
	}
	if !strings.Contains(body, "\n") && len(body) <= 120 {
		fmt.Fprintf(l.f, "[%s] %-10s %-9s %s\n", nowStamp(), ag, kind, body)
		return
	}
	fmt.Fprintf(l.f, "[%s] %-10s %-9s\n", nowStamp(), ag, kind)
	for _, ln := range strings.Split(body, "\n") {
		fmt.Fprintf(l.f, "        %s\n", ln)
	}
}

func nowStamp() string { return time.Now().Format("2006-01-02 15:04:05") }

// safeFileName makes an engagement name safe to use as a filename.
func safeFileName(s string) string {
	return strings.Map(func(r rune) rune {
		switch r {
		case '/', '\\', ':', '*', '?', '"', '<', '>', '|', 0:
			return '_'
		}
		return r
	}, strings.TrimSpace(s))
}
