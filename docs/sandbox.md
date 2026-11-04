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
| isolated | `oline-iso` | nothing but loopback |
| privileged | root | the installer, to set up tooling |

- Pentester: agents are in-scope only; `researcher` reaches the internet but not
  the target.
- Exploiter: the target runs locally, so agents are isolated (loopback only) and
  can never reach a remote host. Only `installer` (download) and `judge`/`skeptic`
  (research) get the internet.
- The `installer` runs as root to install software.

Changing scope while running re-applies these rules. See [tui.md](tui.md).

## WSL backend (Windows)

oline imports or reuses a managed distro. Agents run as the unprivileged `oline-*`
users in a shared workspace, governed by root-owned iptables rules.

The distro persists after oline exits, so at engagement end oline removes its
firewall rules (the `OLINE_OUT` chain, IPv4 and IPv6) once agents have stopped; the
`oline-*` users and workspace stay for reuse.

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
  is cached under `<root>/images`. The wizard asks how many GB of disk to give the
  VM; the overlay is grown to that size (the base stays untouched) and the guest
  grows its root filesystem to use it on first boot.
- **A qcow2 you supply**: drop a cloud image (with cloud-init) at
  `<root>/images/<name>.qcow2` and it appears as an environment.

Per-agent egress is enforced the same way as WSL. On a Linux host, set
`qemu.hostFirewall: true` to also pin the guest to a host-owned tap device and
`nftables` rules: a whole-guest cap (loopback plus the in-scope targets, plus the
internet when the roster has internet agents) that a root agent inside the guest
cannot change. SSH stays on a user-mode NIC. It needs root on the host (oline uses
`sudo`) and is ignored on other hosts.

QEMU VMs are disposable by default: the overlay is discarded on exit. Set
`qemu.persist: true` to keep each engagement's disk (and its SSH key) so you can
resume it with its workspace and installed tooling intact. Resume needs a clean
shutdown; a crash does not persist the disk (the host workspace backup still does).
Resumable engagements appear on the wizard's resume step.

## Tool-layer blocks

On top of the network boundary:

- `bash` and `bash_bg` commands matching the configured deny patterns are blocked
  before they reach the sandbox. The defaults deny the host `/mnt` drives and the
  Windows interop binaries (`cmd.exe`, `powershell.exe`, `wsl.exe`, `explorer.exe`).
  Configure them under `bash.patterns` in `config.yml`.
- The file tools (`read`, `write`, `edit`, and the search tools) reject host `/mnt`
  paths.
- `write` and `edit` only write inside the engagement workspace
  (`/opt/oline/engagements/<name>`); a relative path is taken from that workspace, and
  a write anywhere else on the guest is refused. The `installer` is the exception: it
  runs as root to install tooling and may write anywhere.

## The workspace

Engagement files live inside the guest at `/opt/oline/engagements/<name>`. Agents
work there and never write to the host. oline copies that workspace out for backups
(see [configuration.md](configuration.md)). You can drop files into that directory
yourself or point a resume at an existing one.

## VPN

In pentester engagements (WSL only) you can reach a target over a VPN: in the
wizard's VPN step, point oline at an OpenVPN `.ovpn` config (a guest path, or a host
path it copies in) and it starts OpenVPN in the guest before agents run. You can also
declare a VPN already up, or skip it.

Keep your `.ovpn` outside the sandbox workspace, since a cleanup agent may remove
files left there.
