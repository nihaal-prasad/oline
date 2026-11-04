// Package core holds primitive types shared across the engine, tools, sandbox,
// and TUI packages without creating import cycles.
package core

import "unicode"

// AgentID uniquely identifies a live agent. The orchestrator is always "Main";
// subagents are named like "Web-exploiter#1".
type AgentID string

// Main is the orchestrator's fixed id.
const Main AgentID = "Main"

// CanonRole upper-cases the first letter of a role/agent name for display, so every
// subagent name starts with a capital letter ("recon" -> "Recon",
// "web-exploiter" -> "Web-exploiter"). The rest is left unchanged; it is a no-op on
// an empty or already-capitalized string. Role lookups stay case-insensitive, so
// this only affects how names are presented, not how they resolve.
func CanonRole(s string) string {
	if s == "" {
		return s
	}
	r := []rune(s)
	r[0] = unicode.ToUpper(r[0])
	return string(r)
}

// RunMode is the engagement's operating mode — who drives the work:
//   - Automatic: the AI runs the engagement end-to-end on its own.
//   - Manual: the AI proposes each step and the user approves it before it runs.
//   - Watcher: the operator runs everything themselves; the AI does not execute at
//     all, it only watches and advises (it may still read and write workspace files).
//
// The mode can change mid-engagement. Watcher is enforced in the engine (execution
// tools are hard-blocked), not just by prompt.
type RunMode int

const (
	Automatic RunMode = iota
	Manual
	Watcher
)

func (m RunMode) String() string {
	switch m {
	case Manual:
		return "manual"
	case Watcher:
		return "watcher"
	default:
		return "automatic"
	}
}

// ParseRunMode maps a label (any case) to a RunMode, defaulting to Automatic.
func ParseRunMode(s string) RunMode {
	switch s {
	case "manual", "Manual", "MANUAL":
		return Manual
	case "watcher", "Watcher", "WATCHER":
		return Watcher
	default:
		return Automatic
	}
}

// State is an agent's lifecycle state.
type State int32

const (
	Running State = iota // actively working (LLM stream or tool call in flight)
	Paused               // ceased immediately; blocked until resumed
	Idle                 // live but awaiting a follow-up
	Killed               // terminal
)

func (s State) String() string {
	switch s {
	case Running:
		return "running"
	case Paused:
		return "paused"
	case Idle:
		return "idle"
	case Killed:
		return "killed"
	default:
		return "unknown"
	}
}
