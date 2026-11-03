//go:build windows

package sandbox_test

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nihaal-prasad/oline/internal/sandbox"
)

// TestLiveResumePlacement verifies resuming files that already live in the
// environment: a path the user points to gets copied into the canonical
// workspace, and engagements under oline's own roots are listed/resolved.
func TestLiveResumePlacement(t *testing.T) {
	distro := os.Getenv("OLINE_TEST_WSL")
	if distro == "" {
		t.Skip("OLINE_TEST_WSL not set")
	}
	ctx := context.Background()
	defer sandbox.RunAsRoot(ctx, distro,
		"rm -rf /opt/oline/src-elsewhere "+sandbox.WSLWorkspace+"/placed-eng /root/engagements/listed-eng")

	// (A) Point to an arbitrary path already in the environment -> copied canonical.
	if _, err := sandbox.RunAsRoot(ctx, distro, "set -e\nmkdir -p /opt/oline/src-elsewhere\n"+
		"printf 'In scope: 1.2.3.4\\n' > /opt/oline/src-elsewhere/00-scope.md\necho OK"); err != nil {
		t.Fatal(err)
	}
	if err := sandbox.CopyWSLDirToWorkspace(ctx, distro, "/opt/oline/src-elsewhere", "placed-eng"); err != nil {
		t.Fatalf("CopyWSLDirToWorkspace: %v", err)
	}
	got, err := sandbox.ReadWSLFile(ctx, distro, sandbox.WSLWorkspace+"/placed-eng/00-scope.md")
	if err != nil || !strings.Contains(got, "1.2.3.4") {
		t.Errorf("placed engagement scope = %q err %v", got, err)
	}

	// (B) An engagement under a per-user root is listed and resolved.
	if _, err := sandbox.RunAsRoot(ctx, distro, "mkdir -p /root/engagements/listed-eng && touch /root/engagements/listed-eng/x"); err != nil {
		t.Fatal(err)
	}
	names, err := sandbox.ListWSLEngagements(ctx, distro)
	if err != nil || !contains(names, "listed-eng") {
		t.Errorf("ListWSLEngagements missing listed-eng: %v (err %v)", names, err)
	}
	p, err := sandbox.ResolveWSLEngagementPath(ctx, distro, "listed-eng")
	if err != nil || p != "/root/engagements/listed-eng" {
		t.Errorf("ResolveWSLEngagementPath = %q err %v", p, err)
	}
}

// TestLiveBackup verifies the program-driven host backup: it tars the engagement
// out of the sandbox into a host .tar.gz (the agents never touch the host).
func TestLiveBackup(t *testing.T) {
	distro := os.Getenv("OLINE_TEST_WSL")
	if distro == "" {
		t.Skip("OLINE_TEST_WSL not set")
	}
	ctx := context.Background()
	defer sandbox.RunAsRoot(ctx, distro, "rm -rf "+sandbox.WSLWorkspace+"/bkptest")
	if _, err := sandbox.RunAsRoot(ctx, distro, "set -e\nmkdir -p "+sandbox.WSLWorkspace+
		"/bkptest\nprintf 'hello backup\\n' > "+sandbox.WSLWorkspace+"/bkptest/note.txt\necho OK"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	dest := filepath.Join(t.TempDir(), "bkptest", "snap.tar.gz")
	if err := sandbox.BackupWSLEngagement(ctx, distro, "bkptest", dest); err != nil {
		t.Fatalf("backup: %v", err)
	}
	f, err := os.Open(dest)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gz)
	found := false
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if strings.HasSuffix(h.Name, "note.txt") {
			b, _ := io.ReadAll(tr)
			if strings.Contains(string(b), "hello backup") {
				found = true
			}
		}
	}
	if !found {
		t.Error("backup archive does not contain bkptest/note.txt with its content")
	}
}

// TestLiveEngagementMgmt verifies list/read/import of engagements in WSL, and
// the VPN helpers' graceful behavior. Reversible; gated on OLINE_TEST_WSL.
func TestLiveEngagementMgmt(t *testing.T) {
	distro := os.Getenv("OLINE_TEST_WSL")
	if distro == "" {
		t.Skip("OLINE_TEST_WSL not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	defer sandbox.RunAsRoot(context.Background(), distro,
		"rm -rf "+sandbox.WSLWorkspace+"/testeng "+sandbox.WSLWorkspace+"/imported1 /opt/oline/vpn")

	// Seed a dummy engagement directly in the workspace.
	if _, err := sandbox.RunAsRoot(ctx, distro, `set -e
mkdir -p `+sandbox.WSLWorkspace+`/testeng
printf 'In scope: 203.0.113.7\nGoal: demo\n' > `+sandbox.WSLWorkspace+`/testeng/00-scope.md
echo SEEDED`); err != nil {
		t.Fatalf("seed: %v", err)
	}

	names, err := sandbox.ListWSLEngagements(ctx, distro)
	if err != nil {
		t.Fatal(err)
	}
	if !contains(names, "testeng") {
		t.Errorf("ListWSLEngagements = %v, want to contain testeng", names)
	}

	scopeDoc, err := sandbox.ReadWSLFile(ctx, distro, sandbox.WSLWorkspace+"/testeng/00-scope.md")
	if err != nil || !strings.Contains(scopeDoc, "203.0.113.7") {
		t.Errorf("ReadWSLFile = %q, err %v", scopeDoc, err)
	}

	// Import a host directory as a new engagement.
	hostDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(hostDir, "notes.md"), []byte("prior findings\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := sandbox.ImportWSLEngagement(ctx, distro, hostDir, "imported1"); err != nil {
		t.Fatalf("ImportWSLEngagement: %v", err)
	}
	got, err := sandbox.ReadWSLFile(ctx, distro, sandbox.WSLWorkspace+"/imported1/notes.md")
	if err != nil || !strings.Contains(got, "prior findings") {
		t.Errorf("imported file = %q, err %v", got, err)
	}

	// VPN helpers: a missing config fails cleanly (we never start a real VPN).
	if _, err := sandbox.StartWSLVPN(ctx, distro, "/opt/oline/does-not-exist.ovpn"); err == nil {
		t.Error("StartWSLVPN with a missing config should error")
	}
	t.Log("engagement list/read/import + VPN helpers OK")
}

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}
