// Package wire bridges the loaded roster + config into the engine: it resolves
// each subagent's model and thinking effort, renders system prompts, grants the
// coordination primitives, and builds the LLM/Config resolvers the engine needs.
package wire

import (
	"fmt"
	"strings"

	"github.com/nihaal-prasad/oline/internal/agents"
	"github.com/nihaal-prasad/oline/internal/config"
	"github.com/nihaal-prasad/oline/internal/core"
	"github.com/nihaal-prasad/oline/internal/engine"
	"github.com/nihaal-prasad/oline/internal/llm"
	"github.com/nihaal-prasad/oline/internal/sandbox"
)

// internetAgents may reach the wider internet (but never the in-scope target).
var internetAgents = map[string]bool{
	"researcher": true, // pentester
	"judge":      true, // exploiter
	"skeptic":    true,
}

// policyFor is the egress class for an agent in a given mode.
//
// Exploiter analyzes a LOCAL application, so no exploiter agent may reach the
// network to attack a remote host: every one that isn't an explicit internet agent
// is Isolated (loopback, and thus the local target, still works). Only the installer
// (downloads the application) and the judge/skeptic (look things up) get internet.
// Pentester attacks the in-scope targets, so its default is InScopeOnly.
func policyFor(mode config.Mode, role string) sandbox.EgressPolicy {
	if role == "installer" {
		// The installer is the ONLY agent that runs as root (to install software and,
		// in exploiter mode, download the target application).
		return sandbox.Privileged
	}
	if internetAgents[role] {
		return sandbox.InternetNoTarget
	}
	if mode == config.ModeExploiter {
		return sandbox.Isolated
	}
	return sandbox.InScopeOnly
}

// RosterAllowsInternet reports whether any agent in the roster has a network
// policy that can reach the internet (InternetNoTarget or Privileged). The QEMU
// host firewall uses it to decide whether its whole-guest cap must permit general
// egress; the in-guest per-agent rules still do the fine-grained split.
func RosterAllowsInternet(mode config.Mode, roster *agents.Roster) bool {
	for _, name := range roster.Names() {
		switch policyFor(mode, name) {
		case sandbox.InternetNoTarget, sandbox.Privileged:
			return true
		}
	}
	return false
}

// NewLLMResolver builds an engine.LLMResolver from the config store: it maps a
// "provider/model-id" selector to a client (baseURL + resolved key) and the bare
// model id.
func NewLLMResolver(store *config.Store) engine.LLMResolver {
	return func(sel string) (engine.LLM, string, error) {
		provider, modelID, found := strings.Cut(sel, "/")
		if !found {
			return nil, "", fmt.Errorf("wire: bad model selector %q (want provider/model-id)", sel)
		}
		p, ok := store.Effective().Providers[provider]
		if !ok {
			return nil, "", fmt.Errorf("wire: unknown provider %q in %q", provider, sel)
		}
		key, err := store.ResolveKey(provider)
		if err != nil {
			return nil, "", err
		}
		return llm.New(p.BaseURL, key), modelID, nil
	}
}

// Build resolves the orchestrator config and a ConfigResolver for the roster's
// subagents. taskRole supplies the model for agents with no explicit model; the
// orchestrator uses the default role at that model's own default thinking level.
func Build(mode config.Mode, store *config.Store, roster *agents.Roster) (engine.AgentConfig, engine.ConfigResolver) {
	toConfig := func(eff config.Root, def agents.AgentDef) engine.AgentConfig {
		tier := tierOf(def.Tier)
		model := eff.ModelForTier(tier)
		tools := grantCoordination([]string(def.Tools))
		if containsStr(tools, "bash") {
			tools = appendUnique(tools, "bash_bg")  // agents with bash can also run background jobs
			tools = appendUnique(tools, "revshell") // ...and set up reverse-shell listener channels
		}
		// Time budget (minutes): the agent's own value, else the config default;
		// a negative frontmatter value means "no limit" (0 minutes).
		budget := def.TimeBudget
		if budget == 0 {
			budget = eff.Budget.DefaultMinutes
		}
		if budget < 0 {
			budget = 0
		}
		ctxTokens := def.MaxContextTokens
		if ctxTokens <= 0 {
			ctxTokens = contextForTier(eff, model, tier)
		}
		return engine.AgentConfig{
			Role:             def.Name,
			Model:            model,
			Effort:           effortFor(eff, model, def.ThinkingLevel),
			System:           subagentSystem(def.Prompt),
			Tools:            tools,
			Spawns:           []string(def.Spawns),
			Policy:           policyFor(mode, def.Name),
			BudgetMinutes:    budget,
			MaxContextTokens: ctxTokens,
			MaxTokens:        modelMaxTokens(eff, model),
		}
	}

	// The resolver reads the LIVE config on every spawn (not a snapshot taken at
	// Build time), so a model/tier/price change made in the editor during an
	// engagement takes effect for newly spawned agents, as the editor promises.
	resolver := func(role string) (engine.AgentConfig, bool) {
		d, ok := roster.Lookup(role) // case-insensitive: "Recon" and "recon" both resolve
		if !ok {
			return engine.AgentConfig{}, false
		}
		return toConfig(store.Effective(), d), true
	}

	// The orchestrator is instantiated once, at engagement start, so it is built
	// from the config as it stands now.
	eff := store.Effective()
	orchTools := []string{"task", "wait", "reprompt", "inspect", "read", "write", "find", "grep", "glob", "send_message", "ask", "update_summary", "update_diagram", "record_finding", "resources"}
	if mode == config.ModeExploiter {
		orchTools = append(orchTools, "runtime", "heartbeat")
	}
	bigModel := eff.ModelForTier("big")
	orch := engine.AgentConfig{
		Role:             "Main",
		Model:            bigModel,
		Effort:           modelDefaultLevel(eff, bigModel),
		System:           orchestratorSystem(roster.Orchestrator) + tierRoster(roster),
		Tools:            orchTools,
		MaxContextTokens: contextForTier(eff, bigModel, "big"),
		MaxTokens:        modelMaxTokens(eff, bigModel),
		IsOrchestrator:   true,
	}
	return orch, resolver
}

// modelMaxTokens returns the declared output-token cap for a "provider/model-id"
// selector, or 0 when unknown (the engine then uses its own default).
func modelMaxTokens(eff config.Root, sel string) int {
	provider, modelID, _ := strings.Cut(sel, "/")
	p, ok := eff.Providers[provider]
	if !ok {
		return 0
	}
	for _, m := range p.Models {
		if m.ID == modelID {
			return m.MaxTokens
		}
	}
	return 0
}

// tierOf normalizes an agent's declared tier, defaulting a missing/unknown value to
// medium.
func tierOf(t string) string {
	switch strings.ToLower(strings.TrimSpace(t)) {
	case "big":
		return "big"
	case "small":
		return "small"
	default:
		return "medium"
	}
}

// tierRoster renders a section, appended to the orchestrator's system prompt, that
// tells it which subagents run on which model tier and how to use that: a weaker
// (small-tier) agent that is struggling with something needing stronger reasoning
// should be handed off to a higher-tier agent, not endlessly repromted.
func tierRoster(roster *agents.Roster) string {
	byTier := map[string][]string{}
	for _, name := range roster.Names() {
		d := roster.Agents[name]
		t := tierOf(d.Tier)
		entry := core.CanonRole(name) // present names capitalized
		if desc := firstLine(d.Description); desc != "" {
			entry += " (" + desc + ")"
		}
		byTier[t] = append(byTier[t], entry)
	}
	var sb strings.Builder
	sb.WriteString("\n\n# Subagent model tiers\n")
	sb.WriteString("Each subagent runs on a FIXED model tier that you cannot change at spawn time. Spawn one by its name (the part before the parenthesis), which routes the task to the agent whose purpose fits:\n")
	for _, t := range []string{"big", "medium", "small"} {
		label := map[string]string{"big": "big (strongest reasoning)", "medium": "medium (balanced)", "small": "small (fastest, weakest)"}[t]
		list := "none"
		if len(byTier[t]) > 0 {
			list = strings.Join(byTier[t], "; ")
		}
		fmt.Fprintf(&sb, "- %s: %s\n", label, list)
	}
	sb.WriteString("You run on the big tier. If a lower-tier subagent keeps failing at a task that needs stronger reasoning (bad analysis, repeated mistakes, stuck), do not just reprompt it — hand that piece to a higher-tier agent from the lists above (`task` it), which is more capable. Match the work to the tier: routine, high-volume steps are fine on small/medium; hard reasoning belongs on big.\n")
	return sb.String()
}

// firstLine returns a one-line, length-capped form of an agent description for the
// roster listing (so the orchestrator knows each agent's purpose, custom ones
// included), without dumping a whole paragraph.
func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = strings.TrimSpace(s[:i])
	}
	const max = 160
	if len(s) > max {
		s = strings.TrimSpace(s[:max]) + "…"
	}
	return s
}

// grantCoordination ensures every agent can message peers/orchestrator, ask the
// user a question directly, check its remaining time via `runtime`, and record a
// confirmed security finding to the live findings panel / report via record_finding.
func grantCoordination(toolset []string) []string {
	return appendUnique(toolset, "send_message", "ask", "runtime", "record_finding")
}

// subagentSystem prepends a shared reporting preamble to every subagent's prompt,
// so all of them stream incremental progress to the orchestrator as work happens
// (not only in their final result). This is harness-level behavior that applies
// to the whole roster, on top of whatever each agent's own prompt says.
func subagentSystem(body string) string {
	var sb strings.Builder
	sb.WriteString("# Reporting to the orchestrator\n")
	sb.WriteString("You are a subagent coordinated by the orchestrator (the Main agent). As you work, " +
		"send Main a brief update with `send_message` each time you accomplish or find something that " +
		"matters — a completed scan or step, a foothold, a confirmed finding, a credential, a dead end, " +
		"or a blocker — the moment it happens, not only when you finish. Keep each update to a sentence " +
		"or two. Reporting continuously lets the orchestrator act on and coordinate your work in real " +
		"time (for example, starting another agent on what you just found) while you keep going. Do not " +
		"hold everything back for your final result.\n\n")
	sb.WriteString("# Your time budget\n")
	sb.WriteString("The engine enforces a time budget on you, and time you spend blocked in `wait` (or any " +
		"sleep) counts against it — so wait or sleep only the minimum needed to get what you are waiting for, " +
		"and don't idle with nothing outstanding. To know how much time you have left, call the `runtime` " +
		"tool (it returns your remaining budget from oline) or rely on oline's reminder messages; judge your " +
		"remaining time ONLY from those, never from wall-clock guesses or how long the conversation feels. " +
		"It is OK if you cannot finish in time: when you return or are terminated at your budget, the " +
		"orchestrator can reprompt you (keeping your context) or spawn a fresh agent to carry on. Work " +
		"efficiently, and if time runs short, return your partial progress and clear next steps rather than " +
		"trying to overrun. Always end a cycle with a brief summary of what you did and found — never return " +
		"nothing.\n\n")
	sb.WriteString("# Recording findings\n")
	sb.WriteString("The moment you CONFIRM something security-relevant — a vulnerability, an exposed or cracked " +
		"credential, a foothold, a dangerous misconfiguration — call `record_finding` with a short factual " +
		"title, a severity (critical/high/medium/low/info), the affected target, and a line or two of " +
		"evidence. This logs it immediately and hands it to the reporter for the final report. " +
		"Record only confirmed, non-duplicate findings; do not record hunches or things you have not verified. " +
		"Recording a finding does not replace reporting it to the orchestrator with `send_message`.\n\n")
	sb.WriteString("# Output format\n")
	sb.WriteString("You are running inside software that renders Markdown, and your replies are shown to the user " +
		"and the orchestrator that way. Prefer Markdown over plain text: use headings, bold, bullet and numbered " +
		"lists, tables, and fenced code blocks for commands, code, file paths, and tool output. Default to " +
		"well-structured Markdown in everything you write.\n\n")
	sb.WriteString(strings.TrimSpace(body))
	return sb.String()
}

func containsStr(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func appendUnique(list []string, extra ...string) []string {
	out := append([]string(nil), list...)
	for _, e := range extra {
		found := false
		for _, x := range out {
			if x == e {
				found = true
				break
			}
		}
		if !found {
			out = append(out, e)
		}
	}
	return out
}

// contextForTier derives an agent's auto-compaction ceiling from its model's
// context window, scaled by tier: a big-tier agent (heavy reasoning) keeps more of
// its window before compacting; a small-tier agent compacts sooner. This is only a
// default — frontmatter `max-context-tokens` and the per-agent config override both
// win over it.
func contextForTier(eff config.Root, modelSel, tier string) int {
	win := modelContextWindow(eff, modelSel)
	if win <= 0 {
		win = 128000 // conservative fallback when the model doesn't declare a window
	}
	switch tier {
	case "big":
		return win * 9 / 10
	case "small":
		return win / 2
	default:
		return win * 3 / 4
	}
}

// modelContextWindow returns the declared context window for a "provider/model-id"
// selector, or 0 if unknown.
func modelContextWindow(eff config.Root, sel string) int {
	provider, modelID, _ := strings.Cut(sel, "/")
	p, ok := eff.Providers[provider]
	if !ok {
		return 0
	}
	for _, m := range p.Models {
		if m.ID == modelID {
			return m.ContextWindow
		}
	}
	return 0
}

func modelDefaultLevel(eff config.Root, sel string) string {
	provider, modelID, _ := strings.Cut(sel, "/")
	p, ok := eff.Providers[provider]
	if !ok {
		return ""
	}
	for _, m := range p.Models {
		if m.ID == modelID {
			return m.Thinking.DefaultLevel
		}
	}
	return ""
}

func effortFor(eff config.Root, modelSel, explicit string) string {
	if explicit != "" {
		return explicit
	}
	return modelDefaultLevel(eff, modelSel)
}

// orchestratorSystem renders the orchestrator's system prompt from the mode's
// SYSTEM_TEMPLATE.md, prepending oline's preamble and the mode personality. If a
// template carries a leading HTML-comment preamble (e.g. in a user override), it
// is stripped so only the orchestration prompt remains.
func orchestratorSystem(orch agents.OrchDef) string {
	body := orch.SystemTemplate
	if s := strings.Index(body, "<!--"); s >= 0 {
		if e := strings.Index(body[s:], "-->"); e >= 0 {
			body = body[s+e+len("-->"):]
		}
	}
	body = strings.TrimSpace(body)

	var sb strings.Builder
	sb.WriteString("You are the orchestrator (the Main agent) of oline, an authorized offensive-security agent harness. ")
	sb.WriteString("You coordinate a fleet of specialized subagents via the `task` tool, collect their work with `wait`, ")
	sb.WriteString("steer peers with `send_message`, and consult the user with `ask`. You never run offensive tools yourself — you delegate.\n")
	sb.WriteString("Your subagents stream you incremental progress updates with `send_message` as they work (a finished scan, a foothold, a finding, a blocker), not only in their final results. These arrive in your context and interrupt a `wait`. Read them and act on them as they come — fold them into your plan, re-task or coordinate agents based on what just arrived (for example, start an exploiter on a service recon just reported), and keep the engagement moving rather than waiting idly for a subagent to fully finish.\n")
	sb.WriteString("Subagents run under engine-enforced time budgets and may finish or be terminated before completing their task — that is expected. When one returns partial work or times out, `reprompt` it to continue with its context intact, or `task` a fresh one, rather than treating an unfinished subagent as a dead end. You are never time-limited yourself; keep sleeps/`heartbeat` bounded to the minimum.\n")
	sb.WriteString("Keep the user oriented with two live panels, Summary and Diagram. Call `update_summary` whenever the state of play changes meaningfully (a new foothold, a confirmed finding, a phase done, a change of plan) with a concise plain-language state-of-play — what is done, confirmed, in progress, and next; this replaces the previous summary and saves the user from having to ask.\n")
	sb.WriteString("Keep the Diagram panel current with `update_diagram`: a breadth-first ASCII map of the engagement. In a NETWORK pentest, draw the hosts in play, mark which you have a foothold on versus root on, show how hosts connect to each other, and flag which hosts run important services. In a SOFTWARE/exploiter engagement, draw the target's main components and how they interact, choosing which components are worth showing. Redraw it whenever the picture changes — a host discovered, a foothold or root gained, a key service or component identified. Legibility matters more than completeness: in a large environment show only the most important hosts/components and connections rather than every one, and include a short legend for your markers. The Diagram pane has a FIXED width (its exact current value is given to you at the start of the engagement): every line of the map must fit within it, because the pane clips over-wide lines rather than wrapping them. `update_diagram` enforces this — it REJECTS a map with any line wider than the pane, tells you exactly which lines are too wide and the current width limit, and does not change the panel; shorten those lines and call it again. The width can change if the user resizes the pane, so `update_diagram` always reports the current width (on success and on rejection) — treat its latest report as the authoritative limit. The map replaces the previous one each time.\n")
	sb.WriteString("Also call `record_finding` yourself for any confirmed, security-relevant result a subagent reports to you that it did not already record, so every real finding is logged for the reporter exactly once (do not duplicate one a subagent already recorded).\n")
	sb.WriteString("Watch the shared sandbox so your subagents never overload it. Every subagent runs inside the SAME sandbox (one small VM/distro), so several heavy jobs at once — scanners, fuzzers, crackers, brute-forcers, large builds — can exhaust its CPU, memory, or disk and slow or stall the whole fleet. The engine already caps how many agents run at once (the maxConcurrency setting counts you, the orchestrator, too), but staying under that cap is not the same as staying under the machine's limits. Call the `resources` tool to snapshot the sandbox's load (CPU cores and load average, memory, workspace disk, and a per-agent-class breakdown of who is consuming what) BEFORE fanning out several heavy subagents, and again whenever work feels slow or an agent is quiet. Read it like an operator: load average well above the core count, memory near full, or disk filling means the box is saturated. When it is, do NOT pile on — stagger the heavy spawns instead of launching them together, hold off starting more until load drops, and `send_message` the heaviest current subagents telling them to ease off (lower their scan rate/threads/parallelism, run one tool at a time, or pause non-essential work) so the others keep making progress. Keep the fleet busy but within the machine's headroom.\n")
	sb.WriteString("You are running inside software that renders Markdown, and your replies are shown to the user that way. Prefer Markdown over plain text: use headings, bold, lists, tables, and fenced code blocks. Write well-structured Markdown by default.\n\n")
	if p := strings.TrimSpace(orch.Personality); p != "" {
		sb.WriteString("# Personality\n")
		sb.WriteString(p)
		sb.WriteString("\n\n")
	}
	sb.WriteString(body)
	return sb.String()
}
