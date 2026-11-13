package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/glamour"
	"github.com/charmbracelet/glamour/styles"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

// mdThrottle bounds how often the still-streaming text segment is re-rendered
// through glamour, so a fast token stream renders markdown live without a
// full-buffer glamour pass on every chunk.
const mdThrottle = 75 * time.Millisecond

// A transcript is an ordered list of typed segments. Streamed text accumulates
// into "open" segments that are rendered ONCE (and cached), rather than styling
// and re-wrapping every token — which previously split ANSI codes mid-stream and
// scrambled the output.
type segKind int

const (
	segText  segKind = iota // assistant text (markdown once finalized)
	segThink                // reasoning (dim; toggleable)
	segTool                 // a tool call: one-line header + its (collapsed) output
	segLine                 // a discrete line (pre-styled, or styled+wrapped)
	segMsg                  // a received prompt or inter-agent message: one-line header + (collapsed) body, expanded on click — like segTool but with a 🗨️ header
)

type segment struct {
	kind   segKind
	raw    strings.Builder // segTool: the tool's streamed output
	header string          // segTool: the one-line (truncatable) "⚙ tool args" header
	full   string          // segTool: the FULL "⚙ tool <full args>" shown when expanded
	style  *lipgloss.Style // segLine: nil => use raw verbatim; else wrap+apply
	done   bool            // text/think finalized (markdown may render)

	expanded bool // segTool: show the full output rather than just the header line

	// render cache
	cw  int
	out string

	// live-markdown cache for an open (streaming) text segment
	mdOut string
	mdCW  int
	mdAt  time.Time

	// last render position (set by render; used for mouse hit-testing)
	startLine int // first content line this segment occupies
	nLines    int // number of content lines it occupies (0 = hidden)
}

type transcript struct {
	segs []*segment
}

func (t *transcript) last() *segment {
	if len(t.segs) == 0 {
		return nil
	}
	return t.segs[len(t.segs)-1]
}

// openSeg returns the trailing streaming segment of the given kind, creating one
// (after finalizing a different open segment) if needed.
func (t *transcript) openSeg(k segKind) *segment {
	s := t.last()
	if s != nil && s.kind == k && !s.done {
		return s
	}
	t.finalizeOpen()
	s = &segment{kind: k}
	t.segs = append(t.segs, s)
	return s
}

func (t *transcript) appendText(s string) {
	seg := t.openSeg(segText)
	seg.raw.WriteString(s)
	seg.out = ""
}
func (t *transcript) appendThink(s string) {
	seg := t.openSeg(segThink)
	seg.raw.WriteString(s)
	seg.out = ""
}

// startTool opens a new tool-call segment with its one-line header (shown
// collapsed) and the full header (shown when expanded). Subsequent appendToolOut
// calls accumulate that tool's output into the same segment.
func (t *transcript) startTool(header, full string) {
	t.finalizeOpen()
	t.segs = append(t.segs, &segment{kind: segTool, header: header, full: full})
}

func (t *transcript) appendToolOut(s string) {
	seg := t.last()
	if seg == nil || seg.kind != segTool || seg.done {
		t.startTool("⚙  tool", "") // output with no preceding header (shouldn't happen)
		seg = t.last()
	}
	seg.raw.WriteString(s)
	seg.out = ""
}

// startMsg appends a collapsed message/prompt segment: a received prompt (from the
// user or an agent) or an inter-agent message. It shows as a single 🗨️ header line
// with a click-to-expand hint — exactly like a tool call — and reveals the full body
// only when the user clicks it. header is the one-line summary (carry the 🗨️ emoji in
// it); body is the full text shown when expanded.
func (t *transcript) startMsg(header, body string) {
	t.finalizeOpen()
	s := &segment{kind: segMsg, header: header, full: header, done: true}
	s.raw.WriteString(body)
	t.segs = append(t.segs, s)
}

// addRich appends a short, already-styled line (not re-wrapped).
func (t *transcript) addRich(pre string) {
	t.finalizeOpen()
	s := &segment{kind: segLine, done: true}
	s.raw.WriteString(pre)
	t.segs = append(t.segs, s)
}

// addStyled appends a line whose raw text is wrapped to the pane width and styled.
func (t *transcript) addStyled(raw string, style lipgloss.Style) {
	t.finalizeOpen()
	st := style
	s := &segment{kind: segLine, style: &st, done: true}
	s.raw.WriteString(raw)
	t.segs = append(t.segs, s)
}

// finalizeOpen marks the trailing streaming segment complete.
func (t *transcript) finalizeOpen() {
	if s := t.last(); s != nil && !s.done && (s.kind == segText || s.kind == segThink || s.kind == segTool) {
		s.done = true
		s.out = ""
	}
}

// render produces the full viewport content at width w. md is a glamour renderer
// sized for w (nil to skip markdown). Hidden reasoning segments are omitted.
func (t *transcript) render(w int, showThoughts bool, md *glamour.TermRenderer) string {
	var b strings.Builder
	cur := 0
	prev := segKind(-1)
	for _, s := range t.segs {
		var piece string
		switch s.kind {
		case segText:
			// A blank line separates a tool call or a message from the LLM's following text.
			if prev == segTool || prev == segMsg {
				b.WriteByte('\n')
				cur++
			}
			piece = s.renderText(w, md)
		case segThink:
			if !showThoughts {
				s.startLine, s.nLines = cur, 0 // hidden: occupies no lines
				continue                       // leave prev unchanged
			}
			piece = s.renderCached(w, func(raw string) string {
				return styleThink.Render(wrapPlain(raw, w))
			})
		case segTool, segMsg:
			piece = renderTool(s, w)
		case segLine:
			if s.style == nil {
				// Pre-styled line: wrap it to the pane width (lipgloss wrapping is
				// ANSI-aware) so a long one isn't cut off at the edge.
				piece = s.renderCached(w, func(raw string) string {
					return wrapPlain(raw, w)
				})
			} else {
				piece = s.renderCached(w, func(raw string) string {
					return s.style.Render(wrapPlain(raw, w))
				})
			}
		}
		s.startLine = cur
		s.nLines = strings.Count(piece, "\n") + 1
		b.WriteString(piece)
		b.WriteByte('\n')
		cur += s.nLines
		prev = s.kind
	}
	return b.String()
}

// renderTool renders a collapsible segment — a tool call (segTool) or a received
// prompt/message (segMsg) — as a single header line (truncated to the pane width,
// with a click hint). When expanded, the full body follows below the header. The
// header is indented to match the LLM text (both start at column 0).
func renderTool(s *segment, w int) string {
	out := strings.TrimRight(s.raw.String(), "\n")
	hasOut := strings.TrimSpace(out) != ""
	full := s.full
	if full == "" {
		full = s.header
	}
	if s.expanded {
		// Show the WHOLE command (wrapped, never truncated), then the output, then
		// the collapse hint LAST so it clearly marks where the expanded block ends.
		// The call + its parameters render dim (styleMuted); the output renders in
		// the normal foreground (styleToolOut) so the two are in different fonts. The
		// arrow points up (▴) because the expanded text is now above the hint.
		b := styleMuted.Render(wrapPlain(full, w))
		if hasOut {
			b += "\n" + styleToolOut.Render(wrapPlain(out, w))
		}
		b += "\n" + styleAccent.Render("▴ click to collapse")
		return b
	}
	hint := "  ▸ click to expand"
	if hasOut {
		hint = fmt.Sprintf("  ▸ click to expand · %d lines", strings.Count(out, "\n")+1)
	}
	return styleMuted.Render(truncCols(s.header, maxInt(w-lipgloss.Width(hint), 1))) + styleAccent.Render(hint)
}

// truncCols truncates s to at most w display columns, appending "…" if cut.
func truncCols(s string, w int) string {
	if w <= 0 {
		return ""
	}
	if lipgloss.Width(s) <= w {
		return s
	}
	var out []rune
	width := 0
	for _, r := range s {
		rw := lipgloss.Width(string(r))
		if width+rw > w-1 {
			break
		}
		out = append(out, r)
		width += rw
	}
	return string(out) + "…"
}

func (s *segment) renderText(w int, md *glamour.TermRenderer) string {
	if s.done {
		if s.out != "" && s.cw == w {
			return s.out
		}
		s.out, s.cw = s.renderMD(w, md), w
		return s.out
	}
	// Streaming: render markdown ON THE FLY, but throttle the glamour pass so a
	// fast stream doesn't re-render the whole growing buffer on every token.
	if md == nil {
		return wrapPlain(s.raw.String(), w)
	}
	if s.mdOut == "" || s.mdCW != w || time.Since(s.mdAt) >= mdThrottle {
		s.mdOut, s.mdCW, s.mdAt = s.renderMD(w, md), w, time.Now()
	}
	return s.mdOut
}

// renderMD renders the segment's raw text as markdown (falling back to a plain
// wrap if there's no renderer, the text is blank, or rendering fails).
func (s *segment) renderMD(w int, md *glamour.TermRenderer) string {
	raw := s.raw.String()
	if md != nil && strings.TrimSpace(raw) != "" {
		if r, err := md.Render(raw); err == nil {
			// glamour word-wraps to w, but a long unbroken token or a wide table can
			// still exceed w; hard-wrap the result so it can never bleed into the next pane.
			return wrapPlain(strings.Trim(r, "\n"), w)
		}
	}
	return wrapPlain(raw, w)
}

func (s *segment) renderCached(w int, fn func(string) string) string {
	if s.out != "" && s.cw == w {
		return s.out
	}
	s.out, s.cw = fn(s.raw.String()), w
	return s.out
}

// plainText returns the transcript as unstyled plain text (no ANSI, no borders)
// — for cleanly copying a single pane's content without other panes' text.
func (t *transcript) plainText(showThoughts bool) string {
	var b strings.Builder
	for _, s := range t.segs {
		switch s.kind {
		case segText:
			b.WriteString(s.raw.String())
		case segThink:
			if !showThoughts {
				continue
			}
			b.WriteString(s.raw.String())
		case segTool, segMsg:
			h := s.full
			if h == "" {
				h = s.header
			}
			b.WriteString(h)
			if out := strings.TrimRight(s.raw.String(), "\n"); strings.TrimSpace(out) != "" {
				b.WriteByte('\n')
				b.WriteString(out)
			}
		case segLine:
			b.WriteString(ansi.Strip(s.raw.String()))
		}
		b.WriteByte('\n')
	}
	return b.String()
}

// wrapPlain wraps s to w columns so no line can exceed the pane width and bleed
// into an adjacent pane. It uses ansi.Wrap (not lipgloss's Width), because lipgloss
// word-wraps but leaves a single word longer than w intact — and tool output is full
// of such tokens (hashes, base64, long paths, JSON), which then overflow the pane.
// ansi.Wrap hard-breaks those over-long words and is ANSI- and wide-char-aware, so
// styled and UTF-8 content stays intact.
func wrapPlain(s string, w int) string {
	if w <= 0 {
		return s
	}
	return ansi.Wrap(s, w, "")
}

// newMarkdownRenderer builds a glamour renderer wrapped to width w (dark theme).
// The document margin is zeroed so rendered text starts at column 0 — matching
// the tool-call lines, so everything shares one indentation.
func newMarkdownRenderer(w int) *glamour.TermRenderer {
	if w < 10 {
		w = 10
	}
	cfg := styles.DarkStyleConfig
	zero := uint(0)
	cfg.Document.Margin = &zero
	r, err := glamour.NewTermRenderer(glamour.WithStyles(cfg), glamour.WithWordWrap(w))
	if err != nil {
		return nil
	}
	return r
}
