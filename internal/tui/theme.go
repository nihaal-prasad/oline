package tui

import "github.com/charmbracelet/lipgloss"

// Dark-only palette for oline.
var (
	colBg     = lipgloss.Color("#0c0c0c") // Windows Terminal's default dark background
	colFg     = lipgloss.Color("#d7dce2")
	colMuted  = lipgloss.Color("#6b7684")
	colBorder = lipgloss.Color("#2a3340")
	colAccent = lipgloss.Color("#4aa3ff") // oline blue
	colGreen  = lipgloss.Color("#53d38a")
	colYellow = lipgloss.Color("#e0c060")
	colRed    = lipgloss.Color("#ff6b6b")
	colGray   = lipgloss.Color("#8a94a0")
)

var (
	styleMuted  = lipgloss.NewStyle().Foreground(colMuted)
	styleAccent = lipgloss.NewStyle().Foreground(colAccent).Bold(true)
	styleThink  = lipgloss.NewStyle().Foreground(colGray).Italic(true)
	styleErr    = lipgloss.NewStyle().Foreground(colRed)
	styleWarn   = lipgloss.NewStyle().Foreground(colYellow)
	// styleToolOut renders a tool's OUTPUT in the normal foreground, distinct from
	// the dim muted style used for the tool call + its parameters, so the command
	// and its result are visually different.
	styleToolOut = lipgloss.NewStyle().Foreground(colFg)

	paneBorder = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(colBorder)
	paneBorderFocused = lipgloss.NewStyle().
				Border(lipgloss.RoundedBorder()).
				BorderForeground(colAccent)

	titleBar  = lipgloss.NewStyle().Foreground(lipgloss.Color("#ffffff")).Background(colAccent).Bold(true).Padding(0, 1)
	statusBar = lipgloss.NewStyle().Foreground(colFg).Background(colBorder).Padding(0, 1)

	// screenStyle paints the entire TUI on a pure-black background so the logo's
	// black blends in and there is no mismatched backdrop.
	screenStyle = lipgloss.NewStyle().Background(colBg).Foreground(colFg)
)

// severityStyle colors a finding severity label: critical/high are red, medium
// yellow, low green, and info/anything-else muted.
func severityStyle(sev string) lipgloss.Style {
	switch sev {
	case "critical", "high":
		return lipgloss.NewStyle().Foreground(colRed).Bold(true)
	case "medium":
		return lipgloss.NewStyle().Foreground(colYellow)
	case "low":
		return lipgloss.NewStyle().Foreground(colGreen)
	default:
		return lipgloss.NewStyle().Foreground(colMuted)
	}
}

// stateStyle colors an agent lifecycle state label.
func stateStyle(s string) lipgloss.Style {
	switch s {
	case "running":
		return lipgloss.NewStyle().Foreground(colGreen)
	case "paused":
		return lipgloss.NewStyle().Foreground(colYellow)
	case "killed":
		return lipgloss.NewStyle().Foreground(colRed)
	default:
		return lipgloss.NewStyle().Foreground(colMuted)
	}
}
