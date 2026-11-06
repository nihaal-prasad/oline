package tui

import (
	"fmt"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"github.com/nihaal-prasad/oline/assets"
	"github.com/nihaal-prasad/oline/internal/agents"
	"github.com/nihaal-prasad/oline/internal/config"
	"github.com/nihaal-prasad/oline/internal/core"
	"github.com/nihaal-prasad/oline/internal/engine"
	"github.com/nihaal-prasad/oline/internal/event"
	"github.com/nihaal-prasad/oline/internal/sandbox"
)

// TestResumePromptIncludesExtraInstructions checks the optional extra instructions
// typed when resuming are carried into the orchestrator's kickoff prompt, and are
// omitted when the field is left blank.
func TestResumePromptIncludesExtraInstructions(t *testing.T) {
	m := testModel(t)
	m.sess = &engine.Session{}
	m.resume = true
	m.engagement = "acme-engagement"
	m.scopeIn.SetValue("10.0.0.5")

	m.goalIn.SetValue("re-test the SQLi on /login first")
	p := m.composePrompt()
	if !strings.Contains(p, "RESUME") {
		t.Fatalf("expected a resume prompt:\n%s", p)
	}
	if !strings.Contains(p, "ADDITIONAL INSTRUCTIONS") || !strings.Contains(p, "re-test the SQLi on /login first") {
		t.Errorf("resume prompt should carry the extra instructions:\n%s", p)
	}

	m.goalIn.SetValue("")
	if strings.Contains(m.composePrompt(), "ADDITIONAL INSTRUCTIONS") {
		t.Error("no additional-instructions line should appear when the field is blank")
	}
}

func testModel(t *testing.T) *model {
	t.Helper()
	store, err := config.Load(config.ModePentester, assets.PentesterFS)
	if err != nil {
		t.Fatal(err)
	}
	roster, err := agents.LoadRoster(config.ModePentester, assets.PentesterFS, store)
	if err != nil {
		t.Fatal(err)
	}
	return newModel(config.ModePentester, store, roster)
}

func exploiterModel(t *testing.T) *model {
	t.Helper()
	store, err := config.Load(config.ModeExploiter, assets.ExploiterFS)
	if err != nil {
		t.Fatal(err)
	}
	roster, err := agents.LoadRoster(config.ModeExploiter, assets.ExploiterFS, store)
	if err != nil {
		t.Fatal(err)
	}
	return newModel(config.ModeExploiter, store, roster)
}

func key(s string) tea.KeyMsg { return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)} }
func enter() tea.KeyMsg       { return tea.KeyMsg{Type: tea.KeyEnter} }

func down() tea.KeyMsg { return tea.KeyMsg{Type: tea.KeyDown} }

// fireworksProvider is a provider fixture for tests (oline ships none by default).
func fireworksProvider() config.Provider {
	mk := func(id, name string, ctx, max int, def string, in, out, cr, cw float64) config.ModelDef {
		return config.ModelDef{
			ID: id, Name: name, ContextWindow: ctx, MaxTokens: max, Reasoning: true,
			Thinking: config.ThinkingSpec{Mode: "effort", Efforts: []string{"low", "medium", "high"}, DefaultLevel: def},
			Compat:   config.Compat{ThinkingFormat: "openai"},
			Cost:     config.Cost{Input: in, Output: out, CacheRead: cr, CacheWrite: cw},
		}
	}
	return config.Provider{
		BaseURL: "https://api.fireworks.ai/inference/v1", API: "openai", APIKeyRef: "env:FIREWORKS_API_KEY",
		Models: []config.ModelDef{
			mk("accounts/fireworks/models/deepseek-v4p1-flash", "DeepSeek V4.1 Flash", 163840, 32768, "low", 0.30, 1.2, 0.006, 1.2),
			mk("accounts/fireworks/models/glm-5p3-flash", "GLM 5.3 Flash", 131072, 32768, "medium", 0.15, 0.50, 0.03, 0.15),
			mk("accounts/fireworks/models/nemotron-lightning-3p5-30b-a3b", "Nemotron Lightning 3.5 30B A3B", 131072, 32768, "low", 0.05, 0.20, 0.01, 0.05),
		},
	}
}

// withFireworks defines the fireworks provider fixture in the store (in-memory).
func withFireworks(s *config.Store) { s.DefineProvider("fireworks", fireworksProvider()) }

// completeSetup drives the first-run provider+models form to a valid submission:
// the prefilled OpenAI defaults plus the given API key (blank to let the key step
// appear). It asserts the form is on screen and returns the model after submit.
func completeSetup(t *testing.T, tm tea.Model, apiKey string) tea.Model {
	t.Helper()
	m := tm.(*model)
	if m.step != wSetup {
		t.Fatalf("expected wSetup, got %v", m.step)
	}
	if r := m.setupRowByKey("provider.key"); r != nil {
		r.ti.SetValue(apiKey)
	}
	tm, _ = tm.Update(enter()) // submit the prefilled form
	return tm
}

func TestWizardNewFlow(t *testing.T) {
	t.Setenv("FIREWORKS_API_KEY", "test-key") // key present -> no key step
	m, _ := tempHomeModel(t)                  // isolated empty home: models unconfigured
	var tm tea.Model = m
	tm, _ = tm.Update(tea.WindowSizeMsg{Width: 120, Height: 40})

	// Mode -> Setup form (first run, no provider configured yet)
	tm, _ = tm.Update(enter())
	// Fill the provider+models form (prefilled defaults + a key) -> Start
	tm = completeSetup(t, tm, "test-key")
	if tm.(*model).step != wStart {
		t.Fatalf("after setup: step=%v, want wStart", tm.(*model).step)
	}
	// Start=new -> Env (scan cmd)
	tm, cmd := tm.Update(enter())
	if tm.(*model).step != wEnv || cmd == nil {
		t.Fatalf("after start: step=%v cmd=%v", tm.(*model).step, cmd != nil)
	}
	tm, _ = tm.Update(scanDoneMsg{cands: []sandbox.Candidate{{ID: "kali-linux", Backend: sandbox.BackendWSL}}})
	// Env -> Scope (new path)
	tm, _ = tm.Update(enter())
	if tm.(*model).step != wScope {
		t.Fatalf("after env: step=%v, want wScope", tm.(*model).step)
	}
	tm, _ = tm.Update(key("127.0.0.1"))
	tm, _ = tm.Update(enter())
	if tm.(*model).step != wGoal || tm.(*model).scopeIn.Value() != "127.0.0.1" {
		t.Fatalf("scope step wrong: step=%v scope=%q", tm.(*model).step, tm.(*model).scopeIn.Value())
	}
	tm, _ = tm.Update(key("prove recon"))
	tm, _ = tm.Update(enter())
	if tm.(*model).step != wVPN {
		t.Fatalf("after goal: step=%v, want wVPN", tm.(*model).step)
	}
	// VPN=none -> optional data folder
	tm, _ = tm.Update(enter())
	if tm.(*model).step != wData || tm.(*model).vpnMode != "none" {
		t.Fatalf("after vpn: step=%v vpnMode=%q, want wData", tm.(*model).step, tm.(*model).vpnMode)
	}
	// Skip the optional data folder -> Automation
	tm, _ = tm.Update(enter())
	if tm.(*model).step != wAuto || tm.(*model).dataDir != "" {
		t.Fatalf("after data step: step=%v dataDir=%q, want wAuto", tm.(*model).step, tm.(*model).dataDir)
	}
	if tm.(*model).View() == "" {
		t.Error("empty wizard view")
	}
}

// TestWizardDiskStep: the new-VM sizing steps parse valid values into
// diskGB/cpus/memGB and chain disk -> CPUs -> RAM -> Scope, rejecting bad entries.
func TestWizardDiskStep(t *testing.T) {
	m, _ := tempHomeModel(t)
	var tm tea.Model = m
	tm, _ = tm.Update(tea.WindowSizeMsg{Width: 120, Height: 40})

	mm := tm.(*model)
	mm.step = wDisk
	mm.diskIn.SetValue("50")
	mm.diskIn.Focus()
	tm, _ = tm.Update(enter())
	if mm = tm.(*model); mm.step != wCPUs {
		t.Fatalf("after disk entry: step=%v, want wCPUs", mm.step)
	}
	if mm.diskGB != 50 {
		t.Errorf("diskGB = %d, want 50", mm.diskGB)
	}

	// CPUs -> RAM
	mm.cpusIn.SetValue("6")
	tm, _ = tm.Update(enter())
	if mm = tm.(*model); mm.step != wMem {
		t.Fatalf("after cpu entry: step=%v, want wMem", mm.step)
	}
	if mm.cpus != 6 {
		t.Errorf("cpus = %d, want 6", mm.cpus)
	}

	// RAM -> Scope
	mm.memIn.SetValue("16")
	tm, _ = tm.Update(enter())
	if mm = tm.(*model); mm.step != wScope {
		t.Fatalf("after ram entry: step=%v, want wScope", mm.step)
	}
	if mm.memGB != 16 {
		t.Errorf("memGB = %d, want 16", mm.memGB)
	}

	// Non-numeric entries are rejected and the wizard stays on each step.
	mm.step = wDisk
	mm.diskIn.SetValue("abc")
	tm, _ = tm.Update(enter())
	if tm.(*model).step != wDisk {
		t.Errorf("invalid disk size should stay on wDisk, got step=%v", tm.(*model).step)
	}
	mm.step = wCPUs
	mm.cpusIn.SetValue("lots")
	tm, _ = tm.Update(enter())
	if tm.(*model).step != wCPUs {
		t.Errorf("invalid vCPU count should stay on wCPUs, got step=%v", tm.(*model).step)
	}
	mm.step = wMem
	mm.memIn.SetValue("8gb")
	tm, _ = tm.Update(enter())
	if tm.(*model).step != wMem {
		t.Errorf("invalid RAM should stay on wMem, got step=%v", tm.(*model).step)
	}
}

func TestWizardResumeImportAndVPN(t *testing.T) {
	t.Setenv("FIREWORKS_API_KEY", "test-key") // key present -> no key step
	m, _ := tempHomeModel(t)                  // isolated empty home: models unconfigured
	var tm tea.Model = m
	tm, _ = tm.Update(tea.WindowSizeMsg{Width: 120, Height: 40})

	tm, _ = tm.Update(enter()) // mode -> setup form (first run)
	tm = completeSetup(t, tm, "test-key")
	tm, _ = tm.Update(down())     // start: select "resume"
	tm, cmd := tm.Update(enter()) // -> env (scan cmd)
	if tm.(*model).step != wEnv || !tm.(*model).resume || cmd == nil {
		t.Fatalf("resume start: step=%v resume=%v", tm.(*model).step, tm.(*model).resume)
	}
	tm, _ = tm.Update(scanDoneMsg{cands: []sandbox.Candidate{{ID: "kali-linux", Backend: sandbox.BackendWSL}}})
	tm, cmd = tm.Update(enter()) // env -> resume (listEngagements cmd)
	if tm.(*model).step != wResume || cmd == nil {
		t.Fatalf("after env(resume): step=%v cmd=%v", tm.(*model).step, cmd != nil)
	}
	// No saved engagements -> only the "import" option (index 0). Select it.
	tm, _ = tm.Update(engListMsg{names: nil})
	tm, _ = tm.Update(enter()) // -> import
	if tm.(*model).step != wImport {
		t.Fatalf("after resume pick: step=%v, want wImport", tm.(*model).step)
	}
	tm, _ = tm.Update(key(`C:\prev\acme-engagement`))
	tm, _ = tm.Update(enter()) // -> scope (resume still confirms scope)
	if tm.(*model).step != wScope || tm.(*model).engagement != "acme-engagement" {
		t.Fatalf("after import: step=%v engagement=%q", tm.(*model).step, tm.(*model).engagement)
	}
	// re-enter scope (targets may have changed after a lab revert), then -> extra
	// instructions (resume-only), then -> VPN
	tm, _ = tm.Update(key("10.0.0.0/24"))
	tm, _ = tm.Update(enter())
	if tm.(*model).step != wGoal {
		t.Fatalf("after resume scope: step=%v, want wGoal (additional instructions)", tm.(*model).step)
	}
	tm, _ = tm.Update(key("re-test the SQLi")) // optional extra instructions
	tm, _ = tm.Update(enter())
	mm := tm.(*model)
	if mm.step != wVPN || mm.scopeIn.Value() != "10.0.0.0/24" {
		t.Fatalf("after resume instructions: step=%v scope=%q", mm.step, mm.scopeIn.Value())
	}
	if mm.goalIn.Value() != "re-test the SQLi" {
		t.Errorf("resume extra instructions = %q", mm.goalIn.Value())
	}
	// VPN: choose "upload from host" (index 3) -> input
	tm, _ = tm.Update(down())
	tm, _ = tm.Update(down())
	tm, _ = tm.Update(down())
	tm, _ = tm.Update(enter())
	if tm.(*model).step != wVPNInput || tm.(*model).vpnMode != "hostfile" {
		t.Fatalf("after vpn pick: step=%v vpnMode=%q", tm.(*model).step, tm.(*model).vpnMode)
	}
	tm, _ = tm.Update(key("/home/me/target.ovpn"))
	tm, _ = tm.Update(enter())
	if tm.(*model).step != wData {
		t.Fatalf("after vpn input: step=%v, want wData (optional data folder)", tm.(*model).step)
	}
	if tm.(*model).vpnIn.Value() != "/home/me/target.ovpn" {
		t.Errorf("vpn path = %q", tm.(*model).vpnIn.Value())
	}
	// Optional data folder: leave blank to skip -> Automation, with nothing to copy.
	tm, _ = tm.Update(enter())
	if mm := tm.(*model); mm.step != wAuto || mm.dataDir != "" {
		t.Fatalf("after skipping the data step: step=%v dataDir=%q, want wAuto and no folder", mm.step, mm.dataDir)
	}
}

func transcriptText(t *transcript) string {
	if t == nil {
		return ""
	}
	var b strings.Builder
	for _, s := range t.segs {
		b.WriteString(s.raw.String())
	}
	return b.String()
}

func TestThoughtsToggleIsRetroactive(t *testing.T) {
	m := testModel(t)
	m.w, m.h = 120, 40
	m.addAgent(core.Main)
	m.paneRoot = newLeaf(core.Main)
	m.focusPane = m.paneRoot
	m.running = true
	m.showThoughts = true // this test exercises the toggle; thoughts are off by default
	m.resize()

	// A reasoning segment that has ALREADY appeared in the transcript.
	m.apply(event.TokenDelta{Agent: core.Main, Reasoning: "SECRET_THOUGHT_XYZ"})
	m.apply(event.TokenDelta{Agent: core.Main, Text: "visible answer"})
	if !strings.Contains(ansi.Strip(m.focusPane.vp.View()), "SECRET_THOUGHT_XYZ") {
		t.Fatal("thought not shown while thoughts are on")
	}

	press := func(s string) {
		m.prefix = true
		var tm tea.Model = m
		tm.Update(key(s))
	}

	// Hide: the already-appeared thought must disappear too.
	press("t")
	if m.showThoughts {
		t.Fatal("toggle did not turn thoughts off")
	}
	if strings.Contains(ansi.Strip(m.focusPane.vp.View()), "SECRET_THOUGHT_XYZ") {
		t.Error("hiding thoughts did NOT hide an already-appeared thought")
	}
	if !strings.Contains(ansi.Strip(m.focusPane.vp.View()), "visible answer") {
		t.Error("hiding thoughts wrongly hid normal text")
	}

	// Unhide: the already-appeared thought must come back.
	press("t")
	if !m.showThoughts {
		t.Fatal("toggle did not turn thoughts back on")
	}
	if !strings.Contains(ansi.Strip(m.focusPane.vp.View()), "SECRET_THOUGHT_XYZ") {
		t.Error("unhiding thoughts did NOT restore an already-appeared thought")
	}
}

func TestRunningViewFitsHeight(t *testing.T) {
	for _, dim := range [][2]int{{100, 30}, {80, 24}, {160, 50}} {
		m := testModel(t)
		m.w, m.h = dim[0], dim[1]
		m.addAgent(core.Main)
		m.paneRoot = newLeaf(core.Main)
		m.focusPane = m.paneRoot
		m.running = true
		m.resize()
		v := m.View()
		if n := strings.Count(v, "\n") + 1; n > m.h {
			t.Errorf("%dx%d: view is %d lines, overflows height %d (top would scroll off)", dim[0], dim[1], n, m.h)
		}
		// The pane top border must be present (row 1, just under the header).
		rows := strings.Split(v, "\n")
		if len(rows) < 2 || !strings.Contains(rows[1], "╭") {
			t.Errorf("%dx%d: pane top border not on the first body row", dim[0], dim[1])
		}
	}
}

func TestToolOutputCollapseToggle(t *testing.T) {
	tr := &transcript{}
	// Collapsed header is short; the FULL command is long and ends in a marker
	// past the truncation point, so we can assert it only appears when expanded.
	full := "⚙  bash  {\"cmd\":\"" + strings.Repeat("echo x; ", 60) + "ENDMARKER\"}"
	tr.startTool("⚙  bash  {\"cmd\":\"echo x; …\"}", full)
	var body strings.Builder
	for i := 0; i < 12; i++ {
		fmt.Fprintf(&body, "line-%02d\n", i)
	}
	tr.appendToolOut(body.String())
	tr.finalizeOpen()

	// Collapsed: just the one-line header with an expand hint; no output lines.
	collapsed := tr.render(120, true, nil)
	if !strings.Contains(collapsed, "click to expand") {
		t.Error("tool call missing the expand hint")
	}
	if strings.Contains(collapsed, "line-00") || strings.Contains(collapsed, "line-11") {
		t.Error("collapsed tool call should show no output lines")
	}
	if n := strings.Count(strings.TrimRight(collapsed, "\n"), "\n"); n != 0 {
		t.Errorf("collapsed tool call should be one line, got %d extra lines", n)
	}

	// Expand it, as a click would.
	for _, s := range tr.segs {
		if s.kind == segTool {
			s.expanded = true
		}
	}
	expanded := tr.render(120, true, nil)
	if !strings.Contains(expanded, "line-00") || !strings.Contains(expanded, "line-11") {
		t.Error("expanded tool call did not show the full output")
	}
	if !strings.Contains(expanded, "click to collapse") {
		t.Error("expanded tool call missing the collapse hint")
	}
	// The WHOLE command must be visible when expanded (not truncated).
	if strings.Contains(collapsed, "ENDMARKER") {
		t.Error("collapsed header should be truncated (no ENDMARKER)")
	}
	if !strings.Contains(ansi.Strip(expanded), "ENDMARKER") {
		t.Error("expanded tool call did not show the full command")
	}
}

func TestRosterActiveIdleSections(t *testing.T) {
	m := testModel(t)
	m.w, m.h = 120, 40
	m.addAgent(core.Main)
	m.addAgent("recon#1")
	m.addAgent("web#1")
	m.paneRoot = newLeaf(core.Main)
	m.focusPane = m.paneRoot
	m.running = true
	m.resize()
	m.states[core.Main] = core.Running
	m.states["recon#1"] = core.Idle
	m.states["web#1"] = core.Running

	out := ansi.Strip(m.renderRoster(m.rightW, m.bodyH))
	if !strings.Contains(out, "Options") {
		t.Error("roster missing the 'Options' label")
	}
	// With no shell or channel open, there is no "command panes" section and no
	// "Shell" row: a shell is opened only via Ctrl-b $ and then listed under command
	// panes (reverse-shell channels appear there too, once agents open them).
	if strings.Contains(out, "Shell") {
		t.Errorf("Shell must not appear when no shell is open:\n%s", out)
	}
	if strings.Contains(out, "command panes") {
		t.Errorf("command panes section must not appear when no shell or channel is open:\n%s", out)
	}
	// Info panes (Main + summary + diagram) come first, then active subagents, then
	// idle. Main is under info panes, not counted as a subagent.
	ip, as, is := strings.Index(out, "info panes"), strings.Index(out, "active subagents ("), strings.Index(out, "idle subagents (")
	if ip < 0 || as < 0 || is < 0 || !(ip < as && as < is) {
		t.Errorf("want info panes < active subagents < idle subagents (ip=%d as=%d is=%d)\n%s", ip, as, is, out)
	}
	if !strings.Contains(out, "Summary") || !strings.Contains(out, "Diagram") {
		t.Errorf("info panes must list Summary and Diagram:\n%s", out)
	}
	if !strings.Contains(out, "active subagents (1)") || !strings.Contains(out, "idle subagents (1)") {
		t.Errorf("want active subagents (1) / idle subagents (1) (Main excluded):\n%s", out)
	}
	if strings.Index(out, "recon#1") < strings.Index(out, "web#1") {
		t.Error("idle recon#1 should be listed after the active subagents")
	}
}

// TestNavOrderIncludesInfoPanes checks up/down navigation cycles through the info
// views (Main, summary, diagram) as well as the subagents.
func TestNavOrderIncludesInfoPanes(t *testing.T) {
	m := testModel(t)
	m.addAgent(core.Main)
	m.addAgent("recon#1")
	order := m.navOrder()
	if len(order) < 4 || order[0] != core.Main || order[1] != summaryPaneID || order[2] != diagramPaneID {
		t.Errorf("navOrder must start Main, summary, diagram then subagents: %v", order)
	}
	// setFocus onto the diagram view turns the focused pane into a diagram pane.
	m.paneRoot = newLeaf(core.Main)
	m.focusPane = m.paneRoot
	m.w, m.h = 120, 40
	m.resize()
	m.setFocus(diagramPaneID)
	if !m.focusPane.diagram {
		t.Error("setFocus(diagramPaneID) should make the focused pane a diagram pane")
	}
	m.setFocus("recon#1")
	if m.focusPane.diagram || m.focusPane.summary || m.focusPane.agent != "recon#1" {
		t.Errorf("setFocus to an agent should clear info flags and retarget: %+v", m.focusPane)
	}
}

// TestBatchedApplyRenders verifies the mutate (no render) + flushDirty (render
// once) path the live loop uses to batch engine events.
func TestBatchedApplyRenders(t *testing.T) {
	m := testModel(t)
	m.w, m.h = 120, 40
	m.addAgent(core.Main)
	m.paneRoot = newLeaf(core.Main)
	m.focusPane = m.paneRoot
	m.running = true
	m.resize()

	// A burst of events applied WITHOUT per-event rendering.
	m.mutate(event.TokenDelta{Agent: core.Main, Text: "hello "})
	m.mutate(event.TokenDelta{Agent: core.Main, Text: "world"})
	if len(m.dirty) == 0 {
		t.Fatal("mutate did not mark the pane dirty")
	}
	m.flushDirty() // single render for the whole batch
	if len(m.dirty) != 0 {
		t.Error("flushDirty did not clear the dirty set")
	}
	if !strings.Contains(ansi.Strip(m.focusPane.vp.View()), "hello world") {
		t.Errorf("batched render missing text:\n%s", m.focusPane.vp.View())
	}
}

func TestApplyEvents(t *testing.T) {
	m := testModel(t)
	withFireworks(m.store) // define the cost fixture in-memory so the cost assertion is hermetic
	m.w, m.h = 120, 40
	m.addAgent(core.Main)
	m.paneRoot = newLeaf(core.Main)
	m.focusPane = m.paneRoot
	m.running = true
	m.resize()

	m.apply(event.Spawned{Parent: core.Main, Child: "recon#1", Role: "recon"})
	if len(m.order) != 2 || m.order[1] != "recon#1" {
		t.Fatalf("spawn not tracked: %v", m.order)
	}
	m.apply(event.AgentState{Agent: "recon#1", State: core.Running})
	m.apply(event.TokenDelta{Agent: "recon#1", Text: "scanning host"})
	m.setFocus("recon#1")
	if !strings.Contains(transcriptText(m.transcripts["recon#1"]), "scanning host") {
		t.Error("token delta not written to transcript")
	}
	m.apply(event.AgentState{Agent: "recon#1", State: core.Paused})
	if m.states["recon#1"] != core.Paused {
		t.Error("state not updated")
	}
	m.apply(event.Notice{Level: "error", Text: "boom"})
	if !strings.Contains(transcriptText(m.transcripts[core.Main]), "boom") {
		t.Error("notice not written to Main transcript")
	}
	// cost accrues from usage
	m.apply(event.Usage{Agent: core.Main, Model: "accounts/fireworks/models/deepseek-v4p1-flash",
		PromptTokens: 1_000_000, CompletionTokens: 1_000_000})
	if m.totalCost <= 0 {
		t.Errorf("cost not accumulated from usage: %v", m.totalCost)
	}
	// split into two panes, navigate, and render
	m.doSplit(splitH)
	if len(leaves(m.paneRoot)) != 2 {
		t.Errorf("split did not create 2 panes: %d", len(leaves(m.paneRoot)))
	}
	if m.View() == "" {
		t.Error("empty running view")
	}
}
