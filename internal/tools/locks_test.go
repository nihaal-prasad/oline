package tools

import (
	"context"
	"strings"
	"testing"
)

func TestFileLocksConflict(t *testing.T) {
	fl := NewFileLocks()
	if h, ok := fl.Acquire("/x", "a"); !ok || h != "a" {
		t.Fatalf("first acquire should succeed: holder=%s ok=%v", h, ok)
	}
	if h, ok := fl.Acquire("/x", "b"); ok || h != "a" {
		t.Fatalf("conflicting acquire should fail and name holder a: holder=%s ok=%v", h, ok)
	}
	if _, ok := fl.Acquire("/x", "a"); !ok {
		t.Error("re-acquiring a path you hold should succeed")
	}
	fl.Release("/x", "a")
	if h, ok := fl.Acquire("/x", "b"); !ok || h != "b" {
		t.Fatalf("after release, b should acquire: holder=%s ok=%v", h, ok)
	}
}

// TestLockKeyCanonical checks different spellings of the same file map to one lock
// key, so the cross-agent write lock actually catches concurrent writers.
func TestLockKeyCanonical(t *testing.T) {
	want := lockKey("/opt/oline/report.md")
	for _, p := range []string{"report.md", "./report.md", "/opt/oline/report.md", "/opt/oline/sub/../report.md"} {
		if got := lockKey(p); got != want {
			t.Errorf("lockKey(%q) = %q, want %q", p, got, want)
		}
	}
	fl := NewFileLocks()
	fl.Acquire(lockKey("report.md"), "a")
	if holder, ok := fl.Acquire(lockKey("./report.md"), "b"); ok || holder != "a" {
		t.Errorf("different spellings of the same file should conflict (holder=%s ok=%v)", holder, ok)
	}
}

// TestWriteRejectsConcurrentWriter checks the write tool returns an error naming
// the holding subagent when another agent already holds the file.
func TestWriteRejectsConcurrentWriter(t *testing.T) {
	fl := NewFileLocks()
	fl.Acquire("/opt/oline/engagements/report.md", "recon#1") // another agent holds it

	res, _ := Write{}.Execute(context.Background(), Call{
		Agent: "web#1",
		Args:  []byte(`{"path":"/opt/oline/engagements/report.md","content":"x"}`),
		Locks: fl,
		// no Sandbox: the conflict is detected before any write is attempted
	})
	if !res.IsError {
		t.Fatal("concurrent write should be rejected")
	}
	if !strings.Contains(res.Content, "recon#1") {
		t.Errorf("error should name the holding subagent (recon#1): %q", res.Content)
	}
}
