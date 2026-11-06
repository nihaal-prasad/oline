package engine_test

import (
	"testing"

	"github.com/nihaal-prasad/oline/internal/core"
	"github.com/nihaal-prasad/oline/internal/engine"
	"github.com/nihaal-prasad/oline/internal/event"
	"github.com/nihaal-prasad/oline/internal/llm"
	"github.com/nihaal-prasad/oline/internal/tools"
)

// TestSnapshotIncludesSeedAgents: subagents restored on resume (SeedAgents) are kept
// in Snapshot even though they are not live engine agents, so the saved history and
// the backup bundle built from it never drop an earlier session's subagent
// transcripts. A live agent of the same id shadows its seed (no double-count).
func TestSnapshotIncludesSeedAgents(t *testing.T) {
	llmFor := func(string) (engine.LLM, string, error) { return nil, "m", nil }
	cfgFor := func(string) (engine.AgentConfig, bool) { return engine.AgentConfig{}, false }
	events := make(chan event.Event, 8)
	s := engine.NewSession(tools.NewRegistry(), mockSB{}, llmFor, cfgFor, events, 5)

	s.SeedAgents = []engine.AgentRecord{{
		ID:      core.AgentID("recon#1"),
		Role:    "recon",
		History: []llm.Message{{Role: llm.RoleUser, Content: "prior recon transcript"}},
	}}

	snap := s.Snapshot()
	var got *engine.AgentRecord
	for i := range snap {
		if snap[i].ID == core.AgentID("recon#1") {
			got = &snap[i]
		}
	}
	if got == nil {
		t.Fatalf("Snapshot dropped the restored subagent; a re-save would lose its history. got %+v", snap)
	}
	if len(got.History) != 1 || got.History[0].Content != "prior recon transcript" {
		t.Errorf("restored subagent history not preserved in Snapshot: %+v", got.History)
	}
}
