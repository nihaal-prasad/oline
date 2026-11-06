package tui

import (
	"os"
	"path/filepath"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/nihaal-prasad/oline/internal/sandbox"
)

// TestWizardNameStepBlocksCollision drives the engagement-name setup step itself (not
// just the helper): a blank name and a name whose host artifacts already exist both
// keep the wizard on wName with an error, so a new engagement can never silently
// overwrite an earlier one's logs/history/backup. A unique name advances.
func TestWizardNameStepBlocksCollision(t *testing.T) {
	m, _ := tempHomeModel(t)
	var tm tea.Model = m
	tm, _ = tm.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	m = tm.(*model)

	// Seed a colliding prior engagement (a backup bundle named "taken").
	bdir := m.store.BackupDir()
	if err := os.MkdirAll(bdir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bdir, "taken.tar.gz"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	m.step = wName

	// Blank name: refused, stays on wName.
	m.nameIn.SetValue("")
	tm, _ = m.Update(enter())
	m = tm.(*model)
	if m.step != wName || m.wizErr == "" {
		t.Fatalf("blank name: step=%v wizErr=%q, want to stay on wName with an error", m.step, m.wizErr)
	}

	// Colliding name: refused, stays on wName, engagement not set.
	m.nameIn.SetValue("taken")
	tm, _ = m.Update(enter())
	m = tm.(*model)
	if m.step != wName || m.wizErr == "" {
		t.Fatalf("colliding name: step=%v wizErr=%q, want to stay on wName with an error", m.step, m.wizErr)
	}
	if m.engagement == "taken" {
		t.Error("a colliding name must not be accepted as the engagement")
	}

	// A fresh, unique name advances past the name step.
	m.nameIn.SetValue("fresh-eng")
	tm, _ = m.Update(enter())
	m = tm.(*model)
	if m.step != wVPN {
		t.Fatalf("unique name: step=%v, want wVPN", m.step)
	}
	if m.engagement != "fresh-eng" || m.wizErr != "" {
		t.Errorf("unique name not accepted cleanly: engagement=%q wizErr=%q", m.engagement, m.wizErr)
	}
}

// TestWizardResumeSelectsSavedEngagement exercises the resume list choosing an actual
// saved engagement (not the host-import fallback): selecting a listed name sets it as
// the engagement and moves on to confirm scope, while the last row (import) routes to
// the host-path import step. A QEMU candidate is used so no WSL scope-file read runs.
func TestWizardResumeSelectsSavedEngagement(t *testing.T) {
	m, _ := tempHomeModel(t)
	var tm tea.Model = m
	tm, _ = tm.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	m = tm.(*model)

	m.resume = true
	m.cands = []sandbox.Candidate{{ID: "vm", Backend: sandbox.BackendQEMU}}
	m.envSel = 0
	m.engagements = []string{"acme", "beta"}
	m.step = wResume

	// Select the second saved engagement (index 1).
	m.engSel = 1
	tm, _ = m.Update(enter())
	m = tm.(*model)
	if m.engagement != "beta" {
		t.Fatalf("resume selection: engagement=%q, want beta", m.engagement)
	}
	// No saved history under the temp home, so it goes straight to scope confirmation.
	if m.step != wScope {
		t.Fatalf("after selecting a saved engagement: step=%v, want wScope", m.step)
	}

	// Re-enter the list and pick the final (import) row.
	m.step = wResume
	m.engSel = len(m.engagements) // the "import from host" option
	tm, _ = m.Update(enter())
	m = tm.(*model)
	if m.step != wImport {
		t.Fatalf("selecting the import row: step=%v, want wImport", m.step)
	}
}
