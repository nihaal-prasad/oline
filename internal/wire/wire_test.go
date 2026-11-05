package wire_test

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/nihaal-prasad/oline/assets"
	"github.com/nihaal-prasad/oline/internal/agents"
	"github.com/nihaal-prasad/oline/internal/config"
	"github.com/nihaal-prasad/oline/internal/engine"
	"github.com/nihaal-prasad/oline/internal/event"
	"github.com/nihaal-prasad/oline/internal/sandbox"
	"github.com/nihaal-prasad/oline/internal/tools"
	"github.com/nihaal-prasad/oline/internal/wire"
)

func has(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func load(t *testing.T, mode config.Mode) (*config.Store, *agents.Roster) {
	t.Helper()
	fsys := assets.PentesterFS
	if mode == config.ModeExploiter {
		fsys = assets.ExploiterFS
	}
	store, err := config.Load(mode, fsys)
	if err != nil {
		t.Fatal(err)
	}
	roster, err := agents.LoadRoster(mode, fsys, store)
	if err != nil {
		t.Fatal(err)
	}
	// oline ships no provider; define one and configure the three tiers for tests.
	store.DefineProvider("fireworks", fireworksProvider())
	store.SetModelRoles(
		"fireworks/accounts/fireworks/models/deepseek-v4p1-flash",            // big
		"fireworks/accounts/fireworks/models/glm-5p3-flash",                  // medium
		"fireworks/accounts/fireworks/models/nemotron-lightning-3p5-30b-a3b", // small
	)
	return store, roster
}

// fireworksProvider is a provider fixture for tests (oline ships none by default).
// The models carry the thinking default levels and context windows the wire tests
// assert on (for example recon on the medium GLM resolves to "medium" effort).
func fireworksProvider() config.Provider {
	mk := func(id, name string, ctx int, def string) config.ModelDef {
		return config.ModelDef{
			ID: id, Name: name, ContextWindow: ctx, MaxTokens: 32768, Reasoning: true,
			Thinking: config.ThinkingSpec{Mode: "effort", Efforts: []string{"low", "medium", "high"}, DefaultLevel: def},
			Compat:   config.Compat{ThinkingFormat: "openai"},
		}
	}
	return config.Provider{
		BaseURL: "https://api.fireworks.ai/inference/v1", API: "openai", APIKeyRef: "env:FIREWORKS_API_KEY",
		Models: []config.ModelDef{
			mk("accounts/fireworks/models/deepseek-v4p1-flash", "DeepSeek V4.1 Flash", 163840, "low"),
			mk("accounts/fireworks/models/glm-5p3-flash", "GLM 5.3 Flash", 131072, "medium"),
			mk("accounts/fireworks/models/nemotron-lightning-3p5-30b-a3b", "Nemotron Lightning", 131072, "low"),
		},
	}
}

// TestResolverReadsLiveConfig checks the per-spawn ConfigResolver reflects a model
// change made AFTER Build (as happens when the user edits a tier's model mid-run):
// the next spawn of a subagent on that tier must use the new model, not the one
// that was configured when Build ran.
func TestResolverReadsLiveConfig(t *testing.T) {
	store, roster := load(t, config.ModePentester)
	_, resolver := wire.Build(config.ModePentester, store, roster)

	// recon is a medium-tier agent; it starts on the GLM configured by load().
	if recon, _ := resolver("recon"); !strings.Contains(recon.Model, "glm-5p3-flash") {
		t.Fatalf("recon model = %q, want the initial medium-tier GLM", recon.Model)
	}

	// Change the medium tier after Build, as the editor does during an engagement
	// (in memory only, to keep the test off the real ~/oline config).
	store.SetModelRoles(
		"fireworks/accounts/fireworks/models/deepseek-v4p1-flash",
		"fireworks/accounts/fireworks/models/nemotron-lightning-3p5-30b-a3b", // new medium
		"fireworks/accounts/fireworks/models/nemotron-lightning-3p5-30b-a3b",
	)

	// The next spawn must pick up the new model.
	if recon, _ := resolver("recon"); !strings.Contains(recon.Model, "nemotron-lightning") {
		t.Errorf("recon model after live change = %q, want the new medium-tier model", recon.Model)
	}
}

func TestBuildPentester(t *testing.T) {
	store, roster := load(t, config.ModePentester)
	orch, resolver := wire.Build(config.ModePentester, store, roster)

	if !orch.IsOrchestrator {
		t.Error("orchestrator not flagged")
	}
	if orch.Model != "fireworks/accounts/fireworks/models/deepseek-v4p1-flash" {
		t.Errorf("orchestrator model = %q", orch.Model)
	}
	for _, want := range []string{"task", "wait", "ask", "send_message"} {
		if !has(orch.Tools, want) {
			t.Errorf("orchestrator missing tool %q", want)
		}
	}
	// Orchestrator must NOT be able to run offensive tools directly.
	if has(orch.Tools, "bash") {
		t.Error("orchestrator should not have bash")
	}
	if !strings.Contains(orch.System, "orchestrator") {
		t.Error("orchestrator system prompt not rendered")
	}
	// The rendered orchestrator prompt must not contain any template handlebars.
	if strings.Contains(orch.System, "{{#") {
		t.Error("orchestrator system prompt still contains unresolved handlebars")
	}

	// Orchestrator prompt lists the subagent model tiers so it can route around a
	// weak agent.
	if !strings.Contains(orch.System, "Subagent model tiers") {
		t.Error("orchestrator prompt missing the model-tier roster")
	}

	// recon: medium tier (=> GLM) + medium effort, offline, has bash, send_message appended.
	recon, _ := resolver("recon")
	if !strings.Contains(recon.Model, "glm-5p3-flash") {
		t.Errorf("recon model = %q (want the medium-tier GLM)", recon.Model)
	}
	if recon.Effort != "medium" {
		t.Errorf("recon effort = %q, want medium", recon.Effort)
	}
	if !has(recon.Tools, "bash") || !has(recon.Tools, "send_message") {
		t.Errorf("recon tools = %v", recon.Tools)
	}
	// agents with bash also get the background-job primitive.
	if !has(recon.Tools, "bash_bg") {
		t.Errorf("recon should get bash_bg alongside bash: %v", recon.Tools)
	}
	if has(recon.Tools, "web_search") {
		t.Error("recon must stay offline (no web_search)")
	}
	// recon is unprivileged and scope-restricted.
	if recon.Policy != sandbox.InScopeOnly {
		t.Errorf("recon policy = %v, want InScopeOnly", recon.Policy)
	}
	// Every subagent carries the shared reporting preamble telling it to stream
	// progress updates to the orchestrator via send_message as work happens.
	if !strings.Contains(recon.System, "Reporting to the orchestrator") || !strings.Contains(recon.System, "send_message") {
		t.Error("recon system prompt missing the shared progress-reporting preamble")
	}
	// The preamble is prepended, not replacing the agent's own prompt body.
	if !strings.Contains(strings.ToLower(recon.System), "recon") {
		t.Error("recon system prompt lost its own body after prepending the preamble")
	}

	// Time budgets come from each agent's frontmatter now (recon is 15), and the
	// REAL number is injected into the agent's system prompt by the shared preamble.
	if recon.BudgetMinutes != 15 {
		t.Errorf("recon budget = %d, want 15 (from frontmatter)", recon.BudgetMinutes)
	}
	if !strings.Contains(recon.System, "15 minutes") {
		t.Error("recon system prompt should state its real 15-minute budget")
	}
	// The shared preamble documents all three run modes (incl. Watcher) and, for a
	// bash-capable agent, surfaces the reverse-shell tool.
	if !strings.Contains(recon.System, "Watcher") {
		t.Error("recon system prompt should document Watcher mode")
	}
	if !strings.Contains(recon.System, "revshell") {
		t.Error("recon (has bash) system prompt should surface the revshell tool")
	}
	// Every subagent is granted the read-only `terminal` tool AND told it exists, so
	// agents actually know they can read the operator's own terminals.
	if !has(recon.Tools, "terminal") {
		t.Error("recon should be granted the terminal tool (so it is advertised in the function schema)")
	}
	if !strings.Contains(recon.System, "terminal") {
		t.Error("recon system prompt should surface the terminal tool")
	}

	// installer is the ONLY root/privileged agent; it may reach the internet to pull tooling.
	inst, ok := resolver("installer")
	if !ok {
		t.Fatal("pentester installer not resolved")
	}
	if inst.BudgetMinutes != 0 {
		t.Errorf("installer budget = %d, want 0 (no limit, from frontmatter)", inst.BudgetMinutes)
	}
	if inst.Policy != sandbox.Privileged {
		t.Errorf("pentester installer policy = %v, want Privileged", inst.Policy)
	}
	if !has(inst.Tools, "web_search") {
		t.Error("installer should have web_search (to pull tooling)")
	}
	// No OTHER subagent may be privileged.
	for _, name := range roster.Names() {
		if name == "installer" {
			continue
		}
		cfg, _ := resolver(name)
		if cfg.Policy == sandbox.Privileged {
			t.Errorf("%s is privileged; only the installer may be", name)
		}
	}

	// web-exploiter: big tier (=> deepseek); explicit high effort; online.
	web, _ := resolver("web-exploiter")
	if !strings.Contains(web.Model, "deepseek-v4p1-flash") {
		t.Errorf("web-exploiter model = %q, want big-tier deepseek", web.Model)
	}
	if web.Effort != "high" {
		t.Errorf("web-exploiter effort = %q, want high", web.Effort)
	}
	if !has(web.Tools, "web_search") {
		t.Error("web-exploiter should have web_search")
	}

	// researcher: the internet role.
	res, _ := resolver("researcher")
	if !has(res.Tools, "web_search") {
		t.Error("researcher must have web_search")
	}

	// Context-token ceilings (for auto-compaction) are tiered: a big-tier agent keeps
	// a larger fraction of its window than a medium-tier one; all positive. (recon is
	// medium, thinker is big.)
	thinker, _ := resolver("thinker")
	if recon.MaxContextTokens <= 0 || web.MaxContextTokens <= 0 || thinker.MaxContextTokens <= 0 {
		t.Errorf("context ceilings must be positive: recon=%d web=%d thinker=%d", recon.MaxContextTokens, web.MaxContextTokens, thinker.MaxContextTokens)
	}
	if !(recon.MaxContextTokens < thinker.MaxContextTokens) {
		t.Errorf("want medium < big context ceiling: recon(medium)=%d thinker(big)=%d", recon.MaxContextTokens, thinker.MaxContextTokens)
	}

	// every subagent gets send_message.
	for _, name := range roster.Names() {
		cfg, _ := resolver(name)
		if !has(cfg.Tools, "send_message") {
			t.Errorf("%s missing send_message", name)
		}
	}
}

func TestBuildExploiter(t *testing.T) {
	store, roster := load(t, config.ModeExploiter)
	orch, resolver := wire.Build(config.ModeExploiter, store, roster)
	if !has(orch.Tools, "runtime") || !has(orch.Tools, "heartbeat") {
		t.Error("exploiter orchestrator should have runtime+heartbeat")
	}
	// analyst: no explicit model => task role deepseek, high.
	an, _ := resolver("analyst")
	if !strings.Contains(an.Model, "deepseek") || an.Effort != "high" {
		t.Errorf("analyst resolved = model %q effort %q", an.Model, an.Effort)
	}
	// installer carries web_search and is the ONLY privileged agent.
	inst, _ := resolver("installer")
	if !has(inst.Tools, "web_search") {
		t.Error("installer should have web_search")
	}
	if inst.Policy != sandbox.Privileged {
		t.Errorf("exploiter installer policy = %v, want Privileged", inst.Policy)
	}
	for _, name := range roster.Names() {
		if name == "installer" {
			continue
		}
		cfg, _ := resolver(name)
		if cfg.Policy == sandbox.Privileged {
			t.Errorf("%s is privileged; only the installer may be", name)
		}
	}
	// Exploiter analyzes a LOCAL application: internet is limited to installer
	// (Privileged, download) and judge/skeptic (InternetNoTarget, research). Every
	// other agent is Isolated, so it can reach only the local target over loopback
	// and can never attack a remote host.
	internet := map[string]bool{"installer": true, "judge": true, "skeptic": true}
	for _, name := range roster.Names() {
		cfg, _ := resolver(name)
		switch {
		case name == "judge" || name == "skeptic":
			if cfg.Policy != sandbox.InternetNoTarget {
				t.Errorf("%s policy = %v, want InternetNoTarget", name, cfg.Policy)
			}
		case !internet[name]:
			if cfg.Policy != sandbox.Isolated {
				t.Errorf("%s policy = %v, want Isolated (exploiter exec agents are network-isolated)", name, cfg.Policy)
			}
		}
	}
}

// TestLiveRecon runs the REAL recon subagent prompt against WSL Kali with a
// benign, localhost-only task. Gated on FIREWORKS_API_KEY + OLINE_TEST_WSL.
func TestLiveRecon(t *testing.T) {
	if os.Getenv("FIREWORKS_API_KEY") == "" || os.Getenv("OLINE_TEST_WSL") == "" {
		t.Skip("set FIREWORKS_API_KEY and OLINE_TEST_WSL to run the live recon test")
	}
	store, roster := load(t, config.ModePentester)
	llmFor := wire.NewLLMResolver(store)
	_, resolver := wire.Build(config.ModePentester, store, roster)

	reg := tools.NewRegistry()
	reg.Register(tools.Bash{})
	reg.Register(tools.Read{})
	reg.Register(tools.Write{})
	reg.Register(tools.Find{})
	reg.Register(tools.Grep{})
	reg.Register(tools.Glob{})

	h, err := sandbox.NewWSL().Open(context.Background(), os.Getenv("OLINE_TEST_WSL"))
	if err != nil {
		t.Fatal(err)
	}

	events := make(chan event.Event, 512)
	done := make(chan struct{})
	sawBash := false
	go func() {
		for e := range events {
			if s, ok := e.(event.ToolStarted); ok && s.Tool == "bash" {
				sawBash = true
			}
		}
		close(done)
	}()

	s := engine.NewSession(reg, h, llmFor, resolver, events, 5)
	recon, ok := resolver("recon")
	if !ok {
		t.Fatal("recon not resolved")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	res := s.Run(ctx, recon,
		"In scope: THIS local host only (localhost / 127.0.0.1). Strictly non-destructive and NO network scanning. "+
			"Goal: identify the operating system and kernel version of this host using a single simple local command, then report it in one sentence.")
	close(events)
	<-done

	if res.Err != nil {
		t.Fatalf("recon run error: %v", res.Err)
	}
	t.Logf("recon report: %q", res.Text)
	if !sawBash {
		t.Error("recon did not run a bash command in the sandbox")
	}
	low := strings.ToLower(res.Text)
	if !strings.Contains(low, "linux") && !strings.Contains(low, "kernel") {
		t.Errorf("recon report did not identify the OS/kernel: %q", res.Text)
	}
}
