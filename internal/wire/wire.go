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
	"github.com/nihaal-prasad/oline/internal/decide"
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
	// Roles are compared case-insensitively here as everywhere else (Roster.Lookup,
	// core.CanonRole); a capitalized name: in frontmatter must still get the right
	// egress class, not silently fall through to the default.
	role = strings.ToLower(strings.TrimSpace(role))
	if role == "installer" {
		// The installer is the ONLY agent that runs as root (to install software and,
		// in exploiter mode, download the target application).
		return sandbox.Privileged
	}
	if internetAgents[role] {
		return sandbox.InternetNoTarget
	}
	// Exploiter analyzes a LOCAL app and Programmer builds tooling locally — neither
	// has a remote target to reach, so every non-internet, non-installer agent is
	// isolated (loopback only). In Programmer mode this is the "installer-only
	// internet" posture: code agents build offline against what the root installer
	// provisioned, and only the installer (and researcher, for lookups) touch the net.
	if mode == config.ModeExploiter || mode == config.ModeProgrammer {
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
func Build(mode config.Mode, store *config.Store, roster *agents.Roster, decisionEnabled bool) (engine.AgentConfig, engine.ConfigResolver) {
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
		// Severity ownership in the decision-on exploiter run. The judge gets its
		// severity-assignment wording surgically stripped (it instead reports the
		// facts that determine severity; the decision model rates it when the judge
		// records the finding). Every other agent gets the appended override. If the
		// judge prompt has been customized so the strip anchors no longer match, fall
		// back to the override so the no-self-rating guarantee still holds.
		severityAutomatic := mode == config.ModeExploiter && decisionEnabled
		body := def.Prompt
		appendOverride := severityAutomatic
		if severityAutomatic && strings.EqualFold(def.Name, "judge") {
			if stripped, ok := stripJudgeSeverity(def.Prompt); ok {
				body, appendOverride = stripped, false
			}
		}
		return engine.AgentConfig{
			Role:             def.Name,
			Model:            model,
			Effort:           effortFor(eff, model, def.ThinkingLevel),
			System:           subagentSystem(body, budget, containsStr(tools, "bash"), appendOverride),
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
	// `revshell` is included so the orchestrator can arbitrate reverse-shell write
	// access (it alone may `grant` the token — see the revshell tool), as the prompts
	// describe; the engine still restricts the `grant` action itself to Main.
	orchTools := []string{"task", "wait", "reprompt", "inspect", "read", "write", "find", "grep", "glob", "send_message", "ask", "update_summary", "update_diagram", "record_finding", "resources", "terminal", "revshell"}
	if mode == config.ModeExploiter {
		orchTools = append(orchTools, "runtime", "heartbeat")
	}
	// The orchestrator runs on a configurable tier (default big) at a configurable
	// thinking level (default high) — see config.OrchestratorConfig.
	orchTier := tierOf(eff.Orchestrator.Tier)
	orchModel := eff.ModelForTier(orchTier)
	orchEffort := effortFor(eff, orchModel, eff.Orchestrator.ThinkingLevel)
	orchCtx := eff.Orchestrator.MaxContextTokens
	if orchCtx <= 0 {
		orchCtx = contextForTier(eff, orchModel, orchTier)
	}
	// When the decision model is on, the orchestrator still sees WHAT the specialists
	// do (a capability summary, for planning and writing good task prompts) but does
	// NOT pick one — a router assigns it — and is told the automated gates exist.
	// Otherwise it gets the normal per-tier roster with the spawn-by-name mechanics.
	sys := orchestratorSystem(roster.Orchestrator)
	if decisionEnabled {
		sys += capabilityRoster(roster) + decisionGatesPrompt()
	} else {
		sys += tierRoster(roster, orchTier)
	}
	// In exploiter mode the decision model assigns severity itself when it is on
	// (see engine/decision_gates.go: triageFinding), so the orchestrator must not
	// route findings/exploits for a human-style CRITICAL/HIGH/MEDIUM/LOW rating by a
	// subagent. This comes last so it overrides the template's "have it judged" steps.
	if decisionEnabled && mode == config.ModeExploiter {
		sys += severityAutomaticPrompt()
	}
	orch := engine.AgentConfig{
		Role:             "Main",
		Model:            orchModel,
		Effort:           orchEffort,
		System:           sys,
		Tools:            orchTools,
		MaxContextTokens: orchCtx,
		MaxTokens:        modelMaxTokens(eff, orchModel),
		IsOrchestrator:   true,
	}
	return orch, resolver
}

// capabilityRoster lists the mode's specialists and what each does, so the
// orchestrator can plan the workflow and write precise task descriptions when the
// decision router is on. Unlike tierRoster it gives no "spawn one by its name"
// mechanics and no tier-handoff advice, because the orchestrator does not pick the
// specialist — the router does.
func capabilityRoster(roster *agents.Roster) string {
	var sb strings.Builder
	sb.WriteString("\n\n# Available specialists\n")
	sb.WriteString("These specialists exist and can be assigned work. You do NOT pick one — you describe each task and the right specialist is selected automatically — but knowing what they do helps you plan the engagement and write precise task descriptions:\n")
	for _, name := range roster.Names() {
		d := roster.Agents[name]
		entry := core.CanonRole(name)
		if desc := firstLine(d.Description); desc != "" {
			entry += " — " + desc
		}
		fmt.Fprintf(&sb, "- %s\n", entry)
	}
	return sb.String()
}

// decisionGatesPrompt is appended to the orchestrator's system prompt when the
// decision model is enabled. It tells the orchestrator that specialist assignment,
// finding triage, and judge pre-screening are automatic.
func decisionGatesPrompt() string {
	return "\n\n# Automated assignment and gates\n" +
		"A fast automated system assists you; rely on it rather than working around it.\n" +
		"- **Specialist assignment:** You do NOT have to pick or name a specialist when starting a subagent. The `task` tool takes only a task description (no agent/role argument); describe the work fully and self-containedly and the right specialist from the list above is assigned for you automatically. (`reprompt` still targets an agent you already started, by its id.)\n" +
		"- **Findings:** Findings you or your subagents record are automatically deduplicated, assigned a severity, and noise-filtered. If a finding is dropped as noise or out of scope, you will be told — if you believe that was wrong, have it re-recorded with stronger, concrete evidence.\n" +
		"- **Verification:** When you `task` a verification of an exploit, it is automatically pre-screened for sufficient evidence before a verifier runs. If it does not meet the bar you will be told to gather stronger, reproducible proof first.\n"
}

// DecisionClient builds the optional decision-model client from a "provider/model-id"
// selector, returning the client, the bare model id, and the model's context window
// (0 if unknown). It resolves the provider base URL + API key exactly like the LLM
// resolver. Used at engagement start; the caller probes it before enabling the
// decision features.
func DecisionClient(store *config.Store, selector string) (*decide.Client, string, int, error) {
	eff := store.Effective()
	provider, modelID, ok := strings.Cut(selector, "/")
	if !ok || provider == "" || modelID == "" {
		return nil, "", 0, fmt.Errorf("decision model %q must be in provider/model-id form", selector)
	}
	p, ok := eff.Providers[provider]
	if !ok {
		return nil, "", 0, fmt.Errorf("decision model: unknown provider %q", provider)
	}
	key, err := store.ResolveKey(provider)
	if err != nil {
		return nil, "", 0, err
	}
	return decide.New(p.BaseURL, key, modelID), modelID, modelContextWindow(eff, selector), nil
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
func tierRoster(roster *agents.Roster, orchTier string) string {
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
	fmt.Fprintf(&sb, "You run on the %s tier. If a subagent keeps failing at a task that needs stronger reasoning (bad analysis, repeated mistakes, stuck), do not just reprompt it — hand that piece to a higher-tier agent from the lists above (`task` it), which is more capable. Match the work to the tier: routine, high-volume steps are fine on small/medium; the hardest reasoning belongs on big.\n", orchTier)
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
	return appendUnique(toolset, "send_message", "ask", "runtime", "record_finding", "terminal")
}

// subagentSystem prepends the shared harness-level preamble to every subagent's
// prompt: how to report, the current run modes, staying on objective, the agent's
// REAL enforced time budget (budgetMinutes, 0 = no limit), finding recording, the
// reverse-shell tool (only when the agent has bash), and output format. This is
// the single source of truth for that boilerplate, so individual agent prompts do
// not repeat it. body is the agent's own role/task definition.
func subagentSystem(body string, budgetMinutes int, hasBash, severityAutomatic bool) string {
	var sb strings.Builder

	sb.WriteString("# Reporting to the orchestrator\n")
	sb.WriteString("You are a subagent coordinated by the orchestrator (the Main agent). As you work, send " +
		"Main a brief `send_message` update the moment something matters — a completed scan or step, a " +
		"foothold, a confirmed finding, a credential, a dead end, or a blocker — not only when you finish. " +
		"One or two sentences each. Reporting continuously lets the orchestrator act on your work in real " +
		"time (e.g. start another agent on what you just found) while you keep going; don't hold it all back " +
		"for your final result.\n")
	sb.WriteString("You CANNOT spawn subagents yourself — only the orchestrator can. If you need a " +
		"specialist's help (e.g. research, custom code, a scan of a newly reachable host), `send_message` " +
		"the orchestrator describing what you need and let it start one; do not try to do that work outside " +
		"your own role.\n\n")

	sb.WriteString("# Operating mode — automatic, planner, or watcher\n")
	sb.WriteString("Your task prompt names the current mode, and the operator can switch it mid-run.\n")
	sb.WriteString("- **Automatic** — proceed autonomously within the boundary rules, no per-step approval. " +
		"On a genuine blocker, stop and return to the orchestrator rather than guessing.\n")
	sb.WriteString("- **Planner** — the USER decides, not you. Before doing any work, get your plan approved by " +
		"asking the user DIRECTLY with the `ask` tool (what you intend, which tools/commands, against which " +
		"in-scope target). Treat the plan as UNAPPROVED until the user explicitly approves it — silence, a " +
		"question, an acknowledgement (\"ok\"), or feedback is NOT approval. Do only what was approved; for any " +
		"choice or action the plan did not cover, stop and `ask` again. Never run ahead of an approval.\n")
	sb.WriteString("- **Watcher** — you do NOT execute anything; the operator runs every command themselves. The " +
		"harness hard-blocks `bash`, `bash_bg`, and `revshell` for you, so you cannot run, scan, or drive the " +
		"target even if you try. Your job is to advise: read and write workspace files and give concise, " +
		"concrete guidance (what to run next and why, how to read a result, what to watch for). Don't try to " +
		"act; if you need to see some output, ask the operator for it.\n\n")

	sb.WriteString("# Stay on your assigned objective\n")
	sb.WriteString("Pursue the EXACT objective your task prompt gave you, and only that. Don't drift to a " +
		"different target, service, or technique because it looks easier or more interesting; note-and-report " +
		"side findings to the orchestrator rather than chasing them. If an approach fails after an honest " +
		"attempt or two, try one clearly different angle on the SAME objective, then return to the orchestrator " +
		"with what you tried — don't grind or spiral. If the user or the orchestrator tells you (via " +
		"`send_message`) that you're rabbit-holing, or to stop/refocus/change course, STOP at once — don't " +
		"finish the tangent or defend it — and follow the new direction; it overrides your current plan.\n\n")

	sb.WriteString("# Your time budget\n")
	if budgetMinutes > 0 {
		fmt.Fprintf(&sb, "The engine enforces a time budget of about **%d minutes** on you (time blocked in "+
			"`wait` or any sleep counts against it, so wait only the minimum needed). Spend it well: take the "+
			"most time-efficient path and surface the highest-value results first in case you run short. As you "+
			"near the limit, WRAP UP rather than push on — make sure your notes capture what you ran and found, "+
			"return your partial results, and tell the orchestrator any follow-ups you didn't reach. ", budgetMinutes)
	} else {
		sb.WriteString("You have **no fixed time limit** — the engine will not terminate you on a clock — so a " +
			"long-running task (installing, cracking, persistence, cleanup) can finish. Still work efficiently " +
			"and don't idle with nothing outstanding; return once the task is done. ")
	}
	sb.WriteString("Call the `runtime` tool to check your remaining budget, and judge your remaining time ONLY " +
		"from it or oline's reminders, never from wall-clock guesses. If you are terminated at your budget the " +
		"orchestrator can reprompt you (keeping your context) or spawn a fresh agent to continue, so always end " +
		"a cycle with a brief summary of what you did and found — never return nothing.\n\n")

	sb.WriteString("# Recording findings\n")
	sb.WriteString("The moment you CONFIRM something security-relevant — a vulnerability, an exposed or cracked " +
		"credential, a foothold, a dangerous misconfiguration — call `record_finding` with a short factual " +
		"title, a severity (critical/high/medium/low/info), the affected target, and a line or two of " +
		"evidence. This logs it immediately and hands it to the reporter. Record only confirmed, non-duplicate " +
		"findings, not hunches; recording does not replace a `send_message` to the orchestrator.\n\n")

	sb.WriteString("# Reading the operator's terminals\n")
	sb.WriteString("The human operator has their own interactive terminals in the UI — line shells and full-screen " +
		"terminals — separate from yours. Use the read-only **`terminal`** tool to see what they are doing: " +
		"`terminal list` shows every open operator terminal (e.g. `Shell#1`, `Term#1`) and `terminal read <id>` " +
		"returns its recent output or current screen. Check it to coordinate with the operator, build on what " +
		"they have already run, and avoid repeating their work. It is READ-ONLY — you cannot type into their " +
		"terminals.")
	if hasBash {
		sb.WriteString(" Run your own commands with `bash` and drive a reverse shell with `revshell`.")
	}
	sb.WriteString("\n\n")

	if hasBash {
		sb.WriteString("# Reverse shells and background jobs\n")
		sb.WriteString("When you need a reverse shell, the **`revshell`** tool is the PREFERRED way to catch and " +
			"drive one — use it instead of a raw `nc`/`socat`/bash handler unless the user asks otherwise or it " +
			"genuinely cannot cover the scenario. It runs a named listener in the sandbox and keeps the channel " +
			"writable: `open` a listener on a port, `send` a command line and read what follows, `read` recent " +
			"output, `list`, `close`. A channel can be shared, but only ONE agent may `send` at a time via a " +
			"write token the **orchestrator (Main) arbitrates** — if you need a channel another agent holds, ask " +
			"Main to grant it to you, and `release` yours when you're done. For any long-running command " +
			"(servers, listeners, scans, large builds) use **`bash_bg`** to run it as a background job instead of " +
			"blocking on `bash`.\n\n")
	}

	sb.WriteString("# Output format\n")
	sb.WriteString("You run inside software that renders Markdown, and your replies are shown to the user and the " +
		"orchestrator that way. Prefer Markdown over plain text: headings, bold, bullet and numbered lists, " +
		"tables, and fenced code blocks for commands, code, file paths, and tool output.\n\n")

	sb.WriteString(strings.TrimSpace(body))

	// When the decision model owns severity (exploiter mode + decision enabled), no
	// subagent should also rate criticality. Appended LAST so it authoritatively
	// overrides any earlier instruction in the role body (e.g. the judge's CVSS step).
	if severityAutomatic {
		sb.WriteString(severityAutomaticPrompt())
	}
	return sb.String()
}

// severityAutomaticPrompt is appended to every exploiter-mode agent's system prompt —
// the orchestrator and all subagents — when the decision model is enabled. In that
// configuration the decision model assigns severity itself (engine/decision_gates.go:
// triageFinding always sets the finding's severity when the Decider is on), so no
// agent should also judge criticality: two raters would conflict and the decision
// model's rating is the one that is kept. It is appended LAST so it overrides any
// earlier instruction — most importantly the judge's "assign a CVSS severity" step —
// that tells an agent to rate a finding itself.
// stripJudgeSeverity rewrites the judge's prompt body for the decision-on exploiter
// case: it removes the two passages that tell the judge to assign a CVSS severity
// rating and replaces them with instructions to report the facts that DETERMINE
// severity. The decision model assigns the final rating from those facts when the
// judge records its finding (engine/decision_gates.go: triageFinding). It returns the
// rewritten body and true only when every expected passage was found; if the judge
// prompt has been customized so an anchor no longer matches, it returns ("", false)
// and the caller appends severityAutomaticPrompt() instead, so the no-self-rating
// guarantee holds regardless.
func stripJudgeSeverity(body string) (string, bool) {
	repls := []struct{ old, new string }{
		{
			old: "5. **Assign a CVSS-based severity: CRITICAL / HIGH / MEDIUM / LOW.** Rate the exploit's real-world\n" +
				"   severity in line with CVSS reasoning (impact on confidentiality/integrity/availability, attack\n" +
				"   vector, complexity, privileges and user interaction required). **An exploit that only works under\n" +
				"   unrealistic conditions or in rare/atypical deployments must not be rated CRITICAL** — the rating\n" +
				"   must reflect how exploitable real deployments actually are, not the best-case lab scenario. Briefly\n" +
				"   justify the rating (you may cite an approximate CVSS vector/score if helpful).",
			new: "5. **Report the factors that determine severity — do NOT assign a severity rating yourself.** Lay\n" +
				"   out the CVSS-style inputs needed to rate this exploit: the impact on confidentiality / integrity /\n" +
				"   availability, the attack vector, attack complexity, the privileges required, and any user\n" +
				"   interaction required — grounded in what you observed while reproducing. Make clear how exploitable\n" +
				"   **typical real-world deployments** actually are versus the best-case lab scenario (e.g. whether the\n" +
				"   required conditions are rare or atypical in practice), since that bounds the severity. **An\n" +
				"   automated decision model assigns the final CRITICAL / HIGH / MEDIUM / LOW rating from the facts you\n" +
				"   report** immediately after you return, so make those facts complete and honest rather than picking\n" +
				"   a label yourself.",
		},
		{
			old: "- the **severity: CRITICAL / HIGH / MEDIUM / LOW**, with a brief CVSS-based justification;",
			new: "- the **severity-determining factors** (CVSS-style: impact on confidentiality/integrity/availability,\n" +
				"  attack vector, attack complexity, privileges required, user interaction, and how exploitable typical\n" +
				"  real-world deployments are) — the inputs needed to rate it; **do not assign the CRITICAL / HIGH /\n" +
				"  MEDIUM / LOW rating yourself — that is determined automatically from these facts**;",
		},
	}
	out := body
	for _, r := range repls {
		if !strings.Contains(out, r.old) {
			return "", false
		}
		out = strings.Replace(out, r.old, r.new, 1)
	}
	return out, true
}

func severityAutomaticPrompt() string {
	return "\n\n# Severity is assigned automatically — do not rate criticality\n" +
		"An automated decision model owns severity in this run. Do NOT assign, rate, score, or otherwise " +
		"judge the **CRITICAL / HIGH / MEDIUM / LOW** severity — the criticality — of any specific finding, " +
		"exploit, crash, or leak yourself, and disregard any instruction elsewhere in this prompt that tells " +
		"you to produce a CVSS score or a severity rating for a result: that rating is derived automatically " +
		"from the facts you report. Your job is to establish and report those facts — what the issue is, " +
		"whether it reproduces from a clean state, the exact conditions it requires, and how realistic those " +
		"conditions are in real-world deployments — and to leave the rating to the decision model. This does " +
		"not change which work to prioritize: you may still reason about likely impact and focus your effort " +
		"on the highest-impact classes of issue; you simply never stamp a severity label on an individual " +
		"result. When you would otherwise state a severity, report the underlying facts instead.\n"
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
	sb.WriteString("Your subagents stream you incremental progress updates with `send_message` as they work (a finished scan, a foothold, a finding, a blocker), not only in their final results. These arrive in your context and interrupt a `wait`. Read them and act on them as they come — fold them into your plan, re-task or coordinate agents based on what just arrived (for example, start follow-up work on a service a subagent just reported), and keep the engagement moving rather than waiting idly for a subagent to fully finish.\n")
	sb.WriteString("Subagents run under engine-enforced time budgets and may finish or be terminated before completing their task — that is expected. When one returns partial work or times out, `reprompt` it to continue with its context intact, or `task` a fresh one, rather than treating an unfinished subagent as a dead end. You are never time-limited yourself; keep any sleeps or waits bounded to the minimum.\n")
	sb.WriteString("Keep the user oriented with two live panels, Summary and Diagram. Call `update_summary` whenever the state of play changes meaningfully (a new foothold, a confirmed finding, a phase done, a change of plan) with a concise plain-language state-of-play — what is done, confirmed, in progress, and next; this replaces the previous summary and saves the user from having to ask.\n")
	sb.WriteString("Keep the Diagram panel current with `update_diagram`: a breadth-first ASCII map of the engagement. In a NETWORK pentest, draw the hosts in play, mark which you have a foothold on versus root on, show how hosts connect to each other, and flag which hosts run important services. In a SOFTWARE/exploiter engagement, draw the target's main components and how they interact, choosing which components are worth showing. In a TOOL-DEVELOPMENT (programmer) engagement, draw the tool or codebase you are building: its modules/packages/files and how they depend on and call one another, choosing which parts are worth showing. Redraw it whenever the picture changes — a host discovered, a foothold or root gained, a key service or component identified. Legibility matters more than completeness: in a large environment show only the most important hosts/components and connections rather than every one, and include a short legend for your markers. The Diagram pane has a FIXED width (its exact current value is given to you at the start of the engagement): every line of the map must fit within it, because the pane clips over-wide lines rather than wrapping them. `update_diagram` enforces this — it REJECTS a map with any line wider than the pane, tells you exactly which lines are too wide and the current width limit, and does not change the panel; shorten those lines and call it again. The width can change if the user resizes the pane, so `update_diagram` always reports the current width (on success and on rejection) — treat its latest report as the authoritative limit. The map replaces the previous one each time.\n")
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
