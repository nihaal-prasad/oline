//go:build windows

package sandbox_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/nihaal-prasad/oline/internal/sandbox"
)

// TestLiveTerminalPTY_WSL opens a PTY-backed terminal in real WSL and proves the
// child is attached to a true pty: `tty` prints a /dev/pts path (not "not a tty").
// It also exercises writing keystrokes in and resizing. Gated on OLINE_TEST_WSL;
// benign (runs `tty` then exits). Needs `script` (util-linux) in the distro.
func TestLiveTerminalPTY_WSL(t *testing.T) {
	distro := testDistro(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	h, err := sandbox.NewWSL().Open(ctx, distro)
	if err != nil {
		t.Fatal(err)
	}
	inter, ok := h.(sandbox.Interactive)
	if !ok {
		t.Fatal("WSL handle does not implement Interactive")
	}

	buf := &syncBuf{}
	sess, err := inter.OpenTerminal(ctx, "test", sandbox.Cmd{Argv: []string{"bash", "-li"}}, 24, 80, buf)
	if err != nil {
		t.Fatalf("OpenTerminal: %v", err)
	}
	defer sess.Close()

	if _, err := sess.Write([]byte("tty\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	time.Sleep(1500 * time.Millisecond)
	_, _ = sess.Write([]byte("exit\n"))
	time.Sleep(300 * time.Millisecond)

	got := buf.String()
	if !strings.Contains(got, "/dev/pts/") {
		t.Errorf("terminal did not report a pty (tty output lacks /dev/pts): %q", got)
	}
	if strings.Contains(strings.ToLower(got), "not a tty") {
		t.Errorf("program saw no tty: %q", got)
	}
	if err := sess.Resize(30, 100); err != nil { // WSL returns nil (size fixed at open)
		t.Errorf("Resize returned error: %v", err)
	}
	t.Logf("pty terminal output:\n%s", got)
}
