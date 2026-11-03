# Sandbox and egress

All agent code runs inside a sandbox, never on the host. oline supports two
backends: WSL and QEMU. Each one enforces a per-agent network boundary that an
agent cannot remove, even though agents have a real shell.

## Why there is a boundary

An agent acts in two ways: through the model stream and through tool calls. oline
mediates every tool call and every command, so it controls where each agent can
reach on the network. The rules are owned by root inside the guest (and, on a
Linux host with QEMU, can be owned by the host), while agents run as unprivileged
users with no sudo. An agent cannot flush or edit the rules, so the boundary holds.

Egress setup is mandatory. oline refuses to start an engagement if it cannot apply
the rules, rather than running unconfined.

Loopback is never blocked. Connections to 127.0.0.1 and localhost are always
allowed, regardless of an agent's policy, so local tools, a local proxy, or a relay
keep working.

Targets can be given as IP addresses, CIDRs, or domain names. A domain target is
resolved to its IPv4 addresses when the firewall is applied, and those addresses
are enforced like any other target (in-scope agents can reach them; internet-only
agents cannot). Agents can resolve hostnames because the firewall opens DNS to the
guest's configured resolver (port 53 only, to the nameservers in the guest's
`/etc/resolv.conf` - not DNS to anywhere). Domain resolution is a snapshot taken
when scope is applied; if a target's address changes, re-apply scope (`Ctrl-b e`)
to pick up the new address.

IPv6 egress is off by default. The firewall drops all outbound non-loopback IPv6
for the agent users, so an agent cannot slip past the IPv4 scope rules over IPv6.
If a target is only reachable over IPv6, set `network.allowIPv6: true` in
`config.yml`; the same per-agent scope policy is then enforced over IPv6 as well
(in-scope IPv6 addresses for scope agents, everything-but-target for internet
agents, nothing for isolated). The orchestrator is told whether IPv6 is on.

## Per-agent egress policy

Each agent class runs as a distinct guest user, and the rules route per user:

| policy | guest user | what it can reach |
|---|---|---|
| in-scope only | `oline-scope` | only the in-scope target |
| internet, no target | `oline-net` | the internet, but never the target |
| isolated | `oline-iso` | nothing |
| privileged | root | used by the installer to set up tooling |

Which agent gets which policy:

- Most agents are in-scope only. In pentester mode they reach the target and
  nothing else.
- The `researcher` (pentester) and `judge` and `skeptic` (exploiter) reach the
  internet but never the target, so they can look things up without touching the
  system under test.
- The `installer` runs as root so it can install software.

Changing the scope while running re-applies these rules on the fly. See
[tui.md](tui.md).

The agents run unprivileged, but oline lowers the guest's unprivileged port floor
(`net.ipv4.ip_unprivileged_port_start=0`) during setup so they can bind ports below
1024. Pentest work needs this for listeners and reverse-shell handlers, for example
catching a callback on port 443. This only changes what ports a process may bind
locally; it does not touch the egress firewall, so the per-agent network boundary is
unchanged.

## WSL backend (Windows)

oline imports or reuses a managed distro. Agents run as the unprivileged
`oline-*` users in a shared workspace, governed by root-owned iptables owner-match
rules (IPv4 rules plus a full block on IPv6 egress).

The one exception to the unprivileged rule is the privileged installer, which runs
as root. On WSL a root agent could change in-guest rules, so treat the WSL
boundary as in-guest. Use QEMU when you need the boundary to hold against a root
agent.

## QEMU backend (Windows or Linux)

QEMU gives the strongest isolation: a separate kernel, so there is no shared-kernel
escape and the host drives are not present in the guest at all.

Install QEMU (`qemu-system-x86_64` and `qemu-img`; oline also finds the Windows
installer at `C:\Program Files\qemu`). Then the environment step of the wizard
offers, in addition to any WSL distros:

- **Debian 13 VM (download on first use)** - oline downloads the official Debian 13
  generic-cloud qcow2 image, verifies it against the published `SHA512SUMS`,
  generates a throwaway SSH key and a cloud-init seed to inject it, boots the VM
  headless on a disposable copy-on-write overlay, waits for SSH, and provisions the
  per-agent users and egress. The base image is cached under `<root>/images` and
  reused.
- **A qcow2 you supply** - drop any cloud image that includes cloud-init at
  `<root>/images/<name>.qcow2` and it shows up as an environment to open.

oline talks to the VM over SSH and runs each command as the right guest user.
Per-agent egress is enforced the same way as WSL (root-owned in-guest iptables
owner rules, applied over SSH). On a Linux host you can additionally pin the guest
to a tap device and filter with host `nftables`, for a boundary the guest cannot
touch even as root.

QEMU VMs are disposable: the overlay and the seed are discarded on exit (the base
image is kept), so QEMU engagements do not support resume.

## Tool-layer defense-in-depth

On top of the network boundary, oline also blocks the obvious ways an agent would
try to reach the Windows host:

- `bash` and `bash_bg` commands matching the configured deny patterns are blocked
  before they reach the sandbox. The defaults deny the host `/mnt` drive mounts
  and the Windows interop binaries (`cmd.exe`, `powershell.exe`, `wsl.exe`,
  `explorer.exe`). Configure them under `bash.patterns` in `config.yml`.
- The structured file tools (`read`, `write`, `edit`, and the search tools) reject
  host `/mnt` paths.

These are a backstop. The hard boundary is still the per-agent firewall and the
unprivileged guest user.

## The workspace

Engagement files live inside the guest at `/opt/oline/engagements/<name>`. Agents
work there; they never write to the host. The oline program copies that workspace
out to the host for backups (see [configuration.md](configuration.md)). You can
also drop engagement files into that directory yourself, or point a resume at an
existing one.

## VPN

In pentester engagements you can reach a target over a VPN. In the wizard's VPN
step, point oline at an OpenVPN `.ovpn` config (a path inside the guest, or a path
on the host that oline copies in) and it starts OpenVPN inside the guest before the
agents run. You can also tell it a VPN is already up, or skip it. VPN support is
WSL only.

Keep your `.ovpn` file somewhere outside the sandbox workspace. Agents have a real
shell inside the sandbox, and a cleanup agent reverts artifacts, so a config file
left in the workspace could be removed by an agent during the run.

## Live tests

Some tests exercise the real provider and sandbox and are skipped unless enabled:

```bash
FIREWORKS_API_KEY=...  OLINE_TEST_WSL=<distro>  go test ./...
OLINE_TEST_QEMU=1  go test ./internal/sandbox/ -run TestLiveQEMU -timeout 20m
```

They are benign (they inspect the local sandbox and never attack a target) and
reversible (the egress tests restore firewall state and remove the users they
create; the QEMU test tears its VM down).
