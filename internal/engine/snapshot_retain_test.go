package engine_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/nihaal-prasad/oline/internal/core"
	"github.com/nihaal-prasad/oline/internal/engine"
	"github.com/nihaal-prasad/oline/internal/event"
	"github.com/nihaal-prasad/oline/internal/tools"
)

// TestSnapshotRetainsFinishedAgents is the regression for the resume-comes-back-blank
// bug: after Run returns, the orchestrator (and any subagent) has unregistered, yet
// Snapshot must still include their conversation. Otherwise the shutdown-time history
// save — which runs AFTER Run returns — overwrites the saved history with an empty
// agent list, so a resumed engagement restores nothing.
func TestSnapshotRetainsFinishedAgents(t *testing.T) {
	mainMock := &mockLLM{steps: []step{
		{toolID: "t1", toolName: "task", toolArgs: `{"agent":"probe","prompt":"scan"}`},
		{toolID: "w1", toolName: "wait", toolArgs: `{}`}, // wait for the probe to report (so its turn completes)
		{text: "SUMMARY: recon complete, SSH on 22"},
	}}
	probeMock := &mockLLM{steps: []step{{text: "PROBE: found a web app on 8080"}}}
	llmFor := func(sel string) (engine.LLM, string, error) {
		if sel == "mock/probe" {
			return probeMock, "p", nil
		}
		return mainMock, "m", nil
	}
	cfgFor := func(role string) (engine.AgentConfig, bool) {
		if role == "probe" {
			return engine.AgentConfig{Role: "probe", Model: "mock/probe"}, true
		}
		return engine.AgentConfig{}, false
	}

	events := make(chan event.Event, 256)
	go func() {
		for range events {
		}
	}()
	s := engine.NewSession(tools.NewRegistry(), mockSB{}, llmFor, cfgFor, events, 5)
	orch := engine.AgentConfig{Role: "Main", Model: "mock/main", Tools: []string{"task", "wait"}, IsOrchestrator: true, System: "x"}

	// Non-interactive: Main runs its turns (spawn probe, then a text summary) and
	// returns; Run then cancels, so the probe and Main both unregister.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() { s.Run(ctx, orch, "recon the target"); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return")
	}

	// Snapshot AFTER Run returned (every agent unregistered) must still carry both
	// conversations — exactly what the saved history needs on resume.
	snap := s.Snapshot()
	byID := map[core.AgentID]engine.AgentRecord{}
	for _, r := range snap {
		byID[r.ID] = r
	}

	mainRec, ok := byID[core.Main]
	if !ok {
		t.Fatal("Snapshot after Run dropped the orchestrator — a resumed engagement would come back blank")
	}
	if !historyContains(mainRec, "SUMMARY: recon complete") {
		t.Errorf("retained orchestrator lost its conversation: %+v", mainRec.History)
	}

	probeRec, ok := byID["Probe#1"]
	if !ok {
		t.Fatalf("Snapshot after Run dropped the finished subagent (have: %v)", keys(byID))
	}
	if !historyContains(probeRec, "found a web app on 8080") {
		t.Errorf("retained subagent lost its transcript: %+v", probeRec.History)
	}
}

func historyContains(r engine.AgentRecord, want string) bool {
	for _, m := range r.History {
		if strings.Contains(m.Content, want) {
			return true
		}
	}
	return false
}

func keys(m map[core.AgentID]engine.AgentRecord) []core.AgentID {
	out := make([]core.AgentID, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
