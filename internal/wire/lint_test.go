package wire_test

import (
	"strings"
	"testing"

	"github.com/nihaal-prasad/oline/assets"
	"github.com/nihaal-prasad/oline/internal/agents"
	"github.com/nihaal-prasad/oline/internal/config"
	"github.com/nihaal-prasad/oline/internal/wire"
)

func TestLintCatchesMistakes(t *testing.T) {
	store, err := config.Load(config.ModePentester, assets.PentesterFS)
	if err != nil {
		t.Fatal(err)
	}
	reg := wire.BuildRegistry(store)
	roster := &agents.Roster{
		Mode: config.ModePentester,
		Agents: map[string]agents.AgentDef{
			"good": {Name: "good", Description: "a fine agent", Tier: "medium", Tools: agents.CSV{"bash", "read"}},
			"bad":  {Name: "bad", Tier: "huge", Tools: agents.CSV{"bash", "nonexistent_tool"}, Spawns: agents.CSV{"ghost"}},
		},
	}
	joined := strings.Join(wire.Lint(store, roster, reg), "\n")
	for _, want := range []string{
		`unknown tier "huge"`,
		`agent "bad": no description`,
		`tool "nonexistent_tool"`,
		`spawns "ghost"`,
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("lint should warn %q; got:\n%s", want, joined)
		}
	}
	if strings.Contains(joined, `agent "good"`) {
		t.Errorf("the well-formed agent should produce no warning:\n%s", joined)
	}
	// Both shipped rosters must lint clean: every tool their agents grant is
	// registered, and no agent spawns an unknown child. This is a regression
	// guard — the lint originally caught stale frontmatter (eval/lsp/debug/todo)
	// that referenced tools the harness never implemented.
	for _, mode := range []config.Mode{config.ModePentester, config.ModeExploiter} {
		fsys := assets.PentesterFS
		if mode == config.ModeExploiter {
			fsys = assets.ExploiterFS
		}
		s, err := config.Load(mode, fsys)
		if err != nil {
			t.Fatalf("load %s config: %v", mode, err)
		}
		full, err := agents.LoadRoster(mode, fsys, s)
		if err != nil {
			t.Fatalf("load %s roster: %v", mode, err)
		}
		if w := wire.Lint(s, full, wire.BuildRegistry(s)); len(w) != 0 {
			t.Errorf("the shipped %s roster should lint clean, got:\n%s", mode, strings.Join(w, "\n"))
		}
	}
}
