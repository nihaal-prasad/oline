# Sandbox and egress

All agent code runs inside a sandbox, never on the host. oline supports two
backends, WSL and QEMU. Each enforces a per-agent network boundary: agents run as
unprivileged guest users with no sudo, and the firewall rules are owned by root, so
an agent cannot change them. oline refuses to start an engagement if the rules
cannot be applied.

Loopback (127.0.0.1 and localhost) is always allowed.

Targets can be IPs, CIDRs, or domains. A domain is resolved to its IPv4 addresses
when the firewall is applied, and those are enforced like any other target. Agents
can resolve hostnames (the firewall opens port 53 to the guest's resolver).
Resolution is a snapshot; if a target's address changes, re-apply scope
(`Ctrl-b e`).

IPv6 egress is off by default; the firewall drops outbound non-loopback IPv6 for
agents. If a target is only reachable over IPv6, set `network.allowIPv6: true` in
`config.yml`, and the same scope policy is enforced over IPv6.

## Per-agent egress policy

Each agent class runs as a distinct guest user:

| policy | guest user | what it can reach |
|---|---|---|
| in-scope only | `oline-scope` | only the in-scope target |
| internet, no target | `oline-net` | the internet, but never the target |
| isolated | `oline-iso` | nothing |
| privileged | root | the installer, to set up tooling |

- Most agents are in-scope only.
- The `researcher` (pentester) and `judge` and `skeptic` (exploiter) reach the
  internet but never the target.
- The `installer` runs as root to install software.

Changing scope while running re-applies these rules. See [tui.md](tui.md).

## WSL backend (Windows)

oline imports or reuses a managed distro. Agents run as the unprivileged `oline-*`
users in a shared workspace, governed by root-owned iptables rules.

The installer runs as root, which on WSL can change the in-guest rules. Use QEMU
when you need the boundary to hold against a root agent.

## QEMU backend (Windows or Linux)

QEMU runs a separate kernel, so the host drives are not present in the guest.

Install QEMU (`qemu-system-x86_64` and `qemu-img`; oline also finds the Windows
installer at `C:\Program Files\qemu`). The environment step of the wizard then
offers, besides any WSL distros:

- **Debian 13 VM (download on first use)**: oline downloads the official Debian 13
  cloud image, verifies it against the published `SHA512SUMS`, boots it on a
  disposable overlay, and provisions the per-agent users and egress. The base image
  is cached under `<root>/images`.
- **A qcow2 you supply**: drop a cloud image (with cloud-init) at
  `<root>/images/<name>.qcow2` and it appears as an environment.

Per-agent egress is enforced the same way as WSL. On a Linux host you can also pin
the guest to a tap device and filter with host `nftables`, for a boundary the guest
cannot change even as root.

QEMU VMs are disposable: the overlay is discarded on exit, so QEMU engagements do
not support resume.

## Tool-layer blocks

On top of the network boundary:

- `bash` and `bash_bg` commands matching the configured deny patterns are blocked
  before they reach the sandbox. The defaults deny the host `/mnt` drives and the
  Windows interop binaries (`cmd.exe`, `powershell.exe`, `wsl.exe`, `explorer.exe`).
  Configure them under `bash.patterns` in `config.yml`.
- The file tools (`read`, `write`, `edit`, and the search tools) reject host `/mnt`
  paths.

## The workspace

Engagement files live inside the guest at `/opt/oline/engagements/<name>`. Agents
work there and never write to the host. oline copies that workspace out for backups
(see [configuration.md](configuration.md)). You can drop files into that directory
yourself or point a resume at an existing one.

## VPN

In pentester engagements you can reach a target over a VPN. In the wizard's VPN
step, point oline at an OpenVPN `.ovpn` config (a path inside the guest, or a path
on the host that oline copies in) and it starts OpenVPN in the guest before agents
run. You can also say a VPN is already up, or skip it. WSL only.

Keep your `.ovpn` file outside the sandbox workspace, since a cleanup agent may
remove files left there during the run.
