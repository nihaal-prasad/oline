//go:build windows

package sandbox_test

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/nihaal-prasad/oline/internal/sandbox"
)

// These tests touch real WSL. Gated on OLINE_TEST_WSL=<distro> so CI without WSL
// skips them. Run with: $env:OLINE_TEST_WSL="kali-linux"; go test ./internal/sandbox -run WSL -v
func testDistro(t *testing.T) string {
	d := os.Getenv("OLINE_TEST_WSL")
	if d == "" {
		t.Skip("OLINE_TEST_WSL not set; skipping live WSL test")
	}
	return d
}

func TestWSLScanAndExec(t *testing.T) {
	distro := testDistro(t)
	sb := sandbox.NewWSL()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cands, err := sb.Scan(ctx)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	found := false
	for _, c := range cands {
		t.Logf("candidate: id=%s running=%v managed=%v", c.ID, c.Running, c.Managed)
		if c.ID == distro {
			found = true
		}
	}
	if !found {
		t.Fatalf("distro %q not found by Scan", distro)
	}

	h, err := sb.Open(ctx, distro)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	var out bytes.Buffer
	st, err := h.Exec(ctx, "test-agent", sandbox.Cmd{Argv: []string{"bash", "-lc", "id"}}, &out)
	if err != nil {
		t.Fatalf("Exec id: %v", err)
	}
	if st.Code != 0 {
		t.Errorf("exit code = %d, want 0", st.Code)
	}
	if !strings.Contains(out.String(), "uid=") {
		t.Errorf("unexpected id output: %q", out.String())
	}
	t.Logf("id => %s", strings.TrimSpace(out.String()))
}

func TestWSLExecCancel(t *testing.T) {
	distro := testDistro(t)
	sb := sandbox.NewWSL()
	h, err := sb.Open(context.Background(), distro)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	var out bytes.Buffer
	done := make(chan error, 1)
	start := time.Now()
	go func() {
		_, e := h.Exec(ctx, "test-agent", sandbox.Cmd{Argv: []string{"bash", "-lc", "sleep 30"}}, &out)
		done <- e
	}()
	time.Sleep(1 * time.Second)
	cancel() // simulate a pause/abort
	select {
	case err := <-done:
		if time.Since(start) > 5*time.Second {
			t.Errorf("cancel took too long: %v", time.Since(start))
		}
		if ctx.Err() == nil {
			t.Errorf("context not cancelled; exec returned %v", err)
		}
		t.Logf("cancelled sleep after %v (err=%v)", time.Since(start), err)
	case <-time.After(10 * time.Second):
		t.Fatal("exec did not return within 10s of cancel")
	}
}

func TestWSLDestroyRefusesUnmanaged(t *testing.T) {
	sb := sandbox.NewWSL()
	if err := sb.Destroy(context.Background(), "kali-linux"); err == nil {
		t.Error("Destroy should refuse a non-oline- distro")
	}
}
