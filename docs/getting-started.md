# Getting started

## Requirements

- Go 1.27 or newer, to build.
- A sandbox backend on the machine you run it on:
  - WSL on Windows (a distro oline creates, or one you point it at), or
  - QEMU (`qemu-system-x86_64` and `qemu-img`) on Windows or Linux.
- An OpenAI-compatible model provider and an API key for it.

## Build

```bash
go build ./cmd/oline      # or: make build
go test ./...
make dist                 # cross-compiled binaries into the project root (or: pwsh ./build.ps1)
```

## Provider and API key

`oline` ships with no provider. On the first run, the setup wizard asks for your
OpenAI-compatible provider: a name, a base URL, an API key, and a model for each
tier. It saves the provider and your tier choices to `~/oline/config/models.yml`
and the key to `~/oline/config/secrets.yml`. Because that file is shared by every
mode, you set this up once and every mode (pentester, exploiter, programmer) uses it.

A provider's key can instead come from an environment variable: set its `apiKeyRef`
in `models.yml` to `env:NAME` and export `NAME`. See
[configuration.md](configuration.md).

## Run

```bash
oline -mode pentester     # launch the TUI with the setup wizard
oline -mode exploiter
oline -mode programmer
oline -edit               # open the model/provider editor and exit
oline -print              # print the resolved roster and exit
oline -version
```

### Skip the wizard

Pass the engagement settings on the command line to start directly:

```bash
oline -mode pentester -env <wsl-distro> -scope "10.0.0.5, 10.0.0.0/24" -goal "prove RCE" [-planner]
```

### Flags

| flag | meaning |
|---|---|
| `-mode pentester\|exploiter\|programmer` | which roster to load (default `pentester`) |
| `-env <name>` | skip the wizard and start in this sandbox (for example a WSL distro name) |
| `-scope "<targets>"` | pentester: in-scope IPs, CIDRs, and domains. exploiter: the application to analyze (a local path, or a URL/name to fetch). programmer: the project/source to work on (optional) |
| `-goal "<text>"` | engagement goal |
| `-planner` | planner mode (AI proposes, you approve each step); default is automatic. `-manual` is a deprecated alias |
| `-edit` | open the model/provider editor and exit |
| `-destroy` | interactively destroy an oline-managed WSL/QEMU environment and exit (only `oline-` envs; asks you to confirm by name; never touches anything else) |
| `-init-config` | write the mode's default `config.yml`, `models.yml`, and prompt files to `~/oline/config/<mode>/`, then exit (`-force` overwrites existing) |
| `-audit-verify "<engagement>"` | verify an engagement's tamper-evident audit log and exit |
| `-print` | print the resolved roster and exit |
| `-version` | print the version and exit |

## Next

- [tui.md](tui.md): the setup wizard and the running interface.
- [AGENTS.md](AGENTS.md): the modes, the agent rosters, and the tools.
- [configuration.md](configuration.md): config files, models, secrets, backups, and logs.
- [sandbox.md](sandbox.md): the WSL and QEMU backends and the network boundary.
