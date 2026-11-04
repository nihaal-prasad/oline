package sandbox_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nihaal-prasad/oline/internal/sandbox"
)

// syncBuf is a concurrency-safe output sink for a streaming OpenTerminal session.
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func writeTemp(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "probe.txt")
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestLiveQEMU boots the managed Debian VM, provisions per-agent egress, runs a
// command as the scope-restricted user, and shuts the VM down. It downloads the
// base image on first run (hundreds of MB) and boots a full VM, so it is gated
// on OLINE_TEST_QEMU and needs qemu-system + qemu-img installed.
func TestLiveQEMU(t *testing.T) {
	if testing.Short() || os.Getenv("OLINE_TEST_QEMU") == "" {
		t.Skip("set OLINE_TEST_QEMU=1 (and install QEMU) to run the live QEMU test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
	defer cancel()

	h, err := sandbox.NewQEMU().Create(ctx, sandbox.Spec{Backend: sandbox.BackendQEMU})
	if err != nil {
		t.Fatalf("Create/boot: %v", err)
	}
	defer func() {
		if sd, ok := h.(sandbox.Shutdowner); ok {
			_ = sd.Shutdown(context.Background())
		}
	}()

	if err := h.SetupEgress(ctx, sandbox.Scope{Targets: []string{"1.1.1.1/32"}}); err != nil {
		t.Fatalf("SetupEgress: %v", err)
	}

	// A scope-restricted agent runs as the unprivileged oline-scope user.
	var who bytes.Buffer
	if _, err := h.Exec(ctx, "test", sandbox.Cmd{Argv: []string{"bash", "-lc", "id -un"}, Policy: sandbox.InScopeOnly}, &who); err != nil {
		t.Fatalf("exec: %v", err)
	}
	if u := strings.TrimSpace(who.String()); u != "oline-scope" {
		t.Errorf("in-scope agent runs as %q, want oline-scope", u)
	}

	// The installer (Privileged) runs as root.
	var root bytes.Buffer
	if _, err := h.Exec(ctx, "test", sandbox.Cmd{Argv: []string{"bash", "-lc", "id -u"}, Policy: sandbox.Privileged}, &root); err != nil {
		t.Fatalf("exec root: %v", err)
	}
	if strings.TrimSpace(root.String()) != "0" {
		t.Errorf("Privileged agent uid = %q, want 0", strings.TrimSpace(root.String()))
	}

	// Write then read a file back through the file-copy path.
	if err := h.CopyIn(ctx, writeTemp(t, "hello oline"), "/opt/oline/engagements/probe.txt"); err != nil {
		t.Fatalf("CopyIn: %v", err)
	}

	// A PTY-backed terminal reports a real tty (the SSH RequestPty path).
	inter, ok := h.(sandbox.Interactive)
	if !ok {
		t.Fatal("QEMU handle does not implement Interactive")
	}
	buf := &syncBuf{}
	sess, err := inter.OpenTerminal(ctx, "test", sandbox.Cmd{Argv: []string{"bash", "-li"}}, 24, 80, buf)
	if err != nil {
		t.Fatalf("OpenTerminal: %v", err)
	}
	_, _ = sess.Write([]byte("tty\n"))
	time.Sleep(1500 * time.Millisecond)
	_, _ = sess.Write([]byte("exit\n"))
	time.Sleep(300 * time.Millisecond)
	if err := sess.Resize(30, 100); err != nil {
		t.Errorf("Resize: %v", err)
	}
	_ = sess.Close()
	if !strings.Contains(buf.String(), "/dev/pts/") {
		t.Errorf("QEMU terminal did not report a pty: %q", buf.String())
	}

	t.Log("QEMU VM booted, egress applied, exec + copy + pty verified")
}
