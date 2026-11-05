package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"

	"github.com/nihaal-prasad/oline/internal/sandbox"
)

// TestEnvResumeNoResumable: choosing Resume when nothing is resumable shows a clear
// "nothing to resume" note instead of silently listing only create-new entries.
func TestEnvResumeNoResumable(t *testing.T) {
	m := testModel(t)
	m.w, m.h = 120, 40
	m.step = wEnv
	m.scanned = true
	m.resume = true
	m.cands = []sandbox.Candidate{
		{ID: sandbox.QEMUDebianID, Name: "Create a new … QEMU VM", Backend: sandbox.BackendQEMU, Managed: true, NeedsCreate: true},
		{ID: newWSLSentinel, Name: "Create a new … WSL distro", Backend: sandbox.BackendWSL, Managed: true, NeedsCreate: true},
	}
	out := ansi.Strip(m.envBodyView())
	if !strings.Contains(out, "No previous engagement is available to resume") {
		t.Errorf("want a 'nothing to resume' note when resuming with no resumable env, got:\n%s", out)
	}

	// With a saved QEMU VM present, the note disappears.
	m.cands = append(m.cands, sandbox.Candidate{ID: "engagement-x", Name: "engagement-x", Backend: sandbox.BackendQEMU, Managed: true, Engagement: "engagement-x"})
	if strings.Contains(ansi.Strip(m.envBodyView()), "No previous engagement is available") {
		t.Error("the note must not show when a resumable saved VM is listed")
	}
}
