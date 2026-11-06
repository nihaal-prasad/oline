package sandbox

import (
	"strings"
	"testing"
)

// TestEgressUsersScriptEnablesLowPorts checks the shared user-setup script lowers
// the unprivileged port floor so pentest agents can bind ports below 1024 (for
// listeners / reverse-shell handlers). Both backends run this script.
func TestEgressUsersScriptEnablesLowPorts(t *testing.T) {
	if !strings.Contains(egressUsersScript, "net.ipv4.ip_unprivileged_port_start=0") {
		t.Error("egressUsersScript should lower ip_unprivileged_port_start so agents can bind low ports")
	}
	// It must stay best-effort so a kernel that refuses the write doesn't abort
	// user provisioning (the script runs under `set -e`).
	if !strings.Contains(egressUsersScript, "|| true") {
		t.Error("the sysctl line should be best-effort (|| true)")
	}
}

// TestResolveScopeCIDRs checks IP/CIDR targets are kept and domain targets are
// resolved to IPv4 so they get firewall-enforced.
func TestResolveScopeCIDRs(t *testing.T) {
	v4, _, _ := resolveScope([]string{"10.0.0.0/24", "192.168.1.5", "localhost"})
	has := func(s string) bool {
		for _, g := range v4 {
			if g == s {
				return true
			}
		}
		return false
	}
	if !has("10.0.0.0/24") || !has("192.168.1.5") {
		t.Errorf("IP/CIDR targets not preserved: %v", v4)
	}
	if !has("127.0.0.1") {
		t.Errorf("domain 'localhost' was not resolved to 127.0.0.1: %v", v4)
	}
}

// TestResolveScopeSplitsFamilies checks IPv6 targets land in the v6 list, not v4.
func TestResolveScopeSplitsFamilies(t *testing.T) {
	v4, v6, _ := resolveScope([]string{"10.0.0.5", "2001:db8::1", "fd00::/8"})
	if len(v4) != 1 || v4[0] != "10.0.0.5" {
		t.Errorf("v4 = %v, want [10.0.0.5]", v4)
	}
	if len(v6) != 2 {
		t.Errorf("v6 = %v, want the two IPv6 targets", v6)
	}
}

// TestEgressBlocksIPv6ByDefault checks that with AllowIPv6 off the v6 chain drops
// every oline user, and with it on the scope policy is mirrored over v6 instead.
func TestEgressBlocksIPv6ByDefault(t *testing.T) {
	off := buildEgressRules(1001, 1002, 1003, []string{"10.0.0.0/24"}, []string{"2001:db8::/64"}, false)
	if !strings.Contains(off, "ip6tables -A "+egChain+" -m owner --uid-owner 1001 -j DROP") {
		t.Error("IPv6 disabled: scope user should be dropped over v6")
	}
	if strings.Contains(off, "2001:db8::/64") {
		t.Error("IPv6 disabled: no v6 target rule should be emitted")
	}
	on := buildEgressRules(1001, 1002, 1003, []string{"10.0.0.0/24"}, []string{"2001:db8::/64"}, true)
	if !strings.Contains(on, "ip6tables -A "+egChain+" -m owner --uid-owner 1001 -d 2001:db8::/64 -j ACCEPT") {
		t.Error("IPv6 enabled: scope user should be allowed to reach the in-scope v6 target")
	}
}

// TestEgressRulesAllowDNS checks the ruleset opens DNS to the guest resolver so
// agents can resolve hostnames (including a domain target).
func TestEgressRulesAllowDNS(t *testing.T) {
	rules := buildEgressRules(1001, 1002, 1003, []string{"10.0.0.0/24"}, nil, false)
	for _, want := range []string{"/etc/resolv.conf", "--dport 53"} {
		if !strings.Contains(rules, want) {
			t.Errorf("egress rules missing DNS allowance %q", want)
		}
	}
}

// TestSafeEngagementName checks engagement names can't escape the workspace.
func TestSafeEngagementName(t *testing.T) {
	cases := map[string]string{
		"engagement-20260101-120000": "engagement-20260101-120000",
		"../../etc/passwd":           "passwd",
		"a/../b":                     "b",
		"foo/bar":                    "bar",
		"win\\share\\x":              "x",
		"..":                         "engagement",
		"":                           "engagement",
	}
	for in, want := range cases {
		if got := SafeEngagementName(in); got != want {
			t.Errorf("SafeEngagementName(%q) = %q, want %q", in, got, want)
		}
		if got := SafeEngagementName(in); strings.Contains(got, "/") || strings.Contains(got, "..") {
			t.Errorf("SafeEngagementName(%q) = %q still contains a traversal sequence", in, got)
		}
	}
}

// TestEgressRulesNeverBlockLoopback checks loopback is always accepted, by both
// the loopback interface and destination, ahead of any owner-match DROP.
func TestEgressRulesNeverBlockLoopback(t *testing.T) {
	rules := buildEgressRules(1001, 1002, 1003, []string{"10.0.0.0/24"}, nil, false)
	for _, want := range []string{
		"-o lo -j ACCEPT",
		"-d 127.0.0.0/8 -j ACCEPT",
		"-d ::1/128 -j ACCEPT",
	} {
		if !strings.Contains(rules, want) {
			t.Errorf("egress rules missing loopback accept %q", want)
		}
	}
	// The loopback accepts must precede the scope user's DROP, or they wouldn't
	// take effect.
	if lo, drop := strings.Index(rules, "-d 127.0.0.0/8 -j ACCEPT"), strings.Index(rules, "--uid-owner 1001 -j DROP"); lo < 0 || drop < 0 || lo > drop {
		t.Errorf("loopback accept must come before the scope DROP (lo=%d drop=%d)", lo, drop)
	}
}
