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

// TestLiveEgress proves, through the real Go sandbox path, that per-agent egress
// is enforced by root-owned firewall rules an unprivileged agent cannot change.
// 1.1.1.1 stands in for the in-scope target. Reversible: teardown restores rules
// and removes the oline users. Gated on OLINE_TEST_WSL.
func TestLiveEgress(t *testing.T) {
	distro := os.Getenv("OLINE_TEST_WSL")
	if distro == "" {
		t.Skip("OLINE_TEST_WSL not set; skipping live egress test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	h, err := sandbox.NewWSL().Open(ctx, distro)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.SetupEgress(ctx, sandbox.Scope{Targets: []string{"1.1.1.1/32"}}); err != nil {
		t.Fatalf("SetupEgress: %v", err)
	}
	defer func() {
		if err := sandbox.TeardownWSLEgress(distro); err != nil {
			t.Logf("teardown warning: %v", err)
		}
	}()

	connect := func(policy sandbox.EgressPolicy, ip string) string {
		var out bytes.Buffer
		script := "timeout 3 bash -c 'echo > /dev/tcp/" + ip + "/53' 2>/dev/null && echo OK || echo BLOCKED"
		_, err := h.Exec(ctx, "test", sandbox.Cmd{Argv: []string{"bash", "-lc", script}, Policy: policy}, &out)
		if err != nil {
			t.Fatalf("exec: %v", err)
		}
		return strings.TrimSpace(out.String())
	}

	if got := connect(sandbox.InScopeOnly, "1.1.1.1"); got != "OK" {
		t.Errorf("in-scope agent -> target = %q, want OK", got)
	}
	if got := connect(sandbox.InScopeOnly, "8.8.8.8"); got != "BLOCKED" {
		t.Errorf("in-scope agent -> internet = %q, want BLOCKED", got)
	}
	if got := connect(sandbox.InternetNoTarget, "1.1.1.1"); got != "BLOCKED" {
		t.Errorf("internet agent -> target = %q, want BLOCKED", got)
	}
	if got := connect(sandbox.InternetNoTarget, "8.8.8.8"); got != "OK" {
		t.Errorf("internet agent -> internet = %q, want OK", got)
	}

	// The agent runs as an unprivileged user and cannot change the rules.
	var who bytes.Buffer
	_, _ = h.Exec(ctx, "test", sandbox.Cmd{Argv: []string{"bash", "-lc", "whoami"}, Policy: sandbox.InScopeOnly}, &who)
	if u := strings.TrimSpace(who.String()); u != "oline-scope" {
		t.Errorf("in-scope agent runs as %q, want oline-scope", u)
	}
	var flush bytes.Buffer
	_, _ = h.Exec(ctx, "test", sandbox.Cmd{Argv: []string{"bash", "-lc", "iptables -F OUTPUT 2>&1 | head -1"}, Policy: sandbox.InScopeOnly}, &flush)
	if !strings.Contains(strings.ToLower(flush.String()), "permission denied") {
		t.Errorf("agent could attempt iptables flush unexpectedly: %q", flush.String())
	}
	t.Log("per-agent egress enforced and un-bypassable by the agent")
}

// TestLiveEgressTeardownClears proves the per-agent firewall is fully removed after
// teardown: the OLINE_OUT chain exists once SetupEgress has run, and is gone after
// TeardownWSLEgress. Gated on OLINE_TEST_WSL; reversible.
func TestLiveEgressTeardownClears(t *testing.T) {
	distro := os.Getenv("OLINE_TEST_WSL")
	if distro == "" {
		t.Skip("OLINE_TEST_WSL not set; skipping live egress-teardown test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	h, err := sandbox.NewWSL().Open(ctx, distro)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.SetupEgress(ctx, sandbox.Scope{Targets: []string{"1.1.1.1/32"}}); err != nil {
		t.Fatalf("SetupEgress: %v", err)
	}
	chain := func() string {
		var out bytes.Buffer
		_, _ = h.Exec(ctx, "test", sandbox.Cmd{Argv: []string{"bash", "-lc", "iptables -L OLINE_OUT 2>&1 | head -1"}, Policy: sandbox.Privileged}, &out)
		return out.String()
	}
	if !strings.Contains(chain(), "OLINE_OUT") {
		t.Fatalf("OLINE_OUT chain missing after SetupEgress: %q", chain())
	}
	if err := sandbox.TeardownWSLEgress(distro); err != nil {
		t.Fatalf("teardown: %v", err)
	}
	if strings.Contains(chain(), "Chain OLINE_OUT") {
		t.Errorf("OLINE_OUT chain still present after teardown: %q", chain())
	}
}

// TestLivePrivilegedIsRoot proves the installer's Privileged egress policy runs
// as root inside the sandbox (so it can install software), while every other
// policy runs as an unprivileged, scope-restricted user with no root. Gated on
// OLINE_TEST_WSL; reversible (teardown removes the oline users and rules).
func TestLivePrivilegedIsRoot(t *testing.T) {
	distro := os.Getenv("OLINE_TEST_WSL")
	if distro == "" {
		t.Skip("OLINE_TEST_WSL not set; skipping live privileged-root test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	h, err := sandbox.NewWSL().Open(ctx, distro)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.SetupEgress(ctx, sandbox.Scope{Targets: []string{"1.1.1.1/32"}}); err != nil {
		t.Fatalf("SetupEgress: %v", err)
	}
	defer func() {
		if err := sandbox.TeardownWSLEgress(distro); err != nil {
			t.Logf("teardown warning: %v", err)
		}
	}()

	whoami := func(policy sandbox.EgressPolicy) string {
		var out bytes.Buffer
		_, err := h.Exec(ctx, "test", sandbox.Cmd{Argv: []string{"bash", "-lc", "id -un; id -u"}, Policy: policy}, &out)
		if err != nil {
			t.Fatalf("exec: %v", err)
		}
		return strings.TrimSpace(out.String())
	}

	// Privileged (the installer) -> root, uid 0.
	if got := whoami(sandbox.Privileged); !strings.Contains(got, "root") || !strings.Contains(got, "0") {
		t.Errorf("Privileged agent identity = %q, want root / uid 0", got)
	}
	// Every other policy is unprivileged (not root, non-zero uid).
	for _, p := range []sandbox.EgressPolicy{sandbox.InScopeOnly, sandbox.InternetNoTarget, sandbox.Isolated} {
		got := whoami(p)
		if strings.Contains(got, "root") || strings.HasSuffix(got, "\n0") || got == "root\n0" {
			t.Errorf("policy %v runs as %q; only the installer (Privileged) may be root", p, got)
		}
	}
	t.Log("installer (Privileged) runs as root; all other agents are unprivileged")
}
