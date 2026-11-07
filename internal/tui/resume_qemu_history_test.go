package tui

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/nihaal-prasad/oline/internal/core"
	"github.com/nihaal-prasad/oline/internal/engine"
	"github.com/nihaal-prasad/oline/internal/llm"
	"github.com/nihaal-prasad/oline/internal/sandbox"
)

// TestResumeQEMUReuseDiskRestoresHistory reproduces the reported bug: Mode=Exploiter,
// Start=Resume, Env=a saved QEMU VM (reuseDisk), History=Load. beginSession must seed
// the orchestrator with the prior conversation so the agents have the old context.
func TestResumeQEMUReuseDiskRestoresHistory(t *testing.T) {
	m, _ := tempHomeModel(t)
	configureTestModels(m)
	var tm tea.Model = m
	tm, _ = tm.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	m = tm.(*model)

	const eng = "qemu-eng"
	// A saved engagement with a prior Main conversation on the host.
	eh := engagementHistory{
		Engagement: eng,
		Agents: []engine.AgentRecord{{
			ID:   core.Main,
			Role: "Main",
			History: []llm.Message{
				{Role: llm.RoleUser, Content: "recon the target 10.0.0.5"},
				{Role: llm.RoleAssistant, Content: "PRIOR-CONTEXT: found SSH on 22 and a web app on 8080"},
			},
		}},
	}
	data, err := json.MarshalIndent(eh, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(historyDirForTest(t), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(historyPath(eng), data, 0o600); err != nil {
		t.Fatal(err)
	}
	if !historyExists(eng) {
		t.Fatal("precondition: the saved history file should exist")
	}

	// The exact wizard state for "resume a saved QEMU VM and load its history".
	m.mode = "exploiter"
	m.resume = true
	m.reuseDisk = true
	m.loadHist = true
	m.engagement = eng
	m.prepCand = sandbox.Candidate{ID: eng, Name: eng, Backend: sandbox.BackendQEMU, Managed: true, Engagement: eng}
	m.cands = []sandbox.Candidate{m.prepCand}

	h := &fakeHandle{}
	tmodel, _ := m.beginSession(sessionPrep1{h: h})
	m = tmodel.(*model)

	if m.sess == nil {
		t.Fatal("beginSession did not build a session")
	}
	if len(m.sess.SeedHistory) == 0 {
		t.Fatal("BUG: resuming a saved QEMU VM with 'load history' did not seed the orchestrator — agents start with no prior context")
	}
	var joined strings.Builder
	for _, msg := range m.sess.SeedHistory {
		joined.WriteString(msg.Content)
		joined.WriteString("\n")
	}
	if !strings.Contains(joined.String(), "PRIOR-CONTEXT") {
		t.Errorf("SeedHistory does not contain the prior conversation: %q", joined.String())
	}

	// Clean up the engagement context started by beginSession.
	if m.cancel != nil {
		m.cancel()
	}
	if m.log != nil {
		m.log.close()
	}
	if m.audit != nil {
		_ = m.audit.Close()
	}
}

// historyDirForTest returns the host history dir for the current (temp-home) test.
func historyDirForTest(t *testing.T) string {
	t.Helper()
	return historyPath("x")[:len(historyPath("x"))-len("x.json")]
}
