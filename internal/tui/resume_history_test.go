package tui

import (
	"os"
	"path/filepath"
	"testing"
)

// TestNewVMKeepsResumeHistory: choosing "resume" and then creating a NEW QEMU VM must
// still offer the previous engagement's host-saved history to load (reuse of the disk
// and loading of history are decoupled).
func TestNewVMKeepsResumeHistory(t *testing.T) {
	home := t.TempDir()
	t.Setenv("OLINE_HOME", home)
	hd := filepath.Join(home, "history")
	if err := os.MkdirAll(hd, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hd, "engagement-old.json"), []byte(`{"engagement":"engagement-old"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	m := testModel(t)
	m.resume = true      // user chose "resume a previous engagement" at the Start step
	m.afterNewVMConfig() // after sizing a freshly-created VM

	if m.step != wResume {
		t.Fatalf("a new VM should still offer the previous-engagement step when resuming, got %v", m.step)
	}
	if len(m.engagements) == 0 || m.engagements[0] != "engagement-old" {
		t.Errorf("the saved engagement should be offered to load, got %v", m.engagements)
	}
	if !m.resume {
		t.Error("the resume intent must be preserved when creating a new VM")
	}
	if m.reuseDisk {
		t.Error("a freshly-created VM must not be marked as reusing a saved disk")
	}
}

// TestNewVMNoSavedHistory: with nothing saved, creating a new VM falls back to a clean
// start rather than a dead-end empty selection.
func TestNewVMNoSavedHistory(t *testing.T) {
	t.Setenv("OLINE_HOME", t.TempDir())
	m := testModel(t)
	m.resume = true
	m.afterNewVMConfig()
	if m.step != wScope || m.resume {
		t.Errorf("with no saved history a new VM should fall back to a clean start (wScope, resume=false), got step=%v resume=%v", m.step, m.resume)
	}
}
