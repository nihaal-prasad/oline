// Package agents parses the embedded (and on-disk overriding) agent definitions
// for a roster: the orchestrator prompt bundle plus one AgentDef per subagent.
package agents

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/nihaal-prasad/oline/internal/config"
	"gopkg.in/yaml.v3"
)

// CSV is a list that unmarshals from either a YAML sequence or a single
// comma-separated scalar (the reference frontmatter uses the comma form, e.g.
// `tools: bash, read, write`).
type CSV []string

// UnmarshalYAML accepts a scalar comma-list or a sequence.
func (c *CSV) UnmarshalYAML(n *yaml.Node) error {
	switch n.Kind {
	case yaml.ScalarNode:
		var s string
		if err := n.Decode(&s); err != nil {
			return err
		}
		*c = splitCSV(s)
		return nil
	case yaml.SequenceNode:
		var xs []string
		if err := n.Decode(&xs); err != nil {
			return err
		}
		out := make(CSV, 0, len(xs))
		for _, x := range xs {
			if x = strings.TrimSpace(x); x != "" {
				out = append(out, x)
			}
		}
		*c = out
		return nil
	case 0: // empty/absent
		return nil
	}
	return fmt.Errorf("agents: expected a string or list, got yaml kind %d", n.Kind)
}

func splitCSV(s string) CSV {
	parts := strings.Split(s, ",")
	out := make(CSV, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// Has reports whether the list contains name.
func (c CSV) Has(name string) bool {
	for _, x := range c {
		if x == name {
			return true
		}
	}
	return false
}

// AgentDef is one subagent's resolved definition: frontmatter + prompt body.
type AgentDef struct {
	Name          string `yaml:"name"`
	Description   string `yaml:"description"`
	Tier          string `yaml:"tier"`           // big|medium|small model tier ("" => medium)
	ThinkingLevel string `yaml:"thinking-level"` // low|medium|high => reasoning_effort
	Tools         CSV    `yaml:"tools"`
	Spawns        CSV    `yaml:"spawns"`
	Prompt        string `yaml:"-"` // markdown body (this agent's system prompt)
	// Frontmatter is the raw YAML header text (between the `---` fences, with its
	// trailing newline, fences excluded) exactly as read. The settings editor keeps
	// it verbatim when it rewrites a `.md` after a prompt edit, so tier/tools/etc.
	// survive untouched.
	Frontmatter string `yaml:"-"`
	// TimeBudget: 0 = inherit the config default; >0 = that many minutes;
	// <0 = no time limit. Set in frontmatter as `time-budget: <n>` / `none`.
	TimeBudget int `yaml:"-"`
	// MaxContextTokens: 0 = use the derived default; >0 = the context-token ceiling
	// at which this agent auto-compacts. Set in frontmatter as `max-context-tokens`.
	MaxContextTokens int `yaml:"-"`
}

// OrchDef is the orchestrator's prompt bundle. The orchestrator gets the full
// toolset (no tools allow-list) and the mode's personality.
type OrchDef struct {
	SystemTemplate string
	Personality    string
}

// Roster is a fully-loaded mode: orchestrator + every subagent, with user
// overrides from the config store already applied.
type Roster struct {
	Mode         config.Mode
	Orchestrator OrchDef
	Agents       map[string]AgentDef
}

// Lookup returns the agent definition for a role name, matching case-insensitively
// (so "Recon", "recon", and "RECON" all resolve to the same agent).
func (r *Roster) Lookup(role string) (AgentDef, bool) {
	if d, ok := r.Agents[role]; ok {
		return d, true
	}
	for name, d := range r.Agents {
		if strings.EqualFold(name, role) {
			return d, true
		}
	}
	return AgentDef{}, false
}

// Names returns the subagent names in sorted order.
func (r *Roster) Names() []string {
	out := make([]string, 0, len(r.Agents))
	for n := range r.Agents {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// LoadRoster assembles the roster for mode. defaultsFS is the embedded FS
// (rooted so "<mode>/..." resolves); on-disk files under
// ~/oline/config/<mode>/ take precedence over the embedded defaults.
func LoadRoster(mode config.Mode, defaultsFS fs.FS, store *config.Store) (*Roster, error) {
	if !mode.Valid() {
		return nil, fmt.Errorf("agents: unknown mode %q", mode)
	}
	userDir := filepath.Join(store.ConfigDir(), string(mode))

	readResolved := func(rel string) ([]byte, bool, error) {
		if data, err := os.ReadFile(filepath.Join(userDir, filepath.FromSlash(rel))); err == nil {
			return data, true, nil
		} else if !os.IsNotExist(err) {
			return nil, false, err
		}
		data, err := fs.ReadFile(defaultsFS, string(mode)+"/"+rel)
		if err != nil {
			return nil, false, err
		}
		return data, false, nil
	}

	orch := OrchDef{}
	if data, _, err := readResolved("SYSTEM_TEMPLATE.md"); err != nil {
		return nil, fmt.Errorf("agents: SYSTEM_TEMPLATE.md: %w", err)
	} else {
		orch.SystemTemplate = string(data)
	}
	if data, _, err := readResolved("PERSONALITY.md"); err != nil {
		return nil, fmt.Errorf("agents: PERSONALITY.md: %w", err)
	} else {
		orch.Personality = string(data)
	}

	// Enumerate subagent files from the embedded roster, then read each resolved
	// (on-disk override wins). On-disk-only extra agents are also picked up.
	names := map[string]struct{}{}
	entries, err := fs.ReadDir(defaultsFS, string(mode)+"/agents")
	if err != nil {
		return nil, fmt.Errorf("agents: listing embedded agents: %w", err)
	}
	for _, e := range entries {
		if n, ok := mdName(e.Name()); ok {
			names[n] = struct{}{}
		}
	}
	if extra, err := os.ReadDir(filepath.Join(userDir, "agents")); err == nil {
		for _, e := range extra {
			if n, ok := mdName(e.Name()); ok {
				names[n] = struct{}{}
			}
		}
	}

	eff := store.Effective()
	roster := &Roster{Mode: mode, Orchestrator: orch, Agents: make(map[string]AgentDef, len(names))}
	for n := range names {
		data, _, err := readResolved("agents/" + n + ".md")
		if err != nil {
			return nil, fmt.Errorf("agents: reading %s: %w", n, err)
		}
		def, err := parseAgent(data)
		if err != nil {
			return nil, fmt.Errorf("agents: parsing %s: %w", n, err)
		}
		if def.Name == "" {
			def.Name = n
		}
		applyOverride(&def, eff.Agents[def.Name])
		roster.Agents[def.Name] = def
	}
	return roster, nil
}

// parseAgent splits the frontmatter from the markdown body. The frontmatter is
// a flat set of `key: value` lines; values are parsed as free text (they may
// contain colons, e.g. "enumerates it: users, ..."), so this is a line-based
// parse rather than strict YAML. Unknown keys are ignored.
func parseAgent(data []byte) (AgentDef, error) {
	header, body, err := splitFrontmatter(data)
	if err != nil {
		return AgentDef{}, err
	}
	def := AgentDef{Prompt: body, Frontmatter: string(header)}
	for _, line := range strings.Split(string(header), "\n") {
		if strings.TrimSpace(line) == "" || strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t") {
			continue // blank or an indented continuation line
		}
		key, val, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		val = strings.TrimSpace(val)
		switch key {
		case "name":
			def.Name = val
		case "description":
			def.Description = val
		case "tier":
			def.Tier = val
		case "thinking-level":
			def.ThinkingLevel = val
		case "tools":
			def.Tools = splitCSV(val)
		case "spawns":
			def.Spawns = splitCSV(val)
		case "time-budget":
			def.TimeBudget = parseBudget(val)
		case "max-context-tokens":
			if n, err := strconv.Atoi(strings.TrimSpace(val)); err == nil && n > 0 {
				def.MaxContextTokens = n
			}
		}
	}
	return def, nil
}

// splitFrontmatter returns the YAML header (between the leading `---` fence and
// the next `---` line) and the remaining body.
func splitFrontmatter(data []byte) (header []byte, body string, err error) {
	s := strings.ReplaceAll(string(data), "\r\n", "\n")
	if !strings.HasPrefix(s, "---\n") {
		return nil, "", fmt.Errorf("missing opening --- frontmatter fence")
	}
	rest := s[len("---\n"):]
	idx := strings.Index(rest, "\n---")
	if idx < 0 {
		return nil, "", fmt.Errorf("missing closing --- frontmatter fence")
	}
	header = []byte(rest[:idx+1])
	after := rest[idx+len("\n---"):]
	after = strings.TrimPrefix(after, "\n")
	return header, strings.TrimLeft(after, "\n"), nil
}

func applyOverride(def *AgentDef, ov config.AgentOverride) {
	if ov.Tier != "" {
		def.Tier = ov.Tier
	}
	if ov.ThinkingLevel != "" {
		def.ThinkingLevel = ov.ThinkingLevel
	}
	if len(ov.Tools) > 0 {
		def.Tools = CSV(ov.Tools)
	}
	if len(ov.Spawns) > 0 {
		def.Spawns = CSV(ov.Spawns)
	}
	if ov.TimeBudget != nil {
		if *ov.TimeBudget == 0 {
			def.TimeBudget = -1 // explicit "no limit"
		} else {
			def.TimeBudget = *ov.TimeBudget
		}
	}
	if ov.MaxContextTokens != nil && *ov.MaxContextTokens > 0 {
		def.MaxContextTokens = *ov.MaxContextTokens
	}
}

// parseBudget reads a frontmatter time-budget value: "none"/"off"/"0" => no
// limit (-1); a positive number => that many minutes; anything else => 0
// (inherit the default).
func parseBudget(val string) int {
	switch strings.ToLower(strings.TrimSpace(val)) {
	case "none", "off", "no", "unlimited", "0":
		return -1
	}
	if n, err := strconv.Atoi(strings.TrimSpace(val)); err == nil && n > 0 {
		return n
	}
	return 0
}

func mdName(filename string) (string, bool) {
	if strings.HasSuffix(filename, ".md") {
		return strings.TrimSuffix(filename, ".md"), true
	}
	return "", false
}
