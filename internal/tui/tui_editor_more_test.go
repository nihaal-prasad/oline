package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
)

// TestEditorRejectsInvalidInput covers the validation branches of saveItem: a bad
// value in any guarded field must set an error status and must NOT clobber the
// previously-saved-good configuration. These rejection paths had no coverage, yet a
// slip here would write a malformed tier/level/price that start-time validation then
// trips over.
func TestEditorRejectsInvalidInput(t *testing.T) {
	m, _ := tempHomeModel(t)
	m.openEditor()

	// Main: save a valid baseline, then confirm each invalid field is rejected and
	// leaves the baseline intact.
	m.eSel = findItem(m, "main", "Primary Agent Modifications")
	m.buildFields()
	m.eFields[0].SetValue("big")
	m.eFields[1].SetValue("high")
	m.saveItem()
	if oc := m.store.Orchestrator(); oc.Tier != "big" || oc.ThinkingLevel != "high" {
		t.Fatalf("precondition: a valid Main save failed: %+v", oc)
	}

	bad := []struct {
		field int
		val   string
		want  string
	}{
		{0, "huge", "model tier must be"},
		{1, "sometimes", "thinking-level must be"},
		{2, "-5", "max context tokens must be"},
		{2, "abc", "max context tokens must be"},
	}
	for _, bc := range bad {
		m.buildFields() // reseed inputs from the saved-good state
		m.eFields[bc.field].SetValue(bc.val)
		m.saveItem()
		if got := ansi.Strip(m.eStatus); !strings.Contains(got, bc.want) {
			t.Errorf("Main field %d = %q: status = %q, want error containing %q", bc.field, bc.val, got, bc.want)
		}
		if oc := m.store.Orchestrator(); oc.Tier != "big" || oc.ThinkingLevel != "high" {
			t.Errorf("a rejected Main save clobbered the saved-good config: %+v", oc)
		}
	}

	// TUI: a non-boolean toggle is rejected.
	m.eSel = findItem(m, "tui", "User Preferences")
	m.buildFields()
	m.eFields[0].SetValue("maybe")
	m.eFields[1].SetValue("false")
	m.saveItem()
	if got := ansi.Strip(m.eStatus); !strings.Contains(got, "must be true or false") {
		t.Errorf("TUI non-bool: status = %q, want a true/false error", got)
	}

	// Role price: a negative price is rejected by costFromFields.
	withFireworks(m.store)
	const glm = "fireworks/accounts/fireworks/models/glm-5p3-flash"
	m.store.SetModelRoles(glm, glm, glm)
	m.openEditor() // providers changed; rebuild the item list
	m.eSel = findItem(m, "role", "big")
	m.buildFields()
	m.eFields[1].SetValue("-1") // input $/1M
	m.saveItem()
	if got := ansi.Strip(m.eStatus); !strings.Contains(got, "non-negative numbers") {
		t.Errorf("role negative price: status = %q, want a non-negative-number error", got)
	}
}

// TestEditorNavigationKeys exercises the keyboard model of the settings editor through
// updateEditor: list navigation, opening an item (focus moves onto a field), Tab
// cycling fields, and the two-stage Esc (field editing -> list -> close). A regression
// that trapped focus or swallowed Esc would leave the operator stuck in the editor.
func TestEditorNavigationKeys(t *testing.T) {
	m, _ := tempHomeModel(t)
	m.openEditor()
	if !m.editing || m.eFocus != -1 {
		t.Fatalf("openEditor state: editing=%v eFocus=%d, want editing with no field focused", m.editing, m.eFocus)
	}
	send := func(t tea.KeyType) { m.updateEditor(tea.KeyMsg{Type: t}) }

	start := m.eSel
	send(tea.KeyDown)
	if m.eSel != start+1 {
		t.Fatalf("Down did not advance the selection: %d -> %d", start, m.eSel)
	}

	send(tea.KeyEnter)
	if m.eFocus != 0 {
		t.Fatalf("Enter did not focus the first field: eFocus=%d", m.eFocus)
	}

	if m.numFields() > 1 {
		send(tea.KeyTab)
		if m.eFocus != 1 {
			t.Errorf("Tab did not move to field 1: eFocus=%d", m.eFocus)
		}
	}

	send(tea.KeyEsc) // leave field editing, back to the item list
	if m.eFocus != -1 {
		t.Errorf("Esc from a field did not return to the list: eFocus=%d", m.eFocus)
	}
	send(tea.KeyEsc) // leave the editor entirely (not editorOnly => stays in the app)
	if m.editing {
		t.Error("a second Esc did not close the editor")
	}
}
