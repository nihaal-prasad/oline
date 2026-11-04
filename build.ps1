#!/usr/bin/env pwsh
# Cross-compile oline for Linux and Windows into the project root.
param([string]$Version = "")
if (-not $Version) {
  try { $Version = (git describe --tags --always 2>$null) } catch {}
}
if (-not $Version) { $Version = "dev" }
$ld = "-s -w -X main.version=$Version"

# Embed the Windows .exe icon from assets/oline.ico by generating a resource
# object (cmd/oline/rsrc_windows_amd64.syso) that `go build` links in for Windows.
# Uses the `rsrc` tool; skipped if the icon or network is unavailable.
$syso = "cmd/oline/rsrc_windows_amd64.syso"
if ((Test-Path "assets/oline.ico") -and -not (Test-Path $syso)) {
  Write-Host "generating Windows icon resource ($syso)"
  go run github.com/akavel/rsrc@latest -ico assets/oline.ico -arch amd64 -o $syso
}
$targets = @(
  @{ os = "linux";   arch = "amd64"; ext = "" },
  @{ os = "windows"; arch = "amd64"; ext = ".exe" }
)
foreach ($t in $targets) {
  $env:GOOS = $t.os; $env:GOARCH = $t.arch
  $out = "oline-$($t.os)-$($t.arch)$($t.ext)"
  Write-Host "building $out (version $Version)"
  go build -ldflags $ld -o $out ./cmd/oline
}
$env:GOOS = ""; $env:GOARCH = ""
Write-Host "done - binaries in the project root"
