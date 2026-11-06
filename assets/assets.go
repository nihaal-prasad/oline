// Package assets embeds the default agent definitions, orchestrator prompts,
// personalities, and mode configuration for each roster. On-disk files under
// ~/oline/config/<mode>/ override these defaults at load time.
package assets

import (
	"embed"
	"io/fs"
)

// Logo is the oline mark rendered as a truecolor ANSI half-block image on a pure
// black background. Shown on the start screen as the portable fallback when the
// terminal cannot render a real image. Generated from docs/logo.png by
// scripts/mklogo.
//
//go:embed logo.ans
var Logo string

// LogoPNG is the full-resolution banner, shown crisply via a terminal graphics
// protocol (Sixel) when the terminal supports it.
//
//go:embed logo.png
var LogoPNG []byte

// PentesterFS holds the full Pentester roster: SYSTEM_TEMPLATE.md, PERSONALITY.md,
// config.yml, models.yml, and agents/*.md (17 subagents).
//
//go:embed pentester
var PentesterFS embed.FS

// ExploiterFS holds the full Exploiter roster: SYSTEM_TEMPLATE.md, PERSONALITY.md,
// config.yml, models.yml, and agents/*.md (9 subagents).
//
//go:embed exploiter
var ExploiterFS embed.FS

// ProgrammerFS holds the full Programmer roster (red-team tool development):
// SYSTEM_TEMPLATE.md, PERSONALITY.md, config.yml, models.yml, and agents/*.md.
//
//go:embed programmer
var ProgrammerFS embed.FS

// For returns the embedded defaults filesystem for a roster mode. It is the single
// place that maps a mode name to its bundle; callers (cmd, tui) use it instead of
// hand-written per-mode branches. An unknown mode falls back to the Pentester
// bundle (the historical default), matching the prior inline behavior.
func For(mode string) fs.FS {
	switch mode {
	case "exploiter":
		return ExploiterFS
	case "programmer":
		return ProgrammerFS
	default:
		return PentesterFS
	}
}
