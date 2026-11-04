// Package sandbox defines the execution + egress boundary for agent code. All
// agent commands run inside a WSL distro or QEMU VM — never on the host.
//
// Egress boundary, by backend:
//   - WSL (active): per-agent egress is enforced by root-owned iptables
//     owner-match rules inside the guest. Agents run as distinct unprivileged
//     users with no sudo, so they cannot alter the rules — the boundary holds
//     for every normal agent. The sole exception is the Privileged (root)
//     installer, which could change in-guest rules; keep that in mind when
//     granting Privileged. Because the kernel is shared with the host, WSL is
//     the weaker boundary.
//   - QEMU (active): a separate kernel, so the stronger isolation. Per-agent
//     egress is currently enforced by the same root-owned in-guest iptables
//     owner-match rules as WSL (applied over SSH). The stronger host-side tap +
//     nftables filter the guest cannot touch regardless of in-guest root
//     (BuildNftRules) is implemented and unit-tested but not yet wired into the
//     launch path.
//
// Tool-layer checks (bash deny patterns, the file tools' host-path guard) are
// defense-in-depth on top of the network boundary.
package sandbox

import (
	"context"
	"io"

	"github.com/nihaal-prasad/oline/internal/core"
)

// Backend selects the sandbox technology.
type Backend string

const (
	BackendWSL  Backend = "wsl"
	BackendQEMU Backend = "qemu"
)

// EgressPolicy is the network policy applied to a given agent's traffic.
type EgressPolicy int

const (
	InScopeOnly      EgressPolicy = iota // may reach in-scope targets only (default)
	InternetNoTarget                     // may reach the wider internet but NOT the target
	Isolated                             // no network
	Privileged                           // runs as root: unrestricted (the installer only)
)

// Scope is the engagement's network scope.
type Scope struct {
	Targets    []string // in-scope IPs / CIDRs / domains
	Exclusions []string // explicitly off-limits
	// AllowIPv6 enables outbound non-loopback IPv6 for the agent users. Off by
	// default: without it the per-agent firewall drops all IPv6 egress for the oline
	// users (targets are matched over IPv4), so an agent cannot slip past the v4
	// scope rules over IPv6. When on, the same scope/net/iso owner-match policy is
	// mirrored over IPv6 (in-scope v6 addresses allowed for scope agents, etc.).
	AllowIPv6 bool
}

// Candidate is an existing environment found by Scan.
type Candidate struct {
	ID          string
	Name        string
	Backend     Backend
	Running     bool
	Managed     bool // true if created by oline (name has the "oline-" prefix)
	NeedsCreate bool // true for a synthetic entry whose image must be built/downloaded on open (QEMU)
}

// Shutdowner is an optional Handle capability: a backend that must stop a running
// VM/process when the engagement ends implements it (QEMU). The TUI calls it on
// quit; backends with nothing to stop (WSL) simply don't implement it.
type Shutdowner interface {
	Shutdown(ctx context.Context) error
}

// WorkspaceSetter is an optional Handle capability: it makes the engagement's
// workspace directory the default working directory for agent commands (creating it
// if needed), so a relative path resolves there and writes can be confined to it.
// The TUI calls it once the engagement name is known.
type WorkspaceSetter interface {
	SetWorkspace(ctx context.Context, dir string) error
}

// Spec describes a new environment to create.
type Spec struct {
	Backend Backend
	Name    string // without the "oline-" prefix; the impl adds it
	Scope   Scope
}

// EnvInfo identifies a live environment.
type EnvInfo struct {
	ID      string
	Backend Backend
}

// Cmd is a command to run inside the guest.
type Cmd struct {
	Argv   []string
	Dir    string // working dir inside the guest (the workspace, never /tmp); "" => home
	Policy EgressPolicy
}

// ExitStatus is the result of an Exec.
type ExitStatus struct {
	Code int
}

// NetPath is the concrete network path assigned to an agent for a policy. On the
// WSL backend the discriminator is the guest USER (a distinct unprivileged uid
// per policy, enforced by root-owned iptables owner-match rules the agent cannot
// change).
type NetPath struct {
	User string // guest user to exec as for this policy
}

// EgressController applies and routes the host-enforced egress rules. Nothing it
// does can be undone from inside the guest.
type EgressController interface {
	ApplyBaseline(scope Scope) error
	PathFor(agent core.AgentID, policy EgressPolicy) (NetPath, error)
	Teardown() error
}

// Handle is a live environment.
type Handle interface {
	// Exec runs cmd inside the guest as the agent's unprivileged user on the
	// egress path chosen for that agent. Cancelling ctx kills the remote process.
	Exec(ctx context.Context, agent core.AgentID, cmd Cmd, out io.Writer) (ExitStatus, error)
	CopyIn(ctx context.Context, localPath, guestPath string) error
	CopyOut(ctx context.Context, guestPath, localPath string) error
	// Backup writes a compressed (.tar.gz) archive of the named engagement's
	// workspace to destPath on the HOST. The program calls this; agents never do.
	// Writes are atomic (temp + rename) and overwrite any previous archive, so
	// there is at most one backup file per engagement.
	Backup(ctx context.Context, engagement, destPath string) error
	EgressController() EgressController
	// SetupEgress provisions per-agent egress enforcement for this handle:
	// unprivileged per-policy users and root-owned firewall rules derived from
	// scope. Until called, Exec runs as the sandbox's default user with no
	// egress restriction. Safe to call once at engagement start.
	SetupEgress(ctx context.Context, scope Scope) error
	Info() EnvInfo
}

// Sandbox manages environments of one backend.
type Sandbox interface {
	Scan(ctx context.Context) ([]Candidate, error)
	Create(ctx context.Context, spec Spec) (Handle, error)
	Open(ctx context.Context, id string) (Handle, error)
	Destroy(ctx context.Context, id string) error
}

// Prefix is prepended to every environment oline creates, so the harness can
// tell its own sandboxes apart from the host's other VMs/distros and refuse to
// touch anything it did not create.
const Prefix = "oline-"

// noopEgress is the fallback egress controller returned before SetupEgress has
// run (e.g. in dev/test exec paths). It applies no rules; the live app refuses
// to start an engagement until real egress is in place (see the TUI).
type noopEgress struct{}

func (noopEgress) ApplyBaseline(Scope) error                           { return nil }
func (noopEgress) PathFor(core.AgentID, EgressPolicy) (NetPath, error) { return NetPath{}, nil }
func (noopEgress) Teardown() error                                     { return nil }
