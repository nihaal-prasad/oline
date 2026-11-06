package engine

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/nihaal-prasad/oline/internal/core"
	"github.com/nihaal-prasad/oline/internal/decide"
	"github.com/nihaal-prasad/oline/internal/event"
)

// Decision-gate tuning. All conservative except the finding is_real drop, which is
// aggressive (per the user): a finding scoring below realThreshold is dropped.
const (
	realThreshold   = 0.40             // finding "is_real" probability below this -> hard-drop as noise
	dupThreshold    = 0.75             // "duplicate_of" confidence at/above this -> merge
	judgeThreshold  = 1                // "warrants_judgement" level index (0..2) at/above this -> run the judge
	spiralThreshold = 1                // "progress" level index (0..3) at/below this -> intervene
	decisionTimeout = 12 * time.Second // per decision call
)

// severityChoices are the canonical severity levels the decision model picks from.
var severityChoices = []decide.Choice{
	{Value: "critical"}, {Value: "high"}, {Value: "medium"}, {Value: "low"}, {Value: "info"},
}

// decisionCtx returns a bounded context for one decision call, derived from the
// engagement context so engagement-end cancels it.
func (a *Agent) decisionCtx() (context.Context, context.CancelFunc) {
	base := a.s.base()
	if base == nil {
		base = context.Background()
	}
	return context.WithTimeout(base, decisionTimeout)
}

// fitForDecision shrinks doc to the decision model's context window using the
// GENERATIVE summarizer (the decision model cannot summarize). Falls back to the
// agent's own model, then to truncation, so it never fails the caller.
func (a *Agent) fitForDecision(ctx context.Context, doc string) string {
	budget := inputBudgetChars(a.s.DecisionContextTokens, 0)
	if len(doc) <= budget {
		return doc
	}
	client, model := a.s.SummarizerLLM, a.s.SummarizerModel
	if client == nil || model == "" {
		client, model = a.llm, a.modelID
	}
	if client != nil && model != "" {
		if out, err := a.summarizeText(ctx, client, model, doc, budget, summaryMaxTokens); err == nil && strings.TrimSpace(out) != "" {
			return out
		}
	}
	if len(doc) > budget {
		return doc[:budget]
	}
	return doc
}

// --- Gate 1: finding triage + dedup -----------------------------------------

// findingDecision is the outcome of the triage gate.
type findingDecision struct {
	drop     bool   // hard-drop as noise (do not record)
	mergeIdx int    // >=0: merge into existing findings[mergeIdx]; -1 otherwise
	severity string // decision-assigned severity ("" = leave the agent's)
}

// triageFinding runs the finding gate. It fails open: any decision error leaves the
// finding untouched (recorded with the agent's own severity). It short-circuits on
// the first error (assume the provider is down) to bound latency.
func (a *Agent) triageFinding(newF Finding) findingDecision {
	d := findingDecision{mergeIdx: -1}
	ctx, cancel := a.decisionCtx()
	defer cancel()
	existing := a.s.Findings()
	doc := a.fitForDecision(ctx, renderFindingDoc(newF, existing))

	// Severity is always decided by the decision model when it is on.
	sev, _, err := a.s.Decider.Choice(ctx, doc,
		"What is the severity of the NEW finding described at the top of the document?", severityChoices)
	if err != nil {
		return d // provider down -> fail open
	}
	d.severity = normalizeSeverity(sev)

	// Duplicate of an existing finding?
	if len(existing) > 0 {
		choices := make([]decide.Choice, 0, len(existing)+1)
		for i, f := range existing {
			choices = append(choices, decide.Choice{Value: strconv.Itoa(i), Description: f.Title + " — " + f.Target})
		}
		choices = append(choices, decide.Choice{Value: "none", Description: "the NEW finding is not a duplicate of any listed finding"})
		val, conf, err := a.s.Decider.Choice(ctx, doc,
			"Is the NEW finding the same issue as one of the EXISTING findings? Answer its number, or \"none\".", choices)
		if err != nil {
			return d
		}
		if val != "none" && conf >= dupThreshold {
			if idx, perr := strconv.Atoi(val); perr == nil && idx >= 0 && idx < len(existing) {
				d.mergeIdx = idx
				return d // a duplicate is never also dropped
			}
		}
	}

	// Noise? (aggressive: drop below the threshold)
	p, err := a.s.Decider.Predicate(ctx, doc,
		"Is the NEW finding a confirmed, in-scope, security-relevant issue — not a hunch, a duplicate, or noise?")
	if err != nil {
		return d
	}
	if p < realThreshold {
		d.drop = true
	}
	return d
}

func renderFindingDoc(newF Finding, existing []Finding) string {
	var b strings.Builder
	b.WriteString("NEW FINDING:\n")
	fmt.Fprintf(&b, "title: %s\nclaimed severity: %s\ntarget: %s\ndetail: %s\n\n", newF.Title, newF.Severity, newF.Target, newF.Detail)
	if len(existing) == 0 {
		b.WriteString("EXISTING FINDINGS: none\n")
		return b.String()
	}
	b.WriteString("EXISTING FINDINGS (numbered):\n")
	for i, f := range existing {
		fmt.Fprintf(&b, "%d. [%s] %s — %s: %s\n", i, f.Severity, f.Title, f.Target, f.Detail)
	}
	return b.String()
}

// mergeFindingDetail appends the new finding's evidence to an existing finding, so a
// merged duplicate still contributes its detail. Guarded by resMu.
func (s *Session) mergeFindingDetail(idx int, add Finding) {
	s.resMu.Lock()
	defer s.resMu.Unlock()
	if idx < 0 || idx >= len(s.findings) {
		return
	}
	extra := strings.TrimSpace(add.Detail)
	if extra == "" {
		return
	}
	note := fmt.Sprintf("\n[merged from %s]: %s", add.Agent, extra)
	s.findings[idx].Detail += note
}

// --- Gate 2: judge pre-screen ----------------------------------------------

// judgeGate reports whether a verification (judge) task has enough evidence to run.
// Fails open (returns true) on any decision error so an outage never blocks the judge.
func (a *Agent) judgeGate(prompt string) bool {
	ctx, cancel := a.decisionCtx()
	defer cancel()
	doc := a.fitForDecision(ctx, fmt.Sprintf("GOAL: %s\nTARGET: %s\n\nCLAIM TO VERIFY:\n%s", a.s.Goal, a.s.Target, prompt))
	idx, _, err := a.s.Decider.Score(ctx, doc,
		"How strong is the evidence that this describes a real, reproducible vulnerability worth full verification?",
		[]string{"no — weak or unsupported", "borderline", "yes — strong, reproducible evidence"})
	if err != nil {
		return true
	}
	return idx >= judgeThreshold
}

// --- Gate 4: task routing ---------------------------------------------------

// routeTask asks the decision model which existing subagent role (of the CURRENT
// mode) should handle a task described only by its prompt. The answer is constrained
// to the listed roles, so it can never be a role from another mode or a nonexistent
// one. Returns an error on a decision failure (caller surfaces it to the orchestrator).
func (a *Agent) routeTask(prompt string) (string, error) {
	roles := a.s.RouterRoles
	if len(roles) == 0 {
		return "", fmt.Errorf("no subagent roles available to route to")
	}
	choices := make([]decide.Choice, len(roles))
	for i, r := range roles {
		choices[i] = decide.Choice{Value: r.Name, Description: r.Description}
	}
	ctx, cancel := a.decisionCtx()
	defer cancel()
	doc := a.fitForDecision(ctx, fmt.Sprintf("GOAL: %s\nTARGET: %s\n\nTASK TO ASSIGN TO A SPECIALIST:\n%s", a.s.Goal, a.s.Target, prompt))
	val, _, err := a.s.Decider.Choice(ctx, doc, "Which specialist should handle this task? Pick exactly one.", choices)
	if err != nil {
		return "", err
	}
	for _, r := range roles { // defensive: the API constrains to choices, but verify
		if strings.EqualFold(r.Name, val) {
			return r.Name, nil
		}
	}
	return "", fmt.Errorf("router returned unknown role %q", val)
}

// --- Gate 3: spiral / progress detector -------------------------------------

// progressCheck grades the agent's progress toward the goal and, when it looks stuck,
// notifies the orchestrator AND steers the subagent. Fails open (silent) on error.
// Runs in its own goroutine (launched from checkBudget) so it never stalls the monitor.
func (a *Agent) progressCheck() {
	ctx, cancel := a.decisionCtx()
	defer cancel()
	doc := a.fitForDecision(ctx, fmt.Sprintf("GOAL: %s\nTARGET: %s\n\nAGENT'S RECENT TRANSCRIPT:\n%s", a.s.Goal, a.s.Target, a.inspectText()))
	idx, _, err := a.s.Decider.Score(ctx, doc,
		"Does this agent appear to be making progress towards the overarching goal?",
		[]string{"stuck / rabbit-holing", "little progress", "steady progress", "strong progress"})
	if err != nil || idx > spiralThreshold {
		return
	}
	a.s.router.send(Envelope{From: systemSender, To: core.Main, Kind: MsgSteer,
		Body: fmt.Sprintf("%s appears to be making little progress toward the goal (automatic progress check). Consider refocusing it, reprompting it with a sharper instruction, or killing it.", a.id)})
	a.s.router.send(Envelope{From: systemSender, To: a.id, Kind: MsgSteer,
		Body: "Automatic progress check: you may be rabbit-holing. Step back, re-read your objective, and either switch to a clearly different approach on the SAME objective or return what you have to the orchestrator now."})
	a.s.emit(event.Notice{Agent: a.id, Level: "warn", Text: string(a.id) + ": low progress — nudged to refocus"})
}
