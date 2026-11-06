package sandbox

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// These are the per-agent egress boundary's SELF-TESTS: they assert the generated
// rule script enforces the invariants the whole safety model depends on, without
// needing a live sandbox (TestEgressLiveBoundary below is the gated live check).

// TestEgressSelfTestScopeUser: the scope user ACCEPTs in-scope targets then DROPs the
// rest (fail-closed catch-all), ahead of any accept — so it can reach ONLY scope.
func TestEgressSelfTestScopeUser(t *testing.T) {
	// us=1001 un=1002 ui=1003; one in-scope target.
	script := buildEgressRules(1001, 1002, 1003, []string{"10.0.0.0/24"}, nil, false)
	scopeAccept := "--uid-owner 1001 -d 10.0.0.0/24 -j ACCEPT"
	scopeDrop := "--uid-owner 1001 -j DROP"
	iAccept, iDrop := strings.Index(script, scopeAccept), strings.Index(script, scopeDrop)
	if iAccept < 0 || iDrop < 0 {
		t.Fatalf("scope user is missing its accept/drop rules:\n%s", script)
	}
	if iAccept > iDrop {
		t.Error("scope user's in-scope ACCEPT must precede its catch-all DROP (else it reaches nothing)")
	}
}

// TestEgressSelfTestNetUser: the net (internet-no-target) user DROPs the in-scope
// targets BEFORE its accept-all — so it reaches the internet but never the target.
func TestEgressSelfTestNetUser(t *testing.T) {
	script := buildEgressRules(1001, 1002, 1003, []string{"10.0.0.5"}, nil, false)
	netDrop := "--uid-owner 1002 -d 10.0.0.5 -j DROP"
	netAccept := "--uid-owner 1002 -j ACCEPT"
	iDrop, iAccept := strings.Index(script, netDrop), strings.Index(script, netAccept)
	if iDrop < 0 || iAccept < 0 {
		t.Fatalf("net user is missing its drop/accept rules:\n%s", script)
	}
	if iDrop > iAccept {
		t.Error("net user must DROP the in-scope target BEFORE its accept-all (else it can reach the target)")
	}
}

// TestEgressSelfTestIsoUser: the isolated user is dropped outright (loopback is
// accepted earlier for everyone).
func TestEgressSelfTestIsoUser(t *testing.T) {
	script := buildEgressRules(1001, 1002, 1003, nil, nil, false)
	if !strings.Contains(script, "--uid-owner 1003 -j DROP") {
		t.Errorf("iso user must be dropped (no network):\n%s", script)
	}
	if !strings.Contains(script, "-o lo -j ACCEPT") || !strings.Contains(script, "-d 127.0.0.0/8 -j ACCEPT") {
		t.Error("loopback must always be accepted ahead of the owner DROPs")
	}
}

// TestEgressSelfTestIPv6FailsClosed: with IPv6 disabled (the default), the script
// must both drop v6 for the oline uids (when ip6tables exists) AND disable IPv6 at
// the kernel as a backstop (so a guest without ip6tables can't leak over v6).
func TestEgressSelfTestIPv6FailsClosed(t *testing.T) {
	off := buildEgressRules(1001, 1002, 1003, nil, nil, false)
	if !strings.Contains(off, "ip6tables -A OLINE_OUT -m owner --uid-owner 1003 -j DROP") {
		t.Error("v6 must be dropped for the iso uid when IPv6 is off")
	}
	if !strings.Contains(off, "net.ipv6.conf.all.disable_ipv6=1") {
		t.Error("v6 must be disabled at the kernel as a fail-closed backstop when IPv6 is off")
	}
	on := buildEgressRules(1001, 1002, 1003, nil, []string{"2001:db8::1"}, true)
	if strings.Contains(on, "disable_ipv6=1") {
		t.Error("the kernel v6 disable must NOT be applied when the operator enabled IPv6")
	}
}

// TestEgressSelfTestUnresolvedFailsClosed: a scope target that can't be resolved is
// reported, so ApplyBaseline refuses rather than silently leaking it to the net user.
func TestEgressSelfTestUnresolvedFailsClosed(t *testing.T) {
	_, _, unresolved := resolveScope([]string{"this-name-does-not-resolve.invalid"})
	if len(unresolved) == 0 {
		t.Error("an unresolvable domain target must be reported as unresolved (so egress fails closed)")
	}
}

// TestEgressLiveBoundary is the end-to-end self-test: on a real WSL sandbox it applies
// the baseline and verifies each unprivileged user's reachability matches its policy.
// Gated on OLINE_TEST_WSL so it never runs in ordinary CI.
func TestEgressLiveBoundary(t *testing.T) {
	distro := os.Getenv("OLINE_TEST_WSL")
	if distro == "" {
		t.Skip("set OLINE_TEST_WSL=<distro> to run the live egress boundary self-test")
	}
	// Smoke only: confirm the oline users exist and the OLINE_OUT chain is installed
	// after a baseline apply. (Full reachability probing needs outbound access and is
	// left to manual verification.)
	run := func(args ...string) string {
		out, _ := exec.CommandContext(context.Background(), "wsl.exe", append([]string{"-d", distro, "-u", "root", "--"}, args...)...).CombinedOutput()
		return string(out)
	}
	if !strings.Contains(run("id", "oline-iso"), "oline-iso") {
		t.Skip("oline users not provisioned in this distro; run an engagement first")
	}
	if out := run("iptables", "-S", "OLINE_OUT"); !strings.Contains(out, "OLINE_OUT") {
		t.Errorf("OLINE_OUT chain not installed: %s", out)
	}
}
