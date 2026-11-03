# Getting started

## Requirements

- Go 1.27 or newer, to build.
- A sandbox backend on the machine you run it on:
  - WSL on Windows (a distro oline creates, or one you point it at), or
  - QEMU (`qemu-system-x86_64` and `qemu-img`) on Windows or Linux.
- An OpenAI-compatible model provider and an API key for it. oline ships with no
  default provider; you configure one in the first-run setup wizard.

## Build

```bash
go build ./cmd/oline      # or: make build
go test ./...
make dist                 # cross-compiled binaries into dist/ (or: pwsh ./build.ps1)
```

## Provider and API key

oline ships with no provider. On the first run, the setup wizard asks for your
OpenAI-compatible provider: a name, a base URL, an API key, and a model for each
tier. It saves the provider to `~/oline/config/<mode>/models.yml` and the key to
`~/oline/config/secrets.yml`.

A provider's key can instead come from an environment variable: set its `apiKeyRef`
in `models.yml` to `env:NAME` and export `NAME`. See
[configuration.md](configuration.md).

## Run

```bash
oline -mode pentester     # launch the TUI with the setup wizard
oline -mode exploiter
oline -edit               # open the model/provider editor and exit
oline -print              # print the resolved roster and exit
oline -version
```

### Skip the wizard

Pass the engagement settings on the command line to start directly:

```bash
oline -mode pentester -env <wsl-distro> -scope "10.0.0.5, 10.0.0.0/24" -goal "prove RCE" [-manual]
```

### Flags

| flag | meaning |
|---|---|
| `-mode pentester\|exploiter` | which roster to load (default `pentester`) |
| `-env <name>` | skip the wizard and start in this sandbox (for example a WSL distro name) |
| `-scope "<targets>"` | in-scope IPs, CIDRs, and domains |
| `-goal "<text>"` | engagement goal |
| `-manual` | manual mode; default is automatic |
| `-edit` | open the model/provider editor and exit |
| `-init-config` | write the mode's default `config.yml`, `models.yml`, and prompt files to `~/oline/config/<mode>/`, then exit (`-force` overwrites existing) |
| `-print` | print the resolved roster and exit |
| `-version` | print the version and exit |

## Next

- [tui.md](tui.md): the setup wizard and the running interface.
- [agents.md](agents.md): the modes, the agent rosters, and the tools.
- [configuration.md](configuration.md): config files, models, secrets, backups, and logs.
- [sandbox.md](sandbox.md): the WSL and QEMU backends and the network boundary.
