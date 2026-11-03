// Package engine is oline's native agent runtime: an actor-model fleet where
// each agent is a goroutine with a mailbox, coordinated by a Router. It owns the
// agent loop (LLM stream -> tool calls -> repeat), the spawn/wait/steer/kill
// primitives, and immediate pause/resume (cancel the in-flight LLM stream and
// sandbox command, then block the loop until resumed).
package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/charmbracelet/x/ansi"

	"github.com/nihaal-prasad/oline/internal/core"
	"github.com/nihaal-prasad/oline/internal/event"
	"github.com/nihaal-prasad/oline/internal/llm"
	"github.com/nihaal-prasad/oline/internal/sandbox"
	"github.com/nihaal-prasad/oline/internal/tools"
)

const (
	defaultMaxTokens = 8192
	// llmTurnTimeout backstops a single hung LLM stream even when the parent
	// context has no deadline.
	llmTurnTimeout = 180 * time.Second
	// foregroundTimeout bounds how long a foreground `bash` command blocks the
	// agent before it is auto-moved to the background (it keeps running; the agent
	// is freed to continue). Without this, a long-running or input-waiting command
	// hangs the agent indefinitely.
	foregroundTimeout = 2 * time.Minute
)

// LLM is the streaming client contract (satisfied by *llm.Client; mockable).
type LLM interface {
	Stream(ctx context.Context, req llm.ChatReq, ch chan<- llm.Delta) error
}

// LLMResolver maps a "provider/model-id" selector to a client and the bare model id.
type LLMResolver func(selector string) (client LLM, modelID string, err error)

// ConfigResolver maps a subagent role name to its AgentConfig.
type ConfigResolver func(role string) (AgentConfig, bool)

// AgentConfig is everything the engine needs to run one agent.
type AgentConfig struct {
	Role          string
	Model         string // "provider/model-id" selector
	Effort        string // low|medium|high (already resolved)
	System        string // system prompt
	Tools         []string
	Spawns        []string
	Policy        sandbox.EgressPolicy // egress class (routes sandbox exec to the agent's guest user)
	BudgetMinutes int                  // engine-enforced time budget; 0 = no limit (orchestrator is never limited)
	// MaxContextTokens is the estimated-token ceiling at which this agent's
	// conversation is auto-compacted (older turns summarized). 0 = no compaction.
	MaxContextTokens int
	// MaxTokens caps a single reply (the request's max_tokens). 0 falls back to
	// defaultMaxTokens; wire fills it from the model's declared maxTokens.
	MaxTokens      int
	IsOrchestrator bool
}

func (c AgentConfig) maySpawn(role string) bool {
	if c.IsOrchestrator {
		return true
	}
	for _, r := range c.Spawns {
		if strings.EqualFold(r, role) { // role names are matched case-insensitively
			return true
		}
	}
	return false
}

// MsgKind classifies a mailbox envelope.
type MsgKind int

const (
	MsgSteer      MsgKind = iota // orchestrator/user steering a running agent
	MsgPeer                      // a peer or the user chatting with this agent
	MsgUserAnswer                // manual-mode USER-ANSWER relay
	MsgResult                    // a spawned child's final result
	MsgJobDone                   // a background job this agent started has finished
	MsgBudget                    // an engine time-budget reminder / notice
)

// Time-budget enforcement (engine-driven, not AI-driven): once a subagent runs
// past its budget it gets three escalating reminders, then — if still running —
// is terminated. These are hardcoded; the budget value itself is configurable.
const (
	budgetTick        = 15 * time.Second // how often the monitor checks
	budgetReminderGap = 2 * time.Minute  // spacing between the three reminders
	budgetKillGrace   = 5 * time.Minute  // kill this long after the 3rd reminder
)

// systemSender is the From id for engine-generated messages (time budgets, etc.).
const systemSender = core.AgentID("oline")

// Envelope is a message delivered to an agent's mailbox.
type Envelope struct {
	From core.AgentID
	To   core.AgentID
	Kind MsgKind
	Body string
}

// Result is an agent's terminal outcome.
type Result struct {
	Agent  core.AgentID
	Text   string
	Err    error
	Killed bool
}

// Session holds the shared dependencies for one engagement run.
type Session struct {
	Registry  *tools.Registry
	Sandbox   sandbox.Handle
	LLMFor    LLMResolver
	ConfigFor ConfigResolver
	Events    chan event.Event
	Manual    bool // manual mode: `ask` blocks for a real user answer; automatic mode: `ask` returns "proceed"
	// Interactive keeps the orchestrator alive between turns: when it finishes a
	// turn with no tool calls it goes idle and WAITS for the next message (a user
	// instruction or a peer) instead of ending the run. The TUI sets this so the
	// user can keep talking to Main; headless callers leave it false so Run
	// returns the orchestrator's final result.
	Interactive bool

	// BashDeny are defense-in-depth deny patterns (from config `bash.patterns`):
	// a bash / bash_bg command matching any of them is blocked before it reaches
	// the sandbox. Build them with CompileDeny. Empty means no filtering.
	BashDeny []*regexp.Regexp

	// SeedHistory, when set, restores the orchestrator's prior conversation on a
	// resumed engagement: Run prepends it to Main's history so the model continues
	// with full context, before the new resume prompt. Empty = a clean start.
	SeedHistory []llm.Message

	// SummarizerLLM / SummarizerModel, when set, route context auto-compaction
	// through the cheaper small-tier model (config ModelRoles.Small) instead of each
	// agent's own (expensive) model. A big cost saving on long engagements, since
	// compaction is frequent and the summary quality a small model gives is
	// sufficient. Nil => compaction uses the agent's own model.
	SummarizerLLM   LLM
	SummarizerModel string

	// DiagramWidth returns the LIVE content width (in display columns) of the TUI's
	// Diagram pane, so update_diagram can reject a map whose lines would be clipped
	// and tell the orchestrator the exact current limit. It is read fresh on every
	// update_diagram call, so a pane resize takes effect immediately. Nil, or a value
	// <= 0, means "unknown / no limit" (e.g. headless runs) and the check is skipped.
	DiagramWidth func() int

	router    *Router
	baseCtx   context.Context  // engagement-lifetime context; subagents run under this, NOT a parent's per-turn context
	fileLocks *tools.FileLocks // cross-agent file-write locks
}

// AgentRecord is a persisted snapshot of one agent's conversation, used to save an
// engagement to the host and reload it on resume.
type AgentRecord struct {
	ID      core.AgentID
	Role    string
	Parent  core.AgentID
	State   core.State
	History []llm.Message
}

// Snapshot returns a copy of every live agent's conversation, safe to serialize.
func (s *Session) Snapshot() []AgentRecord { return s.router.snapshotAll() }

func (r *Router) snapshotAll() []AgentRecord {
	r.mu.Lock()
	ags := make([]*Agent, 0, len(r.agents))
	for _, a := range r.agents {
		ags = append(ags, a)
	}
	r.mu.Unlock()
	recs := make([]AgentRecord, 0, len(ags))
	for _, a := range ags {
		a.mu.Lock()
		h := append([]llm.Message(nil), a.history...)
		a.mu.Unlock()
		recs = append(recs, AgentRecord{ID: a.id, Role: a.cfg.Role, Parent: a.parent, State: a.sm.State(), History: h})
	}
	return recs
}

// CompileDeny turns fnmatch-style bash deny globs (e.g. "*/mnt/c*") into anchored
// regexps for Session.BashDeny. Each `*` matches any run of characters; invalid
// entries are skipped.
func CompileDeny(globs []string) []*regexp.Regexp {
	var out []*regexp.Regexp
	for _, g := range globs {
		if strings.TrimSpace(g) == "" {
			continue
		}
		var b strings.Builder
		b.WriteString("(?s)^")
		for _, r := range g {
			if r == '*' {
				b.WriteString(".*")
			} else {
				b.WriteString(regexp.QuoteMeta(string(r)))
			}
		}
		b.WriteString("$")
		if re, err := regexp.Compile(b.String()); err == nil {
			out = append(out, re)
		}
	}
	return out
}

// bashDenied reports whether a bash/bash_bg tool-call's command matches a deny
// pattern.
func (s *Session) bashDenied(argsJSON string) bool {
	if len(s.BashDeny) == 0 {
		return false
	}
	var a struct {
		Cmd string `json:"cmd"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &a); err != nil || a.Cmd == "" {
		return false
	}
	for _, re := range s.BashDeny {
		if re.MatchString(a.Cmd) {
			return true
		}
	}
	return false
}

// NewSession wires a session and its router.
func NewSession(reg *tools.Registry, sb sandbox.Handle, llmFor LLMResolver, cfgFor ConfigResolver, events chan event.Event, maxConcurrency int) *Session {
	if maxConcurrency <= 0 {
		maxConcurrency = 5
	}
	// maxConcurrency is the cap on the TOTAL number of agents running at once — the
	// orchestrator (Main) included — so the sandbox is never overloaded beyond what
	// the user configured. Main never takes a semaphore slot (it must never be
	// starved of its turn, e.g. to steer or kill an overloaded subagent), so we
	// reserve one slot for it and let the subagents share the rest. With the default
	// of 5 that means up to 4 subagents run concurrently alongside Main. A value
	// below 2 still leaves room for exactly one subagent.
	subSlots := maxConcurrency - 1
	if subSlots < 1 {
		subSlots = 1
	}
	s := &Session{Registry: reg, Sandbox: sb, LLMFor: llmFor, ConfigFor: cfgFor, Events: events, fileLocks: tools.NewFileLocks()}
	s.router = &Router{agents: map[core.AgentID]*Agent{}, sem: make(chan struct{}, subSlots), counts: map[string]int{}, sess: s}
	return s
}

// Router returns the session's router (for TUI control: Pause/Resume/Kill).
func (s *Session) Router() *Router { return s.router }

func (s *Session) emit(ev event.Event) {
	if s.Events == nil {
		return
	}
	select {
	case s.Events <- ev:
		return
	default:
	}
	// Channel full. High-frequency STREAM deltas are safe to drop (they are
	// cosmetic and superseded by later output), so never block the engine on a
	// slow UI for those. LIFECYCLE events are authoritative state, though — a
	// dropped AgentState would strand the roster on a stale state (e.g. showing
	// "running" forever after an agent went idle). Deliver those reliably,
	// bounded by the engagement context so quitting never deadlocks.
	if !mustDeliver(ev) {
		return
	}
	base := s.baseCtx
	if base == nil {
		s.Events <- ev // no engagement context (tests): the reader always drains
		return
	}
	select {
	case s.Events <- ev:
	case <-base.Done():
	}
}

// emitAsync delivers a lifecycle event WITHOUT ever blocking the caller: a
// non-blocking send first, and if the channel is full, delivery continues from a
// goroutine (bounded by the engagement context) instead of blocking. Control
// operations (Pause/Resume/Kill) use this because they are invoked from the TUI's
// own goroutine — the same goroutine that DRAINS Events — so a blocking mustDeliver
// send from there would deadlock if the channel were full (the drainer is stuck in
// the send and so never drains). The engine's own Run goroutines keep using the
// blocking emit, which is safe because Session.Run waits for them before returning.
func (s *Session) emitAsync(ev event.Event) {
	if s.Events == nil {
		return
	}
	select {
	case s.Events <- ev:
		return
	default:
	}
	base := s.baseCtx
	go func() {
		if base == nil {
			s.Events <- ev
			return
		}
		select {
		case s.Events <- ev:
		case <-base.Done():
		}
	}()
}

// mustDeliver reports whether an event is authoritative lifecycle state that
// must not be dropped (as opposed to droppable streaming output).
func mustDeliver(ev event.Event) bool {
	switch ev.(type) {
	case event.AgentState, event.Spawned, event.UserQuestion:
		return true
	default:
		return false
	}
}

// Run executes the orchestrator with the user's initial prompt and returns its
// result. Subagents spawned during the run live on the engagement context, so
// they are unaffected by the orchestrator's per-turn cancellations; they are
// torn down only when this context ends.
func (s *Session) Run(ctx context.Context, orch AgentConfig, userPrompt string) Result {
	client, modelID, err := s.LLMFor(orch.Model)
	if err != nil {
		return Result{Agent: core.Main, Err: err}
	}
	baseCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	s.baseCtx = baseCtx

	s.router.wg.Add(1)
	go s.router.budgetMonitor(baseCtx) // engine-enforced per-subagent time budgets

	a := newAgent(core.Main, orch, client, modelID, s, "", userPrompt)
	if len(s.SeedHistory) > 0 {
		// Resume: restore the prior conversation, then let the new resume prompt
		// (already appended by newAgent as the trailing user turn) follow it.
		a.history = append(append([]llm.Message(nil), s.SeedHistory...), a.history...)
		a.tokensEst = estimateTokens(orch.System, a.history) // recount after prepending the restored history
	}
	s.router.register(a)
	defer s.router.unregister(core.Main)
	res := a.Run(baseCtx)
	// The orchestrator has finished. Cancel the engagement context and wait for
	// every background goroutine that can emit (subagents, the budget monitor,
	// background jobs) to exit before returning, so the caller can safely stop
	// reading / close the event channel without racing a late emit.
	cancel()
	s.router.wg.Wait()
	return res
}

// Router routes envelopes and owns the concurrency semaphore and control ops.
type Router struct {
	mu     sync.Mutex
	agents map[core.AgentID]*Agent
	sem    chan struct{}
	counts map[string]int
	sess   *Session
	// wg tracks every background goroutine that can emit events (subagents, the
	// budget monitor, background bash jobs). Session.Run waits on it after the
	// orchestrator returns, so once Run returns nothing emits — the caller can then
	// close the event channel without racing a late emit.
	wg sync.WaitGroup
}

func (r *Router) register(a *Agent) {
	r.mu.Lock()
	r.agents[a.id] = a
	r.mu.Unlock()
}

func (r *Router) unregister(id core.AgentID) {
	r.mu.Lock()
	delete(r.agents, id)
	r.mu.Unlock()
}

func (r *Router) get(id core.AgentID) (*Agent, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if a, ok := r.agents[id]; ok {
		return a, true
	}
	// Fall back to a case-insensitive match so a model that types an id in a
	// different case (e.g. "recon#1" vs the canonical "Recon#1") still reaches it.
	for aid, a := range r.agents {
		if strings.EqualFold(string(aid), string(id)) {
			return a, true
		}
	}
	return nil, false
}

func (r *Router) newID(role string) core.AgentID {
	r.mu.Lock()
	defer r.mu.Unlock()
	// Canonicalize so every subagent id starts with a capital letter ("Recon#1"),
	// and count per canonical role regardless of the case the caller used.
	role = core.CanonRole(role)
	r.counts[role]++
	return core.AgentID(fmt.Sprintf("%s#%d", role, r.counts[role]))
}

// send delivers an envelope to the target's mailbox. Returns false if unknown.
func (r *Router) send(env Envelope) bool {
	a, ok := r.get(env.To)
	if !ok {
		return false
	}
	select {
	case a.box <- env:
		a.wake(env)
		return true
	default:
	}
	// Mailbox full (rare — mailboxes are generously buffered): deliver from a
	// goroutine rather than drop, but don't leak it forever if the target has
	// stopped draining (killed/finished). Give up when the engagement ends.
	base := r.sess.baseCtx
	go func() {
		if base == nil {
			a.box <- env
			a.wake(env)
			return
		}
		select {
		case a.box <- env:
			a.wake(env)
		case <-base.Done():
		}
	}()
	return true
}

// wake nudges an agent that is blocking on a foreground command so it stops to
// read a newly-arrived user/peer message (the message itself is already in the
// mailbox; this just interrupts the wait). Only user/peer messages wake it — a
// subagent result or a background-job notice does not interrupt a running command.
func (a *Agent) wake(env Envelope) {
	if env.Kind != MsgSteer && env.Kind != MsgUserAnswer && env.Kind != MsgPeer {
		return
	}
	select {
	case a.nudge <- struct{}{}:
	default:
	}
}

// Send is the public steer/peer entry point (used by the TUI and peers).
func (r *Router) Send(env Envelope) bool { return r.send(env) }

// spawn creates, registers, and starts a child agent; its result posts back to
// the parent's mailbox. Blocks on the concurrency semaphore when full. The child
// runs under the engagement context (not the caller's per-turn context), so the
// parent finishing its turn never cancels the child.
func (r *Router) spawn(parent core.AgentID, role, prompt string) (core.AgentID, error) {
	cfg, ok := r.sess.ConfigFor(role)
	if !ok {
		return "", fmt.Errorf("unknown agent role %q", role)
	}
	base := r.sess.baseCtx
	if base == nil {
		base = context.Background()
	}
	select {
	case r.sem <- struct{}{}:
	case <-base.Done():
		return "", base.Err()
	}
	client, modelID, err := r.sess.LLMFor(cfg.Model)
	if err != nil {
		<-r.sem
		return "", err
	}
	id := r.newID(role)
	child := newAgent(id, cfg, client, modelID, r.sess, parent, prompt)
	child.holdsSem = true // spawn acquired the slot above; the agent holds it while running
	r.register(child)
	r.sess.emit(event.Spawned{Parent: parent, Child: id, Role: core.CanonRole(role), Prompt: prompt})
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		res := child.Run(base)
		r.unregister(id)
		child.releaseSem() // release the slot if still held (idle agents already released it)
		// The roster must never strand this agent as "running" once its loop has
		// ended. A killed agent already emitted Killed; for any other terminal exit
		// (e.g. a stream error) emit a final Idle so the roster reflects that it
		// stopped. Skip this once the engagement context is done — the whole UI is
		// tearing down then, and the event channel may already be closed.
		if !res.Killed && base.Err() == nil {
			r.sess.emit(event.AgentState{Agent: id, State: core.Idle})
		}
		// A persistent subagent posts its per-cycle result from Run when it goes
		// idle; here we only post a FINAL result on a terminal exit (killed/errored)
		// so a parent blocked in `wait` still unblocks.
		if res.Killed || res.Err != nil {
			r.send(Envelope{From: id, To: parent, Kind: MsgResult, Body: resultBody(res)})
		}
	}()
	return id, nil
}

func resultBody(res Result) string {
	switch {
	case res.Killed:
		return "[killed]"
	case res.Err != nil:
		return "[error] " + res.Err.Error()
	default:
		return res.Text
	}
}

// Control operations (safe from any goroutine; take effect immediately).

func (r *Router) Pause(id core.AgentID) {
	if a, ok := r.get(id); ok {
		a.Pause()
	}
}
func (r *Router) Resume(id core.AgentID) {
	if a, ok := r.get(id); ok {
		a.Resume()
	}
}
func (r *Router) Kill(id core.AgentID) {
	if a, ok := r.get(id); ok {
		a.Kill()
	}
}

// StopCommands cancels the commands an agent is currently running (foreground +
// background) without pausing or killing it. Returns how many were stopped.
func (r *Router) StopCommands(id core.AgentID) int {
	if a, ok := r.get(id); ok {
		return a.StopCommands()
	}
	return 0
}
func (r *Router) PauseAll() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, a := range r.agents {
		a.Pause()
	}
}
func (r *Router) ResumeAll() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, a := range r.agents {
		a.Resume()
	}
}

// SetBudget changes (or removes, with d<=0) a live agent's time budget. Removing
// the budget of a still-running agent notifies it that the limit was lifted.
func (r *Router) SetBudget(id core.AgentID, d time.Duration) {
	if a, ok := r.get(id); ok {
		a.setBudget(d)
	}
}

// Budget returns an agent's current time budget (0 = no limit) and whether the
// agent is known (for the TUI roster).
func (r *Router) Budget(id core.AgentID) (time.Duration, bool) {
	a, ok := r.get(id)
	if !ok {
		return 0, false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.budget, true
}

// Remaining returns how much of an agent's time budget is left (budget minus the
// time it has been running), and whether a budget applies at all. The TUI shows a
// live countdown from it. Not meaningful while paused, so the caller shows it only
// for a running agent.
func (r *Router) Remaining(id core.AgentID) (time.Duration, bool) {
	a, ok := r.get(id)
	if !ok {
		return 0, false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.budget <= 0 {
		return 0, false
	}
	rem := a.budget - time.Since(a.budgetFrom)
	if rem < 0 {
		rem = 0
	}
	return rem, true
}

// ContextUsage returns the percentage (0-100) of an agent's context-token ceiling
// its conversation currently occupies, and whether that is known (the agent is
// live and has a ceiling). The TUI shows it per agent.
func (r *Router) ContextUsage(id core.AgentID) (int, bool) {
	a, ok := r.get(id)
	if !ok {
		return 0, false
	}
	ceil := a.cfg.MaxContextTokens
	if ceil <= 0 {
		return 0, false
	}
	a.mu.Lock()
	used := a.tokensEst // maintained incrementally; O(1) instead of re-summing the history
	a.mu.Unlock()
	pct := used * 100 / ceil
	if pct > 100 {
		pct = 100
	}
	return pct, true
}

// budgetMonitor periodically enforces per-subagent time budgets until ctx ends.
func (r *Router) budgetMonitor(ctx context.Context) {
	defer r.wg.Done()
	t := time.NewTicker(budgetTick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			r.mu.Lock()
			agents := make([]*Agent, 0, len(r.agents))
			for _, a := range r.agents {
				agents = append(agents, a)
			}
			r.mu.Unlock()
			for _, a := range agents {
				a.checkBudget()
			}
		}
	}
}

// State returns an agent's current lifecycle state (for the TUI roster).
func (r *Router) State(id core.AgentID) (core.State, bool) {
	a, ok := r.get(id)
	if !ok {
		return 0, false
	}
	return a.sm.State(), true
}

// Live returns the ids of all currently registered agents.
func (r *Router) Live() []core.AgentID {
	r.mu.Lock()
	defer r.mu.Unlock()
	ids := make([]core.AgentID, 0, len(r.agents))
	for id := range r.agents {
		ids = append(ids, id)
	}
	return ids
}

// Agent is one actor: a goroutine running the agent loop with a mailbox.
type Agent struct {
	id      core.AgentID
	cfg     AgentConfig
	llm     LLM
	modelID string
	s       *Session
	box     chan Envelope
	nudge   chan struct{} // signals "a user/peer message arrived" so a blocking foreground command can wake
	sm      *stateMachine
	started time.Time

	parent   core.AgentID // who spawned this agent ("" for the orchestrator / a directly-run agent)
	holdsSem bool         // whether this spawned agent currently holds a concurrency slot

	mu         sync.Mutex
	stepCancel context.CancelFunc
	history    []llm.Message
	lastThink  string         // latest turn's reasoning/thoughts (for the inspect tool)
	lastText   string         // last non-empty assistant text this cycle (result fallback)
	jobSeq     int            // id counter for commands / background jobs
	budget     time.Duration  // engine time budget; 0 = no limit
	budgetFrom time.Time      // when the CURRENT budget window started (reset on a manual change / reprompt)
	budgetSent int            // reminders already sent (reset when the budget changes)
	pausedAt   time.Time      // when the agent was paused (to exclude paused time from the budget)
	fgID       int            // id of the current foreground command (0 = none)
	fgJob      *bgJob         // the current foreground command's job (nil = none)
	jobs       map[int]*bgJob // backgrounded jobs, by id
	// tokensEst is a running estimate of the agent's context size (system + history)
	// in tokens, maintained incrementally so ContextUsage is O(1) instead of
	// re-summing the whole history on every roster render.
	tokensEst int
}

func newAgent(id core.AgentID, cfg AgentConfig, client LLM, modelID string, s *Session, parent core.AgentID, seed string) *Agent {
	a := &Agent{
		id:      id,
		cfg:     cfg,
		llm:     client,
		modelID: modelID,
		s:       s,
		parent:  parent,
		box:     make(chan Envelope, 64),
		nudge:   make(chan struct{}, 1),
		sm:      newStateMachine(),
		started: time.Now(),
		budget:  time.Duration(cfg.BudgetMinutes) * time.Minute,
		jobs:    map[int]*bgJob{},
	}
	a.budgetFrom = a.started
	if seed != "" {
		a.history = append(a.history, llm.Message{Role: llm.RoleUser, Content: seed})
	}
	a.tokensEst = estimateTokens(cfg.System, a.history)
	return a
}

func (a *Agent) setCancel(c context.CancelFunc) {
	a.mu.Lock()
	a.stepCancel = c
	a.mu.Unlock()
}

// appendHistory adds a message under the lock, so a concurrent reader (the
// inspect tool, or a persistence snapshot) never races the agent's own goroutine.
func (a *Agent) appendHistory(m llm.Message) {
	a.mu.Lock()
	a.history = append(a.history, m)
	a.tokensEst += msgTokens(m) // keep the context-size estimate current (cheap, incremental)
	a.mu.Unlock()
}

// historySnapshot returns a copy of the conversation, safe to read from another
// goroutine.
func (a *Agent) historySnapshot() []llm.Message {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]llm.Message(nil), a.history...)
}

// inspectText renders a readable snapshot of this agent for the inspect tool:
// lifecycle state, timing, latest thoughts, and recent conversation. It is safe
// to call from another agent's goroutine (the orchestrator).
func (a *Agent) inspectText() string {
	a.mu.Lock()
	hist := append([]llm.Message(nil), a.history...)
	think := a.lastThink
	started := a.started
	budget := a.budget
	a.mu.Unlock()
	st := a.sm.State()

	var b strings.Builder
	fmt.Fprintf(&b, "agent %s — role %s, state %s, running %s", a.id, core.CanonRole(a.cfg.Role), st, time.Since(started).Round(time.Second))
	if budget > 0 {
		fmt.Fprintf(&b, ", time budget %s", budget)
	}
	b.WriteByte('\n')
	if strings.TrimSpace(think) != "" {
		fmt.Fprintf(&b, "latest thoughts: %s\n", clip(oneLine(think), 800))
	}
	// Recent conversation, newest last. Cap the message count and each line's
	// length so a long history can't flood the caller's context window.
	const maxMsgs = 24
	start := 0
	if len(hist) > maxMsgs {
		start = len(hist) - maxMsgs
		fmt.Fprintf(&b, "history (last %d of %d messages):\n", maxMsgs, len(hist))
	} else {
		fmt.Fprintf(&b, "history (%d messages):\n", len(hist))
	}
	for _, m := range hist[start:] {
		switch m.Role {
		case llm.RoleUser:
			fmt.Fprintf(&b, "  [in] %s\n", clip(oneLine(m.Content), 240))
		case llm.RoleAssistant:
			if txt := strings.TrimSpace(m.Content); txt != "" {
				fmt.Fprintf(&b, "  [assistant] %s\n", clip(oneLine(txt), 320))
			}
			for _, c := range m.ToolCalls {
				fmt.Fprintf(&b, "  [tool-call] %s %s\n", c.Name, clip(oneLine(c.Arguments), 200))
			}
		case llm.RoleTool:
			fmt.Fprintf(&b, "  [tool-result] %s\n", clip(oneLine(m.Content), 240))
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// oneLine collapses whitespace/newlines to single spaces.
func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

// clip truncates s to at most n runes, appending an ellipsis when cut.
func clip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

func (a *Agent) cancelStep() {
	a.mu.Lock()
	c := a.stepCancel
	// Pausing/aborting the step also ceases the in-flight FOREGROUND command (a
	// backgrounded command/job keeps running — stop it with StopCommands instead).
	// Cancelling the job's context stops the command and its children.
	fg := a.fgJob
	a.fgJob, a.fgID = nil, 0
	a.mu.Unlock()
	if c != nil {
		c()
	}
	if fg != nil {
		fg.cancel()
	}
}

// StopCommands cancels every command this agent is running right now — the
// foreground command and any backgrounded commands/jobs — WITHOUT pausing or
// killing the agent. The agent gets each command's (aborted) result and carries
// on. This is the user's "stop what this agent is running" control.
func (a *Agent) StopCommands() int {
	a.mu.Lock()
	var jobs []*bgJob
	if a.fgJob != nil {
		jobs = append(jobs, a.fgJob)
		a.fgJob, a.fgID = nil, 0
	}
	for id, j := range a.jobs {
		jobs = append(jobs, j)
		delete(a.jobs, id)
	}
	a.mu.Unlock()
	// Cancel each job's context: that aborts its Exec, which kills the command and
	// the whole process tree it spawned (server + children).
	for _, j := range jobs {
		j.cancel()
	}
	return len(jobs)
}

// releaseSem gives up the concurrency slot if this agent holds one.
func (a *Agent) releaseSem() {
	a.mu.Lock()
	held := a.holdsSem
	a.holdsSem = false
	a.mu.Unlock()
	if held {
		<-a.s.router.sem
	}
}

// acquireSem blocks until a concurrency slot is free, the engagement ends, or the
// agent is killed. Watching the kill signal matters: without it, an agent killed
// while waiting for a slot would stay blocked here until some unrelated slot frees.
func (a *Agent) acquireSem(ctx context.Context) bool {
	for {
		if a.sm.State() == core.Killed {
			return false
		}
		_, killOrResume := a.sm.snapshot() // closed on resume/kill
		select {
		case a.s.router.sem <- struct{}{}:
			a.mu.Lock()
			a.holdsSem = true
			a.mu.Unlock()
			return true
		case <-ctx.Done():
			return false
		case <-killOrResume:
			// State changed (e.g. killed): loop and re-check at the top.
		}
	}
}

func (a *Agent) emitState(st core.State) { a.s.emit(event.AgentState{Agent: a.id, State: st}) }

// emitStateAsync is emitState for the control operations (Pause/Resume/Kill), which
// run on the TUI goroutine; it never blocks the caller (see Session.emitAsync).
func (a *Agent) emitStateAsync(st core.State) {
	a.s.emitAsync(event.AgentState{Agent: a.id, State: st})
}

// finalBody chooses a subagent's result body: the final turn's text, else the last
// substantive text it produced this cycle, else a clear marker — so the
// orchestrator never receives an empty result.
func (a *Agent) finalBody(text string) string {
	if strings.TrimSpace(text) != "" {
		return text
	}
	a.mu.Lock()
	lt := a.lastText
	a.mu.Unlock()
	if strings.TrimSpace(lt) != "" {
		return lt
	}
	return "[finished with no summary]"
}

// setBudget changes this agent's time budget (d<=0 removes it) and resets the
// reminder schedule. If the limit is removed while the agent is still running,
// the agent is told the user lifted its time limit.
func (a *Agent) setBudget(d time.Duration) {
	if d < 0 {
		d = 0
	}
	a.mu.Lock()
	had := a.budget > 0
	a.budget = d
	a.budgetSent = 0
	a.budgetFrom = time.Now() // a manual change means "N minutes from now"
	a.mu.Unlock()

	running := a.sm.State() == core.Running || a.sm.State() == core.Paused
	switch {
	case d == 0 && had && running:
		a.s.router.send(Envelope{From: systemSender, To: a.id, Kind: MsgBudget,
			Body: "The user has removed your time limit — you may take the time you need. Still work efficiently and return as soon as the task is genuinely done."})
		a.s.emit(event.Notice{Agent: a.id, Level: "info", Text: "time budget removed by user"})
	case d > 0 && running:
		a.s.emit(event.Notice{Agent: a.id, Level: "info", Text: fmt.Sprintf("time budget set to %dm by user", int(d.Minutes()))})
	}
}

// checkBudget is called by the monitor each tick: it sends escalating reminders
// once the agent runs past its budget, and terminates it if it keeps running.
func (a *Agent) checkBudget() {
	if a.cfg.IsOrchestrator {
		return // the orchestrator is interactive and never time-limited
	}
	if st := a.sm.State(); st != core.Running {
		return // only an actively-running subagent is on the clock
	}
	a.mu.Lock()
	budget, sent, from := a.budget, a.budgetSent, a.budgetFrom
	a.mu.Unlock()
	if budget <= 0 {
		return // no limit
	}
	elapsed := time.Since(from)
	switch {
	case sent == 0 && elapsed >= budget:
		a.sendBudgetReminder(1, budget)
	case sent == 1 && elapsed >= budget+budgetReminderGap:
		a.sendBudgetReminder(2, budget)
	case sent == 2 && elapsed >= budget+2*budgetReminderGap:
		a.sendBudgetReminder(3, budget)
	case sent >= 3 && elapsed >= budget+2*budgetReminderGap+budgetKillGrace:
		a.s.emit(event.Notice{Agent: a.id, Level: "error",
			Text: fmt.Sprintf("terminated by oline — ran past its %dm time budget (after 3 reminders)", int(budget.Minutes()))})
		a.Kill()
	}
}

// sendBudgetReminder delivers reminder n (1..3) to the agent and surfaces it.
func (a *Agent) sendBudgetReminder(n int, budget time.Duration) {
	a.mu.Lock()
	if a.budgetSent >= n {
		a.mu.Unlock()
		return
	}
	a.budgetSent = n
	a.mu.Unlock()

	mins := int(budget.Minutes())
	var body string
	switch n {
	case 1:
		body = fmt.Sprintf("TIME BUDGET (reminder 1 of 3): you have reached your %d-minute time budget. Wrap up now — finish your current step and return your results and follow-up ideas; do not start new work.", mins)
	case 2:
		body = fmt.Sprintf("TIME BUDGET (reminder 2 of 3): you are over your %d-minute budget. Stop and return what you have now.", mins)
	default:
		body = fmt.Sprintf("TIME BUDGET (FINAL reminder 3 of 3): you are well past your %d-minute budget. Return your current results immediately — oline will terminate you in %d minutes if you keep running.", mins, int(budgetKillGrace.Minutes()))
	}
	a.s.router.send(Envelope{From: systemSender, To: a.id, Kind: MsgBudget, Body: body})
	a.s.emit(event.Notice{Agent: a.id, Level: "warn", Text: fmt.Sprintf("time-budget reminder %d/3 (past %dm)", n, mins)})
}

// Pause ceases the agent immediately: cancel the in-flight step (LLM stream +
// any running sandbox command) and block the loop until Resume.
func (a *Agent) Pause() {
	if a.sm.pause() {
		a.mu.Lock()
		a.pausedAt = time.Now()
		a.mu.Unlock()
		a.cancelStep()
		a.emitStateAsync(core.Paused)
	}
}

func (a *Agent) Resume() {
	if a.sm.resumeNow() {
		// Exclude the paused interval from the time budget by pushing `started`
		// forward, so a long pause doesn't instantly exhaust the budget on resume.
		a.mu.Lock()
		if !a.pausedAt.IsZero() {
			paused := time.Since(a.pausedAt)
			a.started = a.started.Add(paused)
			a.budgetFrom = a.budgetFrom.Add(paused) // paused time doesn't count against the budget
			a.pausedAt = time.Time{}
		}
		a.mu.Unlock()
		a.emitStateAsync(core.Running)
	}
}

func (a *Agent) Kill() {
	a.sm.kill()
	a.cancelStep()
	a.emitStateAsync(core.Killed)
}

// gate blocks while paused. Returns true if the agent has been killed.
func (a *Agent) gate() (killed bool) {
	for {
		st, ch := a.sm.snapshot()
		switch st {
		case core.Killed:
			return true
		case core.Paused:
			select {
			case <-ch: // resumed or killed
			case env := <-a.box: // accept steering while paused
				a.foldEnvelope(env)
			}
		default:
			return false
		}
	}
}

// Run is the agent loop. It returns the agent's terminal Result.
func (a *Agent) Run(parent context.Context) Result {
	a.emitState(core.Running)
	// There is no turn cap: an agent runs until it finishes, is killed, the
	// engagement context ends, or (for a subagent) its engine-enforced TIME budget
	// terminates it. The only limits are time and lifecycle, never a turn count.
	for {
		// Exit cleanly if the engagement/parent context is done (timeout/abort).
		if parent.Err() != nil {
			return Result{Agent: a.id, Err: parent.Err(), Text: "[aborted]"}
		}
		if a.gate() {
			a.emitState(core.Killed)
			return Result{Agent: a.id, Killed: true}
		}
		a.drainMailbox()
		a.compactIfNeeded(parent) // keep the conversation under this agent's context ceiling

		stepCtx, cancel := context.WithCancel(parent)
		a.setCancel(cancel)

		// Drop any stale nudge so only a message arriving DURING this turn interrupts it.
		select {
		case <-a.nudge:
		default:
		}

		// Each LLM turn gets its own timeout so a single hung stream can't block
		// forever; tool calls below use stepCtx (no per-turn deadline). The stream
		// runs in a goroutine so a user/peer message can INTERRUPT it mid-thought:
		// the agent stops thinking, the loop folds the message (drainMailbox at the
		// top), and the turn is re-issued with the message in context.
		streamCtx, streamCancel := context.WithTimeout(stepCtx, llmTurnTimeout)
		type streamRes struct {
			text  string
			frags []llm.ToolCallDelta
			err   error
		}
		sc := make(chan streamRes, 1)
		go func() {
			t, f, e := a.stream(streamCtx)
			sc <- streamRes{t, f, e}
		}()
		var text string
		var frags []llm.ToolCallDelta
		var serr error
		select {
		case r := <-sc:
			text, frags, serr = r.text, r.frags, r.err
			streamCancel()
		case <-a.nudge:
			streamCancel() // stop thinking
			cancel()
			<-sc // let the stream goroutine unwind
			a.s.emit(event.Notice{Agent: a.id, Level: "info", Text: string(a.id) + ": stopped to read a new message"})
			continue // re-issue the turn; drainMailbox at the top folds the message in
		}

		if strings.TrimSpace(text) != "" { // remember the last substantive output for the result fallback
			a.mu.Lock()
			a.lastText = text
			a.mu.Unlock()
		}

		switch a.sm.State() {
		case core.Killed:
			cancel()
			a.emitState(core.Killed)
			return Result{Agent: a.id, Killed: true}
		case core.Paused:
			cancel() // discard the partial step; re-issue after resume
			continue
		}
		if parent.Err() != nil {
			cancel()
			return Result{Agent: a.id, Err: parent.Err(), Text: "[aborted]"}
		}
		if serr != nil {
			cancel()
			a.s.emit(event.Notice{Level: "error", Text: fmt.Sprintf("%s: %v", a.id, serr)})
			return Result{Agent: a.id, Err: serr, Text: text}
		}

		calls := llm.AssembleToolCalls(frags)
		a.appendHistory(assistantMessage(text, calls))
		if len(calls) == 0 {
			cancel()
			a.sm.setIdle()
			a.emitState(core.Idle)
			switch {
			case a.cfg.IsOrchestrator && a.s.Interactive:
				// The orchestrator stays alive between turns: wait for the next
				// message (user or peer) and continue rather than ending the run.
				if !a.awaitInput(parent) {
					return Result{Agent: a.id, Killed: a.sm.State() == core.Killed, Text: text}
				}
				a.sm.setRunning()
				a.emitState(core.Running)
				continue
			case a.parent != "":
				// A spawned subagent delivers this cycle's result to its parent, then
				// idles — RELEASING its concurrency slot — and waits to be repromted
				// (so the orchestrator can restart / re-task a finished agent with its
				// context intact). It re-acquires a slot before running again.
				// If the final turn had no text, fall back to the last substantive
				// output so the orchestrator never receives an empty result.
				a.s.router.send(Envelope{From: a.id, To: a.parent, Kind: MsgResult, Body: a.finalBody(text)})
				a.releaseSem()
				if !a.awaitInput(parent) {
					return Result{Agent: a.id, Killed: a.sm.State() == core.Killed, Text: text}
				}
				if !a.acquireSem(parent) {
					return Result{Agent: a.id, Text: text} // engagement ending
				}
				// A reprompt is a new cycle: give it a fresh time budget rather than
				// counting the idle wait against it.
				a.mu.Lock()
				a.started = time.Now()
				a.budgetFrom = a.started
				a.budgetSent = 0
				a.lastText = "" // a new cycle: don't carry the prior result's text forward
				a.mu.Unlock()
				a.sm.setRunning()
				a.emitState(core.Running)
				continue
			default:
				// A directly-run, non-interactive agent (e.g. an isolated test) just
				// returns its result.
				return Result{Agent: a.id, Text: text}
			}
		}

		for i, tc := range calls {
			if a.gate() {
				cancel()
				a.emitState(core.Killed)
				return Result{Agent: a.id, Killed: true}
			}
			content, _ := a.dispatch(stepCtx, tc)
			a.appendHistory(toolMessage(tc, content))
			if st := a.sm.State(); st == core.Killed || st == core.Paused || parent.Err() != nil {
				// Stopping mid-batch leaves the assistant's remaining tool_calls
				// unanswered; the provider rejects a tool_call with no matching result
				// on the next (resumed) request. Pair every remaining call with an
				// aborted result so the history stays well-formed.
				for _, rem := range calls[i+1:] {
					a.appendHistory(toolMessage(rem, "[aborted: agent paused or stopped before this tool ran]"))
				}
				break
			}
		}
		cancel()
	}
}

// stream runs one LLM turn and returns the assistant text, tool-call fragments,
// and any stream error.
func (a *Agent) stream(ctx context.Context) (string, []llm.ToolCallDelta, error) {
	ch := make(chan llm.Delta, 128)
	errc := make(chan error, 1)
	go func() { errc <- a.llm.Stream(ctx, a.buildReq(), ch) }()

	var text strings.Builder
	var reason strings.Builder
	var frags []llm.ToolCallDelta
	for d := range ch {
		switch {
		case d.TextDelta != "":
			text.WriteString(d.TextDelta)
			a.s.emit(event.TokenDelta{Agent: a.id, Text: d.TextDelta})
		case d.ReasoningDelta != "":
			reason.WriteString(d.ReasoningDelta)
			a.s.emit(event.TokenDelta{Agent: a.id, Reasoning: d.ReasoningDelta})
		case d.ToolCall != nil:
			frags = append(frags, *d.ToolCall)
		case d.Usage != nil:
			a.s.emit(event.Usage{Agent: a.id, Model: a.modelID,
				PromptTokens: d.Usage.PromptTokens, CompletionTokens: d.Usage.CompletionTokens,
				CachedPromptTokens: d.Usage.CachedPromptTokens})
		}
	}
	if reason.Len() > 0 { // remember the latest turn's thoughts for the inspect tool
		a.mu.Lock()
		a.lastThink = reason.String()
		a.mu.Unlock()
	}
	return text.String(), frags, <-errc
}

func (a *Agent) buildReq() llm.ChatReq {
	msgs := make([]llm.Message, 0, len(a.history)+1)
	if a.cfg.System != "" {
		msgs = append(msgs, llm.Message{Role: llm.RoleSystem, Content: a.cfg.System})
	}
	msgs = append(msgs, a.history...)
	maxTokens := a.cfg.MaxTokens
	if maxTokens <= 0 {
		maxTokens = defaultMaxTokens
	}
	return llm.ChatReq{
		Model:           a.modelID,
		Messages:        msgs,
		Tools:           a.toolDefs(),
		ReasoningEffort: a.cfg.Effort,
		MaxTokens:       maxTokens,
	}
}

// toolDefs advertises only the agent's allow-listed tools (engine primitives +
// registry tools). This is the per-agent scoping enforcement point.
func (a *Agent) toolDefs() []llm.FunctionDef {
	var defs []llm.FunctionDef
	for _, name := range a.cfg.Tools {
		if d, ok := primitiveSchemas[name]; ok {
			defs = append(defs, d)
			continue
		}
		if t, ok := a.s.Registry.Get(name); ok {
			sc := t.Schema()
			defs = append(defs, llm.FunctionDef{Name: sc.Name, Description: sc.Description, Parameters: sc.Parameters})
		}
	}
	return defs
}

// dispatch executes one tool call: engine primitives directly, everything else
// via the registry (after re-checking the allow-list).
func (a *Agent) dispatch(ctx context.Context, tc llm.ToolCall) (content string, isErr bool) {
	// Defense-in-depth: block bash / bash_bg commands matching a configured deny
	// pattern (host /mnt access, Windows-interop binaries, …) before they reach
	// the sandbox. The sandbox is the hard boundary; this is a secondary guard.
	if tc.Name == "bash" || tc.Name == "bash_bg" {
		if a.s.bashDenied(tc.Arguments) {
			a.s.emit(event.Notice{Agent: a.id, Level: "warn", Text: string(a.id) + ": blocked a bash command by deny policy"})
			return "blocked by policy: this command matches a bash deny pattern (host /mnt access and Windows-interop binaries are off-limits). Work inside the sandbox workspace instead.", true
		}
	}

	switch tc.Name {
	case "task":
		return a.doTask(ctx, tc)
	case "wait":
		return a.doWait(ctx)
	case "reprompt":
		return a.doReprompt(tc)
	case "bash":
		// bash runs in the engine (not the registry) so it can enforce the
		// foreground timeout + auto-background and be stoppable. Gated by the
		// per-agent allow-list, like any exec tool.
		if !allowed(a.cfg.Tools, "bash") {
			return fmt.Sprintf("tool %q is not available to %s", tc.Name, a.id), true
		}
		return a.doBash(tc)
	case "bash_bg":
		// bash_bg is a sandbox-exec primitive, so it must be gated by the same
		// per-agent allow-list as bash (wire grants bash_bg only to agents that
		// already have bash). The pure coordination primitives above are always
		// available; an exec primitive is not.
		if !allowed(a.cfg.Tools, "bash_bg") {
			return fmt.Sprintf("tool %q is not available to %s", tc.Name, a.id), true
		}
		return a.doBashBg(tc)
	case "runtime":
		return a.doRuntime(), false
	case "heartbeat":
		return a.doHeartbeat(ctx, tc)
	case "send_message":
		return a.doSendMessage(tc)
	case "inspect":
		// Orchestrator-only: gate by the allow-list (only Main is granted it).
		if !allowed(a.cfg.Tools, "inspect") {
			return fmt.Sprintf("tool %q is not available to %s", tc.Name, a.id), true
		}
		return a.doInspect(tc)
	case "ask":
		return a.doAsk(ctx, tc)
	case "record_finding":
		return a.doRecordFinding(tc)
	case "update_summary":
		// Orchestrator-only (only Main is granted it), like inspect.
		if !allowed(a.cfg.Tools, "update_summary") {
			return fmt.Sprintf("tool %q is not available to %s", tc.Name, a.id), true
		}
		return a.doUpdateSummary(tc)
	case "update_diagram":
		// Orchestrator-only (only Main is granted it), like update_summary.
		if !allowed(a.cfg.Tools, "update_diagram") {
			return fmt.Sprintf("tool %q is not available to %s", tc.Name, a.id), true
		}
		return a.doUpdateDiagram(tc)
	case "resources":
		// Orchestrator-only: a read-only sandbox load snapshot for throttling.
		if !allowed(a.cfg.Tools, "resources") {
			return fmt.Sprintf("tool %q is not available to %s", tc.Name, a.id), true
		}
		return a.doResources(ctx)
	}

	if !allowed(a.cfg.Tools, tc.Name) {
		return fmt.Sprintf("tool %q is not available to %s", tc.Name, a.id), true
	}
	t, ok := a.s.Registry.Get(tc.Name)
	if !ok {
		return fmt.Sprintf("unknown tool %q", tc.Name), true
	}
	a.s.emit(event.ToolStarted{Agent: a.id, Tool: tc.Name, ArgsPreview: preview(tc.Arguments), Args: tc.Arguments})
	res, err := t.Execute(ctx, tools.Call{
		Agent:   a.id,
		Args:    json.RawMessage(tc.Arguments),
		Sandbox: a.s.Sandbox,
		Policy:  a.cfg.Policy,
		Emit:    func(r tools.Result) { a.s.emit(event.ToolOutput{Agent: a.id, Chunk: r.Content}) },
		Locks:   a.s.fileLocks,
	})
	if err != nil {
		return fmt.Sprintf("tool %q failed: %v", tc.Name, err), true
	}
	return res.Content, res.IsError
}

func (a *Agent) doTask(ctx context.Context, tc llm.ToolCall) (string, bool) {
	var args struct {
		Agent  string `json:"agent"`
		Prompt string `json:"prompt"`
	}
	if err := json.Unmarshal([]byte(tc.Arguments), &args); err != nil {
		return "task: invalid arguments: " + err.Error(), true
	}
	if args.Agent == "" {
		return "task: 'agent' is required", true
	}
	if !a.cfg.maySpawn(args.Agent) {
		return fmt.Sprintf("task: %s is not permitted to spawn %q", a.id, args.Agent), true
	}
	id, err := a.s.router.spawn(a.id, args.Agent, args.Prompt)
	if err != nil {
		return "task: " + err.Error(), true
	}
	return fmt.Sprintf("spawned subagent %s (role %s). Call wait to collect its result.", id, core.CanonRole(args.Agent)), false
}

// doReprompt re-tasks an existing subagent with a new instruction. A finished or
// idle subagent stays alive (with its context), so repromting wakes it; a running
// one has the instruction folded in. If the agent is gone (killed/torn down), a
// fresh instance of its role is started instead.
func (a *Agent) doReprompt(tc llm.ToolCall) (string, bool) {
	var args struct {
		Agent  string `json:"agent"`
		Prompt string `json:"prompt"`
	}
	if err := json.Unmarshal([]byte(tc.Arguments), &args); err != nil {
		return "reprompt: invalid arguments: " + err.Error(), true
	}
	if args.Agent == "" || strings.TrimSpace(args.Prompt) == "" {
		return "reprompt: 'agent' and 'prompt' are required", true
	}
	target := core.AgentID(args.Agent)
	if _, ok := a.s.router.get(target); ok {
		a.s.router.send(Envelope{From: a.id, To: target, Kind: MsgSteer, Body: args.Prompt})
		return fmt.Sprintf("repromted %s. Call wait to collect its next result.", args.Agent), false
	}
	role := roleOf(args.Agent)
	if !a.cfg.maySpawn(role) {
		return fmt.Sprintf("reprompt: %s is gone and %s may not spawn %q", args.Agent, a.id, role), true
	}
	id, err := a.s.router.spawn(a.id, role, args.Prompt)
	if err != nil {
		return "reprompt: " + err.Error(), true
	}
	return fmt.Sprintf("%s was gone; started a fresh %s (%s). Call wait to collect its result.", core.CanonRole(args.Agent), core.CanonRole(role), id), false
}

// roleOf strips the "#n" instance suffix from an agent id ("recon#2" -> "recon").
func roleOf(agentID string) string {
	if i := strings.LastIndexByte(agentID, '#'); i >= 0 {
		return agentID[:i]
	}
	return agentID
}

func (a *Agent) doWait(ctx context.Context) (string, bool) {
	select {
	case env := <-a.box:
		switch env.Kind {
		case MsgResult:
			return fmt.Sprintf("result from %s:\n%s", env.From, env.Body), false
		case MsgJobDone:
			return env.Body, false
		default:
			// A steer / user message INTERRUPTS the wait (folded into context) so the
			// agent reacts promptly instead of staying blocked until a subagent or
			// job finishes. It can call wait again to keep waiting.
			a.foldEnvelope(env)
			return fmt.Sprintf("you were woken early: a message from %s arrived while you were waiting (added to your context) — act on it now, or call wait again to keep waiting for a subagent/job.", env.From), false
		}
	case <-ctx.Done():
		return "wait aborted", true
	}
}

// doBash runs a shell command inside the sandbox in the FOREGROUND, but only for
// up to foregroundTimeout; if the command is still running then, it is moved to
// the background (it keeps running and posts a MsgJobDone when it finishes) and
// the agent is freed to continue instead of hanging. The command is registered
// so the user can stop it (StopCommands) and so pausing the agent cancels it.
func (a *Agent) doBash(tc llm.ToolCall) (string, bool) {
	var args struct {
		Cmd string `json:"cmd"`
	}
	if err := json.Unmarshal([]byte(tc.Arguments), &args); err != nil {
		return "bash: invalid arguments: " + err.Error(), true
	}
	if strings.TrimSpace(args.Cmd) == "" {
		return "bash: 'cmd' is required", true
	}
	if a.s.Sandbox == nil {
		return "bash: no sandbox attached", true
	}

	a.mu.Lock()
	a.jobSeq++
	id := a.jobSeq
	jobID := fmt.Sprintf("job-%d", id)
	a.mu.Unlock()

	a.s.emit(event.ToolStarted{Agent: a.id, Tool: "bash", ArgsPreview: preview(tc.Arguments), Args: tc.Arguments})

	// Run the command as one streaming Exec on the ENGAGEMENT context, so it (and any
	// children) survive this agent turn and are stopped only by StopCommands/pause or
	// engagement end. The agent waits up to foregroundTimeout (or a user message);
	// after that the still-running command is moved to the background and the agent is
	// freed to continue.
	ctx, cancel := context.WithCancel(a.jobBaseCtx())
	job := &bgJob{cancel: cancel}
	a.mu.Lock()
	a.fgID, a.fgJob = id, job
	a.mu.Unlock()

	inline := make(chan cmdResult, 1)
	a.s.router.wg.Add(1)
	go func() {
		defer a.s.router.wg.Done()
		r := a.execCommand(ctx, args.Cmd)
		job.mu.Lock()
		if job.promoted {
			job.mu.Unlock()
			a.forgetJob(id)
			if r.finished {
				a.s.router.send(Envelope{From: a.id, To: a.id, Kind: MsgJobDone,
					Body: fmt.Sprintf("background %s finished (exit %d). Output:\n%s", jobID, r.code, r.out)})
			}
			return
		}
		job.reported = true
		job.mu.Unlock()
		a.clearFg(id)
		inline <- r
	}()

	fin := func(r cmdResult) (string, bool) {
		cancel()
		// The command's output was already streamed to the pane; emit just the
		// completion footer so even a no-output command visibly shows it finished.
		if !r.finished {
			a.s.emit(event.ToolOutput{Agent: a.id, Chunk: "[stopped]"})
			return fmt.Sprintf("%s\n[stopped]", r.out), true
		}
		a.s.emit(event.ToolOutput{Agent: a.id, Chunk: fmt.Sprintf("[exit %d]", r.code)})
		return fmt.Sprintf("%s\n[exit %d]", r.out, r.code), r.code != 0
	}
	// Move the still-running command to the background and free the agent. The job
	// keeps running (NOT cancelled); its output keeps streaming.
	promote := func(msg string) (string, bool) {
		job.mu.Lock()
		if job.reported { // finished at this very moment — return its result instead
			job.mu.Unlock()
			return fin(<-inline)
		}
		job.promoted = true
		job.mu.Unlock()
		a.mu.Lock()
		if a.fgID == id {
			a.fgID, a.fgJob = 0, nil
		}
		a.jobs[id] = job
		a.mu.Unlock()
		return msg, false
	}

	// Drop any stale nudge so only a message arriving DURING this command wakes it.
	select {
	case <-a.nudge:
	default:
	}

	select {
	case r := <-inline:
		return fin(r)
	case <-a.nudge:
		return promote(fmt.Sprintf("a message arrived, so you were woken early. The command is still running in the background as %s (its output streams live; call wait or you'll be notified when it finishes, and stop it with StopCommands). Read the new message now, then continue.", jobID))
	case <-time.After(foregroundTimeout):
		return promote(fmt.Sprintf("command still running after %s — moved to background %s (it keeps running; its output streams live, and you'll be notified when it finishes or can call wait). Stop it explicitly with StopCommands.",
			foregroundTimeout, jobID))
	}
}

// doBashBg starts a shell command inside the sandbox as a BACKGROUND job and
// returns immediately with a job id. The job runs under the engagement context
// (not this agent's per-turn context), so it keeps running across the agent's
// turns and even while the agent is paused. When it finishes, a MsgJobDone
// envelope is posted back to this agent's mailbox (folded into history at the
// next step, or collectable with `wait`), and its output is emitted to the TUI.
func (a *Agent) doBashBg(tc llm.ToolCall) (string, bool) {
	var args struct {
		Cmd string `json:"cmd"`
	}
	if err := json.Unmarshal([]byte(tc.Arguments), &args); err != nil {
		return "bash_bg: invalid arguments: " + err.Error(), true
	}
	if strings.TrimSpace(args.Cmd) == "" {
		return "bash_bg: 'cmd' is required", true
	}
	if a.s.Sandbox == nil {
		return "bash_bg: no sandbox attached", true
	}

	a.mu.Lock()
	a.jobSeq++
	id := a.jobSeq
	jobID := fmt.Sprintf("job-%d", id)
	a.mu.Unlock()
	cmd := args.Cmd

	a.s.emit(event.ToolStarted{Agent: a.id, Tool: "bash_bg", ArgsPreview: preview(tc.Arguments), Args: tc.Arguments})

	// Run on the ENGAGEMENT context in a goroutine that holds the Exec open for the
	// command's whole life, so the job (a server/listener and everything it spawns)
	// keeps running across turns until it finishes, is explicitly stopped
	// (StopCommands), or the engagement ends. Cancelling the job's context stops it
	// and its children.
	ctx, cancel := context.WithCancel(a.jobBaseCtx())
	job := &bgJob{cancel: cancel, promoted: true}
	a.mu.Lock()
	a.jobs[id] = job
	a.mu.Unlock()

	a.s.router.wg.Add(1)
	go func() {
		defer a.s.router.wg.Done()
		r := a.execCommand(ctx, cmd)
		a.forgetJob(id)
		if r.finished {
			a.s.router.send(Envelope{From: a.id, To: a.id, Kind: MsgJobDone,
				Body: fmt.Sprintf("background %s finished (exit %d). Output:\n%s", jobID, r.code, r.out)})
		}
	}()

	return fmt.Sprintf("started background %s: %q. It keeps running until it finishes, you stop it (StopCommands), or the engagement ends; you'll be notified when it finishes (or call wait). Continue with other work meanwhile.", jobID, preview(cmd)), false
}

func (a *Agent) doSendMessage(tc llm.ToolCall) (string, bool) {
	// Surface agent-to-agent messaging as a normal tool call in the sender's
	// transcript: a one-line header (collapsed) with the full {to, body} shown on
	// expand, and the delivery result as its output — just like bash or a registry
	// tool. reply emits that result and returns it.
	a.s.emit(event.ToolStarted{Agent: a.id, Tool: "send_message", ArgsPreview: preview(tc.Arguments), Args: tc.Arguments})
	reply := func(msg string, isErr bool) (string, bool) {
		a.s.emit(event.ToolOutput{Agent: a.id, Chunk: msg})
		return msg, isErr
	}
	var args struct {
		To   string `json:"to"`
		Body string `json:"body"`
	}
	if err := json.Unmarshal([]byte(tc.Arguments), &args); err != nil {
		return reply("send_message: invalid arguments: "+err.Error(), true)
	}
	if args.To == "" || args.Body == "" {
		return reply("send_message: 'to' and 'body' are required", true)
	}
	to := core.AgentID(args.To)
	// send_message is agent↔agent coordination only. To ask the USER a question,
	// an agent uses the `ask` tool directly (no orchestrator relay).
	if !a.s.router.send(Envelope{From: a.id, To: to, Kind: MsgPeer, Body: args.Body}) {
		return reply(fmt.Sprintf("send_message: no agent %q is live", args.To), true)
	}
	return reply("message delivered to "+args.To, false)
}

// doInspect lets the orchestrator view another agent's live state, recent history,
// and latest thoughts at any time. Shown as a tool call in the caller's transcript.
func (a *Agent) doInspect(tc llm.ToolCall) (string, bool) {
	a.s.emit(event.ToolStarted{Agent: a.id, Tool: "inspect", ArgsPreview: preview(tc.Arguments), Args: tc.Arguments})
	reply := func(msg string, isErr bool) (string, bool) {
		a.s.emit(event.ToolOutput{Agent: a.id, Chunk: msg})
		return msg, isErr
	}
	var args struct {
		Agent string `json:"agent"`
	}
	if err := json.Unmarshal([]byte(tc.Arguments), &args); err != nil {
		return reply("inspect: invalid arguments: "+err.Error(), true)
	}
	if args.Agent == "" {
		return reply("inspect: 'agent' is required", true)
	}
	target, ok := a.s.router.get(core.AgentID(args.Agent))
	if !ok {
		return reply(fmt.Sprintf("inspect: no agent %q is live (it may have finished, been killed, or never started)", args.Agent), true)
	}
	return reply(target.inspectText(), false)
}

// normalizeSeverity maps a free-form severity to one of the five canonical levels,
// defaulting unrecognized values to "info".
func normalizeSeverity(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "critical", "crit":
		return "critical"
	case "high":
		return "high"
	case "medium", "med", "moderate":
		return "medium"
	case "low":
		return "low"
	default:
		return "info"
	}
}

// doRecordFinding records a structured security finding, surfaced to the live
// findings panel and the host log. Shown as a tool call in the caller's transcript.
func (a *Agent) doRecordFinding(tc llm.ToolCall) (string, bool) {
	a.s.emit(event.ToolStarted{Agent: a.id, Tool: "record_finding", ArgsPreview: preview(tc.Arguments), Args: tc.Arguments})
	reply := func(msg string, isErr bool) (string, bool) {
		a.s.emit(event.ToolOutput{Agent: a.id, Chunk: msg})
		return msg, isErr
	}
	var args struct {
		Title    string `json:"title"`
		Severity string `json:"severity"`
		Target   string `json:"target"`
		Detail   string `json:"detail"`
	}
	if err := json.Unmarshal([]byte(tc.Arguments), &args); err != nil {
		return reply("record_finding: invalid arguments: "+err.Error(), true)
	}
	if strings.TrimSpace(args.Title) == "" {
		return reply("record_finding: 'title' is required", true)
	}
	sev := normalizeSeverity(args.Severity)
	a.s.emit(event.Finding{Agent: a.id, Severity: sev, Title: strings.TrimSpace(args.Title),
		Target: strings.TrimSpace(args.Target), Detail: strings.TrimSpace(args.Detail)})
	return reply(fmt.Sprintf("recorded %s finding %q (logged for the report). Keep it; the reporter will include it.", sev, strings.TrimSpace(args.Title)), false)
}

// doUpdateSummary sets the orchestrator's live engagement summary (replaces the
// previous one). Shown as a tool call in Main's transcript.
func (a *Agent) doUpdateSummary(tc llm.ToolCall) (string, bool) {
	a.s.emit(event.ToolStarted{Agent: a.id, Tool: "update_summary", ArgsPreview: preview(tc.Arguments), Args: tc.Arguments})
	reply := func(msg string, isErr bool) (string, bool) {
		a.s.emit(event.ToolOutput{Agent: a.id, Chunk: msg})
		return msg, isErr
	}
	var args struct {
		Summary string `json:"summary"`
	}
	if err := json.Unmarshal([]byte(tc.Arguments), &args); err != nil {
		return reply("update_summary: invalid arguments: "+err.Error(), true)
	}
	if strings.TrimSpace(args.Summary) == "" {
		return reply("update_summary: 'summary' is required", true)
	}
	a.s.emit(event.Summary{Text: strings.TrimSpace(args.Summary)})
	return reply("summary panel updated.", false)
}

// doUpdateDiagram sets the orchestrator's live ASCII engagement map (replaces the
// previous one). Shown as a tool call in Main's transcript.
func (a *Agent) doUpdateDiagram(tc llm.ToolCall) (string, bool) {
	a.s.emit(event.ToolStarted{Agent: a.id, Tool: "update_diagram", ArgsPreview: preview(tc.Arguments), Args: tc.Arguments})
	reply := func(msg string, isErr bool) (string, bool) {
		a.s.emit(event.ToolOutput{Agent: a.id, Chunk: msg})
		return msg, isErr
	}
	var args struct {
		Diagram string `json:"diagram"`
	}
	if err := json.Unmarshal([]byte(tc.Arguments), &args); err != nil {
		return reply("update_diagram: invalid arguments: "+err.Error(), true)
	}
	if strings.TrimSpace(args.Diagram) == "" {
		return reply("update_diagram: 'diagram' is required", true)
	}
	// Keep trailing-space trimming off so ASCII-art alignment is preserved; only trim
	// leading/trailing blank lines.
	text := strings.Trim(args.Diagram, "\n")
	// Enforce the live pane width: the Diagram pane CLIPS (does not wrap) each line to
	// its content width, so a wider line silently loses its right side. Reject such a
	// map and tell the orchestrator exactly which lines are too wide and the current
	// limit, rather than drawing a truncated picture. The width is read fresh here, so
	// a pane resize is reflected on the next call.
	maxW := 0
	if a.s.DiagramWidth != nil {
		maxW = a.s.DiagramWidth()
	}
	if maxW > 0 {
		if over := diagramOverflows(text, maxW); len(over) > 0 {
			return reply(diagramWidthError(maxW, over), true)
		}
	}
	a.s.emit(event.Diagram{Text: text})
	if maxW > 0 {
		return reply(fmt.Sprintf("diagram panel updated. The Diagram pane is currently %d columns wide; keep every line at or under %d columns (it can change if the user resizes the pane — this reply always reports the current width).", maxW, maxW), false)
	}
	return reply("diagram panel updated.", false)
}

// overLine records one diagram line that is too wide for the pane.
type overLine struct {
	n int // 1-based line number
	w int // its display width in columns
}

// diagramOverflows returns the lines of text whose display width exceeds maxW,
// measured the same way the Diagram pane clips them (wide-char / ANSI aware), so the
// check matches exactly what would be cut off on screen.
func diagramOverflows(text string, maxW int) []overLine {
	var out []overLine
	for i, line := range strings.Split(text, "\n") {
		if w := ansi.StringWidth(line); w > maxW {
			out = append(out, overLine{n: i + 1, w: w})
		}
	}
	return out
}

// diagramWidthError builds the precise rejection message for update_diagram: the
// current limit, how many lines overflow and by how much (a few named explicitly),
// and what to do about it.
func diagramWidthError(maxW int, over []overLine) string {
	widest := 0
	for _, o := range over {
		if o.w > widest {
			widest = o.w
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "update_diagram REJECTED — the diagram was NOT updated. The Diagram pane is %d columns wide, but %d line(s) are wider and would be clipped (the pane does not wrap). ", maxW, len(over))
	const show = 5
	parts := make([]string, 0, show)
	for i, o := range over {
		if i >= show {
			parts = append(parts, fmt.Sprintf("and %d more", len(over)-show))
			break
		}
		parts = append(parts, fmt.Sprintf("line %d is %d cols", o.n, o.w))
	}
	b.WriteString(strings.Join(parts, ", "))
	fmt.Fprintf(&b, " (widest %d). Rewrite the map so EVERY line is at most %d columns, then call update_diagram again.", widest, maxW)
	return b.String()
}

// resourceProbe is the read-only snapshot command run inside the sandbox to
// report system load. It only reads /proc and runs nproc/free/df/ps, so it is
// safe to run privileged (root, for full process visibility regardless of a
// hidepid mount) and needs no network.
const resourceProbe = `set +e
echo "## Sandbox resources"
ncpu=$(nproc 2>/dev/null || grep -c ^processor /proc/cpuinfo 2>/dev/null || echo '?')
if [ -r /proc/loadavg ]; then
  set -- $(cat /proc/loadavg)
  echo "CPU: ${ncpu} cores | load ${1} ${2} ${3} (1/5/15 min) | procs ${4}"
else
  echo "CPU: ${ncpu} cores"
fi
if command -v free >/dev/null 2>&1; then
  free -m | awk '/^Mem:/{u=$3;t=$2;av=$7;if(t>0)p=100*u/t;printf "Mem: %d/%d MB used (%.0f%%)%s\n",u,t,p,(av!=""?sprintf(", %d MB available",av):"")}'
elif [ -r /proc/meminfo ]; then
  awk '/^MemTotal:/{t=$2}/^MemAvailable:/{a=$2}END{printf "Mem: %d MB available of %d MB\n",a/1024,t/1024}' /proc/meminfo
fi
d=/opt/oline; [ -d "$d" ] || d=/
df -h "$d" 2>/dev/null | awk -v d="$d" 'NR==2{printf "Disk(%s): %s used of %s (%s full)\n",d,$3,$2,$5}'
echo
echo "Agent-class process usage (oline-* users; the installer runs as root):"
ps -eo user:20,pcpu,pmem --no-headers 2>/dev/null | awk '$1 ~ /^oline-/{c[$1]+=$2;m[$1]+=$3;n[$1]++} END{if(!length(c)){print "  (no agent processes active)"}else{for(u in c)printf "  %-12s %5.1f%% CPU  %5.1f%% MEM  (%d procs)\n",u,c[u],m[u],n[u]}}'
echo
echo "Top processes by CPU:"
ps -eo user:12,pid,pcpu,pmem,comm --sort=-pcpu 2>/dev/null | head -n 9`

// doResources runs a read-only snapshot of the sandbox's CPU, memory, disk, and
// per-agent-class process usage, so the orchestrator can tell whether the box is
// overloaded and keep subagents from piling on. Orchestrator-only; the probe runs
// as root for full visibility and touches no network.
func (a *Agent) doResources(ctx context.Context) (string, bool) {
	a.s.emit(event.ToolStarted{Agent: a.id, Tool: "resources"})
	reply := func(msg string, isErr bool) (string, bool) {
		a.s.emit(event.ToolOutput{Agent: a.id, Chunk: msg})
		return msg, isErr
	}
	if a.s.Sandbox == nil {
		return reply("resources: no sandbox is attached", true)
	}
	w := &bgWriter{} // capture only (no emit): the formatted result is emitted once below
	st, err := a.s.Sandbox.Exec(ctx, a.id, sandbox.Cmd{Argv: []string{"bash", "-lc", resourceProbe}, Policy: sandbox.Privileged}, w)
	out := strings.TrimRight(w.String(), "\n")
	if err != nil {
		note := "resources: sandbox probe failed: " + err.Error()
		if strings.TrimSpace(out) != "" {
			note = out + "\n" + note
		}
		return reply(note, true)
	}
	if strings.TrimSpace(out) == "" {
		return reply(fmt.Sprintf("resources: probe produced no output (exit %d)", st.Code), true)
	}
	return reply(out, false)
}

func (a *Agent) doAsk(ctx context.Context, tc llm.ToolCall) (string, bool) {
	var args struct {
		Question string   `json:"question"`
		Options  []string `json:"options"`
	}
	if err := json.Unmarshal([]byte(tc.Arguments), &args); err != nil || strings.TrimSpace(args.Question) == "" {
		return "ask: 'question' is required", true
	}
	a.s.emit(event.UserQuestion{From: a.id, Text: args.Question, Options: args.Options})
	if !a.s.Manual {
		return "Automatic mode: no interactive user is available right now. Proceed using your best judgment, strictly within the authorized scope; do not block waiting on this.", false
	}
	// Manual mode: block for a user answer relayed into the mailbox.
	for {
		select {
		case env := <-a.box:
			if env.Kind == MsgUserAnswer {
				return "user answered: " + env.Body, false
			}
			a.foldEnvelope(env)
		case <-ctx.Done():
			return "ask aborted", true
		}
	}
}

func (a *Agent) doRuntime() string {
	a.mu.Lock()
	started, budget, from := a.started, a.budget, a.budgetFrom
	a.mu.Unlock()
	el := time.Since(started).Round(time.Second)
	now := time.Now().UTC().Format(time.RFC3339)
	if budget <= 0 {
		return fmt.Sprintf("elapsed since you started: %s; you have NO time limit; now: %s. Rely only on this value (or oline's reminder messages) to judge time.", el, now)
	}
	rem := budget - time.Since(from)
	if rem < 0 {
		rem = 0
	}
	return fmt.Sprintf("elapsed since you started: %s; TIME REMAINING in your budget: %s (budget %dm); now: %s. Rely only on this value (or oline's reminder messages) to judge time — do not estimate it any other way.",
		el, rem.Round(time.Second), int(budget.Minutes()), now)
}

func (a *Agent) doHeartbeat(ctx context.Context, tc llm.ToolCall) (string, bool) {
	var args struct {
		Seconds int `json:"seconds"`
	}
	_ = json.Unmarshal([]byte(tc.Arguments), &args)
	secs := args.Seconds
	if secs <= 0 {
		secs = 120
	}
	if secs > 300 {
		secs = 300
	}
	timer := time.NewTimer(time.Duration(secs) * time.Second)
	defer timer.Stop()
	select {
	case <-timer.C:
		return fmt.Sprintf("heartbeat elapsed (%ds); no messages queued.", secs), false
	case env := <-a.box:
		a.foldEnvelope(env)
		return fmt.Sprintf("heartbeat woke early: message from %s.", env.From), false
	case <-ctx.Done():
		return "heartbeat aborted", true
	}
}

// awaitInput blocks an idle interactive orchestrator until the next message
// arrives (folded into history), returning true to continue. It returns false
// if the agent is killed or the engagement context ends.
func (a *Agent) awaitInput(parent context.Context) bool {
	for {
		if a.sm.State() == core.Killed {
			return false
		}
		_, ch := a.sm.snapshot() // closed on resume/kill
		select {
		case env := <-a.box:
			a.foldEnvelope(env)
			return true
		case <-ch:
			continue // state changed (e.g. killed) — re-check at the top
		case <-parent.Done():
			return false
		}
	}
}

// drainMailbox folds any queued envelopes into history without blocking.
func (a *Agent) drainMailbox() {
	for {
		select {
		case env := <-a.box:
			a.foldEnvelope(env)
		default:
			return
		}
	}
}

func (a *Agent) foldEnvelope(env Envelope) {
	var label string
	switch env.Kind {
	case MsgSteer:
		label = "steering"
	case MsgUserAnswer:
		label = "user-answer"
	case MsgResult:
		label = "result"
	case MsgJobDone:
		label = "background-job-done"
	case MsgBudget:
		label = "time-budget"
	default:
		label = "message"
	}
	a.appendHistory(llm.Message{
		Role:    llm.RoleUser,
		Content: fmt.Sprintf("[%s from %s] %s", label, env.From, env.Body),
	})
}

func allowed(list []string, name string) bool {
	for _, n := range list {
		if n == name {
			return true
		}
	}
	return false
}

func assistantMessage(text string, calls []llm.ToolCall) llm.Message {
	return llm.Message{Role: llm.RoleAssistant, Content: text, ToolCalls: calls}
}

func toolMessage(tc llm.ToolCall, content string) llm.Message {
	return llm.Message{Role: llm.RoleTool, ToolCallID: tc.ID, Name: tc.Name, Content: content}
}

// bgWriter accumulates a background job's output (capped) and forwards each
// chunk to the TUI as it arrives.
type bgWriter struct {
	emit func(string)
	mu   sync.Mutex
	b    []byte
}

const bgWriterMax = 1 << 20 // 1 MiB retained per job

func (w *bgWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	if len(w.b) < bgWriterMax {
		if room := bgWriterMax - len(w.b); room >= len(p) {
			w.b = append(w.b, p...)
		} else {
			w.b = append(w.b, p[:room]...)
			w.b = append(w.b, "\n[...output truncated...]"...)
		}
	}
	w.mu.Unlock()
	if w.emit != nil {
		w.emit(string(p))
	}
	return len(p), nil
}

func (w *bgWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return string(w.b)
}

func preview(s string) string {
	s = strings.TrimSpace(s)
	if r := []rune(s); len(r) > 120 {
		return string(r[:120]) + "…"
	}
	return s
}

// primitiveSchemas are the engine-level tools' function definitions.
var primitiveSchemas = map[string]llm.FunctionDef{
	"task": {
		Name:        "task",
		Description: "Spawn a specialized subagent to do a piece of work. Returns its id; call wait to collect its result.",
		Parameters:  json.RawMessage(`{"type":"object","properties":{"agent":{"type":"string","description":"the subagent role to spawn"},"prompt":{"type":"string","description":"the self-contained task for the subagent"}},"required":["agent","prompt"]}`),
	},
	"wait": {
		Name:        "wait",
		Description: "Block until the next spawned subagent returns a result, a background job finishes, or a message arrives. Waiting COUNTS against your time budget, so wait only as long as you actually need — don't idle in wait with nothing outstanding. A user or peer message interrupts the wait so you can act on it right away (you'll be told you were woken early); you can call wait again to keep waiting.",
		Parameters:  json.RawMessage(`{"type":"object","properties":{},"required":[]}`),
	},
	"reprompt": {
		Name:        "reprompt",
		Description: "Re-task an EXISTING subagent by id with a new instruction: restart one that has finished or is idling (it keeps its prior context) or redirect a running one. If that agent is gone, a fresh one of the same role is started. Use `task` to start a brand-new subagent instead.",
		Parameters:  json.RawMessage(`{"type":"object","properties":{"agent":{"type":"string","description":"the subagent id, e.g. recon#1"},"prompt":{"type":"string","description":"the new instruction for it"}},"required":["agent","prompt"]}`),
	},
	"bash_bg": {
		Name:        "bash_bg",
		Description: "Run a shell command inside the sandbox as a BACKGROUND job and return immediately with a job id, instead of blocking until it finishes (as `bash` does). Use it for long-running work — a scan, a fuzzing run, a build, a reverse-shell handler or any server/listener you need up. The job runs DETACHED: it, and every process it spawns (e.g. a server thread's subprocess), keeps running until it finishes on its own, you stop it, or the engagement ends — it is NOT killed when this call returns or when your turn ends. Start servers/listeners with bash_bg directly; do NOT background them yourself with a trailing `&` inside a `bash` command. You are notified when the job finishes, and you can also call `wait`. All execution stays confined to the sandbox.",
		Parameters:  json.RawMessage(`{"type":"object","properties":{"cmd":{"type":"string","description":"the shell command to run in the background"}},"required":["cmd"]}`),
	},
	"runtime": {
		Name:        "runtime",
		Description: "Authoritative time from oline: how long you have been running and, if you have a time budget, how much time you have LEFT. Call this to check your remaining time; rely ONLY on it (or oline's reminder messages) to judge time — never estimate from wall-clock guesses or the conversation length. Takes no arguments.",
		Parameters:  json.RawMessage(`{"type":"object","properties":{},"required":[]}`),
	},
	"heartbeat": {
		Name:        "heartbeat",
		Description: "Bounded, interruptible sleep for the orchestrator (<=300s). Set `seconds` to the MINIMUM you need before re-checking; it returns early the moment a message arrives, so prefer a longer bound (it wakes on its own) over polling in a tight loop.",
		Parameters:  json.RawMessage(`{"type":"object","properties":{"seconds":{"type":"integer","description":"max seconds to sleep (default 120, max 300)"}},"required":[]}`),
	},
	"send_message": {
		Name:        "send_message",
		Description: "Send a message to another live agent by id (the orchestrator is 'Main'). Use it to report a discovery, hand off intel, or steer a peer. To put a question to the user, use the `ask` tool instead.",
		Parameters:  json.RawMessage(`{"type":"object","properties":{"to":{"type":"string","description":"target agent id, e.g. Main"},"body":{"type":"string"}},"required":["to","body"]}`),
	},
	"inspect": {
		Name:        "inspect",
		Description: "View another agent's current state, recent conversation history, and latest thoughts at any time, by id (e.g. recon#1). Read-only; use it to check on a subagent's progress without waiting for it to report.",
		Parameters:  json.RawMessage(`{"type":"object","properties":{"agent":{"type":"string","description":"the agent id to inspect, e.g. recon#1"}},"required":["agent"]}`),
	},
	"ask": {
		Name:        "ask",
		Description: "Put a question to the user. In manual mode this blocks for their answer; in automatic mode it records the question and tells you to proceed.",
		Parameters:  json.RawMessage(`{"type":"object","properties":{"question":{"type":"string"},"options":{"type":"array","items":{"type":"string"}}},"required":["question"]}`),
	},
	"record_finding": {
		Name:        "record_finding",
		Description: "Record a confirmed security finding so it is logged and handed to the reporter for the final report. Call it the moment you confirm something real — a vulnerability, an exposed credential, a foothold, a dangerous misconfiguration. Use a short factual title; set severity to one of critical/high/medium/low/info; put the affected host/component/endpoint in target and one or two lines of evidence in detail. Record only confirmed, non-duplicate findings.",
		Parameters:  json.RawMessage(`{"type":"object","properties":{"title":{"type":"string","description":"short factual title of the finding"},"severity":{"type":"string","description":"critical | high | medium | low | info"},"target":{"type":"string","description":"affected host / component / endpoint"},"detail":{"type":"string","description":"one or two lines of evidence / impact"}},"required":["title","severity"]}`),
	},
	"update_summary": {
		Name:        "update_summary",
		Description: "Set the live engagement summary shown to the user in oline's summary panel (this REPLACES the previous summary, it does not append). Call it whenever the state of play changes meaningfully — a new foothold, a confirmed finding, a phase completed, a shift in plan — so the user can follow progress without asking. Keep it concise and plain-language: what is done, what is confirmed, what is in progress, and what is next.",
		Parameters:  json.RawMessage(`{"type":"object","properties":{"summary":{"type":"string","description":"the current state-of-play summary (plain language, replaces the previous one)"}},"required":["summary"]}`),
	},
	"update_diagram": {
		Name:        "update_diagram",
		Description: "Set the live ASCII map shown to the user in oline's diagram panel (this REPLACES the previous diagram, it does not append). In PENTESTER mode, draw a breadth-first network map: the hosts in play, which you have a foothold on vs. root on, how hosts connect to each other, and which hosts run important services. In EXPLOITER mode, draw a breadth-first map of the target software's components and how they interact; choose which components to show. Keep it LEGIBLE above all: in a large environment show only the most important hosts/components and connections rather than everything, and keep lines short (aim to stay within ~100 columns) so the ASCII art does not wrap. Update it whenever the map changes (new host discovered, foothold or root gained, a key service or component identified).",
		Parameters:  json.RawMessage(`{"type":"object","properties":{"diagram":{"type":"string","description":"the current map as monospaced ASCII art (replaces the previous one); use a legend for markers such as foothold/root"}},"required":["diagram"]}`),
	},
	"resources": {
		Name:        "resources",
		Description: "Snapshot the sandbox's current system load: CPU cores and load average, memory use, workspace disk use, a per-agent-class (oline-*) CPU/memory breakdown, and the top processes by CPU. Read-only and fast. Use it before fanning out several heavy subagents (scanners, fuzzers, crackers, brute-forcers), and whenever the box feels slow, to decide whether to hold off spawning, stagger starts, or tell a heavy subagent to ease off. Compare load average against the core count: load well above the core count means the box is saturated.",
		Parameters:  json.RawMessage(`{"type":"object","properties":{},"required":[]}`),
	},
}
