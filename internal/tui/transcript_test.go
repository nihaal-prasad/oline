package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
)

// maxLineWidth returns the widest visible (ANSI-stripped) line in s.
func maxLineWidth(s string) int {
	w := 0
	for _, line := range strings.Split(s, "\n") {
		if lw := lipgloss.Width(line); lw > w {
			w = lw
		}
	}
	return w
}

// TestExpandedToolCallStaysWithinWidth is the regression test for the bug where an
// expanded tool call bled into adjacent panes: a long unbroken token in the command
// or its output must be hard-wrapped so no rendered line exceeds the pane width.
func TestExpandedToolCallStaysWithinWidth(t *testing.T) {
	const w = 40
	longTok := strings.Repeat("A1b2C3d4", 20) // 160 cols, no spaces -> must be broken
	tr := &transcript{}
	tr.startTool("⚙  bash  cat hash.txt", "⚙  bash  cat "+longTok)
	tr.appendToolOut("sha256: " + longTok + "\nok\n")
	tr.finalizeOpen()
	tr.last().expanded = true

	out := tr.render(w, true, nil)
	if got := maxLineWidth(out); got > w {
		t.Errorf("expanded tool call overflows the pane: widest line %d > %d\n%s", got, w, out)
	}
	// The content must still be present (wrapped, not dropped): the token's prefix
	// survives across the wrapped lines.
	if !strings.Contains(strings.ReplaceAll(out, "\n", ""), "A1b2C3d4") {
		t.Errorf("wrapping dropped the tool output content:\n%s", out)
	}
}

// TestMessageSegmentCollapsesAndExpands checks that a received prompt/message renders
// as a single collapsed 🗨️ line with a click-to-expand hint, hides the body until
// expanded, and shows the full body (not the gear emoji) once expanded.
func TestMessageSegmentCollapsesAndExpands(t *testing.T) {
	const w = 60
	body := "line one of the prompt\nline two of the prompt\nline three"
	tr := &transcript{}
	tr.startMsg("🗨️  message from Recon#1", body)

	collapsed := tr.render(w, true, nil)
	if strings.Count(strings.TrimRight(collapsed, "\n"), "\n") != 0 {
		t.Errorf("collapsed message should be a single line, got:\n%s", collapsed)
	}
	if !strings.Contains(collapsed, "🗨") {
		t.Error("collapsed message should carry the chat-bubble emoji")
	}
	if strings.Contains(collapsed, "⚙") {
		t.Error("message must NOT use the gear emoji")
	}
	if !strings.Contains(collapsed, "click to expand") {
		t.Error("collapsed message should show the click-to-expand hint")
	}
	if strings.Contains(collapsed, "line two of the prompt") {
		t.Error("collapsed message must hide the body until expanded")
	}

	tr.last().expanded = true
	expanded := tr.render(w, true, nil)
	if !strings.Contains(expanded, "line two of the prompt") {
		t.Errorf("expanded message should reveal the full body:\n%s", expanded)
	}
	if !strings.Contains(expanded, "click to collapse") {
		t.Error("expanded message should show the collapse hint")
	}
}

// TestStyledLineStaysWithinWidth checks a long unbroken token in a styled transcript
// line (e.g. a recorded-finding line) is hard-wrapped to the pane width too.
func TestStyledLineStaysWithinWidth(t *testing.T) {
	const w = 32
	tr := &transcript{}
	tr.addStyled("✔ finding [HIGH] "+strings.Repeat("x", 100), styleWarn)
	out := tr.render(w, true, nil)
	if got := maxLineWidth(out); got > w {
		t.Errorf("styled line overflows the pane: widest line %d > %d", got, w)
	}
}
