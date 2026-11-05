// Command oline is the entry point for the O-Line offensive-security agent harness.
//
// By default it launches the full-screen TUI (setup wizard, then the running
// engagement). --print dumps the resolved roster and exits (no terminal needed),
// which is handy for smoke-testing config/roster/wiring in CI.
package main

import (
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/nihaal-prasad/oline/assets"
	"github.com/nihaal-prasad/oline/internal/agents"
	"github.com/nihaal-prasad/oline/internal/config"
	"github.com/nihaal-prasad/oline/internal/core"
	"github.com/nihaal-prasad/oline/internal/tui"
	"github.com/nihaal-prasad/oline/internal/wire"
)

// version is set at build time via -ldflags "-X main.version=..."; defaults to "dev".
var version = "dev"

func main() {
	modeFlag := flag.String("mode", "pentester", "roster mode: pentester | exploiter")
	printFlag := flag.Bool("print", false, "print the resolved roster and exit (no TUI)")
	envFlag := flag.String("env", "", "skip the wizard and start directly in this sandbox (e.g. a WSL distro name)")
	scopeFlag := flag.String("scope", "", "in-scope targets (direct start)")
	goalFlag := flag.String("goal", "", "engagement goal (direct start)")
	plannerFlag := flag.Bool("planner", false, "planner mode (direct start): AI proposes, you approve each step; default automatic")
	manualFlag := flag.Bool("manual", false, "deprecated alias for -planner")
	editFlag := flag.Bool("edit", false, "open the model/provider editor and exit (no engagement)")
	destroyFlag := flag.Bool("destroy", false, "interactively destroy an oline-managed WSL/QEMU environment and exit (only oline-created envs; never touches anything else; asks for confirmation)")
	initFlag := flag.Bool("init-config", false, "write the mode's default config.yml, models.yml and prompt files to ~/oline/config/<mode>/ for editing, then exit")
	forceFlag := flag.Bool("force", false, "with -init-config: overwrite existing config files instead of keeping them")
	versionFlag := flag.Bool("version", false, "print version and exit")
	flag.Parse()

	if *versionFlag {
		fmt.Printf("oline %s\n", version)
		return
	}

	if *destroyFlag {
		runDestroy()
		return
	}

	mode := config.Mode(*modeFlag)
	if !mode.Valid() {
		fmt.Fprintf(os.Stderr, "unknown mode %q (want pentester or exploiter)\n", *modeFlag)
		os.Exit(2)
	}

	if *initFlag {
		initConfig(mode, *forceFlag)
		return
	}

	if *printFlag {
		printRoster(mode)
		return
	}
	opts := tui.Options{Scope: *scopeFlag, Goal: *goalFlag, EnvID: *envFlag, Planner: *plannerFlag || *manualFlag, Edit: *editFlag}
	if err := tui.LaunchWith(mode, opts); err != nil {
		fmt.Fprintln(os.Stderr, "oline:", err)
		os.Exit(1)
	}
}

// initConfig writes the mode's embedded defaults to the on-disk config dir so the
// user can edit any of them (models, costs, token ceilings, time budgets, tools,
// personalities, every agent prompt). Existing files are kept unless force is set.
func initConfig(mode config.Mode, force bool) {
	var fsys fs.FS = assets.PentesterFS
	if mode == config.ModeExploiter {
		fsys = assets.ExploiterFS
	}
	written, err := config.WriteDefaults(mode, fsys, force)
	if err != nil {
		fmt.Fprintln(os.Stderr, "oline: init-config:", err)
		os.Exit(1)
	}
	dir := filepath.Join(config.HostRoot(), "config", string(mode))
	if len(written) == 0 {
		fmt.Printf("oline: config for %s already present at %s (nothing written; pass -force to overwrite)\n", mode, dir)
		return
	}
	fmt.Printf("oline: wrote %d default config/prompt file(s) for %s to %s:\n", len(written), mode, dir)
	for _, p := range written {
		fmt.Println("  " + p)
	}
	fmt.Println("Edit any of these to override the built-in defaults; they are loaded automatically next run.")
}

func printRoster(mode config.Mode) {
	var fsys fs.FS = assets.PentesterFS
	if mode == config.ModeExploiter {
		fsys = assets.ExploiterFS
	}
	store, err := config.Load(mode, fsys)
	if err != nil {
		fmt.Fprintln(os.Stderr, "load config:", err)
		os.Exit(1)
	}
	roster, err := agents.LoadRoster(mode, fsys, store)
	if err != nil {
		fmt.Fprintln(os.Stderr, "load roster:", err)
		os.Exit(1)
	}
	orch, resolver := wire.Build(mode, store, roster)
	fmt.Printf("oline — mode=%s  config=%s\n", mode, store.ConfigDir())
	roles := store.ModelRoles()
	fmt.Printf("model tiers: big=%s medium=%s small=%s\n", dash(roles.Big), dash(roles.Medium), dash(roles.Small))
	if !store.ModelsConfigured() {
		fmt.Println("  (tiers not configured yet — set them in the setup wizard or `oline -edit`; the engagement will not start until they are)")
	}
	fmt.Printf("orchestrator: tier=big model=%s effort=%s tools=%v\n\n", dash(orch.Model), dash(orch.Effort), orch.Tools)
	fmt.Printf("resolved subagents (%d):\n", len(roster.Agents))
	for _, name := range roster.Names() {
		cfg, ok := resolver(name)
		if !ok {
			continue
		}
		web := false
		for _, t := range cfg.Tools {
			if t == "web_search" {
				web = true
			}
		}
		tier := roster.Agents[name].Tier
		if tier == "" {
			tier = "medium"
		}
		fmt.Printf("  %-18s tier=%-6s model=%-55s effort=%-6s web=%v tools=%d spawns=%v\n",
			core.CanonRole(cfg.Role), tier, dash(cfg.Model), dash(cfg.Effort), web, len(cfg.Tools), cfg.Spawns)
	}
	if warns := wire.Lint(store, roster, wire.BuildRegistry(store)); len(warns) > 0 {
		fmt.Printf("\nconfig lint (%d):\n", len(warns))
		for _, w := range warns {
			fmt.Printf("  ! %s\n", w)
		}
	}
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
