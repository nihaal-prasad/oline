package tui

import (
	"fmt"
	"io/fs"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/nihaal-prasad/oline/assets"
	"github.com/nihaal-prasad/oline/internal/agents"
	"github.com/nihaal-prasad/oline/internal/config"
	"github.com/nihaal-prasad/oline/internal/tools"
)

// Options can skip the wizard and start an engagement directly (useful for a
// non-interactive launch and for verifying the running view).
type Options struct {
	Scope  string
	Goal   string
	EnvID  string // when set, the wizard is skipped and this environment is opened
	Manual bool
	Edit   bool // launch straight into the model/provider editor (no engagement)
}

// Launch runs the TUI with the interactive setup wizard.
func Launch(mode config.Mode) error { return LaunchWith(mode, Options{}) }

// LaunchWith loads the roster for mode and runs the full-screen TUI. If
// opts.EnvID is set, the wizard is skipped and the engagement starts directly.
// Dark theme, mouse enabled.
func LaunchWith(mode config.Mode, opts Options) error {
	if !mode.Valid() {
		return fmt.Errorf("tui: unknown mode %q", mode)
	}
	var fsys fs.FS = assets.PentesterFS
	if mode == config.ModeExploiter {
		fsys = assets.ExploiterFS
	}
	store, err := config.Load(mode, fsys)
	if err != nil {
		return fmt.Errorf("tui: load config: %w", err)
	}
	roster, err := agents.LoadRoster(mode, fsys, store)
	if err != nil {
		return fmt.Errorf("tui: load roster: %w", err)
	}

	m := newModel(mode, store, roster)
	if opts.Edit {
		m.editorOnly = true
	} else if opts.EnvID != "" {
		m.configureDirect(opts)
	}

	// On a Sixel-capable terminal, greet with the crisp image banner before the
	// TUI starts; then the wizard omits the (pixelated) block-art logo.
	if !opts.Edit {
		m.hideLogo = ShowSplash()
	}

	p := tea.NewProgram(m, tea.WithAltScreen(), tea.WithMouseCellMotion())
	_, err = p.Run()
	return err
}

// buildRegistry registers every tool oline implements. Per-agent scoping decides
// which of these each agent actually sees.
func buildRegistry() *tools.Registry {
	reg := tools.NewRegistry()
	reg.Register(tools.Bash{})
	reg.Register(tools.Read{})
	reg.Register(tools.Write{})
	reg.Register(tools.Edit{})
	reg.Register(tools.Find{})
	reg.Register(tools.Grep{})
	reg.Register(tools.Glob{})
	reg.Register(tools.WebSearch{})
	reg.Register(tools.WebFetch{})
	return reg
}

// bashDenyGlobs extracts the glob patterns marked `approval: deny` from the
// config's bash rules, for the engine's defense-in-depth bash filter.
func bashDenyGlobs(ps []config.BashPattern) []string {
	var out []string
	for _, p := range ps {
		if strings.EqualFold(strings.TrimSpace(p.Approval), "deny") {
			out = append(out, p.Match)
		}
	}
	return out
}
