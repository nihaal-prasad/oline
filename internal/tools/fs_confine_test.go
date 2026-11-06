package tools

import (
	"context"
	"strings"
	"testing"

	"github.com/nihaal-prasad/oline/internal/sandbox"
)

const confWS = "/opt/oline/engagements/e1"

// TestWriteAllowedLexical pins the exact write-confinement boundary, including the
// prefix-confusion cases a naive HasPrefix(ap, base) (no trailing slash) would wrongly
// admit — e.g. a sibling whose name begins with the workspace's name. A regression
// weakening this check would let an agent write into another engagement's workspace.
func TestWriteAllowedLexical(t *testing.T) {
	c := Call{Workspace: confWS} // default policy: confined (not Privileged)
	allow := []string{
		confWS,            // the workspace dir itself
		confWS + "/x",     // a file in it
		confWS + "/a/b/c", // nested
	}
	for _, ap := range allow {
		if !writeAllowed(c, ap) {
			t.Errorf("writeAllowed(%q) = false, want true (inside the workspace)", ap)
		}
	}
	deny := []string{
		confWS + "evil",                        // prefix confusion: ".../e1evil" is NOT in ".../e1/"
		confWS + "x/y",                         // ".../e1x/..." prefix confusion
		"/opt/oline/engagements/e10/x",         // sibling whose name starts with e1
		"/opt/oline/engagements/e2/x",          // a different engagement
		"/opt/oline/submission/x",              // elsewhere under /opt/oline
		"/opt/oline",                           // the parent
		"/etc/passwd", "/root/.ssh/id", "/tmp", // elsewhere on the guest
	}
	for _, ap := range deny {
		if writeAllowed(c, ap) {
			t.Errorf("writeAllowed(%q) = true, want false (outside the workspace)", ap)
		}
	}
	// The Privileged installer may write anywhere.
	p := Call{Workspace: confWS, Policy: sandbox.Privileged}
	for _, ap := range deny {
		if !writeAllowed(p, ap) {
			t.Errorf("Privileged writeAllowed(%q) = false, want true (installer is exempt)", ap)
		}
	}
}

// TestAbsResolvesAndTraversalIsCaught: a relative path resolves under the workspace,
// path.Clean collapses any ".." — and a traversal that climbs OUT of the workspace
// resolves to a path the confinement check then rejects (the lexical defense holds
// even though Clean itself happily produces the escaped path).
func TestAbsResolvesAndTraversalIsCaught(t *testing.T) {
	c := Call{Workspace: confWS}
	if got := abs(c, "notes.md"); got != confWS+"/notes.md" {
		t.Errorf("abs(relative) = %q, want %q", got, confWS+"/notes.md")
	}
	if got := abs(c, "a/../b"); got != confWS+"/b" {
		t.Errorf("abs(a/../b) = %q, want %q", got, confWS+"/b")
	}
	if got := abs(c, "/etc/passwd"); got != "/etc/passwd" {
		t.Errorf("abs(absolute) = %q, want it unchanged", got)
	}
	// Climbing out of the workspace with ".." must resolve outside and be rejected.
	for _, esc := range []string{"../../../../etc/passwd", "sub/../../../../etc/passwd", "../e2/loot"} {
		ap := abs(c, esc)
		if strings.HasPrefix(ap, confWS+"/") || ap == confWS {
			t.Errorf("abs(%q) = %q still looks inside the workspace", esc, ap)
		}
		if writeAllowed(c, ap) {
			t.Errorf("a ..-traversal escape %q (-> %q) must be rejected by writeAllowed", esc, ap)
		}
	}
}

// TestOffLimits pins the host-/mnt guard: exactly /mnt and anything under /mnt/ is
// off-limits; lookalikes that are NOT the mount root are allowed.
func TestOffLimits(t *testing.T) {
	block := []string{"/mnt", "/mnt/", "/mnt/c", "/mnt/c/Windows/System32", "/mnt/d/x"}
	for _, p := range block {
		if !offLimits(p) {
			t.Errorf("offLimits(%q) = false, want true (host mount)", p)
		}
	}
	ok := []string{"/mnt-data", "/mntfoo", "/opt/oline/mnt", "/home/user/x", "/", confWS + "/x"}
	for _, p := range ok {
		if offLimits(p) {
			t.Errorf("offLimits(%q) = true, want false (not the host mount)", p)
		}
	}
}

// TestFileToolsBlockMntHost: write and edit reject a host /mnt path (including one
// reached by ".." traversal), and read rejects /mnt too — so no file tool reads or
// writes the Windows host through a drive mount.
func TestFileToolsBlockMntHost(t *testing.T) {
	c := func(args string) Call { return Call{Agent: "web#1", Workspace: confWS, Args: []byte(args)} }

	wr, _ := Write{}.Execute(context.Background(), c(`{"path":"/mnt/c/Windows/x","content":"x"}`))
	if !wr.IsError || !strings.Contains(wr.Content, "off-limits") {
		t.Errorf("write to /mnt must be blocked as off-limits, got %q", wr.Content)
	}
	// Reached by traversal (abs cleans it to /mnt/c/x).
	wrT, _ := Write{}.Execute(context.Background(), c(`{"path":"../../../../mnt/c/x","content":"x"}`))
	if !wrT.IsError || !strings.Contains(wrT.Content, "off-limits") {
		t.Errorf("a ..-traversal into /mnt must be blocked as off-limits, got %q", wrT.Content)
	}
	ed, _ := Edit{}.Execute(context.Background(), c(`{"path":"/mnt/c/x","old_string":"a","new_string":"b"}`))
	if !ed.IsError || !strings.Contains(ed.Content, "off-limits") {
		t.Errorf("edit of /mnt must be blocked as off-limits, got %q", ed.Content)
	}
	rd, _ := Read{}.Execute(context.Background(), c(`{"path":"/mnt/c/Windows/System32/config"}`))
	if !rd.IsError || !strings.Contains(rd.Content, "off-limits") {
		t.Errorf("read of /mnt must be blocked as off-limits, got %q", rd.Content)
	}
}

// TestReadNotWorkspaceConfined documents the deliberate asymmetry: READS may reach
// anywhere in the guest (only the host /mnt is blocked), so a confined agent reading
// /etc/passwd is NOT rejected by confinement — it proceeds (and here fails only
// because no sandbox is attached). Writes, by contrast, are workspace-confined.
func TestReadNotWorkspaceConfined(t *testing.T) {
	rd, _ := Read{}.Execute(context.Background(), Call{Agent: "web#1", Workspace: confWS, Args: []byte(`{"path":"/etc/passwd"}`)})
	if strings.Contains(rd.Content, "off-limits") || strings.Contains(rd.Content, "outside the engagement workspace") {
		t.Errorf("a read outside the workspace (not /mnt) must not be confinement-blocked, got %q", rd.Content)
	}
	if !strings.Contains(rd.Content, "no sandbox") {
		t.Errorf("read should proceed past confinement to the missing-sandbox error, got %q", rd.Content)
	}
}
