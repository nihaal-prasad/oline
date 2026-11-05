package sandbox

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"time"

	"github.com/nihaal-prasad/oline/internal/core"
)

// The three unprivileged guest users and the shared engagement home. Every
// backend (WSL, QEMU) provisions the same users and enforces the same
// root-owned iptables owner-match egress against them:
//
//	oline-scope : may reach ONLY the in-scope targets       (InScopeOnly)
//	oline-net   : may reach the internet EXCEPT the targets  (InternetNoTarget)
//	oline-iso   : no network except loopback                 (Isolated)
//
// Agents run as one of these users (no root, no sudo) so they cannot alter the
// rules; only the Privileged installer runs as root.
const (
	egScopeUser = "oline-scope"
	egNetUser   = "oline-net"
	egISOUser   = "oline-iso"
	egChain     = "OLINE_OUT" // dedicated chain holding all oline rules
)

// egressUsersScript creates the shared group/home and the three unprivileged
// users (idempotent) and prints their uids.
const egressUsersScript = `set -e
groupadd -f oline
install -d -m 2775 -o root -g oline /opt/oline
install -d -m 2775 -o root -g oline /opt/oline/engagements
for u in oline-scope oline-net oline-iso; do
  id "$u" >/dev/null 2>&1 || useradd -M -N -d /opt/oline -g oline -s /bin/bash "$u"
  usermod -s /bin/bash -d /opt/oline -g oline "$u" >/dev/null 2>&1 || true
done
# Let the unprivileged agents bind low ports (<1024), which pentest work needs for
# listeners and reverse-shell handlers (e.g. catching a callback on 443). This only
# changes LOCAL bind permission; the owner-match egress firewall below is unchanged,
# so the per-agent network boundary still holds. Best-effort (|| true) so a kernel
# that disallows the write doesn't abort user setup.
sysctl -w net.ipv4.ip_unprivileged_port_start=0 >/dev/null 2>&1 || true
printf 'SCOPE=%s NET=%s ISO=%s\n' "$(id -u oline-scope)" "$(id -u oline-net)" "$(id -u oline-iso)"
`

// egressRemoveUsersScript fully removes the oline users and shared home.
const egressRemoveUsersScript = `userdel -f oline-scope 2>/dev/null; userdel -f oline-net 2>/dev/null; userdel -f oline-iso 2>/dev/null
groupdel oline 2>/dev/null; rm -rf /opt/oline; echo CLEANED`

// rootExecFn runs a bash script as root in the guest and returns combined output.
// A backend supplies this (wsl.exe -u root, or ssh + sudo).
type rootExecFn func(ctx context.Context, script string) (string, error)

// egressCore is the backend-independent per-agent egress controller: the policy,
// users, and firewall rules are identical across backends; only the transport
// (rootExec) differs. It satisfies EgressController.
type egressCore struct {
	root    rootExecFn
	uids    map[string]int
	applied bool
}

func newEgressCore(root rootExecFn) *egressCore {
	return &egressCore{root: root, uids: map[string]int{}}
}

// ensureUsers creates the unprivileged users (idempotent) and records their uids.
func (e *egressCore) ensureUsers(ctx context.Context) error {
	out, err := e.root(ctx, egressUsersScript)
	if err != nil {
		return fmt.Errorf("sandbox egress: ensure users: %w: %s", err, strings.TrimSpace(out))
	}
	for _, f := range strings.Fields(out) {
		k, v, ok := strings.Cut(f, "=")
		if !ok {
			continue
		}
		n, err := strconv.Atoi(v)
		if err != nil {
			continue
		}
		switch k {
		case "SCOPE":
			e.uids[egScopeUser] = n
		case "NET":
			e.uids[egNetUser] = n
		case "ISO":
			e.uids[egISOUser] = n
		}
	}
	if len(e.uids) != 3 {
		return fmt.Errorf("sandbox egress: could not resolve all uids: %q", strings.TrimSpace(out))
	}
	return nil
}

// ApplyBaseline installs the owner-match egress policy derived from scope into the
// dedicated OLINE_OUT chain; Teardown removes that chain. (It does not snapshot or
// restore the host's other rules — the policy lives entirely in its own chain.)
func (e *egressCore) ApplyBaseline(scope Scope) error {
	ctx := context.Background()
	if len(e.uids) == 0 {
		if err := e.ensureUsers(ctx); err != nil {
			return err
		}
	}
	us, un, ui := e.uids[egScopeUser], e.uids[egNetUser], e.uids[egISOUser]
	// Fail closed if the three oline uids didn't resolve to distinct non-root ids:
	// a uid of 0 would emit `--uid-owner 0 -j DROP` and cut off ALL root egress in the
	// guest (OLINE_OUT is hooked at OUTPUT position 1), and a collision would conflate
	// two policies. This should never happen after ensureUsers, but never emit the
	// policy against root.
	if us == 0 || un == 0 || ui == 0 || us == un || us == ui || un == ui {
		return fmt.Errorf("sandbox egress: refusing to apply rules — oline users did not resolve to distinct non-root uids (scope=%d net=%d iso=%d)", us, un, ui)
	}
	// Resolve domain targets to IP addresses so a target given as a hostname is
	// firewall-enforced the same as one given as an IP (in-scope agents can reach
	// it; internet-only agents cannot). buildEgressRules also opens DNS to the
	// guest's resolver so agents can resolve names in the first place.
	v4, v6 := resolveScope(scope.Targets)
	out, err := e.root(ctx, buildEgressRules(us, un, ui, v4, v6, scope.AllowIPv6))
	if err != nil || !strings.Contains(out, "APPLIED") {
		return fmt.Errorf("sandbox egress: apply rules: %w: %s", err, strings.TrimSpace(out))
	}
	e.applied = true
	return nil
}

// PathFor returns the guest user that enforces the given policy. Privileged runs
// as root (the installer only), which the owner-match rules do not restrict.
func (e *egressCore) PathFor(_ core.AgentID, policy EgressPolicy) (NetPath, error) {
	switch policy {
	case Privileged:
		return NetPath{User: "root"}, nil
	case InternetNoTarget:
		return NetPath{User: egNetUser}, nil
	case Isolated:
		return NetPath{User: egISOUser}, nil
	default:
		return NetPath{User: egScopeUser}, nil
	}
}

// Teardown removes the dedicated OLINE_OUT chain installed by ApplyBaseline (it
// takes no snapshot, so there is nothing to restore — only its own chain to delete).
func (e *egressCore) Teardown() error {
	if !e.applied {
		return nil
	}
	_, err := e.root(context.Background(), buildEgressTeardown())
	e.applied = false
	return err
}

// removeUsers removes the oline users and shared home (full cleanup / tests).
func (e *egressCore) removeUsers() error {
	_, err := e.root(context.Background(), egressRemoveUsersScript)
	return err
}

// buildEgressRules composes the owner-match ruleset in a DEDICATED chain
// (egChain) hooked at the top of OUTPUT. The chain is flushed first, so
// re-applying with a changed scope fully replaces the rules and never pollutes
// (or needs to snapshot/restore) the system OUTPUT chain; traffic from non-oline
// uids falls through the chain unaffected. By default (allowIPv6 false) it also
// drops all IPv6 egress for the oline uids, so an agent can't slip past the v4
// scope rules over IPv6; when allowIPv6 is true it mirrors the same owner-match
// scope policy over IPv6 instead (and opens v6 DNS).
func buildEgressRules(us, un, ui int, v4, v6 []string, allowIPv6 bool) string {
	var b strings.Builder
	b.WriteString("set -e\n")
	// Create-or-flush the chain and hook it once at the top of OUTPUT.
	fmt.Fprintf(&b, "iptables -N %s 2>/dev/null || iptables -F %s\n", egChain, egChain)
	fmt.Fprintf(&b, "iptables -C OUTPUT -j %s 2>/dev/null || iptables -I OUTPUT 1 -j %s\n", egChain, egChain)
	// Loopback is NEVER blocked: accept it both by the loopback interface and by
	// destination (127.0.0.0/8), ahead of every owner-match DROP, so an agent can
	// always reach 127.0.0.1 / localhost (local tools, a local proxy, a relay).
	fmt.Fprintf(&b, "iptables -A %s -o lo -j ACCEPT\n", egChain)
	fmt.Fprintf(&b, "iptables -A %s -d 127.0.0.0/8 -j ACCEPT\n", egChain)
	// DNS: let the scope and net users reach the guest's configured resolver(s) on
	// port 53 so hostnames (including a domain target) resolve. Scoped to the actual
	// nameservers from /etc/resolv.conf and port 53 only (not DNS-anywhere). This is
	// ahead of the scope user's catch-all DROP so it takes effect. v4 resolvers go in
	// the v4 chain here; v6 resolvers are handled in the v6 block below (only when
	// IPv6 egress is enabled, else v6 is fully dropped so there is no v6 DNS path).
	fmt.Fprintf(&b, "for ns in $(awk '/^nameserver/{print $2}' /etc/resolv.conf 2>/dev/null); do\n")
	b.WriteString("  case \"$ns\" in *:*) continue;; esac\n") // v4 resolvers only here
	for _, uid := range []int{us, un} {
		fmt.Fprintf(&b, "  iptables -A %s -m owner --uid-owner %d -p udp --dport 53 -d \"$ns\" -j ACCEPT\n", egChain, uid)
		fmt.Fprintf(&b, "  iptables -A %s -m owner --uid-owner %d -p tcp --dport 53 -d \"$ns\" -j ACCEPT\n", egChain, uid)
	}
	b.WriteString("done\n")
	writeOwnerPolicy(&b, "iptables", us, un, ui, v4)
	// IPv6: by default block all v6 egress for the oline users (best-effort; skipped
	// without ip6tables) so an agent can't bypass the v4 scope rules over IPv6. When
	// the operator enables IPv6, mirror the same owner-match scope policy over v6
	// instead, and open v6 DNS to the guest's v6 resolver(s).
	b.WriteString("if command -v ip6tables >/dev/null 2>&1; then\n")
	fmt.Fprintf(&b, "  ip6tables -N %s 2>/dev/null || ip6tables -F %s\n", egChain, egChain)
	fmt.Fprintf(&b, "  ip6tables -C OUTPUT -j %s 2>/dev/null || ip6tables -I OUTPUT 1 -j %s\n", egChain, egChain)
	fmt.Fprintf(&b, "  ip6tables -A %s -o lo -j ACCEPT\n", egChain)
	fmt.Fprintf(&b, "  ip6tables -A %s -d ::1/128 -j ACCEPT\n", egChain)
	if allowIPv6 {
		fmt.Fprintf(&b, "  for ns in $(awk '/^nameserver/{print $2}' /etc/resolv.conf 2>/dev/null); do\n")
		b.WriteString("    case \"$ns\" in *:*) ;; *) continue;; esac\n") // v6 resolvers only
		for _, uid := range []int{us, un} {
			fmt.Fprintf(&b, "    ip6tables -A %s -m owner --uid-owner %d -p udp --dport 53 -d \"$ns\" -j ACCEPT\n", egChain, uid)
			fmt.Fprintf(&b, "    ip6tables -A %s -m owner --uid-owner %d -p tcp --dport 53 -d \"$ns\" -j ACCEPT\n", egChain, uid)
		}
		b.WriteString("  done\n")
		writeOwnerPolicy(&b, "  ip6tables", us, un, ui, v6)
	} else {
		for _, uid := range []int{us, un, ui} {
			fmt.Fprintf(&b, "  ip6tables -A %s -m owner --uid-owner %d -j DROP\n", egChain, uid)
		}
	}
	b.WriteString("fi\n")
	b.WriteString("echo APPLIED\n")
	return b.String()
}

// writeOwnerPolicy emits the per-user owner-match scope policy for one address
// family (iptables or ip6tables), given the in-scope CIDRs for that family: the
// scope user may reach only the in-scope targets, the net user may reach everything
// except them, and the iso user reaches nothing. cmd may carry leading indentation
// (e.g. "  ip6tables") so the lines nest inside a shell `if` block.
func writeOwnerPolicy(b *strings.Builder, cmd string, us, un, ui int, cidrs []string) {
	for _, c := range cidrs { // scope user: allow in-scope, then drop the rest
		fmt.Fprintf(b, "%s -A %s -m owner --uid-owner %d -d %s -j ACCEPT\n", cmd, egChain, us, c)
	}
	fmt.Fprintf(b, "%s -A %s -m owner --uid-owner %d -j DROP\n", cmd, egChain, us)
	for _, c := range cidrs { // net user: drop in-scope targets, then allow the rest
		fmt.Fprintf(b, "%s -A %s -m owner --uid-owner %d -d %s -j DROP\n", cmd, egChain, un, c)
	}
	fmt.Fprintf(b, "%s -A %s -m owner --uid-owner %d -j ACCEPT\n", cmd, egChain, un)
	fmt.Fprintf(b, "%s -A %s -m owner --uid-owner %d -j DROP\n", cmd, egChain, ui) // iso: no network
}

// buildEgressTeardown unhooks and removes the oline chain (v4 + v6), restoring
// the guest to its original egress with no snapshot needed.
func buildEgressTeardown() string {
	v4 := fmt.Sprintf("iptables -D OUTPUT -j %s 2>/dev/null || true; iptables -F %s 2>/dev/null || true; iptables -X %s 2>/dev/null || true\n",
		egChain, egChain, egChain)
	v6 := fmt.Sprintf("if command -v ip6tables >/dev/null 2>&1; then ip6tables -D OUTPUT -j %s 2>/dev/null || true; ip6tables -F %s 2>/dev/null || true; ip6tables -X %s 2>/dev/null || true; fi",
		egChain, egChain, egChain)
	return v4 + v6
}

// resolveScope turns a mixed list of IPs/CIDRs/domains into canonical rule targets,
// split by address family: IPv4 addresses/prefixes and IPv6 addresses/prefixes are
// kept as-is in their respective lists; a domain is resolved (bounded) to its IPv4
// and IPv6 addresses, each added as a host route. Each list is deduplicated. The v6
// list is only used when the operator enabled IPv6 egress; otherwise v6 is dropped
// wholesale and the list is harmless.
func resolveScope(in []string) (v4, v6 []string) {
	seen := map[string]bool{}
	add := func(dst *[]string, s string) {
		if !seen[s] {
			seen[s] = true
			*dst = append(*dst, s)
		}
	}
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		if p, err := netip.ParsePrefix(s); err == nil {
			if p.Addr().Is4() {
				add(&v4, p.String())
			} else {
				add(&v6, p.String())
			}
			continue
		}
		if a, err := netip.ParseAddr(s); err == nil {
			if a.Is4() {
				add(&v4, a.String())
			} else {
				add(&v6, a.String())
			}
			continue
		}
		// A domain target: resolve to IPs (bounded so a slow resolver can't hang
		// engagement start / a live scope change).
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		ips, err := net.DefaultResolver.LookupIP(ctx, "ip", s)
		cancel()
		if err != nil {
			continue
		}
		for _, ip := range ips {
			if b4 := ip.To4(); b4 != nil {
				add(&v4, netip.AddrFrom4([4]byte(b4)).String())
			} else if b16 := ip.To16(); b16 != nil {
				add(&v6, netip.AddrFrom16([16]byte(b16)).String())
			}
		}
	}
	return v4, v6
}

// sanitizeOneCIDR canonicalizes a single IPv4 address or prefix for safe
// interpolation into a root nft script, or returns "" if it is malformed. A bare
// address is returned as-is (a /32 host route in nft daddr terms).
func sanitizeOneCIDR(s string) string {
	s = strings.TrimSpace(s)
	if p, err := netip.ParsePrefix(s); err == nil && p.Addr().Is4() {
		return p.String()
	}
	if a, err := netip.ParseAddr(s); err == nil && a.Is4() {
		return a.String()
	}
	return ""
}

func sanitizeCIDRs(in []string) []string {
	var out []string
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		if p, err := netip.ParsePrefix(s); err == nil && p.Addr().Is4() {
			out = append(out, p.String())
			continue
		}
		if a, err := netip.ParseAddr(s); err == nil && a.Is4() {
			out = append(out, a.String())
		}
	}
	return out
}
