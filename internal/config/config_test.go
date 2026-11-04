package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nihaal-prasad/oline/assets"
	"github.com/nihaal-prasad/oline/internal/config"
)

// TestEnsureModel adds a model the provider's catalog doesn't list yet (inheriting
// a sibling's shape, with zero cost), persists it to the user models.yml, keeps the
// existing models, and is a no-op for a model already present.
func TestEnsureModel(t *testing.T) {
	home := t.TempDir()
	t.Setenv("OLINE_HOME", home)
	s, err := config.Load(config.ModePentester, assets.PentesterFS)
	if err != nil {
		t.Fatal(err)
	}
	// oline ships no provider; define one with two models for this test.
	s.DefineProvider("fireworks", config.Provider{
		BaseURL: "https://api.fireworks.ai/inference/v1", API: "openai", APIKeyRef: "env:FIREWORKS_API_KEY",
		Models: []config.ModelDef{
			{ID: "accounts/fireworks/models/deepseek-v4p1-flash", Name: "DeepSeek", ContextWindow: 163840, MaxTokens: 32768},
			{ID: "accounts/fireworks/models/glm-5p3-flash", Name: "GLM", ContextWindow: 131072, MaxTokens: 32768},
		},
	})
	const newID = "accounts/fireworks/models/gpt-oss-120b"

	// Adding a brand-new model id reports added=true and makes it resolvable.
	added, err := s.EnsureModel("fireworks", newID)
	if err != nil || !added {
		t.Fatalf("EnsureModel new = (%v, %v), want (true, nil)", added, err)
	}
	var got *config.ModelDef
	for i, md := range s.Effective().Providers["fireworks"].Models {
		if md.ID == newID {
			got = &s.Effective().Providers["fireworks"].Models[i]
		}
	}
	if got == nil {
		t.Fatal("new model not added to the in-memory catalog")
	}
	// Inherited a sibling's context window; cost starts at zero.
	if got.ContextWindow == 0 {
		t.Error("new model should inherit a sibling's context window")
	}
	if got.Cost.Input != 0 {
		t.Errorf("new model cost should start at 0, got %v", got.Cost.Input)
	}

	// Persisted to the shared models.yml, and the embedded models survive alongside it.
	data, err := os.ReadFile(filepath.Join(home, "config", "models.yml"))
	if err != nil {
		t.Fatalf("models.yml not written: %v", err)
	}
	ds := string(data)
	if !strings.Contains(ds, "gpt-oss-120b") || !strings.Contains(ds, "deepseek-v4p1-flash") {
		t.Errorf("override missing the new or the existing model:\n%s", ds)
	}

	// Idempotent: a second call for the same id adds nothing.
	if added, err := s.EnsureModel("fireworks", newID); err != nil || added {
		t.Errorf("EnsureModel existing = (%v, %v), want (false, nil)", added, err)
	}
	// Unknown provider is an error, not an add.
	if _, err := s.EnsureModel("nope", "x"); err == nil {
		t.Error("EnsureModel on an unknown provider should error")
	}
}

// TestSaveModeFile writes an override file under config/<mode>/ and rejects paths
// that try to escape that directory.
func TestSaveModeFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("OLINE_HOME", home)
	s, err := config.Load(config.ModePentester, assets.PentesterFS)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SaveModeFile("agents/recon.md", "---\nname: recon\n---\n\nnew body\n"); err != nil {
		t.Fatalf("SaveModeFile: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(home, "config", "pentester", "agents", "recon.md"))
	if err != nil {
		t.Fatalf("override not written: %v", err)
	}
	if string(got) != "---\nname: recon\n---\n\nnew body\n" {
		t.Errorf("override content mismatch:\n%s", got)
	}
	// Path traversal that escapes the mode directory must be refused.
	for _, bad := range []string{"../escape.md", "../../secrets.yml", "agents/../../escape.md"} {
		if err := s.SaveModeFile(bad, "x"); err == nil {
			t.Errorf("SaveModeFile(%q) should have been refused", bad)
		}
	}
}

// TestAddProviderRoundTrips: a provider defined with AddProvider (oline ships none)
// persists to the user models.yml and reloads with its base URL, api, apiKeyRef,
// and models intact.
func TestAddProviderRoundTrips(t *testing.T) {
	home := t.TempDir()
	t.Setenv("OLINE_HOME", home)
	s, err := config.Load(config.ModePentester, assets.PentesterFS)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Providers()) != 0 {
		t.Fatalf("expected no default providers, got %d", len(s.Providers()))
	}
	models := []config.ModelDef{
		{ID: "gpt-4o", Name: "GPT-4o", ContextWindow: 128000, MaxTokens: 16384},
		{ID: "gpt-4o-mini", Name: "GPT-4o mini", ContextWindow: 128000, MaxTokens: 16384},
	}
	if err := s.AddProvider("openai", "https://api.openai.com/v1", "secret:openai", models); err != nil {
		t.Fatalf("AddProvider: %v", err)
	}

	// Reload from disk and confirm the provider survives in full.
	s2, err := config.Load(config.ModePentester, assets.PentesterFS)
	if err != nil {
		t.Fatal(err)
	}
	p, ok := s2.Effective().Providers["openai"]
	if !ok {
		t.Fatal("openai provider missing after reload")
	}
	if p.BaseURL != "https://api.openai.com/v1" {
		t.Errorf("baseUrl = %q", p.BaseURL)
	}
	if p.APIKeyRef != "secret:openai" {
		t.Errorf("apiKeyRef = %q", p.APIKeyRef)
	}
	if len(p.Models) != 2 || p.Models[0].ID != "gpt-4o" {
		t.Errorf("models not preserved: %+v", p.Models)
	}
}

// TestModelSetupSharedAcrossModes: configuring providers and tier roles in one
// mode makes the other mode load fully configured (no second setup wizard). This
// is the fix for the bug where exploiter re-prompted for models already set up in
// pentester.
func TestModelSetupSharedAcrossModes(t *testing.T) {
	home := t.TempDir()
	t.Setenv("OLINE_HOME", home)

	// Configure everything while in pentester mode.
	pent, err := config.Load(config.ModePentester, assets.PentesterFS)
	if err != nil {
		t.Fatal(err)
	}
	if err := pent.AddProvider("fireworks", "https://api.fireworks.ai/inference/v1", "secret:fireworks",
		[]config.ModelDef{{ID: "big-model"}, {ID: "small-model"}}); err != nil {
		t.Fatalf("AddProvider: %v", err)
	}
	for tier, sel := range map[string]string{
		"big":    "fireworks/big-model",
		"medium": "fireworks/big-model",
		"small":  "fireworks/small-model",
	} {
		if err := pent.SaveModelRole(tier, sel); err != nil {
			t.Fatalf("SaveModelRole(%s): %v", tier, err)
		}
	}

	// It landed in the shared file, not a per-mode one.
	if _, err := os.Stat(filepath.Join(home, "config", "models.yml")); err != nil {
		t.Fatalf("shared models.yml should exist: %v", err)
	}

	// Loading exploiter — never configured directly — must inherit all of it.
	expl, err := config.Load(config.ModeExploiter, assets.ExploiterFS)
	if err != nil {
		t.Fatal(err)
	}
	if !expl.ModelsConfigured() {
		t.Error("exploiter should be configured (inherited from pentester), but ModelsConfigured() is false")
	}
	r := expl.ModelRoles()
	if r.Big != "fireworks/big-model" || r.Small != "fireworks/small-model" {
		t.Errorf("exploiter did not inherit tier roles: %+v", r)
	}
	if _, ok := expl.Providers()["fireworks"]; !ok {
		t.Error("exploiter did not inherit the fireworks provider")
	}
}

// TestMigrateLegacyPerModeSetup: a user who configured models before the shared
// layer existed (setup written under config/<mode>/) is migrated on next load so
// both modes inherit it.
func TestMigrateLegacyPerModeSetup(t *testing.T) {
	home := t.TempDir()
	t.Setenv("OLINE_HOME", home)

	// Simulate the old on-disk layout: pentester configured, no shared file.
	pentDir := filepath.Join(home, "config", "pentester")
	if err := os.MkdirAll(pentDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pentDir, "models.yml"),
		[]byte("providers:\n  fireworks:\n    baseUrl: https://api.fireworks.ai/inference/v1\n    api: openai\n    apiKeyRef: secret:fireworks\n    models:\n      - id: big-model\n      - id: small-model\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pentDir, "config.yml"),
		[]byte("modelRoles:\n  big: fireworks/big-model\n  medium: fireworks/big-model\n  small: fireworks/small-model\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Loading exploiter triggers the one-time migration and inherits the setup.
	expl, err := config.Load(config.ModeExploiter, assets.ExploiterFS)
	if err != nil {
		t.Fatal(err)
	}
	if !expl.ModelsConfigured() {
		t.Error("exploiter should be configured after migrating pentester's legacy setup")
	}
	if _, ok := expl.Providers()["fireworks"]; !ok {
		t.Error("legacy provider was not migrated to the shared layer")
	}
	// The shared file now exists, so the migration won't run again.
	if _, err := os.Stat(filepath.Join(home, "config", "models.yml")); err != nil {
		t.Errorf("shared models.yml should have been created by migration: %v", err)
	}
}

func TestLoadDefaultsAndRoles(t *testing.T) {
	// Hermetic: point the oline root at an empty temp dir so the assertions about the
	// embedded defaults don't depend on (or get polluted by) a real ~/oline config.
	t.Setenv("OLINE_HOME", t.TempDir())
	s, err := config.Load(config.ModePentester, assets.PentesterFS)
	if err != nil {
		t.Fatal(err)
	}
	eff := s.Effective()
	// There are NO default models: all three tiers ship empty and the user must
	// configure them before an engagement can run.
	if eff.ModelRoles.Big != "" || eff.ModelRoles.Medium != "" || eff.ModelRoles.Small != "" {
		t.Errorf("model tiers should ship empty (no defaults), got %+v", eff.ModelRoles)
	}
	if s.ModelsConfigured() {
		t.Error("ModelsConfigured should be false with empty defaults")
	}
	if err := s.ValidateModels(); err == nil {
		t.Error("ValidateModels should fail when no tiers are configured")
	}
	if eff.MaxConcurrency != 5 {
		t.Errorf("maxConcurrency = %d, want 5", eff.MaxConcurrency)
	}
	// No provider ships by default; the user configures one in setup.
	if len(eff.Providers) != 0 {
		t.Errorf("no providers should ship by default, got %d", len(eff.Providers))
	}
	// Once a provider is defined, the tiers point at its models, and the key
	// resolves, validation passes.
	t.Setenv("FIREWORKS_API_KEY", "test-key")
	s.DefineProvider("fireworks", config.Provider{
		BaseURL: "https://api.fireworks.ai/inference/v1", API: "openai", APIKeyRef: "env:FIREWORKS_API_KEY",
		Models: []config.ModelDef{
			{ID: "accounts/fireworks/models/deepseek-v4p1-flash"},
			{ID: "accounts/fireworks/models/glm-5p3-flash"},
			{ID: "accounts/fireworks/models/nemotron-lightning-3p5-30b-a3b"},
		},
	})
	s.SetModelRoles(
		"fireworks/accounts/fireworks/models/deepseek-v4p1-flash",
		"fireworks/accounts/fireworks/models/glm-5p3-flash",
		"fireworks/accounts/fireworks/models/nemotron-lightning-3p5-30b-a3b",
	)
	if !s.ModelsConfigured() {
		t.Error("ModelsConfigured should be true after SetModelRoles")
	}
	if err := s.ValidateModels(); err != nil {
		t.Errorf("ValidateModels should pass for configured tiers with a key: %v", err)
	}
	if got := s.Effective().ModelForTier("big"); got == "" {
		t.Error("ModelForTier(big) empty after configuring")
	}
	if len(eff.Bash.Patterns) == 0 {
		t.Error("pentester should carry bash deny patterns")
	}
}

func isolateHome(t *testing.T) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("USERPROFILE", home) // Windows
	t.Setenv("HOME", home)        // others
}

func TestBackupDefaultAndHostRoot(t *testing.T) {
	// OLINE_HOME relocates the whole root (config, secrets, backups).
	home := t.TempDir()
	t.Setenv("OLINE_HOME", home)
	s, err := config.Load(config.ModePentester, assets.PentesterFS)
	if err != nil {
		t.Fatal(err)
	}
	if s.Root() != home {
		t.Errorf("Root() = %q, want %q", s.Root(), home)
	}
	if got, want := s.ConfigDir(), filepath.Join(home, "config"); got != want {
		t.Errorf("ConfigDir() = %q, want %q", got, want)
	}
	if got, want := s.BackupDir(), filepath.Join(home, "backups"); got != want {
		t.Errorf("BackupDir() = %q, want %q", got, want)
	}
	if s.Effective().Backup.IntervalMinutes != 10 {
		t.Errorf("default backup interval = %d, want 10", s.Effective().Backup.IntervalMinutes)
	}
}

func TestResolveKeyFromEnv(t *testing.T) {
	isolateHome(t) // don't read the real ~/oline/config/secrets.yml
	t.Setenv("FIREWORKS_API_KEY", "fw_test_key_123")
	s, err := config.Load(config.ModeExploiter, assets.ExploiterFS)
	if err != nil {
		t.Fatal(err)
	}
	s.DefineProvider("fireworks", config.Provider{APIKeyRef: "env:FIREWORKS_API_KEY"})
	got, err := s.ResolveKey("fireworks")
	if err != nil {
		t.Fatal(err)
	}
	if got != "fw_test_key_123" {
		t.Errorf("ResolveKey = %q, want the env value", got)
	}
}

func TestNoEmbeddedLiteralKey(t *testing.T) {
	// Public-repo safety: a fresh load with no env/secret must NOT yield a key.
	isolateHome(t)
	t.Setenv("FIREWORKS_API_KEY", "")
	s, err := config.Load(config.ModePentester, assets.PentesterFS)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ResolveKey("fireworks"); err == nil {
		t.Error("ResolveKey unexpectedly returned a key with no env/secret set — a literal key may be embedded")
	}
}

// TestWriteDefaults scaffolds a mode's config/prompt files to disk, keeps existing
// files on a second run, and overwrites them with force.
func TestWriteDefaults(t *testing.T) {
	t.Setenv("OLINE_HOME", t.TempDir())
	written, err := config.WriteDefaults(config.ModePentester, assets.PentesterFS, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(written) == 0 {
		t.Fatal("WriteDefaults wrote nothing")
	}
	dir := filepath.Join(config.HostRoot(), "config", "pentester")
	for _, rel := range []string{"config.yml", "models.yml", "SYSTEM_TEMPLATE.md", filepath.Join("agents", "recon.md")} {
		if _, err := os.Stat(filepath.Join(dir, rel)); err != nil {
			t.Errorf("expected %s on disk: %v", rel, err)
		}
	}
	// Second run is non-destructive: existing files are kept.
	if again, err := config.WriteDefaults(config.ModePentester, assets.PentesterFS, false); err != nil {
		t.Fatal(err)
	} else if len(again) != 0 {
		t.Errorf("second run should keep existing files, but wrote %d", len(again))
	}
	// Force rewrites them.
	if forced, err := config.WriteDefaults(config.ModePentester, assets.PentesterFS, true); err != nil {
		t.Fatal(err)
	} else if len(forced) == 0 {
		t.Error("force should rewrite the files")
	}
}
