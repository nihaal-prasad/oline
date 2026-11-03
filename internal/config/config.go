// Package config loads and merges oline's harness configuration.
//
// Three layers, lowest precedence first:
//  1. embedded defaults            (assets/<mode>/config.yml + models.yml)
//  2. on-disk user overrides       (~/oline/config/<mode>/config.yml + models.yml)
//  3. in-session edits             (applied via Save*, persisted to layer 2)
//
// API keys are never stored in config.yml/models.yml. Each provider carries an
// apiKeyRef ("env:NAME" or "secret:<provider>") resolved at runtime; keys saved
// from the in-app editor live in ~/oline/config/secrets.yml (mode 0600, git-ignored).
package config

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// Mode selects which roster/config bundle to load.
type Mode string

const (
	ModePentester Mode = "pentester"
	ModeExploiter Mode = "exploiter"
)

// Valid reports whether m is a known mode.
func (m Mode) Valid() bool { return m == ModePentester || m == ModeExploiter }

// Root is the fully-merged effective configuration.
type Root struct {
	ModelRoles     ModelRoles               `yaml:"modelRoles"`
	Providers      map[string]Provider      `yaml:"providers"`
	Bash           BashRules                `yaml:"bash"`
	Agents         map[string]AgentOverride `yaml:"agents"`
	MaxConcurrency int                      `yaml:"maxConcurrency"`
	Backup         Backup                   `yaml:"backup"`
	Budget         Budget                   `yaml:"budget"`
	Network        Network                  `yaml:"network"`
}

// Network controls the sandbox's per-agent network policy.
type Network struct {
	// AllowIPv6 enables outbound non-loopback IPv6 for the agent users. Off by
	// default: the per-agent firewall otherwise drops all IPv6 egress for the oline
	// users so an agent can't bypass the (IPv4) scope rules over IPv6. Turn it on
	// for an engagement whose targets are reachable only over IPv6; the same
	// scope/net/iso policy is then enforced over IPv6 as well.
	AllowIPv6 bool `yaml:"allowIPv6"`
}

// Backup controls host-side compressed backups of the engagement workspace.
type Backup struct {
	// IntervalMinutes between automatic backups; 0 disables backups entirely.
	IntervalMinutes int `yaml:"intervalMinutes"`
}

// Budget controls per-subagent time budgets, enforced by the engine (not the
// AI): a subagent that runs past its budget gets escalating reminders and is
// terminated if it keeps running. Individual agents may set their own budget in
// frontmatter (`time-budget`), and the user can change or remove any running
// agent's budget live.
type Budget struct {
	// DefaultMinutes is the per-subagent budget for agents that don't set their
	// own; 0 means no default limit. The orchestrator is never time-limited.
	DefaultMinutes int `yaml:"defaultMinutes"`
	// MaxCostUSD is the engagement-wide spend cap in US dollars (summed across all
	// agents, priced from each model's `cost`). When the running total reaches it,
	// oline pauses every agent and tells the operator; 0 disables the cap.
	MaxCostUSD float64 `yaml:"maxCostUSD"`
	// MaxSummaryCostUSD caps the cumulative spend of the `small`-tier summarizer used
	// for context compaction (a guard against a misconfigured, expensive small model
	// silently running up cost). When reached, oline pauses the fleet and tells the
	// operator; 0 disables this sub-cap. This spend also counts toward MaxCostUSD.
	MaxSummaryCostUSD float64 `yaml:"maxSummaryCostUSD"`
}

// ModelRoles maps the three model tiers to "<provider>/<model-id>" selectors.
// Every agent runs on one of these tiers, and context compaction uses `small`.
// There are no defaults: the user must configure all three the first time (the
// engagement will not start otherwise), after which the choices are saved.
type ModelRoles struct {
	Big    string `yaml:"big"`    // strongest: exploiters, thinkers, critiques, the orchestrator
	Medium string `yaml:"medium"` // in-between work
	Small  string `yaml:"small"`  // fastest/cheapest: summarizers, and context compaction
}

// ModelForTier returns the "provider/model-id" selector for a tier name
// (big|medium|small), defaulting an unknown/empty tier to medium.
func (r Root) ModelForTier(tier string) string {
	switch strings.ToLower(strings.TrimSpace(tier)) {
	case "big":
		return r.ModelRoles.Big
	case "small":
		return r.ModelRoles.Small
	default:
		return r.ModelRoles.Medium
	}
}

// Provider is an OpenAI-compatible model provider.
type Provider struct {
	BaseURL   string     `yaml:"baseUrl"`
	API       string     `yaml:"api"`
	APIKeyRef string     `yaml:"apiKeyRef"`
	Models    []ModelDef `yaml:"models"`
}

// ModelDef describes one model offered by a Provider.
type ModelDef struct {
	ID            string       `yaml:"id"`
	Name          string       `yaml:"name"`
	ContextWindow int          `yaml:"contextWindow"`
	MaxTokens     int          `yaml:"maxTokens"`
	Reasoning     bool         `yaml:"reasoning"`
	Thinking      ThinkingSpec `yaml:"thinking"`
	Compat        Compat       `yaml:"compat"`
	Cost          Cost         `yaml:"cost"`
}

type ThinkingSpec struct {
	Mode         string   `yaml:"mode"`
	Efforts      []string `yaml:"efforts"`
	DefaultLevel string   `yaml:"defaultLevel"`
}

type Compat struct {
	ThinkingFormat string `yaml:"thinkingFormat"`
}

type Cost struct {
	Input      float64 `yaml:"input"`
	Output     float64 `yaml:"output"`
	CacheRead  float64 `yaml:"cacheRead"`
	CacheWrite float64 `yaml:"cacheWrite"`
}

type BashRules struct {
	Patterns []BashPattern `yaml:"patterns"`
}

type BashPattern struct {
	Match    string `yaml:"match"`
	Approval string `yaml:"approval"`
}

// AgentOverride is a per-subagent user override applied on top of the agent's
// own agents/<name>.md frontmatter.
type AgentOverride struct {
	// Tier is the model tier this agent runs on (big|medium|small); empty leaves the
	// agent's own frontmatter tier in place.
	Tier          string   `yaml:"tier,omitempty"`
	ThinkingLevel string   `yaml:"thinking-level,omitempty"`
	Tools         []string `yaml:"tools,omitempty"`
	Spawns        []string `yaml:"spawns,omitempty"`
	// TimeBudget overrides this agent's budget in minutes (>0), or removes it
	// (<0 / "none"); nil leaves the frontmatter/default budget in place.
	TimeBudget *int `yaml:"time-budget,omitempty"`
	// MaxContextTokens overrides the context-token ceiling at which this agent's
	// conversation is auto-compacted; nil leaves the frontmatter/derived default.
	MaxContextTokens *int `yaml:"max-context-tokens,omitempty"`
}

// Store holds the merged effective config plus the resolved on-disk locations
// and the loaded secrets, so later Save* calls persist to the right place.
type Store struct {
	mode      Mode
	eff       Root
	root      string            // host oline root ($OLINE_HOME or ~/oline)
	configDir string            // <root>/config
	userDir   string            // <root>/config/<mode>
	secrets   map[string]string // provider -> literal key (from secrets.yml)
}

// HostRoot returns the oline root directory on the host. It is $OLINE_HOME if
// set, otherwise ~/oline. This is where config, secrets, and backups live.
func HostRoot() string {
	if r := strings.TrimSpace(os.Getenv("OLINE_HOME")); r != "" {
		return r
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "oline"
	}
	return filepath.Join(home, "oline")
}

// LogDir returns the host directory that holds per-engagement activity logs
// (<HostRoot>/logs). The program writes these; agents never do.
func LogDir() string {
	return filepath.Join(HostRoot(), "logs")
}

// HistoryDir returns the host directory that holds per-engagement conversation
// history (<HostRoot>/history), used to reload an engagement after a restart.
func HistoryDir() string {
	return filepath.Join(HostRoot(), "history")
}

// WriteDefaults scaffolds the embedded defaults for a mode into its on-disk config
// directory (<HostRoot>/config/<mode>/): config.yml, models.yml, and every prompt
// file (SYSTEM_TEMPLATE.md, PERSONALITY.md, METHODOLOGY.md, agents/*.md). Those
// files override the embedded defaults at load time, so writing them out lets a
// user edit any aspect — models, costs, token ceilings, time budgets, tools,
// personalities, and every agent prompt — in plain files. Existing files are left
// untouched unless force is true. It returns the paths it wrote. defaultsFS is the
// mode's embedded FS (assets.PentesterFS / assets.ExploiterFS).
func WriteDefaults(mode Mode, defaultsFS fs.FS, force bool) ([]string, error) {
	if !mode.Valid() {
		return nil, fmt.Errorf("config: unknown mode %q", mode)
	}
	root := filepath.Join(HostRoot(), "config", string(mode))
	var written []string
	err := fs.WalkDir(defaultsFS, string(mode), func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel := strings.TrimPrefix(strings.TrimPrefix(p, string(mode)), "/")
		dest := filepath.Join(root, filepath.FromSlash(rel))
		if d.IsDir() {
			return os.MkdirAll(dest, 0o700)
		}
		if !force {
			if _, statErr := os.Stat(dest); statErr == nil {
				return nil // keep the user's existing file
			}
		}
		data, readErr := fs.ReadFile(defaultsFS, p)
		if readErr != nil {
			return readErr
		}
		if mkErr := os.MkdirAll(filepath.Dir(dest), 0o700); mkErr != nil {
			return mkErr
		}
		if wErr := os.WriteFile(dest, data, 0o644); wErr != nil {
			return wErr
		}
		written = append(written, dest)
		return nil
	})
	return written, err
}

// Load reads the embedded defaults for mode from defaultsFS (rooted so that
// "<mode>/config.yml" resolves), overlays any on-disk user files, and loads
// secrets. defaultsFS is typically assets.PentesterFS / assets.ExploiterFS.
func Load(mode Mode, defaultsFS fs.FS) (*Store, error) {
	if !mode.Valid() {
		return nil, fmt.Errorf("config: unknown mode %q", mode)
	}
	var root Root
	// Layer 1: embedded defaults.
	for _, name := range []string{"config.yml", "models.yml"} {
		data, err := fs.ReadFile(defaultsFS, string(mode)+"/"+name)
		if err != nil {
			return nil, fmt.Errorf("config: reading embedded %s/%s: %w", mode, name, err)
		}
		if err := yaml.Unmarshal(data, &root); err != nil {
			return nil, fmt.Errorf("config: parsing embedded %s/%s: %w", mode, name, err)
		}
	}

	hostRoot := HostRoot()
	s := &Store{
		mode:      mode,
		root:      hostRoot,
		configDir: filepath.Join(hostRoot, "config"),
		secrets:   map[string]string{},
	}
	s.userDir = filepath.Join(s.configDir, string(mode))

	// Remember the embedded providers so a user override that sets only some of a
	// provider's fields (e.g. SaveProvider writes just baseUrl) doesn't wipe the
	// rest: yaml.v3 replaces a map value wholesale rather than deep-merging, so we
	// restore the untouched fields from the embedded layer below.
	baseProviders := make(map[string]Provider, len(root.Providers))
	for k, v := range root.Providers {
		baseProviders[k] = v
	}

	// Layer 2: on-disk user overrides (optional).
	for _, name := range []string{"config.yml", "models.yml"} {
		p := filepath.Join(s.userDir, name)
		data, err := os.ReadFile(p)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("config: reading %s: %w", p, err)
		}
		if err := yaml.Unmarshal(data, &root); err != nil {
			return nil, fmt.Errorf("config: parsing %s: %w", p, err)
		}
	}

	// Fill back any provider fields the override left unset (zero), so overriding
	// one field keeps the embedded models/api/key-ref/cost for that provider.
	for name, p := range root.Providers {
		bp, ok := baseProviders[name]
		if !ok {
			continue // a brand-new provider the user defined entirely
		}
		if p.BaseURL == "" {
			p.BaseURL = bp.BaseURL
		}
		if p.API == "" {
			p.API = bp.API
		}
		if p.APIKeyRef == "" {
			p.APIKeyRef = bp.APIKeyRef
		}
		if len(p.Models) == 0 {
			p.Models = bp.Models
		}
		root.Providers[name] = p
	}

	// Secrets (optional).
	secPath := filepath.Join(s.configDir, "secrets.yml")
	if data, err := os.ReadFile(secPath); err == nil {
		if err := yaml.Unmarshal(data, &s.secrets); err != nil {
			return nil, fmt.Errorf("config: parsing %s: %w", secPath, err)
		}
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("config: reading %s: %w", secPath, err)
	}

	s.eff = root
	return s, nil
}

// Effective returns the merged configuration.
func (s *Store) Effective() Root { return s.eff }

// Mode returns the store's mode.
func (s *Store) Mode() Mode { return s.mode }

// ConfigDir returns <root>/config.
func (s *Store) ConfigDir() string { return s.configDir }

// Root returns the host oline root directory for this store.
func (s *Store) Root() string { return s.root }

// BackupDir returns the host directory where engagement backups are written.
func (s *Store) BackupDir() string { return filepath.Join(s.root, "backups") }

// ResolveKey returns the API key for a provider, trying its apiKeyRef first
// (env:NAME or secret:NAME) and falling back to a key saved for that provider
// in secrets.yml. It never logs or returns the key as part of an error.
func (s *Store) ResolveKey(provider string) (string, error) {
	p, ok := s.eff.Providers[provider]
	if !ok {
		return "", fmt.Errorf("config: unknown provider %q", provider)
	}
	// An explicit key saved for this provider (e.g. via the in-app editor) is the
	// user's deliberate choice and wins over an ambient env var / ref.
	if v := s.secrets[provider]; v != "" {
		return v, nil
	}
	ref := strings.TrimSpace(p.APIKeyRef)
	if ref != "" {
		kind, name, found := strings.Cut(ref, ":")
		if !found {
			return "", fmt.Errorf("config: provider %q apiKeyRef %q is not <kind>:<name>", provider, ref)
		}
		switch kind {
		case "env":
			if v := os.Getenv(name); v != "" {
				return v, nil
			}
		case "secret":
			if v := s.secrets[name]; v != "" {
				return v, nil
			}
		default:
			return "", fmt.Errorf("config: provider %q apiKeyRef has unknown kind %q", provider, kind)
		}
	}
	return "", fmt.Errorf("config: no API key found for provider %q (set %s or save one in the model editor)", provider, p.APIKeyRef)
}

// KnownSecrets returns the literal API-key values the store can currently
// resolve (from secrets.yml and env refs), for scrubbing from host-side logs.
// Best-effort and deduplicated; it never returns empty or trivially short values.
func (s *Store) KnownSecrets() []string {
	seen := map[string]bool{}
	var out []string
	add := func(v string) {
		if len(v) >= 8 && !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	for _, v := range s.secrets {
		add(v)
	}
	for name := range s.eff.Providers {
		if v, err := s.ResolveKey(name); err == nil {
			add(v)
		}
	}
	return out
}

// SaveSecret persists an API key for a provider to ~/oline/config/secrets.yml
// (mode 0600) and updates the in-memory store.
func (s *Store) SaveSecret(provider, key string) error {
	if s.secrets == nil {
		s.secrets = map[string]string{}
	}
	s.secrets[provider] = key
	if err := os.MkdirAll(s.configDir, 0o700); err != nil {
		return fmt.Errorf("config: creating %s: %w", s.configDir, err)
	}
	data, err := yaml.Marshal(s.secrets)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(s.configDir, "secrets.yml"), data, 0o600)
}

// SaveProvider persists a provider's baseURL into the user models.yml override
// (~/oline/config/<mode>/models.yml) and updates the in-memory store. It never
// writes an API key — keys go to secrets.yml via SaveSecret.
func (s *Store) SaveProvider(name, baseURL string) error {
	p, ok := s.eff.Providers[name]
	if !ok {
		return fmt.Errorf("config: unknown provider %q", name)
	}
	p.BaseURL = baseURL
	s.eff.Providers[name] = p

	// Load the existing user models.yml (if any) and rewrite ONLY this provider's
	// baseUrl, so embedded defaults (and any other user content) are preserved
	// rather than frozen into the override layer.
	userPath := filepath.Join(s.userDir, "models.yml")
	var userRoot map[string]any
	if data, err := os.ReadFile(userPath); err == nil {
		_ = yaml.Unmarshal(data, &userRoot)
	}
	if userRoot == nil {
		userRoot = map[string]any{}
	}
	provMap, _ := userRoot["providers"].(map[string]any)
	if provMap == nil {
		provMap = map[string]any{}
	}
	entry, _ := provMap[name].(map[string]any)
	if entry == nil {
		entry = map[string]any{}
	}
	entry["baseUrl"] = baseURL
	provMap[name] = entry
	userRoot["providers"] = provMap

	if err := os.MkdirAll(s.userDir, 0o700); err != nil {
		return fmt.Errorf("config: creating %s: %w", s.userDir, err)
	}
	data, err := yaml.Marshal(userRoot)
	if err != nil {
		return err
	}
	return os.WriteFile(userPath, data, 0o600)
}

// SaveModelCost persists one model's cost (USD per 1M tokens) into the user
// models.yml override (~/oline/config/<mode>/models.yml) and updates the in-memory
// store. The override layer replaces a provider's whole model list wholesale (see
// Load's fill-back), so this writes the provider's full current model list with the
// updated cost — keeping every other model and all of this model's other fields
// (id, name, context window, thinking spec) intact.
func (s *Store) SaveModelCost(provider, modelID string, cost Cost) error {
	p, ok := s.eff.Providers[provider]
	if !ok {
		return fmt.Errorf("config: unknown provider %q", provider)
	}
	found := false
	for i := range p.Models {
		if p.Models[i].ID == modelID {
			p.Models[i].Cost = cost
			found = true
			break
		}
	}
	if !found {
		return fmt.Errorf("config: provider %q has no model %q", provider, modelID)
	}
	s.eff.Providers[provider] = p
	return s.writeUserProviderModels(provider, p.Models)
}

// EnsureModel makes sure provider's catalog contains a model with id modelID,
// adding one if it doesn't — so pointing a tier at a new model id validates and
// runs instead of being rejected as "not in the model list". A new entry inherits
// the shape of an existing sibling model (context window, output cap, thinking
// spec, compat) when the provider has one, so a model from the same provider works
// out of the box; its cost starts at zero for the user to set. With no sibling it
// gets conservative empty defaults (the engine then uses its fallbacks). It
// persists to the user models.yml and reports whether it added the model.
func (s *Store) EnsureModel(provider, modelID string) (added bool, err error) {
	p, ok := s.eff.Providers[provider]
	if !ok {
		return false, fmt.Errorf("config: unknown provider %q", provider)
	}
	for _, md := range p.Models {
		if md.ID == modelID {
			return false, nil // already in the catalog
		}
	}
	nm := ModelDef{ID: modelID, Name: modelID}
	if len(p.Models) > 0 {
		// Inherit the provider's typical model shape from a sibling; keep the new
		// id/name and leave cost at zero for the user to fill in.
		t := p.Models[0]
		nm.ContextWindow = t.ContextWindow
		nm.MaxTokens = t.MaxTokens
		nm.Reasoning = t.Reasoning
		nm.Thinking = t.Thinking
		nm.Compat = t.Compat
	}
	p.Models = append(p.Models, nm)
	s.eff.Providers[provider] = p
	if err := s.writeUserProviderModels(provider, p.Models); err != nil {
		return false, err
	}
	return true, nil
}

// writeUserProviderModels rewrites one provider's full model list in the user
// models.yml override.
func (s *Store) writeUserProviderModels(provider string, models []ModelDef) error {
	return s.writeUserProviderEntry(provider, map[string]any{"models": models})
}

// writeUserProviderEntry merges fields into one provider's entry in the user
// models.yml override, preserving the rest of that file and the provider's other
// keys. The override layer replaces a provider's model list wholesale, so a models
// field must carry the full list.
func (s *Store) writeUserProviderEntry(provider string, fields map[string]any) error {
	userPath := filepath.Join(s.userDir, "models.yml")
	var userRoot map[string]any
	if data, err := os.ReadFile(userPath); err == nil {
		_ = yaml.Unmarshal(data, &userRoot)
	}
	if userRoot == nil {
		userRoot = map[string]any{}
	}
	provMap, _ := userRoot["providers"].(map[string]any)
	if provMap == nil {
		provMap = map[string]any{}
	}
	entry, _ := provMap[provider].(map[string]any)
	if entry == nil {
		entry = map[string]any{}
	}
	for k, v := range fields {
		entry[k] = v
	}
	provMap[provider] = entry
	userRoot["providers"] = provMap

	if err := os.MkdirAll(s.userDir, 0o700); err != nil {
		return fmt.Errorf("config: creating %s: %w", s.userDir, err)
	}
	data, err := yaml.Marshal(userRoot)
	if err != nil {
		return err
	}
	return os.WriteFile(userPath, data, 0o600)
}

// DefineProvider sets a provider in the in-memory store only (no disk write). Used
// by the setup wizard before it persists, and by tests.
func (s *Store) DefineProvider(name string, p Provider) {
	if s.eff.Providers == nil {
		s.eff.Providers = map[string]Provider{}
	}
	s.eff.Providers[name] = p
}

// AddProvider defines a new OpenAI-compatible provider (base URL, api-key
// reference, and its models) in the user models.yml and the in-memory store. This
// is how a provider is configured: oline ships none by default. The name is the
// selector prefix, so it must be non-empty with no space or '/'.
func (s *Store) AddProvider(name, baseURL, apiKeyRef string, models []ModelDef) error {
	name = strings.TrimSpace(name)
	if name == "" || strings.ContainsAny(name, "/ \t") {
		return fmt.Errorf("config: provider name must be non-empty with no spaces or '/'")
	}
	p := s.eff.Providers[name]
	p.BaseURL = strings.TrimSpace(baseURL)
	if strings.TrimSpace(p.API) == "" {
		p.API = "openai"
	}
	p.APIKeyRef = strings.TrimSpace(apiKeyRef)
	p.Models = models
	s.DefineProvider(name, p)
	return s.writeUserProviderEntry(name, map[string]any{
		"baseUrl":   p.BaseURL,
		"api":       p.API,
		"apiKeyRef": p.APIKeyRef,
		"models":    p.Models,
	})
}

// SaveModeFile writes a user override file under ~/oline/config/<mode>/<rel>
// (creating parent directories), where it takes precedence over the embedded
// default when the roster is next loaded. rel is a slash-separated path relative
// to the mode directory (e.g. "SYSTEM_TEMPLATE.md" or "agents/recon.md"); it must
// not escape that directory. Used by the settings editor to persist edited system
// prompts.
func (s *Store) SaveModeFile(rel, content string) error {
	clean := filepath.Clean(filepath.FromSlash(rel))
	if clean == "." || filepath.IsAbs(clean) {
		return fmt.Errorf("config: invalid mode-file path %q", rel)
	}
	dest := filepath.Join(s.userDir, clean)
	// Containment check: dest must stay inside the mode directory.
	if rp, err := filepath.Rel(s.userDir, dest); err != nil || rp == ".." || strings.HasPrefix(rp, ".."+string(filepath.Separator)) {
		return fmt.Errorf("config: invalid mode-file path %q", rel)
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o700); err != nil {
		return fmt.Errorf("config: creating %s: %w", filepath.Dir(dest), err)
	}
	return os.WriteFile(dest, []byte(content), 0o644)
}

// Providers returns the provider names in the effective config.
func (s *Store) Providers() map[string]Provider { return s.eff.Providers }

// ModelRoles returns the effective tier->model mapping.
func (s *Store) ModelRoles() ModelRoles { return s.eff.ModelRoles }

// SetModelRoles sets the in-memory tier->model mapping without persisting it
// (used by the setup wizard before it saves, and by tests).
func (s *Store) SetModelRoles(big, medium, small string) {
	s.eff.ModelRoles = ModelRoles{Big: big, Medium: medium, Small: small}
}

// ModelsConfigured reports whether all three tiers are set to something.
func (s *Store) ModelsConfigured() bool {
	r := s.eff.ModelRoles
	return strings.TrimSpace(r.Big) != "" && strings.TrimSpace(r.Medium) != "" && strings.TrimSpace(r.Small) != ""
}

// SaveModelRole persists one tier's model selector into the user config.yml
// (~/oline/config/<mode>/config.yml) and updates the in-memory store.
func (s *Store) SaveModelRole(tier, selector string) error {
	switch strings.ToLower(tier) {
	case "big":
		s.eff.ModelRoles.Big = selector
	case "medium":
		s.eff.ModelRoles.Medium = selector
	case "small":
		s.eff.ModelRoles.Small = selector
	default:
		return fmt.Errorf("config: unknown model tier %q (want big|medium|small)", tier)
	}
	userCfgPath := filepath.Join(s.userDir, "config.yml")
	var userRoot map[string]any
	if data, err := os.ReadFile(userCfgPath); err == nil {
		_ = yaml.Unmarshal(data, &userRoot)
	}
	if userRoot == nil {
		userRoot = map[string]any{}
	}
	roles, _ := userRoot["modelRoles"].(map[string]any)
	if roles == nil {
		roles = map[string]any{}
	}
	roles[strings.ToLower(tier)] = selector
	userRoot["modelRoles"] = roles
	if err := os.MkdirAll(s.userDir, 0o700); err != nil {
		return fmt.Errorf("config: creating %s: %w", s.userDir, err)
	}
	data, err := yaml.Marshal(userRoot)
	if err != nil {
		return err
	}
	return os.WriteFile(userCfgPath, data, 0o600)
}

// ValidateModels checks every tier is configured, each resolves to a known
// provider/model-id in the catalog, and each needed provider has a resolvable API
// key. It returns a descriptive error (so the engagement can refuse to start) or nil.
func (s *Store) ValidateModels() error {
	for _, tier := range []string{"big", "medium", "small"} {
		sel := strings.TrimSpace(s.eff.ModelForTier(tier))
		if sel == "" {
			return fmt.Errorf("the %q model is not configured (set it in setup or the editor)", tier)
		}
		prov, id, ok := strings.Cut(sel, "/")
		if !ok || prov == "" || id == "" {
			return fmt.Errorf("the %q model %q must be in provider/model-id form", tier, sel)
		}
		p, ok := s.eff.Providers[prov]
		if !ok {
			return fmt.Errorf("the %q model uses unknown provider %q", tier, prov)
		}
		found := false
		for _, md := range p.Models {
			if md.ID == id {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("the %q model %q is not in provider %q's model list", tier, id, prov)
		}
		if _, err := s.ResolveKey(prov); err != nil {
			return fmt.Errorf("the %q model's provider %q has no usable API key: %w", tier, prov, err)
		}
	}
	return nil
}

// SaveAgent persists a per-agent override into the user config layer
// (~/oline/config/<mode>/config.yml) and applies it to the in-memory store.
func (s *Store) SaveAgent(name string, ov AgentOverride) error {
	if s.eff.Agents == nil {
		s.eff.Agents = map[string]AgentOverride{}
	}
	// Merge onto any existing override so an edit of the model/thinking/context
	// fields (what the in-app editor sets) doesn't wipe a previously-saved
	// tools/spawns/time-budget override for this agent.
	merged := s.eff.Agents[name]
	merged.Tier = ov.Tier
	merged.ThinkingLevel = ov.ThinkingLevel
	merged.MaxContextTokens = ov.MaxContextTokens
	if ov.Tools != nil {
		merged.Tools = ov.Tools
	}
	if ov.Spawns != nil {
		merged.Spawns = ov.Spawns
	}
	if ov.TimeBudget != nil {
		merged.TimeBudget = ov.TimeBudget
	}
	ov = merged
	s.eff.Agents[name] = ov

	// Load the existing user config.yml (if any) so we only rewrite its agents map.
	userCfgPath := filepath.Join(s.userDir, "config.yml")
	var userRoot map[string]any
	if data, err := os.ReadFile(userCfgPath); err == nil {
		_ = yaml.Unmarshal(data, &userRoot)
	}
	if userRoot == nil {
		userRoot = map[string]any{}
	}
	agentsMap, _ := userRoot["agents"].(map[string]any)
	if agentsMap == nil {
		agentsMap = map[string]any{}
	}
	agentsMap[name] = ov
	userRoot["agents"] = agentsMap

	if err := os.MkdirAll(s.userDir, 0o700); err != nil {
		return fmt.Errorf("config: creating %s: %w", s.userDir, err)
	}
	data, err := yaml.Marshal(userRoot)
	if err != nil {
		return err
	}
	return os.WriteFile(userCfgPath, data, 0o600)
}
