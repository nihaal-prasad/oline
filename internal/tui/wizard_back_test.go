package tui

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// TestWizardEscGoesBack: Esc returns to the previous setup question (restoring its
// input focus), and is a no-op on the first question.
func TestWizardEscGoesBack(t *testing.T) {
	m := testModel(t)
	esc := tea.KeyMsg{Type: tea.KeyEsc}

	m.step = wScope
	m.wizHistory = nil

	// Enter advances wScope -> wGoal and records the back-stack.
	if _, _ = m.updateWizard(enter()); m.step != wGoal {
		t.Fatalf("Enter on wScope should advance to wGoal, got %v", m.step)
	}
	if len(m.wizHistory) != 1 || m.wizHistory[0] != wScope {
		t.Fatalf("a forward transition should push the prior step, history=%v", m.wizHistory)
	}

	// Esc returns to wScope, pops the stack, and refocuses its input.
	if _, _ = m.updateWizard(esc); m.step != wScope {
		t.Fatalf("Esc should return to wScope, got %v", m.step)
	}
	if len(m.wizHistory) != 0 {
		t.Fatalf("history should be empty after going back, got %v", m.wizHistory)
	}
	if !m.scopeIn.Focused() {
		t.Error("going back to wScope should refocus the scope input")
	}

	// Esc with no history (first question) is a no-op.
	if _, _ = m.updateWizard(esc); m.step != wScope {
		t.Errorf("Esc with empty history should be a no-op, got %v", m.step)
	}
}
