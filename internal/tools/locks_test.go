package tools

import (
	"context"
	"strings"
	"testing"

	"github.com/nihaal-prasad/oline/internal/sandbox"
)

// TestWriteConfinedToEngagementWorkspace checks that when a call carries the
// engagement workspace, writes are confined to it: a relative path resolves inside
// it, a path anywhere else (even elsewhere under /opt/oline) is rejected, and the
// installer is exempt.
func TestWriteConfinedToEngagementWorkspace(t *testing.T) {
	const ws = "/opt/oline/engagements/e1"
	// Inside the workspace: not blocked (reaches the missing-sandbox error instead).
	for _, p := range []string{ws + "/notes.md", "notes.md", "sub/x.txt", ws + "/sub/../x"} {
		res, _ := Write{}.Execute(context.Background(), Call{
			Agent: "web#1", Workspace: ws,
			Args: []byte(`{"path":"` + p + `","content":"x"}`),
		})
		if strings.Contains(res.Content, "outside the engagement workspace") {
			t.Errorf("write to %q (inside the workspace) must not be blocked: %q", p, res.Content)
		}
	}
	// Outside the engagement workspace, including elsewhere under /opt/oline: rejected.
	for _, p := range []string{"/opt/oline/submission/x", "/opt/oline/engagements/other/x", "/opt/oline/x", "/etc/x", "../x"} {
		res, _ := Write{}.Execute(context.Background(), Call{
			Agent: "web#1", Workspace: ws,
			Args: []byte(`{"path":"` + p + `","content":"x"}`),
		})
		if !res.IsError || !strings.Contains(res.Content, "outside the engagement workspace") {
			t.Errorf("write to %q should be rejected (outside the workspace), got %q", p, res.Content)
		}
	}
	// The installer is exempt even with a workspace set.
	res, _ := Write{}.Execute(context.Background(), Call{
		Agent: "installer#1", Workspace: ws, Policy: sandbox.Privileged,
		Args: []byte(`{"path":"/usr/local/bin/tool","content":"x"}`),
	})
	if strings.Contains(res.Content, "outside the engagement workspace") {
		t.Errorf("the installer should be exempt from workspace confinement: %q", res.Content)
	}
}

// TestWriteConfinedToWorkspace checks a normal agent can only write inside the
// workspace root; a path outside it is rejected (before any sandbox use), and the
// Privileged installer is exempt. Edit uses the same guard.
func TestWriteConfinedToWorkspace(t *testing.T) {
	outside := []string{"/etc/passwd", "/tmp/x", "/root/.ssh/authorized_keys", "../escape", "/opt/other/x"}
	for _, p := range outside {
		res, _ := Write{}.Execute(context.Background(), Call{
			Agent: "web#1",
			Args:  []byte(`{"path":"` + p + `","content":"x"}`),
		})
		if !res.IsError || !strings.Contains(res.Content, "outside the engagement workspace") {
			t.Errorf("write to %q should be rejected as outside the workspace, got %q", p, res.Content)
		}
		er, _ := Edit{}.Execute(context.Background(), Call{
			Agent: "web#1",
			Args:  []byte(`{"path":"` + p + `","old_string":"a","new_string":"b"}`),
		})
		if !er.IsError || !strings.Contains(er.Content, "outside the engagement workspace") {
			t.Errorf("edit of %q should be rejected as outside the workspace, got %q", p, er.Content)
		}
	}

	// Inside the workspace: not blocked by the confinement check (it then fails on
	// the missing sandbox, a different error).
	res, _ := Write{}.Execute(context.Background(), Call{
		Agent: "web#1",
		Args:  []byte(`{"path":"/opt/oline/engagements/e1/notes.md","content":"x"}`),
	})
	if strings.Contains(res.Content, "outside the engagement workspace") {
		t.Errorf("write inside the workspace must not be blocked: %q", res.Content)
	}
	// A bare relative path resolves inside the workspace too.
	res, _ = Write{}.Execute(context.Background(), Call{
		Agent: "web#1",
		Args:  []byte(`{"path":"notes.md","content":"x"}`),
	})
	if strings.Contains(res.Content, "outside the engagement workspace") {
		t.Errorf("relative write inside the workspace must not be blocked: %q", res.Content)
	}

	// The Privileged installer may write anywhere.
	res, _ = Write{}.Execute(context.Background(), Call{
		Agent:  "installer#1",
		Policy: sandbox.Privileged,
		Args:   []byte(`{"path":"/usr/local/bin/tool","content":"x"}`),
	})
	if strings.Contains(res.Content, "outside the engagement workspace") {
		t.Errorf("the installer should be exempt from workspace confinement: %q", res.Content)
	}
}

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
