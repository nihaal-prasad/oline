package engine

import (
	"testing"

	"github.com/nihaal-prasad/oline/internal/core"
)

// TestNewIDCapitalizes checks spawned ids start with a capital letter and that the
// per-role counter is keyed by the canonical (capitalized) role regardless of the
// case the caller used.
func TestNewIDCapitalizes(t *testing.T) {
	r := &Router{agents: map[core.AgentID]*Agent{}, counts: map[string]int{}}
	if id := r.newID("recon"); id != "Recon#1" {
		t.Errorf("newID(recon) = %q, want Recon#1", id)
	}
	if id := r.newID("Recon"); id != "Recon#2" {
		t.Errorf("newID(Recon) = %q, want Recon#2 (same canonical role continues the count)", id)
	}
	if id := r.newID("web-exploiter"); id != "Web-exploiter#1" {
		t.Errorf("newID(web-exploiter) = %q, want Web-exploiter#1", id)
	}
}

// TestRouterGetCaseInsensitive checks a lookup matches regardless of case, so a
// model that types an id in a different case than the canonical one still reaches it.
func TestRouterGetCaseInsensitive(t *testing.T) {
	r := &Router{agents: map[core.AgentID]*Agent{}, counts: map[string]int{}}
	a := &Agent{id: "Recon#1"}
	r.agents["Recon#1"] = a
	if got, ok := r.get("recon#1"); !ok || got != a {
		t.Error("get(recon#1) should match the canonical Recon#1 case-insensitively")
	}
	if got, ok := r.get("Recon#1"); !ok || got != a {
		t.Error("get(Recon#1) should match exactly")
	}
	if _, ok := r.get("nope#9"); ok {
		t.Error("get of an unknown id should fail")
	}
}

// TestMaySpawnCaseInsensitive checks the spawn permission check ignores case.
// TestMaySpawnOrchestratorOnly: only the orchestrator may spawn subagents. A
// subagent never may — even a role listed in its (now-ignored) spawns list.
func TestMaySpawnOrchestratorOnly(t *testing.T) {
	orch := AgentConfig{IsOrchestrator: true}
	if !orch.maySpawn("researcher") || !orch.maySpawn("anything") {
		t.Error("the orchestrator should be able to spawn any role")
	}
	sub := AgentConfig{Spawns: []string{"researcher", "programmer"}}
	if sub.maySpawn("researcher") || sub.maySpawn("programmer") {
		t.Error("a subagent must never spawn — only the orchestrator may (it asks the orchestrator instead)")
	}
}
