package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
)

// TestEditorSaveOrchestratorPrompt checks the orchestrator's three prompt items
// (system prompt, personality, methodology) each expose a box and save to their own
// .md override while updating the matching in-memory roster field.
func TestEditorSaveOrchestratorPrompt(t *testing.T) {
	cases := []struct {
		item string // the editor sub-item name
		file string // the override file it writes
		get  func(*model) string
	}{
		{"system prompt", "SYSTEM_TEMPLATE.md", func(m *model) string { return m.roster.Orchestrator.SystemTemplate }},
		{"personality", "PERSONALITY.md", func(m *model) string { return m.roster.Orchestrator.Personality }},
		{"methodology", "METHODOLOGY.md", func(m *model) string { return m.roster.Orchestrator.Methodology }},
	}
	for _, tc := range cases {
		t.Run(tc.item, func(t *testing.T) {
			m, home := tempHomeModel(t) // pentester → methodology is present
			m.openEditor()
			i := findItem(m, "orchestrator", tc.item)
			if i < 0 {
				t.Fatalf("orchestrator %q not in editor list", tc.item)
			}
			m.eSel = i
			m.buildFields()
			if m.ePrompt == nil {
				t.Fatalf("orchestrator %q has no prompt box", tc.item)
			}
			want := "NEW ORCHESTRATOR " + strings.ToUpper(tc.item)
			m.ePrompt.SetValue(want)
			m.saveItem()

			data, err := os.ReadFile(filepath.Join(home, "oline", "config", "pentester", tc.file))
			if err != nil {
				t.Fatalf("%s not written: %v", tc.file, err)
			}
			if string(data) != want {
				t.Errorf("%s not persisted verbatim:\n%s", tc.file, data)
			}
			if tc.get(m) != want {
				t.Errorf("in-memory orchestrator %q not updated", tc.item)
			}
		})
	}
}

// TestEditorExploiterHasNoMethodology checks the methodology item is hidden for a
// mode that ships no METHODOLOGY.md (exploiter).
func TestEditorExploiterHasNoMethodology(t *testing.T) {
	m := exploiterModel(t)
	m.openEditor()
	if findItem(m, "orchestrator", "methodology") >= 0 {
		t.Error("exploiter should not expose a methodology item")
	}
	if findItem(m, "orchestrator", "personality") < 0 {
		t.Error("exploiter should still expose the personality item")
	}
}

// TestEditorSaveAgentPrompt checks an agent's system prompt saves to its .md with
// the frontmatter preserved, and updates the in-memory roster.
func TestEditorSaveAgentPrompt(t *testing.T) {
	m, home := tempHomeModel(t)
	m.openEditor()
	idx, name := -1, ""
	for i, it := range m.eItems {
		if it.kind == "agent" {
			idx, name = i, it.name
			break
		}
	}
	if idx < 0 {
		t.Fatal("no agent item in editor list")
	}
	m.eSel = idx
	m.buildFields()
	if m.ePrompt == nil {
		t.Fatalf("agent %q has no system-prompt box", name)
	}
	origFront := m.roster.Agents[name].Frontmatter
	if origFront == "" {
		t.Fatalf("agent %q has no parsed frontmatter", name)
	}
	m.ePrompt.SetValue("EDITED AGENT PROMPT BODY")
	m.saveItem()

	data, err := os.ReadFile(filepath.Join(home, "oline", "config", "pentester", "agents", name+".md"))
	if err != nil {
		t.Fatalf("agent .md not written: %v", err)
	}
	s := string(data)
	if !strings.HasPrefix(s, "---\n") || !strings.Contains(s, origFront) {
		t.Errorf("frontmatter not preserved in rewritten %s.md:\n%s", name, s)
	}
	if !strings.Contains(s, "EDITED AGENT PROMPT BODY") {
		t.Errorf("edited prompt body missing from %s.md:\n%s", name, s)
	}
	if m.roster.Agents[name].Prompt != "EDITED AGENT PROMPT BODY" {
		t.Error("in-memory agent prompt not updated")
	}
}

// TestEditorLeftPaneFitsScopeLine checks the left settings pane is wide enough that
// a long item row such as "scope  in-scope targets" stays on one line (the pane
// auto-sizes to its widest line), given a terminal with ample width.
func TestEditorLeftPaneFitsScopeLine(t *testing.T) {
	m := testModel(t)
	m.w, m.h = 120, 40
	m.openEditor()

	var scopeLine string
	for _, line := range strings.Split(ansi.Strip(m.viewEditor()), "\n") {
		if strings.Contains(line, "scope") {
			scopeLine = line
			break
		}
	}
	if scopeLine == "" {
		t.Fatal("scope row not found in the editor view")
	}
	if !strings.Contains(scopeLine, "in-scope targets") {
		t.Errorf("scope row wrapped: 'in-scope targets' is not on the same line as 'scope':\n%q", scopeLine)
	}
}

// tempHomeModel points config at a throwaway HOME so saves don't touch the real
// ~/oline/config, then returns a loaded model.
func tempHomeModel(t *testing.T) (*model, string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("USERPROFILE", home) // Windows
	t.Setenv("HOME", home)        // others
	return testModel(t), home
}

func findItem(m *model, kind, name string) int {
	for i, it := range m.eItems {
		if it.kind == kind && it.name == name {
			return i
		}
	}
	return -1
}

func TestEditorSaveProvider(t *testing.T) {
	m, home := tempHomeModel(t)
	m.openEditor()
	i := findItem(m, "provider", "fireworks")
	if i < 0 {
		t.Fatal("fireworks provider not in editor list")
	}
	m.eSel = i
	m.buildFields()
	m.eFields[0].SetValue("https://new.gateway.example/v1")
	m.eFields[1].SetValue("fw_edited_key")
	m.saveItem()

	data, err := os.ReadFile(filepath.Join(home, "oline", "config", "pentester", "models.yml"))
	if err != nil {
		t.Fatalf("models.yml not written: %v", err)
	}
	if !strings.Contains(string(data), "new.gateway.example") {
		t.Errorf("baseURL not persisted:\n%s", data)
	}
	sec, err := os.ReadFile(filepath.Join(home, "oline", "config", "secrets.yml"))
	if err != nil {
		t.Fatalf("secrets.yml not written: %v", err)
	}
	if !strings.Contains(string(sec), "fw_edited_key") {
		t.Errorf("key not persisted to secrets.yml")
	}
	if got, err := m.store.ResolveKey("fireworks"); err != nil || got != "fw_edited_key" {
		t.Errorf("ResolveKey after save = %q, %v", got, err)
	}
	// the key field is cleared after save
	if m.eFields[1].Value() != "" {
		t.Error("API key field should be cleared after save")
	}
}

// TestEditorRoleCost checks a tier's model prices are editable under its "role"
// item: with the model unchanged, the four price fields persist to the user
// models.yml and immediately change what the live cost readout (costFor) bills,
// without losing the model's other fields or the provider's other models.
func TestEditorRoleCost(t *testing.T) {
	m, home := tempHomeModel(t)
	const id = "accounts/fireworks/models/deepseek-v4p1-flash"
	m.store.SetModelRoles("fireworks/"+id, "fireworks/accounts/fireworks/models/glm-5p3-flash", "fireworks/accounts/fireworks/models/nemotron-lightning-3p5-30b-a3b")

	m.openEditor()
	i := findItem(m, "role", "big")
	if i < 0 {
		t.Fatal("big role not in editor list")
	}
	m.eSel = i
	m.buildFields()
	if len(m.eFields) != 5 {
		t.Fatalf("role editor should have model + 4 price fields (5), got %d", len(m.eFields))
	}
	// Field 0 is the model selector; it must show big's model so prices apply to it.
	if !strings.Contains(m.eFields[0].Value(), "deepseek-v4p1-flash") {
		t.Fatalf("role model field = %q, want big's model", m.eFields[0].Value())
	}
	// Leave the model unchanged; edit its prices.
	m.eFields[1].SetValue("0.9")  // input
	m.eFields[2].SetValue("3.6")  // output
	m.eFields[3].SetValue("0.02") // cache-read
	m.eFields[4].SetValue("0.9")  // cache-write
	m.saveItem()

	// Billed rates change immediately.
	in, out, cr := m.costFor(id)
	if in != 0.9 || out != 3.6 || cr != 0.02 {
		t.Errorf("costFor after save = (%v,%v,%v), want (0.9,3.6,0.02)", in, out, cr)
	}

	// Persisted to the user override, and the provider's other models survive.
	data, err := os.ReadFile(filepath.Join(home, "oline", "config", "pentester", "models.yml"))
	if err != nil {
		t.Fatalf("models.yml not written: %v", err)
	}
	s := string(data)
	if !strings.Contains(s, "input: 0.9") {
		t.Errorf("edited input price not persisted:\n%s", s)
	}
	if !strings.Contains(s, "glm-5p3-flash") || !strings.Contains(s, "nemotron-lightning") {
		t.Errorf("sibling models dropped from the override:\n%s", s)
	}
	if !strings.Contains(s, "DeepSeek V4.1 Flash") {
		t.Errorf("model name not preserved in the override:\n%s", s)
	}
}

// TestEditorRoleNewModelAutoAdded reproduces the reported bug: pointing a tier at a
// model id not yet in the provider's catalog must auto-add it so the engagement is
// valid, instead of failing with "not in provider's model list".
func TestEditorRoleNewModelAutoAdded(t *testing.T) {
	t.Setenv("FIREWORKS_API_KEY", "test-key") // so ValidateModels can resolve the key
	m, _ := tempHomeModel(t)
	const glm = "fireworks/accounts/fireworks/models/glm-5p3-flash"
	m.store.SetModelRoles(glm, glm, glm) // start valid

	m.openEditor()
	m.eSel = findItem(m, "role", "small")
	m.buildFields()
	const newID = "accounts/fireworks/models/gpt-oss-120b"
	m.eFields[0].SetValue("fireworks/" + newID) // a model the catalog doesn't list
	m.saveItem()

	// The model is now in the catalog...
	found := false
	for _, md := range m.store.Providers()["fireworks"].Models {
		if md.ID == newID {
			found = true
		}
	}
	if !found {
		t.Fatalf("new small model %q was not auto-added to the fireworks catalog", newID)
	}
	// ...so validation no longer rejects it as missing from the model list.
	if err := m.store.ValidateModels(); err != nil {
		t.Errorf("ValidateModels still failing after auto-add: %v", err)
	}
}

// TestEditorRoleModelChangeReseedsPrices checks that changing a tier's model does
// NOT write the previously-shown prices onto the newly chosen model: the save
// switches the model and re-seeds the price fields from the new model instead.
func TestEditorRoleModelChangeReseedsPrices(t *testing.T) {
	m, _ := tempHomeModel(t)
	const deepseek = "accounts/fireworks/models/deepseek-v4p1-flash"
	const glm = "accounts/fireworks/models/glm-5p3-flash"
	m.store.SetModelRoles("fireworks/"+deepseek, "fireworks/"+glm, "fireworks/"+glm)

	m.openEditor()
	m.eSel = findItem(m, "role", "big")
	m.buildFields()
	// Switch big from deepseek to glm, while the price fields still hold deepseek's.
	m.eFields[0].SetValue("fireworks/" + glm)
	m.eFields[1].SetValue("99") // deepseek-era value that must NOT land on glm
	m.saveItem()

	// The role switched...
	if got := m.roleModel("big"); got != "fireworks/"+glm {
		t.Errorf("big model = %q, want glm", got)
	}
	// ...and glm's price is still its catalog value (0.15), not the stale 99.
	if in, _, _ := m.costFor(glm); in != 0.15 {
		t.Errorf("glm input price = %v, want 0.15 (stale price must not be written on a model switch)", in)
	}
	// The price fields now show glm's prices, ready to edit.
	if m.eFields[1].Value() != "0.15" {
		t.Errorf("price field after model switch = %q, want glm's 0.15", m.eFields[1].Value())
	}
}

func TestWizardApiKeyStep(t *testing.T) {
	t.Setenv("FIREWORKS_API_KEY", "") // force "no key" so the setup step appears
	m, home := tempHomeModel(t)
	var tm tea.Model = m
	tm, _ = tm.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	// Mode -> models step (no models configured by default).
	tm, _ = tm.Update(enter())
	if tm.(*model).step != wModels {
		t.Fatalf("expected wModels step first-run, got %v", tm.(*model).step)
	}
	// Confirm the default tier picks -> provider still needs a key -> wKey.
	tm, _ = tm.Update(enter())
	if tm.(*model).step != wKey {
		t.Fatalf("expected wKey step after choosing models with no API key, got %v", tm.(*model).step)
	}
	// enter a key -> saved and advance to Start
	tm, _ = tm.Update(key("fw_setup_key_123"))
	tm, _ = tm.Update(enter())
	if tm.(*model).step != wStart {
		t.Fatalf("after key entry: step=%v, want wStart", tm.(*model).step)
	}
	got, err := tm.(*model).store.ResolveKey("fireworks")
	if err != nil || got != "fw_setup_key_123" {
		t.Errorf("ResolveKey after setup = %q, %v", got, err)
	}
	if _, err := os.Stat(filepath.Join(home, "oline", "config", "secrets.yml")); err != nil {
		t.Errorf("secrets.yml not written: %v", err)
	}
}

func TestEditorSaveAgent(t *testing.T) {
	m, home := tempHomeModel(t)
	m.openEditor()
	i := findItem(m, "agent", "recon")
	if i < 0 {
		t.Fatal("recon agent not in editor list")
	}
	m.eSel = i
	m.buildFields()
	m.eFields[0].SetValue("big") // agents now pick a model TIER, not a raw model id
	m.eFields[1].SetValue("high")
	m.saveItem()

	data, err := os.ReadFile(filepath.Join(home, "oline", "config", "pentester", "config.yml"))
	if err != nil {
		t.Fatalf("config.yml not written: %v", err)
	}
	if !strings.Contains(string(data), "tier: big") {
		t.Errorf("agent tier override not persisted:\n%s", data)
	}
	// reflected in the in-memory roster immediately
	if m.roster.Agents["recon"].Tier != "big" {
		t.Error("roster not updated in memory after save")
	}
	if m.roster.Agents["recon"].ThinkingLevel != "high" {
		t.Error("thinking-level not updated in memory")
	}
}
