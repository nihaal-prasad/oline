# Getting started

This page covers building oline, running it, and the command-line flags.

## Requirements

- Go 1.27 or newer (to build).
- A sandbox backend on the machine you run it on:
  - WSL on Windows (a managed distro oline creates, or one you point it at), or
  - QEMU (`qemu-system-x86_64` + `qemu-img`) on Windows or Linux.
- An API key for an OpenAI-compatible model provider (Fireworks by default).

## Build

```bash
go build ./cmd/oline      # or: make build
go test ./...             # unit tests (live tests skip unless enabled)
```

Cross-compiled release binaries go into `dist/`:

```bash
make dist                 # or: pwsh ./build.ps1
```

The Windows build embeds an icon from `assets/oline.ico`. `make dist` and
`build.ps1` generate the resource object `cmd/oline/rsrc_windows_amd64.syso` with
[rsrc](https://github.com/akavel/rsrc) and commit it, so later `go build` calls
pick it up. After changing the `.ico`, delete that `.syso` so the next build
regenerates it.

## Set your API key

oline never stores a key in its config files. Each provider in `models.yml`
carries an `apiKeyRef`. The default is `env:FIREWORKS_API_KEY`, so the simplest
option is an environment variable:

```bash
export FIREWORKS_API_KEY=...        # Linux / WSL
$env:FIREWORKS_API_KEY = "..."      # Windows PowerShell
```

You can also save a key from the in-app editor, which writes it to
`~/oline/config/secrets.yml` (mode 0600, git-ignored). See
[configuration.md](configuration.md).

## Run

```bash
oline -mode pentester     # launch the TUI with the setup wizard
oline -mode exploiter
oline -edit               # open the model/provider editor and exit
oline -print              # print the resolved roster and exit (no TUI)
oline -version
```

`oline -print` is useful for checking your config and roster without a terminal
UI, for example in CI.

### Skip the wizard

Pass the engagement settings on the command line to start straight away:

```bash
oline -mode pentester -env <wsl-distro> -scope "10.0.0.5, 10.0.0.0/24" -goal "prove RCE" [-manual]
```

### Flags

| flag | meaning |
|---|---|
| `-mode pentester\|exploiter` | which roster to load (default `pentester`) |
| `-env <name>` | skip the wizard and start in this sandbox (e.g. a WSL distro name) |
| `-scope "<targets>"` | in-scope IPs, CIDRs, and domains (direct start) |
| `-goal "<text>"` | engagement goal (direct start) |
| `-manual` | manual mode (direct start); default is automatic |
| `-edit` | open the model/provider editor and exit |
| `-init-config` | write the mode's default config.yml, models.yml and prompt files to `~/oline/config/<mode>/` for editing, then exit (`-force` overwrites existing) |
| `-print` | print the resolved roster and exit |
| `-version` | print the version and exit |

## Where to go next

- [tui.md](tui.md) walks through the setup wizard and the running interface.
- [agents.md](agents.md) explains the two modes, the agent rosters, and the tools.
- [configuration.md](configuration.md) covers config files, model settings, backups, and logs.
- [sandbox.md](sandbox.md) covers the WSL and QEMU backends and the egress model.
