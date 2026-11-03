package engine

import (
	"testing"

	"github.com/nihaal-prasad/oline/internal/tools"
)

// TestOrchestratorReservesConcurrencySlot verifies maxConcurrency caps the TOTAL
// number of agents running at once (the orchestrator included): the subagent
// semaphore holds maxConcurrency-1 slots, reserving one for Main, so subagents +
// Main never exceed the configured limit. A value below 2 still leaves room for
// exactly one subagent.
func TestOrchestratorReservesConcurrencySlot(t *testing.T) {
	noCfg := func(string) (AgentConfig, bool) { return AgentConfig{}, false }
	cases := []struct{ max, wantSubSlots int }{
		{5, 4}, // default: Main + up to 4 subagents = 5 total
		{3, 2},
		{2, 1},
		{1, 1}, // floor: still room for one subagent
		{0, 4}, // 0 => default 5 => 4 subagent slots
	}
	for _, c := range cases {
		s := NewSession(tools.NewRegistry(), nil, nil, noCfg, nil, c.max)
		if got := cap(s.router.sem); got != c.wantSubSlots {
			t.Errorf("maxConcurrency %d: subagent slots = %d, want %d (one slot reserved for the orchestrator)",
				c.max, got, c.wantSubSlots)
		}
	}
}
