package tui

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"testing"

	"github.com/nihaal-prasad/oline/internal/sandbox"
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
	m.resume = true             // user chose "resume a previous engagement" at the Start step
	m.afterFreshSandboxConfig() // after sizing a freshly-created VM

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
	m.afterFreshSandboxConfig()
	if m.step != wScope || m.resume {
		t.Errorf("with no saved history a new VM should fall back to a clean start (wScope, resume=false), got step=%v resume=%v", m.step, m.resume)
	}
}

// TestFreshSandbox: a fresh QEMU overlay (reuseDisk=false) and a newly-created WSL
// distro (createWSL=true) both count as fresh — so a resumed engagement's workspace
// is restored into them — while a resumed disk / reused distro does not.
func TestFreshSandbox(t *testing.T) {
	m := testModel(t)
	qemu := sandbox.Candidate{Backend: sandbox.BackendQEMU}
	wsl := sandbox.Candidate{Backend: sandbox.BackendWSL}

	m.reuseDisk, m.createWSL = false, false
	if !m.freshSandbox(qemu) {
		t.Error("a QEMU fresh overlay (reuseDisk=false) should be a fresh sandbox")
	}
	m.reuseDisk = true
	if m.freshSandbox(qemu) {
		t.Error("a resumed QEMU disk (reuseDisk=true) must not be a fresh sandbox")
	}
	m.reuseDisk = false

	if m.freshSandbox(wsl) {
		t.Error("a reused WSL distro (createWSL=false) must not be a fresh sandbox")
	}
	m.createWSL = true
	if !m.freshSandbox(wsl) {
		t.Error("a newly-created WSL distro (createWSL=true) should be a fresh sandbox")
	}
}

// TestExtractWorkspaceTar: the nested workspace.tar.gz is pulled out of a backup
// bundle, and a bundle without one is an error (so the restore degrades gracefully).
func TestExtractWorkspaceTar(t *testing.T) {
	dir := t.TempDir()
	want := []byte("inner-workspace-archive-bytes")

	writeBundle := func(name string, members map[string][]byte) string {
		p := filepath.Join(dir, name)
		f, err := os.Create(p)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		gz := gzip.NewWriter(f)
		tw := tar.NewWriter(gz)
		for n, b := range members {
			if err := tw.WriteHeader(&tar.Header{Name: n, Mode: 0o600, Size: int64(len(b))}); err != nil {
				t.Fatal(err)
			}
			if _, err := tw.Write(b); err != nil {
				t.Fatal(err)
			}
		}
		if err := tw.Close(); err != nil {
			t.Fatal(err)
		}
		if err := gz.Close(); err != nil {
			t.Fatal(err)
		}
		return p
	}

	good := writeBundle("good.tar.gz", map[string][]byte{
		"metadata.json":    []byte(`{}`),
		"workspace.tar.gz": want,
	})
	got, err := extractWorkspaceTar(good)
	if err != nil {
		t.Fatalf("extractWorkspaceTar: %v", err)
	}
	defer os.Remove(got)
	data, err := os.ReadFile(got)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(data, want) {
		t.Errorf("extracted workspace bytes = %q, want %q", data, want)
	}

	none := writeBundle("none.tar.gz", map[string][]byte{"history.json": []byte(`[]`)})
	if _, err := extractWorkspaceTar(none); err == nil {
		t.Error("a bundle with no workspace member should return an error")
	}
	if _, err := extractWorkspaceTar(filepath.Join(dir, "missing.tar.gz")); err == nil {
		t.Error("a missing bundle should return an error")
	}
}
