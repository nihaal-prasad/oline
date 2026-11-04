// Package assets embeds the default agent definitions, orchestrator prompts,
// personalities, and mode configuration for each roster. On-disk files under
// ~/oline/config/<mode>/ override these defaults at load time.
package assets

import "embed"

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
