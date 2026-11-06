package agents_test

import (
	"io/fs"
	"testing"

	"github.com/nihaal-prasad/oline/assets"
	"github.com/nihaal-prasad/oline/internal/agents"
	"github.com/nihaal-prasad/oline/internal/config"
)

func TestLoadRosters(t *testing.T) {
	cases := []struct {
		mode config.Mode
		fsys fs.FS
		want int
	}{
		{config.ModePentester, assets.PentesterFS, 18},
		{config.ModeExploiter, assets.ExploiterFS, 8},
		{config.ModeProgrammer, assets.ProgrammerFS, 6},
	}
	for _, c := range cases {
		t.Run(string(c.mode), func(t *testing.T) {
			store, err := config.Load(c.mode, c.fsys)
			if err != nil {
				t.Fatalf("config.Load: %v", err)
			}
			r, err := agents.LoadRoster(c.mode, c.fsys, store)
			if err != nil {
				t.Fatalf("LoadRoster: %v", err)
			}
			if len(r.Agents) != c.want {
				t.Errorf("agent count = %d, want %d (%v)", len(r.Agents), c.want, r.Names())
			}
			if r.Orchestrator.SystemTemplate == "" {
				t.Error("empty orchestrator SYSTEM_TEMPLATE")
			}
			if r.Orchestrator.Personality == "" {
				t.Error("empty orchestrator PERSONALITY")
			}
			// Every agent must have a name, a non-empty prompt body, and at least one tool.
			for name, def := range r.Agents {
				if def.Name == "" {
					t.Errorf("%s: empty name", name)
				}
				if def.Prompt == "" {
					t.Errorf("%s: empty prompt body", name)
				}
				if len(def.Tools) == 0 {
					t.Errorf("%s: no tools in allow-list", name)
				}
			}
		})
	}
}

func TestPentesterToolScoping(t *testing.T) {
	store, err := config.Load(config.ModePentester, assets.PentesterFS)
	if err != nil {
		t.Fatal(err)
	}
	r, err := agents.LoadRoster(config.ModePentester, assets.PentesterFS, store)
	if err != nil {
		t.Fatal(err)
	}
	recon, ok := r.Agents["recon"]
	if !ok {
		t.Fatal("recon missing")
	}
	if !recon.Tools.Has("bash") || !recon.Tools.Has("wait") {
		t.Errorf("recon tools missing bash/wait: %v", recon.Tools)
	}
	if recon.Tools.Has("web_search") {
		t.Errorf("recon should NOT have web_search (offline role): %v", recon.Tools)
	}
	researcher, ok := r.Agents["researcher"]
	if !ok {
		t.Fatal("researcher missing")
	}
	if !researcher.Tools.Has("web_search") {
		t.Errorf("researcher must have web_search (the internet role): %v", researcher.Tools)
	}
}
