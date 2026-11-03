package engine_test

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/nihaal-prasad/oline/assets"
	"github.com/nihaal-prasad/oline/internal/config"
	"github.com/nihaal-prasad/oline/internal/engine"
	"github.com/nihaal-prasad/oline/internal/event"
	"github.com/nihaal-prasad/oline/internal/llm"
	"github.com/nihaal-prasad/oline/internal/sandbox"
	"github.com/nihaal-prasad/oline/internal/tools"
)

// TestLiveSlice ties real models + the engine + the WSL sandbox together:
// orchestrator -> task -> probe subagent -> one benign bash command in WSL ->
// result -> orchestrator summary. Gated on FIREWORKS_API_KEY and OLINE_TEST_WSL.
// NOTE: benign inspection only (uname). It attacks nothing.
func TestLiveSlice(t *testing.T) {
	if os.Getenv("FIREWORKS_API_KEY") == "" {
		t.Skip("FIREWORKS_API_KEY not set; skipping live slice")
	}
	distro := os.Getenv("OLINE_TEST_WSL")
	if distro == "" {
		t.Skip("OLINE_TEST_WSL not set; skipping live slice")
	}

	store, err := config.Load(config.ModePentester, assets.PentesterFS)
	if err != nil {
		t.Fatal(err)
	}
	llmFor := resolverFromStore(t, store)

	sb := sandbox.NewWSL()
	h, err := sb.Open(context.Background(), distro)
	if err != nil {
		t.Fatalf("open WSL %q: %v", distro, err)
	}

	model := "fireworks/accounts/fireworks/models/glm-5p3-flash"
	cfgFor := func(role string) (engine.AgentConfig, bool) {
		if role == "probe" {
			return engine.AgentConfig{
				Role:   "probe",
				Model:  model,
				Effort: "low",
				Tools:  []string{"bash"},
				System: "You are a probe. Run exactly ONE bash command to answer the task, then reply in one short sentence stating what you found. Do not run anything destructive or networked.",
			}, true
		}
		return engine.AgentConfig{}, false
	}

	reg := tools.NewRegistry()
	reg.Register(tools.Bash{})

	events := make(chan event.Event, 512)
	c := newCollector()
	go drain(events, c)

	s := engine.NewSession(reg, h, llmFor, cfgFor, events, 5)
	orch := engine.AgentConfig{
		Role:           "Main",
		Model:          model,
		Effort:         "low",
		IsOrchestrator: true,
		Tools:          []string{"task", "wait"},
		System:         "You are the orchestrator. You never run commands yourself. To learn about the machine, use the task tool to spawn the 'probe' subagent with a clear prompt, then call the wait tool to collect its result, then reply with ONE sentence naming the operating system or kernel it found. Keep it minimal.",
	}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	res := s.Run(ctx, orch, "Find out what operating system / kernel this machine is running.")
	close(events)
	<-c.done

	if res.Err != nil {
		t.Fatalf("Run error: %v", res.Err)
	}
	t.Logf("orchestrator final: %q", res.Text)
	if !c.sawSpawn("probe") {
		t.Error("orchestrator did not spawn the probe subagent")
	}
	if !c.sawToolStart("bash") {
		t.Error("probe did not run a bash command in the sandbox")
	}
	lower := strings.ToLower(res.Text)
	if !strings.Contains(lower, "linux") && !strings.Contains(lower, "kernel") {
		t.Errorf("final answer does not name the OS/kernel: %q", res.Text)
	}
}

// TestLiveBashBgAlert proves the background-job primitive end-to-end against the
// real WSL sandbox: the agent starts a job with bash_bg (which returns at once),
// then calls wait; wait only unblocks when the job's completion alert is posted
// back to the agent's mailbox. A clean finish therefore proves the alert was
// delivered, and the job's output is streamed to the TUI as a ToolOutput event.
// Gated on OLINE_TEST_WSL; no API key needed (the LLM is scripted). Benign (echo).
func TestLiveBashBgAlert(t *testing.T) {
	distro := os.Getenv("OLINE_TEST_WSL")
	if distro == "" {
		t.Skip("OLINE_TEST_WSL not set; skipping live bash_bg test")
	}
	h, err := sandbox.NewWSL().Open(context.Background(), distro)
	if err != nil {
		t.Fatalf("open WSL %q: %v", distro, err)
	}

	const marker = "OLINE_BG_MARKER_42"
	mock := &mockLLM{steps: []step{
		{toolID: "j1", toolName: "bash_bg", toolArgs: `{"cmd":"echo ` + marker + `"}`},
		{toolID: "w1", toolName: "wait", toolArgs: `{}`},
		{text: "ok"},
	}}
	llmFor := func(string) (engine.LLM, string, error) { return mock, "mock", nil }
	cfgFor := func(string) (engine.AgentConfig, bool) { return engine.AgentConfig{}, false }

	events := make(chan event.Event, 512)
	c := newCollector()
	go drain(events, c)

	s := engine.NewSession(tools.NewRegistry(), h, llmFor, cfgFor, events, 5)
	agent := engine.AgentConfig{
		Role:   "Main",
		Model:  "mock",
		Tools:  []string{"bash", "bash_bg", "wait"},
		Policy: sandbox.InScopeOnly,
		System: "background-job test",
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	res := s.Run(ctx, agent, "start a background job and wait for it")
	close(events)
	<-c.done

	if res.Err != nil {
		t.Fatalf("run error (wait never got the completion alert?): %v", res.Err)
	}
	if !strings.Contains(res.Text, "ok") {
		t.Errorf("agent did not finish cleanly after the job: %q", res.Text)
	}
	if !c.sawToolOutput(marker) {
		t.Error("background job output was not streamed to the TUI")
	}
}

func resolverFromStore(t *testing.T, store *config.Store) engine.LLMResolver {
	t.Helper()
	eff := store.Effective()
	return func(sel string) (engine.LLM, string, error) {
		provider, modelID, _ := strings.Cut(sel, "/")
		p, ok := eff.Providers[provider]
		if !ok {
			t.Fatalf("unknown provider in %q", sel)
		}
		key, err := store.ResolveKey(provider)
		if err != nil {
			t.Fatal(err)
		}
		return llm.New(p.BaseURL, key), modelID, nil
	}
}
