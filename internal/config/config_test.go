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

	// Persisted, and the embedded models survive alongside it.
	data, err := os.ReadFile(filepath.Join(home, "config", "pentester", "models.yml"))
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

// TestProviderOverrideKeepsEmbeddedFields: a user models.yml that overrides only
// a provider's baseUrl must NOT wipe that provider's embedded models / api /
// apiKeyRef (yaml replaces map values wholesale, so Load re-merges).
func TestProviderOverrideKeepsEmbeddedFields(t *testing.T) {
	home := t.TempDir()
	t.Setenv("OLINE_HOME", home)
	dir := filepath.Join(home, "config", "pentester")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "models.yml"),
		[]byte("providers:\n  fireworks:\n    baseUrl: https://changed.example\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := config.Load(config.ModePentester, assets.PentesterFS)
	if err != nil {
		t.Fatal(err)
	}
	p, ok := s.Effective().Providers["fireworks"]
	if !ok {
		t.Fatal("fireworks provider missing after override")
	}
	if p.BaseURL != "https://changed.example" {
		t.Errorf("baseUrl = %q, want the override", p.BaseURL)
	}
	if len(p.Models) != 3 {
		t.Errorf("models = %d, want 3 preserved from embedded", len(p.Models))
	}
	if p.APIKeyRef == "" {
		t.Error("apiKeyRef was wiped by the override")
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
	if _, ok := eff.Providers["fireworks"]; !ok {
		t.Error("fireworks provider not loaded from models.yml")
	}
	if len(eff.Providers["fireworks"].Models) != 3 {
		t.Errorf("fireworks models = %d, want 3", len(eff.Providers["fireworks"].Models))
	}
	// Once the tiers point at real catalog models and the provider key resolves,
	// validation passes.
	t.Setenv("FIREWORKS_API_KEY", "test-key")
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
