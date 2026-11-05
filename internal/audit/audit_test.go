package audit

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestAppendAndVerify(t *testing.T) {
	path := filepath.Join(t.TempDir(), "e1.audit.jsonl")
	l, err := Open(path, "e1")
	if err != nil {
		t.Fatal(err)
	}
	l.Command("recon#1", "bash", "", "nmap -sV 10.0.0.5")
	l.Command("User", "user.shell", "Shell#1", "id")
	l.Command("recon#1", "revshell.send", "foothold", "whoami")
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	res, err := Verify(path)
	if err != nil {
		t.Fatal(err)
	}
	if !res.OK || res.Entries != 3 {
		t.Fatalf("expected an intact 3-entry log, got %+v", res)
	}
}

func TestVerifyDetectsAlteredRecord(t *testing.T) {
	path := filepath.Join(t.TempDir(), "e1.audit.jsonl")
	l, _ := Open(path, "e1")
	l.Command("recon#1", "bash", "", "nmap 10.0.0.5")
	l.Command("recon#1", "bash", "", "curl http://10.0.0.5")
	_ = l.Close()

	data, _ := os.ReadFile(path)
	tampered := bytes.Replace(data, []byte("nmap 10.0.0.5"), []byte("nmap 10.0.0.9"), 1)
	if bytes.Equal(tampered, data) {
		t.Fatal("test setup: nothing was replaced")
	}
	_ = os.WriteFile(path, tampered, 0o600)

	res, _ := Verify(path)
	if res.OK {
		t.Fatal("verify must detect an altered record")
	}
	if res.BrokenAt != 1 {
		t.Errorf("expected the break at entry 1, got %d (%s)", res.BrokenAt, res.Reason)
	}
}

func TestVerifyDetectsDeletedRecord(t *testing.T) {
	path := filepath.Join(t.TempDir(), "e1.audit.jsonl")
	l, _ := Open(path, "e1")
	for _, c := range []string{"one", "two", "three"} {
		l.Command("x", "bash", "", c)
	}
	_ = l.Close()

	data, _ := os.ReadFile(path)
	lines := bytes.Split(bytes.TrimRight(data, "\n"), []byte("\n"))
	// Drop the middle record: the chain (and the sequence) must no longer verify.
	kept := [][]byte{lines[0], lines[2]}
	_ = os.WriteFile(path, append(bytes.Join(kept, []byte("\n")), '\n'), 0o600)

	res, _ := Verify(path)
	if res.OK {
		t.Fatal("verify must detect a deleted record")
	}
}

func TestOpenResumesChain(t *testing.T) {
	path := filepath.Join(t.TempDir(), "e1.audit.jsonl")
	l, _ := Open(path, "e1")
	l.Command("x", "bash", "", "one")
	_ = l.Close()

	l2, err := Open(path, "e1") // resume: must extend the same chain
	if err != nil {
		t.Fatal(err)
	}
	l2.Command("x", "bash", "", "two")
	_ = l2.Close()

	res, _ := Verify(path)
	if !res.OK || res.Entries != 2 {
		t.Fatalf("a resumed chain should verify with 2 entries, got %+v", res)
	}
}

func TestNilLogNoops(t *testing.T) {
	var l *Log
	l.Command("x", "bash", "", "y") // must not panic
	if l.Path() != "" {
		t.Error("nil log path should be empty")
	}
	if err := l.Close(); err != nil {
		t.Error(err)
	}
}
