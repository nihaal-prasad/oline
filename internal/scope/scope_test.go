package scope_test

import (
	"net/netip"
	"testing"

	"github.com/nihaal-prasad/oline/internal/scope"
)

func TestParseMixed(t *testing.T) {
	p := scope.Parse("in scope: 10.0.0.0/24 and 1.2.3.4, plus app.example.com; no scanning outside this")
	cidrs := p.CIDRs()
	if len(cidrs) != 2 {
		t.Fatalf("CIDRs = %v, want 2", cidrs)
	}
	if !contains(cidrs, "10.0.0.0/24") || !contains(cidrs, "1.2.3.4/32") {
		t.Errorf("unexpected CIDRs: %v", cidrs)
	}
	if !contains(p.Domains(), "app.example.com") {
		t.Errorf("domains = %v, want app.example.com", p.Domains())
	}
	if p.Empty() {
		t.Error("should not be empty")
	}

	if !p.ContainsIP(netip.MustParseAddr("10.0.0.7")) {
		t.Error("10.0.0.7 should be in scope")
	}
	if p.ContainsIP(netip.MustParseAddr("10.0.1.7")) {
		t.Error("10.0.1.7 should be out of scope")
	}
	if !p.ContainsHost("1.2.3.4") {
		t.Error("1.2.3.4 host should be in scope")
	}
	if !p.ContainsHost("sub.app.example.com") {
		t.Error("subdomain should be in scope")
	}
	if p.ContainsHost("evil.com") {
		t.Error("evil.com should be out of scope")
	}
}

func TestParseIPv6(t *testing.T) {
	p := scope.Parse("targets: 2001:db8::/32 and the host 2001:db8:1::5, plus 10.0.0.0/24")
	cidrs := p.CIDRs()
	if !contains(cidrs, "2001:db8::/32") {
		t.Errorf("IPv6 prefix not parsed: %v", cidrs)
	}
	if !contains(cidrs, "2001:db8:1::5/128") {
		t.Errorf("IPv6 host not parsed as /128: %v", cidrs)
	}
	if !contains(cidrs, "10.0.0.0/24") {
		t.Errorf("IPv4 prefix lost: %v", cidrs)
	}
	if !p.ContainsIP(netip.MustParseAddr("2001:db8::dead")) {
		t.Error("2001:db8::dead should be in scope")
	}
	if p.ContainsIP(netip.MustParseAddr("2001:dead::1")) {
		t.Error("2001:dead::1 should be out of scope")
	}
	if !p.ContainsHost("2001:db8:1::5") {
		t.Error("the v6 host should be in scope")
	}
}

func TestParseDedupesCIDRs(t *testing.T) {
	p := scope.Parse("10.0.0.5 10.0.0.5 10.0.0.5")
	if cidrs := p.CIDRs(); len(cidrs) != 1 {
		t.Errorf("duplicate IPs not de-duplicated: %v", cidrs)
	}
}

func TestParseProseOnly(t *testing.T) {
	p := scope.Parse("the production web application only, no IPs given")
	if !p.Empty() {
		t.Errorf("expected empty (no CIDRs), got %v", p.CIDRs())
	}
}

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}
