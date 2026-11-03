package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"

	"github.com/nihaal-prasad/oline/internal/core"
	"github.com/nihaal-prasad/oline/internal/event"
)

// TestAnswerTargetRouting verifies a pending `ask` captures the next message as
// an answer (the comprehension-gate bug), and that a long question is wrapped.
func TestAnswerTargetRouting(t *testing.T) {
	m := testModel(t)
	m.w, m.h = 100, 40
	m.addAgent(core.Main)
	m.paneRoot = newLeaf(core.Main)
	m.focusPane = m.paneRoot
	m.running = true
	m.resize()

	// No pending question -> normal steer (no answer target).
	if m.answerTarget(core.Main) != "" {
		t.Error("answerTarget should be empty with no pending question")
	}

	// A long comprehension-gate question from Main.
	q := strings.Repeat("explain exactly what this exploitation step will do and why ", 6)
	m.apply(event.UserQuestion{From: core.Main, Text: q})
	if !m.awaitingAnswer[core.Main] {
		t.Fatal("Main not marked awaiting an answer after UserQuestion")
	}
	if got := m.answerTarget(core.Main); got != core.Main {
		t.Errorf("answerTarget = %q, want Main", got)
	}

	// The rendered question must be WRAPPED (no line wider than the pane).
	content := m.transcripts[core.Main].render(m.focusPane.w-2, true, nil)
	for _, ln := range strings.Split(content, "\n") {
		if w := lipgloss.Width(ln); w > m.focusPane.w-2 {
			t.Errorf("transcript line not wrapped: width %d > %d", w, m.focusPane.w-2)
		}
	}
}
