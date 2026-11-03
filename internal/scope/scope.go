// Package scope parses an engagement's in-scope targets and classifies network
// destinations as in- or out-of-scope. IP/CIDR targets drive the host-enforced
// firewall rules; domains are kept for tool-layer checks and reporting.
package scope

import (
	"net/netip"
	"regexp"
	"strings"
)

// Policy is the parsed engagement scope.
type Policy struct {
	nets    []netip.Prefix // in-scope IP/CIDR targets
	domains []string       // in-scope domain suffixes (lowercased)
	raw     string         // the original free-text scope, for display
}

var reCIDR = regexp.MustCompile(`\b(\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3}(?:/\d{1,2})?)\b`)
var reDomain = regexp.MustCompile(`\b([a-zA-Z0-9](?:[a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?(?:\.[a-zA-Z0-9](?:[a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?)+)\b`)

// Parse extracts IP/CIDR and domain targets from arbitrary scope text (the TUI
// scope field is free-form, e.g. "10.0.0.0/24 and app.example.com only").
func Parse(text string) Policy {
	p := Policy{raw: strings.TrimSpace(text)}
	for _, m := range reCIDR.FindAllString(text, -1) {
		s := m
		if !strings.Contains(s, "/") {
			s += "/32"
		}
		if pref, err := netip.ParsePrefix(s); err == nil {
			p.nets = append(p.nets, pref.Masked())
		}
	}
	// Domains: anything with a dot that isn't an IP.
	for _, m := range reDomain.FindAllString(text, -1) {
		if reCIDR.MatchString(m) {
			continue
		}
		if _, err := netip.ParseAddr(m); err == nil {
			continue
		}
		d := strings.ToLower(strings.TrimSuffix(m, "."))
		if !containsStr(p.domains, d) {
			p.domains = append(p.domains, d)
		}
	}
	return p
}

// CIDRs returns the in-scope prefixes as strings (for firewall rules).
func (p Policy) CIDRs() []string {
	out := make([]string, 0, len(p.nets))
	for _, n := range p.nets {
		out = append(out, n.String())
	}
	return out
}

// Domains returns the in-scope domain suffixes.
func (p Policy) Domains() []string { return append([]string(nil), p.domains...) }

// Raw returns the original scope text.
func (p Policy) Raw() string { return p.raw }

// Empty reports whether no IP/CIDR targets were found (firewall rules would then
// deny all egress for in-scope-only agents — a safe but very strict default).
func (p Policy) Empty() bool { return len(p.nets) == 0 }

// ContainsIP reports whether ip is in scope.
func (p Policy) ContainsIP(ip netip.Addr) bool {
	for _, n := range p.nets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// ContainsHost classifies a host string (IP literal or domain). For a domain it
// matches exact or subdomain of an in-scope domain.
func (p Policy) ContainsHost(host string) bool {
	host = strings.ToLower(strings.TrimSpace(host))
	if ip, err := netip.ParseAddr(host); err == nil {
		return p.ContainsIP(ip)
	}
	for _, d := range p.domains {
		if host == d || strings.HasSuffix(host, "."+d) {
			return true
		}
	}
	return false
}

func containsStr(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}
