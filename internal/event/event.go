// Package event defines the one-way event stream the engine pushes to the TUI.
// The engine never touches the terminal; the TUI adapts these to tea.Msg values.
package event

import "github.com/nihaal-prasad/oline/internal/core"

// Event is any engine-to-UI event.
type Event interface{ isEvent() }

// TokenDelta is streamed assistant text / reasoning for an agent.
type TokenDelta struct {
	Agent     core.AgentID
	Text      string
	Reasoning string
}

// ToolStarted announces a tool call beginning. ArgsPreview is a short one-line
// form for the collapsed header; Args is the full argument string, shown when
// the tool call is expanded so the whole command is visible.
type ToolStarted struct {
	Agent       core.AgentID
	Tool        string
	ArgsPreview string
	Args        string
}

// ToolOutput is streamed output from a running tool.
type ToolOutput struct {
	Agent core.AgentID
	Chunk string
}

// AgentState reports a lifecycle transition.
type AgentState struct {
	Agent core.AgentID
	State core.State
}

// Spawned announces a new subagent.
type Spawned struct {
	Parent core.AgentID
	Child  core.AgentID
	Role   string
	Prompt string // the task prompt the orchestrator gave the new subagent
}

// UserQuestion is a question an agent put to the user via the `ask` tool.
type UserQuestion struct {
	From    core.AgentID
	Text    string
	Options []string
}

// Notice is a general status/log line. Agent, when set, routes it to that
// agent's transcript (empty => the orchestrator / Main).
type Notice struct {
	Agent core.AgentID
	Level string // info | warn | error
	Text  string
}

// Usage reports token usage for one completed LLM turn (for cost tracking).
// CachedPromptTokens is the subset of PromptTokens served from the prompt cache.
type Usage struct {
	Agent              core.AgentID
	Model              string
	PromptTokens       int
	CompletionTokens   int
	CachedPromptTokens int
	// Summarizer marks usage from a context-compaction call (the `small`-tier
	// summarizer), so the TUI can track and cap that spend separately as well as
	// folding it into the engagement total.
	Summarizer bool
}

// Finding is a structured security finding an agent recorded via record_finding.
// The TUI collects these into a live findings panel and the host log; the reporter
// turns them into the written report. Severity is one of
// info|low|medium|high|critical (normalized by the engine).
type Finding struct {
	Agent    core.AgentID
	Severity string
	Title    string
	Target   string // affected host / component / endpoint (optional)
	Detail   string // one or two lines of evidence / summary (optional)
}

// Summary is the orchestrator's current engagement state-of-play, set via
// update_summary. The TUI shows the latest one in a live summary panel (each new
// Summary replaces the previous text, it does not append).
type Summary struct {
	Text string
}

// Diagram is the orchestrator's current ASCII map of the engagement, set via
// update_diagram: in pentester mode a network map (hosts, footholds/root,
// connections, key services); in exploiter mode a software-component map. The TUI
// shows the latest one verbatim in a live diagram panel (each new Diagram replaces
// the previous text, it does not append).
type Diagram struct {
	Text string
}

func (TokenDelta) isEvent()   {}
func (ToolStarted) isEvent()  {}
func (ToolOutput) isEvent()   {}
func (AgentState) isEvent()   {}
func (Spawned) isEvent()      {}
func (UserQuestion) isEvent() {}
func (Notice) isEvent()       {}
func (Usage) isEvent()        {}
func (Finding) isEvent()      {}
func (Summary) isEvent()      {}
func (Diagram) isEvent()      {}
