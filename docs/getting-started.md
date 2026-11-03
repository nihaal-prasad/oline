# Getting started

## Requirements

- Go 1.27 or newer, to build.
- A sandbox backend on the machine you run it on:
  - WSL on Windows (a distro oline creates, or one you point it at), or
  - QEMU (`qemu-system-x86_64` and `qemu-img`) on Windows or Linux.
- An API key for an OpenAI-compatible model provider (Fireworks by default).

## Build

```bash
go build ./cmd/oline      # or: make build
go test ./...
make dist                 # cross-compiled binaries into dist/ (or: pwsh ./build.ps1)
```

## Set your API key

oline reads the key from an environment variable named by each provider's
`apiKeyRef` in `models.yml`. The default is `FIREWORKS_API_KEY`:

```bash
export FIREWORKS_API_KEY=...        # Linux / WSL
$env:FIREWORKS_API_KEY = "..."      # Windows PowerShell
```

You can also save a key in the in-app editor, which writes it to
`~/oline/config/secrets.yml`. See [configuration.md](configuration.md).

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
