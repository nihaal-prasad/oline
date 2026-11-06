package tui

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/glamour"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/nihaal-prasad/oline/assets"
	"github.com/nihaal-prasad/oline/internal/agents"
	"github.com/nihaal-prasad/oline/internal/audit"
	"github.com/nihaal-prasad/oline/internal/config"
	"github.com/nihaal-prasad/oline/internal/core"
	"github.com/nihaal-prasad/oline/internal/engine"
	"github.com/nihaal-prasad/oline/internal/event"
	"github.com/nihaal-prasad/oline/internal/mcp"
	"github.com/nihaal-prasad/oline/internal/sandbox"
	"github.com/nihaal-prasad/oline/internal/scope"
	"github.com/nihaal-prasad/oline/internal/tools"
	"github.com/nihaal-prasad/oline/internal/wire"
)

// ---- messages ------------------------------------------------------------

// engineBatchMsg carries a batch of engine events drained in one go, so the TUI
// renders ONCE per batch instead of once per event (which, under a token burst
// from several agents, let the event channel fill and drop events).
type engineBatchMsg struct{ evs []event.Event }
type scanDoneMsg struct {
	cands []sandbox.Candidate
	err   error
}
type resultMsg struct{ res engine.Result }
type startMsg struct{}

// tickMsg drives the once-a-second re-render that keeps the roster's time-left
// countdowns current.
type tickMsg struct{}

// tickCmd drives the once-a-second roster re-render. It uses tea.Every, which
// aligns to the wall clock (it fires at the next whole-second boundary), so the
// cadence stays steady instead of drifting by the per-tick handling time the way
// tea.Tick (which measures from when the command runs) would.
func tickCmd() tea.Cmd {
	return tea.Every(time.Second, func(time.Time) tea.Msg { return tickMsg{} })
}

type engListMsg struct {
	names []string
	err   error
}

// listEngagementsCmd lists the saved engagements inside a WSL distro for the resume
// step. QEMU resume is handled inline in the environment step (a persisted QEMU VM
// is its own Candidate with Engagement set), so this is only ever called for WSL.
func listEngagementsCmd(cand sandbox.Candidate) tea.Cmd {
	return func() tea.Msg {
		names, err := sandbox.ListWSLEngagements(context.Background(), cand.ID)
		return engListMsg{names: names, err: err}
	}
}

func listen(ch chan event.Event) tea.Cmd {
	return func() tea.Msg {
		ev, ok := <-ch
		if !ok {
			return nil
		}
		// Block for the first event, then greedily drain whatever else is already
		// queued (bounded) so a burst is applied + rendered in one pass.
		batch := []event.Event{ev}
		for len(batch) < 512 {
			select {
			case e, ok := <-ch:
				if !ok {
					return engineBatchMsg{batch}
				}
				batch = append(batch, e)
			default:
				return engineBatchMsg{batch}
			}
		}
		return engineBatchMsg{batch}
	}
}

func waitResult(ch chan engine.Result) tea.Cmd {
	return func() tea.Msg { return resultMsg{<-ch} }
}

// newWSLSentinel is the Candidate ID of the synthetic "create a new WSL distro"
// entry; it is not a real distro name (the oline- name is generated on selection).
const newWSLSentinel = "oline-new-wsl-distro"

func scanEnvsCmd() tea.Cmd {
	return func() tea.Msg {
		var all []sandbox.Candidate
		cs, err := sandbox.NewWSL().Scan(context.Background())
		all = append(all, cs...)
		// Existing QEMU VMs from prior engagements (persisted disks under images/.state).
		// Listing them here lets the user reuse one exactly like reusing a WSL distro:
		// selecting one resumes its disk with its workspace and installed tooling intact.
		// QEMU engagements persist by default (unless qemu.disposable is set, or the VM
		// did not shut down cleanly), so a prior engagement's VM appears here to resume.
		if qc, ok := sandbox.NewQEMU().(sandbox.QEMUControl); ok {
			for _, eng := range qc.ResumableEngagements() {
				all = append(all, sandbox.Candidate{
					ID: eng, Name: eng, Backend: sandbox.BackendQEMU,
					Managed: true, Engagement: eng,
				})
			}
		}
		qs, qerr := sandbox.NewQEMU().Scan(context.Background())
		all = append(all, qs...)
		// Present the managed Debian base as a "create a fresh VM" action, parallel to
		// the WSL create entry below. Every QEMU engagement boots a new disposable
		// overlay on the one read-only base, so selecting it always makes a brand-new
		// instance — it never reuses or overwrites a prior disk (reuse is the Resume
		// step). A bare "oline-debian13" filename read as "reuse this", which was the
		// source of confusion. NeedsCreate here is display-only for QEMU: it selects
		// the sentence-form label, and OpenWith downloads the base if it is missing.
		if sandbox.QEMUAvailable() {
			debianIdx := -1
			for i, c := range all {
				if c.Backend == sandbox.BackendQEMU && c.ID == sandbox.QEMUDebianID {
					debianIdx = i
					break
				}
			}
			if debianIdx >= 0 {
				all[debianIdx].Name = "Create a new oline-managed Debian QEMU VM (fresh disk from the base)"
				all[debianIdx].NeedsCreate = true
			} else {
				all = append(all, sandbox.Candidate{
					ID: sandbox.QEMUDebianID, Name: "Create a new oline-managed Debian QEMU VM (downloads the base on first use)",
					Backend: sandbox.BackendQEMU, Managed: true, NeedsCreate: true,
				})
			}
		}
		// Offer creating a fresh oline-managed Debian WSL distro (vs reusing an
		// existing distro above). The ID is a sentinel; the real oline- name is
		// generated when the entry is chosen.
		if sandbox.WSLAvailable() {
			all = append(all, sandbox.Candidate{
				ID: newWSLSentinel, Name: "Create a new oline-managed Debian WSL distro",
				Backend: sandbox.BackendWSL, Managed: true, NeedsCreate: true,
			})
		}
		if err == nil {
			err = qerr // surface a QEMU scan error when the WSL scan was fine
		}
		return scanDoneMsg{cands: all, err: err}
	}
}

// sandboxReadyMsg reports the result of preparing a sandbox (open/create + egress)
// off the UI goroutine, so a slow QEMU boot/download doesn't freeze the TUI.
type sandboxReadyMsg struct {
	h   sandbox.Handle
	err error
}

// sandboxPrep bundles everything prepareSandboxCmd needs to open or resume an
// environment off the UI goroutine.
type sandboxPrep struct {
	cand       sandbox.Candidate
	scopeStr   string
	allowIPv6  bool
	qemu       sandbox.BootOpts // persist / host firewall / allow-internet (QEMU only)
	resume     bool
	engagement string // the QEMU engagement to resume (resume + QEMU only)
	createWSL  bool   // provision a new WSL distro named newName (vs open an existing one)
	newName    string // oline- name for the new WSL distro (createWSL only)
	wslRootfs  string // rootfs tarball to import ("" => wsl --install; createWSL only)
}

// prepareSandboxCmd opens (or creates/resumes) the chosen environment and sets up
// per-agent egress, all off the UI goroutine.
func prepareSandboxCmd(p sandboxPrep) tea.Cmd {
	cand := p.cand
	return func() tea.Msg {
		ctx := context.Background()
		var h sandbox.Handle
		var err error
		switch cand.Backend {
		case sandbox.BackendQEMU:
			qc, ok := sandbox.NewQEMU().(sandbox.QEMUControl)
			if !ok {
				return sandboxReadyMsg{err: fmt.Errorf("qemu backend does not support controlled boot")}
			}
			if p.resume {
				h, err = qc.Resume(ctx, p.engagement, p.qemu)
			} else {
				h, err = qc.OpenWith(ctx, cand.ID, p.qemu)
			}
		default:
			if p.createWSL {
				wc, ok := sandbox.NewWSL().(sandbox.WSLControl)
				if !ok {
					return sandboxReadyMsg{err: fmt.Errorf("this platform cannot create a WSL distro")}
				}
				h, err = wc.CreateDistro(ctx, p.newName, p.wslRootfs)
			} else {
				h, err = sandbox.NewWSL().Open(ctx, cand.ID)
			}
		}
		if err != nil {
			id := cand.ID
			if p.createWSL {
				id = p.newName
			}
			return sandboxReadyMsg{err: fmt.Errorf("prepare %s: %w", id, err)}
		}
		pol := scope.Parse(p.scopeStr)
		if err := h.SetupEgress(ctx, sandbox.Scope{Targets: append(pol.CIDRs(), pol.Domains()...), AllowIPv6: p.allowIPv6}); err != nil {
			// Don't strand the sandbox we just opened/booted (a QEMU VM may be
			// running) when egress fails and we abort back to the wizard.
			if sd, ok := h.(sandbox.Shutdowner); ok {
				_ = sd.Shutdown(context.Background())
			}
			return sandboxReadyMsg{err: fmt.Errorf("egress hardening failed (%v) — refusing to start without per-agent network isolation", err)}
		}
		return sandboxReadyMsg{h: h}
	}
}

// ---- model ---------------------------------------------------------------

// catalogModel is one selectable provider/model-id in the first-run model picker.
type catalogModel struct {
	sel   string // "provider/model-id"
	label string // human label for display
}

// tierNames are the three model tiers in wModels order.
var tierNames = [3]string{"big", "medium", "small"}

type wizStep int

const (
	wMode   wizStep = iota
	wSetup          // first run, no provider yet: define a provider + a model per tier
	wModels         // a provider exists but tiers unset: pick a model for each tier
	wKey            // shown only if a chosen model's provider has no API key yet
	wStart
	wEnv
	wDisk      // new QEMU engagement: how much disk to allocate to the VM
	wCPUs      // new QEMU engagement: how many vCPUs to give the VM
	wMem       // new QEMU engagement: how much RAM (GB) to give the VM
	wWSLRootfs // new WSL distro, older WSL only: path to a Debian rootfs tarball to import
	wResume
	wImport
	wHistory // resume only: load the prior conversation, or start from a clean slate
	wScope
	wGoal
	wVPN
	wVPNInput
	wData // optional: a host folder to copy into the workspace for agents to read
	wAuto
)

type model struct {
	mode   config.Mode
	store  *config.Store
	roster *agents.Roster

	w, h int

	// wizard
	running     bool
	step        wizStep
	wizHistory  []wizStep // setup steps visited, for Esc = go back to the previous one
	modeSel     int
	startSel    int  // 0 = new engagement, 1 = resume previous
	resume      bool // continuing a previous engagement (offer its saved history to load)
	reuseDisk   bool // reuse an existing QEMU VM disk (vs a fresh overlay); distinct from resume
	histSel     int  // wHistory: 0 = load previous conversation, 1 = clean slate
	loadHist    bool // resume with the saved conversation history (vs a clean slate)
	autoSel     int
	scopeIn     textinput.Model
	goalIn      textinput.Model
	diskIn      textinput.Model // GB to allocate when creating a new VM (wDisk step)
	diskGB      int             // parsed result of diskIn, applied to the created VM's disk
	cpusIn      textinput.Model // vCPUs to allocate when creating a new VM (wCPUs step)
	cpus        int             // parsed result of cpusIn, applied to the created VM
	memIn       textinput.Model // RAM in GB when creating a new VM (wMem step)
	memGB       int             // parsed result of memIn, applied to the created VM
	wslRootfsIn textinput.Model // Debian rootfs tarball path (wWSLRootfs fallback step)
	newEnvName  string          // generated oline- name for a newly created environment
	createWSL   bool            // the chosen env is a NEW WSL distro to provision
	wslRootfs   string          // rootfs tarball for the new WSL distro ("" => wsl --install)
	cands       []sandbox.Candidate
	envSel      int
	scanned     bool
	wizErr      string

	// first-run model configuration (wModels step): the user must pick a model for
	// each of the three tiers from the provider catalog before an engagement can run.
	catalog   []catalogModel // available provider/model-id choices
	roleSel   [3]int         // selected catalog index per tier (0=big,1=medium,2=small)
	roleFocus int            // which tier row the wizard is on
	keyQueue  []string       // providers still needing an API key (wKey walks this)

	// first-run provider+models form (wSetup step): shown when no provider exists.
	setupRows  []*setupRow
	setupFocus int // index into setupRows of the focused row

	// preparing: a sandbox open/create + egress setup is running off the UI
	// goroutine (a QEMU first boot can take minutes); prepCand is the chosen env.
	preparing bool
	prepCand  sandbox.Candidate

	// resume
	selDistro   string
	engagements []string
	engSel      int // index into engagements, or len(engagements) == "import from host"
	engagement  string
	importIn    textinput.Model

	// vpn
	vpnSel  int
	vpnMode string // none | running | envfile | hostfile
	vpnIn   textinput.Model

	// optional host data folder copied (read-only) into the workspace before start
	dataDirIn textinput.Model
	dataDir   string // host folder to copy in; "" = none (nothing is copied or searched for)

	// api key (setup)
	keyProvider string
	keyIn       textinput.Model

	// engagement
	sess           *engine.Session
	router         *engine.Router
	events         chan event.Event
	cancel         context.CancelFunc
	resultCh       chan engine.Result
	transcripts    map[core.AgentID]*transcript
	order          []core.AgentID
	states         map[core.AgentID]core.State
	awaitingAnswer map[core.AgentID]bool // agents blocked on an `ask`, waiting for a user answer
	dirty          map[core.AgentID]bool // agents whose pane needs re-rendering after a batch

	// panes: tmux-style split view of agent transcripts. paneRoot/focusPane/zoom
	// are the ACTIVE window's live state; windows holds every window (tab), with
	// winIdx the active one. saveWindow/activateWindow swap state in and out.
	paneRoot     *pane
	focusPane    *pane
	zoom         bool
	windows      []*windowState
	winIdx       int
	showThoughts bool
	showRoster   bool // the whole right-side Options pane (on by default; Ctrl-b ? toggles)

	// shells: user-driven shell panes, keyed by synthetic id (e.g. "Shell#1").
	shellCh  chan shellMsg
	shells   map[core.AgentID]*shellState
	shellSeq int

	// Reverse-shell channels (set up by agents via the `revshell` tool) and the
	// operator panes attached to them. channels tracks every open channel in
	// insertion order; revs holds the live tail/send state of panes the user opened
	// for them (keyed by the synthetic pane id rev#<name>). Both share shellCh.
	channels   map[string]*channelInfo
	channelOrd []string
	revs       map[core.AgentID]*revState

	// Fully terminal-emulated panes (Ctrl-b T, and the reverse-shell catcher). Each
	// is a PTY session in the sandbox driven by a VT emulator; termCh carries repaint
	// nudges from the output reader goroutines to the Bubble Tea loop.
	terms       map[core.AgentID]*termState
	termSeq     int
	termCh      chan core.AgentID
	listenInput bool // the command line is entering a reverse-shell listener port
	// spawnTime records when each subagent was spawned, so the auto-layout can pick
	// the longest-running pane to reuse once all four slots are full.
	spawnTime map[core.AgentID]time.Time
	// saveReq coalesces event-driven history saves (a spawn, an agent finishing, a
	// finding) so a crash loses at most the in-flight turn, not up to the full timer.
	saveReq     chan struct{}
	histMu      sync.Mutex    // serializes saveHistory across the UI and ticker goroutines
	savedLayout []savedWindow // last UI-goroutine snapshot of the window/pane layout (under histMu)
	savedWinIdx int           // active window index for that snapshot
	hideLogo    bool          // a Sixel splash already showed the banner; skip the block-art logo
	quitArmedAt time.Time     // first Ctrl-c; a second within 3s confirms quit
	log         *englog       // per-engagement host activity log
	audit       *audit.Log    // per-engagement tamper-evident command audit log (host-side)
	backupName  string        // basename for this session's backup bundle; "" = the engagement name (only differs if a resume could not preserve the prior bundle)
	mcp         *mcp.Manager  // connected external MCP tool servers (host-side), closed at shutdown

	budgetInput   bool         // the command line is entering a time budget
	budgetTarget  core.AgentID // which agent the budget entry applies to
	totalCost     float64
	costCap       float64 // engagement spend cap in USD (budget.maxCostUSD); 0 = no cap
	costCapHit    bool    // the cap was reached and all agents paused (fires once)
	summaryCost   float64 // cumulative spend of the summarizer (compaction) model
	summaryCap    float64 // summarizer spend ceiling (budget.maxSummaryCostUSD); 0 = none
	summaryCapHit bool    // the summarizer ceiling was reached and agents paused (fires once)
	summary       string  // orchestrator's live state-of-play (set via update_summary)
	diagram       string  // orchestrator's live ASCII map (set via update_diagram)
	// diagramW is the Diagram pane's current content width in columns, published for
	// the engine's update_diagram width check. Written on the TUI goroutine whenever
	// the diagram pane is laid out, read on an agent goroutine; accessed only via
	// sync/atomic. 0 = not laid out yet / no diagram pane.
	diagramW int64
	mdCache  map[int]*glamour.TermRenderer

	// copy mode: one pane's transcript as plain fullscreen text for clean copying
	copyMode  bool
	copyAgent core.AgentID
	copyVP    viewport.Model

	// layout, computed each render (for mouse hit-testing)
	rightW             int
	mainW              int
	bodyTop            int
	bodyH              int
	rosterX            int
	rosterAgentRows    map[int]core.AgentID // absolute screen Y -> agent, for roster click hit-testing
	rosterUserPaneRows map[int]core.AgentID // absolute screen Y -> opened shell id (command panes section)
	rosterChannelRows  map[int]string       // absolute screen Y -> channel name (command panes section)
	rosterTermRows     map[int]core.AgentID // absolute screen Y -> terminal id (command panes section)
	rosterScroll       int                  // scroll offset (content lines) of the Options pane
	rosterFollow       bool                 // on the next render, scroll the Options pane to show the selection

	// mouse drag state for resizing / moving panes
	dragBoundary *pane // the internal split node whose divider is being dragged (resize)
	dragPane     *pane // the leaf pressed for a potential move/click
	dragStartX   int   // press position, to distinguish a click from a drag
	dragStartY   int
	dragMoved    bool // whether the cursor moved since the press (=> a drag, not a click)

	input  textinput.Model
	prefix bool
	status string
	envID  string
	direct bool

	// model/provider editor
	editorOnly bool // launched directly into the editor (no engagement)
	editing    bool
	eItems     []eItem
	eSel       int
	// eFocus: -1 = navigating the item list; 0..len(eFields)-1 = editing that text
	// field; len(eFields)+j = editing prompt box j (when ePrompts has entries). An
	// item may carry more than one prompt box (the Main section edits both the
	// orchestrator system prompt and the mode personality).
	eFocus        int
	eFields       []textinput.Model
	ePrompts      []*textarea.Model // resizable multi-line boxes (system prompt, personality, …)
	ePromptLabels []string          // label shown above each box in ePrompts (parallel slice)
	ePromptH      int               // current prompt-box height in rows (user-resizable)
	eStatus       string
	// Editor mouse hit-testing anchors, recorded by viewEditor each render so clicks
	// map to what is on screen: the left item list, the right text-field rows, and
	// each prompt box's row span.
	eLeftItemTop int   // terminal row of the first left-list item
	eLeftWidth   int   // width (cols) of the left box, from column 0
	eRightLeft   int   // terminal column where the right box's content starts
	eFieldRows   []int // terminal row of each text field (indexed like eFields)
	ePromptTops  []int // top (label) terminal row of each prompt box (indexed like ePrompts)
	ePromptBots  []int // bottom terminal row of each prompt box (indexed like ePrompts)
}

// configureDirect pre-fills the wizard answers and arms a direct start.
func (m *model) configureDirect(o Options) {
	m.scopeIn.SetValue(o.Scope)
	m.goalIn.SetValue(o.Goal)
	// Direct start (scripting/CI) is automatic unless -planner is passed. Set it
	// explicitly so the wizard's planner-by-default doesn't carry into this path.
	if o.Planner {
		m.autoSel = 1
	} else {
		m.autoSel = 0
	}
	m.cands = []sandbox.Candidate{{ID: o.EnvID, Name: o.EnvID, Backend: sandbox.BackendWSL}}
	m.envSel = 0
	m.scanned = true
	m.step = wEnv
	m.direct = true
}

// inputPromptReserve is how many columns resize() holds back from the command-line
// textinput's width for the prompt glyph and the cursor cell (see resize()).
const inputPromptReserve = 6

// Scope/goal placeholders differ by mode: pentester takes in-scope network
// targets, exploiter takes the application to analyze (local path or an app to
// fetch). applyModePlaceholders keeps them in step when the wizard's mode changes.
const (
	scopePlaceholderPentest = "in-scope only — e.g. 10.0.0.5, 10.0.0.0/24, app.example.com"
	scopePlaceholderExploit = "application to analyze — a local path, or a name/URL for the installer to fetch (e.g. /opt/acme-server, or \"acmed 2.3.1\")"
	goalPlaceholderPentest  = "goal — e.g. capture the flag at /root/flag.txt; prove RCE"
	goalPlaceholderExploit  = "goal — e.g. find and prove an exploitable bug; build a working PoC"
)

// applyModePlaceholders sets the scope/goal input placeholders for the current mode.
func (m *model) applyModePlaceholders() {
	if m.mode == config.ModeExploiter {
		m.scopeIn.Placeholder = scopePlaceholderExploit
		m.goalIn.Placeholder = goalPlaceholderExploit
	} else {
		m.scopeIn.Placeholder = scopePlaceholderPentest
		m.goalIn.Placeholder = goalPlaceholderPentest
	}
}

// Default VM sizing suggested in the wizard's disk/CPU/RAM steps for a new QEMU VM.
// Installing offensive tooling fills a small disk fast, so the disk default is
// generous; the CPU/RAM defaults match sandbox.Default{CPUs,MemMB}.
const (
	defaultDiskGB = 25
	defaultCPUs   = 4
	defaultMemGB  = 8
)

func newModel(mode config.Mode, store *config.Store, roster *agents.Roster) *model {
	si := textinput.New()
	si.Placeholder = scopePlaceholderPentest
	if mode == config.ModeExploiter {
		si.Placeholder = scopePlaceholderExploit
	}
	si.CharLimit = 1024
	si.Width = 64
	gi := textinput.New()
	gi.Placeholder = goalPlaceholderPentest
	if mode == config.ModeExploiter {
		gi.Placeholder = goalPlaceholderExploit
	}
	gi.Width = 64
	di := textinput.New()
	di.Placeholder = "GB for the new VM's disk (e.g. 25)"
	di.SetValue(strconv.Itoa(defaultDiskGB))
	di.CharLimit = 5
	di.Width = 24
	cp := textinput.New()
	cp.Placeholder = "vCPUs for the new VM (e.g. 4)"
	cp.SetValue(strconv.Itoa(defaultCPUs))
	cp.CharLimit = 3
	cp.Width = 24
	mi := textinput.New()
	mi.Placeholder = "GB of RAM for the new VM (e.g. 8)"
	mi.SetValue(strconv.Itoa(defaultMemGB))
	mi.CharLimit = 5
	mi.Width = 24
	ri := textinput.New()
	ri.Placeholder = "path to a Debian rootfs .tar / .tar.gz / .tar.xz"
	ri.CharLimit = 1024
	ri.Width = 64
	ci := textinput.New()
	ci.Placeholder = "message the focused agent…  (Ctrl-b then a key for commands)"
	ci.Width = 64
	ci.Prompt = "" // the running view prepends its own " > "; avoid a double ">"
	ii := textinput.New()
	ii.Placeholder = "host path (folder or .tar.gz) OR a path already inside the environment"
	ii.CharLimit = 1024
	ii.Width = 64
	vi := textinput.New()
	vi.Placeholder = "path to the .ovpn file"
	vi.CharLimit = 1024
	vi.Width = 64
	xi := textinput.New()
	xi.Placeholder = "host folder to copy into the workspace (optional — Enter to skip)"
	xi.CharLimit = 1024
	xi.Width = 64
	ki := textinput.New()
	ki.Placeholder = "paste your API key (stored only in ~/oline/config/secrets.yml)"
	ki.CharLimit = 512
	ki.Width = 64
	ki.EchoMode = textinput.EchoPassword

	m := &model{
		mode: mode, store: store, roster: roster,
		step:           wMode,
		scopeIn:        si,
		goalIn:         gi,
		diskIn:         di,
		cpusIn:         cp,
		memIn:          mi,
		wslRootfsIn:    ri,
		input:          ci,
		importIn:       ii,
		vpnIn:          vi,
		dataDirIn:      xi,
		keyIn:          ki,
		transcripts:    map[core.AgentID]*transcript{},
		dirty:          map[core.AgentID]bool{},
		states:         map[core.AgentID]core.State{},
		awaitingAnswer: map[core.AgentID]bool{},
		spawnTime:      map[core.AgentID]time.Time{},
		showThoughts:   false, // agent thoughts are hidden by default (Ctrl-b t toggles)
		showRoster:     true,  // the right-side Options pane shows by default (Ctrl-b ? toggles)
		autoSel:        1,     // planner mode is selected by default in the wizard (0 = automatic, 1 = planner, 2 = watcher)
		mdCache:        map[int]*glamour.TermRenderer{},
	}
	if mode == config.ModeExploiter {
		m.modeSel = 1
	}
	return m
}

func (m *model) Init() tea.Cmd {
	if m.editorOnly {
		m.openEditor()
		return textinput.Blink
	}
	if m.direct {
		return func() tea.Msg { return startMsg{} }
	}
	return textinput.Blink
}

// ---- update --------------------------------------------------------------

func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.w, m.h = msg.Width, msg.Height
		m.resize()
		return m, nil
	case tea.KeyMsg:
		if msg.String() == "ctrl+c" {
			// Don't quit on a single Ctrl-c (easy to hit by accident and it would
			// tear down a live engagement). Require a second Ctrl-c within a few
			// seconds to confirm.
			if !m.quitArmedAt.IsZero() && time.Since(m.quitArmedAt) < 3*time.Second {
				m.shutdown()
				return m, tea.Quit
			}
			m.quitArmedAt = time.Now()
			m.status = "Press Ctrl-c again to quit (or Ctrl-b q)"
			return m, nil
		}
	case startMsg:
		return m.startEngagement()
	}
	if m.editing {
		return m.updateEditor(msg)
	}
	if m.running {
		return m.updateRunning(msg)
	}
	return m.updateWizard(msg)
}

func (m *model) resize() {
	if m.showRoster {
		// 33 is the narrowest width that fits the widest roster line without wrapping:
		// an active-agent row is cursor(2) + name(10) + " " + state(≤7) + " mm:ss ctx%"
		// badge (≤11) = 31 glyphs, +2 for the pane border. Keeping it tight hands the
		// freed columns to the main panes.
		m.rightW = 33
		if m.w < 96 {
			m.rightW = m.w / 3
		}
		if m.rightW < 18 {
			m.rightW = 18
		}
		m.mainW = m.w - m.rightW - 1 // -1 for the gap column between main and roster
	} else {
		// Options pane hidden: the main panes take the whole width.
		m.rightW = 0
		m.mainW = m.w
	}
	m.bodyTop = 1
	m.bodyH = m.h - 3 // header line + status + input
	if m.bodyH < 3 {
		m.bodyH = 3
	}
	if m.mainW < 20 {
		m.mainW = 20
	}
	m.rosterX = m.mainW + 1
	// The input line is `<prompt><textinput view>`. Reserve room for the prompt and
	// the cursor cell so the composed line never exceeds m.w: an over-wide line wraps
	// onto a second row that falls off the bottom, hiding the end of a long entry.
	// The prompt varies by mode and the widest is a 2-cell emoji with two spaces
	// (" ❓ " when answering, " ⏱ " for a budget) = 4 columns; +1 for the trailing
	// cursor cell and +1 of slack = 6.
	if w := m.w - inputPromptReserve; w > 0 {
		m.input.Width = w
	} else {
		m.input.Width = 1
	}
	m.sizeWizardInputs()
	m.relayout()
}

// sizeWizardInputs fits the setup-wizard text inputs to the terminal width so long
// values (paths, scope lists, provider URLs, API keys) are shown in full instead of
// being truncated to a fixed window on a wide screen. Each input is drawn inside the
// wizard's Padding(1,2) with a few spaces of indent plus the input's own "> " prompt
// and cursor cell, so reserve for all of that to keep the composed line within m.w
// (otherwise the outer screen style would wrap it). Short numeric fields (disk / CPUs
// / RAM) are left narrow on purpose.
func (m *model) sizeWizardInputs() {
	w := m.w - 14
	if w < 20 {
		w = 20
	}
	for _, ti := range []*textinput.Model{&m.scopeIn, &m.goalIn, &m.wslRootfsIn, &m.importIn, &m.vpnIn, &m.dataDirIn, &m.keyIn} {
		ti.Width = w
	}
	// The first-run provider/model form indents its inputs two columns deeper.
	sw := m.w - 16
	if sw < 20 {
		sw = 20
	}
	for _, r := range m.setupRows {
		if !r.toggle {
			r.ti.Width = sw
		}
	}
}

// relayout recomputes pane rectangles and refreshes their content.
func (m *model) relayout() {
	if m.paneRoot == nil {
		return
	}
	if m.zoom && m.focusPane != nil {
		computeRects(m.focusPane, 0, m.bodyTop, m.mainW, m.bodyH)
		m.refreshLeaf(m.focusPane)
		return
	}
	computeRects(m.paneRoot, 0, m.bodyTop, m.mainW, m.bodyH)
	for _, l := range leaves(m.paneRoot) {
		m.refreshLeaf(l)
	}
}

// layoutRectsOnly recomputes pane rectangles and resizes each viewport WITHOUT
// re-rendering its (markdown) content. It is used during a live resize drag so a
// burst of mouse-motion events only moves the borders — the expensive markdown
// reflow is deferred to the drag's release (a single relayout). Content keeps its
// previous wrapping until then, which is imperceptible mid-drag.
func (m *model) layoutRectsOnly() {
	if m.paneRoot == nil {
		return
	}
	root := m.paneRoot
	if m.zoom && m.focusPane != nil {
		root = m.focusPane
	}
	computeRects(root, 0, m.bodyTop, m.mainW, m.bodyH)
	for _, l := range leaves(root) {
		vpW, vpH := l.w-2, l.h-3
		if vpW < 1 {
			vpW = 1
		}
		if vpH < 1 {
			vpH = 1
		}
		l.vp.Width, l.vp.Height = vpW, vpH
	}
}

// refreshLeaf sizes a pane's viewport and re-renders its agent's transcript.
func (m *model) refreshLeaf(p *pane) {
	vpW := p.w - 2
	vpH := p.h - 3 // border (2) + header (1)
	if vpW < 1 {
		vpW = 1
	}
	if vpH < 1 {
		vpH = 1
	}
	p.vp.Width, p.vp.Height = vpW, vpH
	content := ""
	switch {
	case p.summary:
		content = m.summaryContent(vpW)
	case p.diagram:
		// Publish the pane's live content width so update_diagram can reject a map
		// that would be clipped; a resize re-lays-out the pane and updates it here.
		atomic.StoreInt64(&m.diagramW, int64(vpW))
		content = m.diagramContent(vpW)
	default:
		if t := m.transcripts[p.agent]; t != nil {
			// Trim the trailing newline the transcript appends after the last segment:
			// otherwise the viewport counts an extra blank line and GotoBottom shifts the
			// real last line partly out of view (looks like the bottom is cut off).
			content = strings.TrimRight(t.render(vpW, m.showThoughts, m.getMD(vpW)), "\n")
		}
	}
	// The info panes (summary, diagram) pin to the TOP: the orchestrator rewrites
	// them in place, so the top is where to start reading. Stay pinned only while the
	// user is already at the top, so scrolling down to read the rest isn't yanked
	// back. Agent/shell panes keep their follow-the-bottom behavior.
	if p.summary || p.diagram {
		atTop := p.vp.AtTop()
		p.vp.SetContent(content)
		if atTop {
			p.vp.GotoTop()
		}
		return
	}
	atBottom := p.vp.AtBottom()
	p.vp.SetContent(content)
	if atBottom {
		p.vp.GotoBottom()
	}
}

// refreshFor re-renders every pane currently showing an agent.
func (m *model) refreshFor(id core.AgentID) {
	if m.paneRoot == nil {
		return
	}
	for _, l := range leaves(m.paneRoot) {
		if l.agent == id {
			m.refreshLeaf(l)
		}
	}
}

// getMD returns a markdown renderer sized for width w (cached). The cache is keyed
// by width; only a handful of distinct widths occur in practice, but bound it so a
// pathological resize storm can't grow it without limit.
func (m *model) getMD(w int) *glamour.TermRenderer {
	if r, ok := m.mdCache[w]; ok {
		return r
	}
	const maxMDRenderers = 24
	if len(m.mdCache) >= maxMDRenderers {
		m.mdCache = map[int]*glamour.TermRenderer{} // drop the lot; they rebuild on demand
	}
	r := newMarkdownRenderer(w)
	m.mdCache[w] = r
	return r
}

func (m *model) focusedAgent() core.AgentID {
	if m.focusPane != nil {
		return m.focusPane.agent
	}
	return core.Main
}

// focusInfoPane reports whether the focused pane is a read-only info pane (Summary
// or Diagram) rather than an agent or shell pane.
func (m *model) focusInfoPane() bool {
	return m.focusPane != nil && (m.focusPane.summary || m.focusPane.diagram)
}

// ---- wizard --------------------------------------------------------------

func (m *model) updateWizard(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case scanDoneMsg:
		m.scanned = true
		m.cands = managedFirst(msg.cands)
		if m.envSel >= len(m.cands) {
			m.envSel = 0 // a rescan may return fewer candidates than were listed before
		}
		if msg.err != nil {
			m.wizErr = "scan error: " + msg.err.Error()
		}
		return m, nil
	case engListMsg:
		m.engagements = msg.names
		m.engSel = 0
		if msg.err != nil {
			m.wizErr = "list engagements: " + msg.err.Error()
		}
		return m, nil
	case sandboxReadyMsg:
		m.preparing = false
		if msg.err != nil {
			m.wizErr = msg.err.Error()
			return m, nil
		}
		return m.beginSession(msg.h)
	case tea.KeyMsg:
		if m.preparing {
			return m, nil // ignore input while the sandbox is being prepared
		}
		// Esc goes back to the previous setup question (no-op on the first one).
		if msg.String() == "esc" {
			return m.wizardBack()
		}
		// Record a forward step transition so Esc can return to where we came from.
		prev := m.step
		res, cmd := m.wizardKey(msg)
		if m.step != prev && !m.running {
			m.wizHistory = append(m.wizHistory, prev)
		}
		return res, cmd
	}
	return m, nil
}

// wizardBack returns to the previous setup step (restoring its input focus),
// popping the step history. It is a no-op on the first question.
func (m *model) wizardBack() (tea.Model, tea.Cmd) {
	if len(m.wizHistory) == 0 {
		return m, nil
	}
	m.step = m.wizHistory[len(m.wizHistory)-1]
	m.wizHistory = m.wizHistory[:len(m.wizHistory)-1]
	m.wizErr = ""
	m.focusWizardStep(m.step)
	return m, nil
}

// focusWizardStep focuses the text input (or form) a setup step edits, so returning
// to it with Esc lands the cursor in the right field.
func (m *model) focusWizardStep(s wizStep) {
	for _, ti := range []*textinput.Model{&m.scopeIn, &m.goalIn, &m.diskIn, &m.cpusIn, &m.memIn, &m.wslRootfsIn, &m.importIn, &m.vpnIn, &m.dataDirIn, &m.keyIn} {
		ti.Blur()
	}
	switch s {
	case wData:
		m.dataDirIn.Focus()
	case wScope:
		m.scopeIn.Focus()
	case wGoal:
		m.goalIn.Focus()
	case wDisk:
		m.diskIn.Focus()
	case wCPUs:
		m.cpusIn.Focus()
	case wMem:
		m.memIn.Focus()
	case wWSLRootfs:
		m.wslRootfsIn.Focus()
	case wImport:
		m.importIn.Focus()
	case wVPNInput:
		m.vpnIn.Focus()
	case wKey:
		m.keyIn.Focus()
	case wSetup:
		m.syncSetupFocus()
	}
}

// gotoData advances to the optional "copy a host folder into the workspace" step,
// focusing its input.
func (m *model) gotoData() {
	m.step = wData
	m.dataDirIn.Focus()
}

// afterFreshSandboxConfig continues after a brand-new sandbox has been configured (a
// sized QEMU VM, or a named/rootfs WSL distro). If the user chose to resume a previous
// engagement, it offers the engagements whose host-saved conversation history can be
// loaded onto the fresh sandbox (the history — and the backed-up workspace — live on
// the host, independent of the new disk/distro); otherwise it proceeds to the scope
// step for a clean start.
func (m *model) afterFreshSandboxConfig() {
	if m.resume {
		m.engagements = savedEngagements()
		if len(m.engagements) > 0 {
			m.engSel = 0
			m.step = wResume
			return
		}
		m.resume = false // nothing saved to load — this is just a new engagement
	}
	m.step = wScope
	m.scopeIn.Focus()
}

// savedEngagements lists engagements that have a non-empty saved conversation history
// on the host (so they can be loaded/continued), newest names sorted.
func savedEngagements() []string {
	entries, err := os.ReadDir(config.HistoryDir())
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		if info, ierr := e.Info(); ierr != nil || info.Size() == 0 {
			continue
		}
		out = append(out, strings.TrimSuffix(e.Name(), ".json"))
	}
	sort.Strings(out)
	return out
}

func (m *model) wizardKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	k := msg.String()
	switch m.step {
	case wMode:
		switch k {
		case "up", "k":
			m.modeSel = 0
		case "down", "j":
			m.modeSel = 1
		case "enter":
			if m.modeSel == 1 {
				m.mode = config.ModeExploiter
			} else {
				m.mode = config.ModePentester
			}
			m.reloadForMode()
			m.buildCatalog()
			if !m.store.ModelsConfigured() {
				m.gotoModelSetup()
			} else {
				m.gotoKeyOrStart()
			}
		}
		return m, nil

	case wSetup:
		r := m.setupRows[m.setupFocus]
		switch k {
		case "up", "shift+tab":
			m.moveSetupFocus(-1)
			return m, nil
		case "down", "tab":
			m.moveSetupFocus(1)
			return m, nil
		case "enter":
			m.submitSetupForm()
			return m, nil
		}
		if r.toggle {
			switch k {
			case "left", "right", " ", "h", "l":
				r.on = !r.on
				m.syncSetupFocus() // toggling may show/hide rows
			}
			return m, nil
		}
		var cmd tea.Cmd
		r.ti, cmd = r.ti.Update(msg)
		return m, cmd

	case wModels:
		switch k {
		case "up", "k":
			if m.roleFocus > 0 {
				m.roleFocus--
			}
		case "down", "j":
			if m.roleFocus < 2 {
				m.roleFocus++
			}
		case "left", "h":
			if len(m.catalog) > 0 {
				m.roleSel[m.roleFocus] = (m.roleSel[m.roleFocus] - 1 + len(m.catalog)) % len(m.catalog)
			}
		case "right", "l", " ":
			if len(m.catalog) > 0 {
				m.roleSel[m.roleFocus] = (m.roleSel[m.roleFocus] + 1) % len(m.catalog)
			}
		case "enter":
			if len(m.catalog) == 0 {
				m.wizErr = "no models available in models.yml — add a provider/model there first"
				return m, nil
			}
			for i, tier := range tierNames {
				if err := m.store.SaveModelRole(tier, m.catalog[m.roleSel[i]].sel); err != nil {
					m.wizErr = "save model: " + err.Error()
					return m, nil
				}
			}
			m.wizErr = ""
			m.gotoKeyOrStart()
		}
		return m, nil

	case wKey:
		if k == "enter" {
			if v := strings.TrimSpace(m.keyIn.Value()); v != "" {
				if err := m.store.SaveSecret(m.keyProvider, v); err != nil {
					m.wizErr = "save key: " + err.Error()
					return m, nil
				}
				m.wizErr = ""
			}
			m.keyIn.Blur()
			m.keyIn.SetValue("")
			// Advance through any further providers that still need a key, else start.
			if len(m.keyQueue) > 0 {
				m.keyQueue = m.keyQueue[1:]
			}
			m.gotoKeyOrStart()
			return m, nil
		}
		var cmd tea.Cmd
		m.keyIn, cmd = m.keyIn.Update(msg)
		return m, cmd

	case wStart:
		switch k {
		case "up", "k":
			m.startSel = 0
		case "down", "j":
			m.startSel = 1
		case "enter":
			m.resume = m.startSel == 1
			m.step = wEnv
			if !m.scanned {
				return m, scanEnvsCmd()
			}
		}
		return m, nil

	case wEnv:
		switch k {
		case "up", "k":
			if m.envSel > 0 {
				m.envSel--
			}
		case "down", "j":
			if m.envSel < len(m.cands)-1 {
				m.envSel++
			}
		case "r":
			m.scanned = false
			return m, scanEnvsCmd()
		case "enter":
			if len(m.cands) == 0 {
				m.wizErr = "no environments found. Press r to rescan."
				return m, nil
			}
			cand := m.cands[m.envSel]
			if cand.Backend == sandbox.BackendQEMU && !sandbox.QEMUAvailable() {
				m.wizErr = "QEMU not found — install QEMU (on PATH or in Program Files), or pick a WSL distro."
				return m, nil
			}
			m.selDistro = cand.ID
			m.createWSL = false
			m.reuseDisk = false // only a saved-QEMU-VM pick (below) reuses a disk
			m.newEnvName = ""
			m.wslRootfs = ""
			// QEMU: an existing VM (a persisted engagement's disk) chosen from the list is
			// reused by resuming it — the parallel to picking an existing WSL distro. The
			// list selection is authoritative, overriding the Start step's new/resume
			// choice (picking a saved VM always resumes it).
			if cand.Backend == sandbox.BackendQEMU && cand.Engagement != "" {
				m.resume = true
				m.reuseDisk = true // reuse this saved VM's disk
				m.engagement = cand.Engagement
				if historyExists(m.engagement) {
					m.histSel = 0
					m.step = wHistory
				} else {
					m.step = wScope
					m.scopeIn.Focus()
				}
				return m, nil
			}
			// WSL resume: pick an existing distro here, then choose which saved engagement
			// inside it. The "create a new distro" entry is NOT a resume target, so let it
			// fall through to the create branch even when the Start step chose resume.
			if m.resume && cand.Backend == sandbox.BackendWSL && !cand.NeedsCreate {
				m.step = wResume
				return m, listEngagementsCmd(cand)
			}
			// Create a fresh oline-managed WSL distro. Generate its oline- name now;
			// it appears in the reuse list on later runs and is a Destroy target. The
			// distro is fresh, but a Start-step "resume" still holds: its workspace
			// isn't in this new distro, yet a previous engagement's host-saved history
			// (and backed-up workspace) can be loaded onto it — afterFreshSandboxConfig
			// offers the saved engagements, exactly as for a new QEMU VM.
			if cand.Backend == sandbox.BackendWSL && cand.NeedsCreate {
				m.engagement = ""
				m.createWSL = true
				m.newEnvName = sandbox.Prefix + "debian-" + time.Now().Format("20060102-150405")
				m.selDistro = m.newEnvName
				// If this WSL can install under a custom name, no tarball is needed;
				// otherwise collect a Debian rootfs tarball to import first.
				if wc, ok := sandbox.NewWSL().(sandbox.WSLControl); ok && wc.InstallSupportsName(context.Background()) {
					m.afterFreshSandboxConfig()
					return m, nil
				}
				m.step = wWSLRootfs
				m.wslRootfsIn.Focus()
				return m, nil
			}
			// QEMU: create a new engagement on a fresh disposable overlay of the base
			// image; size it here (default 25 GB) so tool installs don't fill a tiny
			// base — otherwise the guest root is only the base image's ~2 GB. The disk is
			// fresh, but a Start-step "resume" still holds: after sizing,
			// afterFreshSandboxConfig offers the prior engagement to load onto this VM.
			if cand.Backend == sandbox.BackendQEMU {
				// A fresh overlay off the base — not a saved disk. But keep the user's
				// "resume a previous engagement" intent: creating a new VM should NOT
				// cancel loading a previous engagement's host-saved history (the history
				// lives on the host, independent of the disposable VM disk). After sizing
				// the VM, afterFreshSandboxConfig offers the saved engagements to load.
				m.reuseDisk = false
				m.engagement = ""
				m.step = wDisk
				m.diskIn.Focus()
				return m, nil
			}
			// Reuse an existing WSL distro as-is (a new engagement inside it).
			m.step = wScope
			m.scopeIn.Focus()
		}
		return m, nil

	case wDisk:
		if k == "enter" {
			v := strings.TrimSpace(m.diskIn.Value())
			n, err := strconv.Atoi(v)
			if err != nil || n < 1 || n > 10000 {
				m.wizErr = "enter a whole number of GB (1-10000), e.g. 20"
				return m, nil
			}
			m.diskGB = n
			m.wizErr = ""
			m.diskIn.Blur()
			m.step = wCPUs
			m.cpusIn.Focus()
			return m, nil
		}
		var cmd tea.Cmd
		m.diskIn, cmd = m.diskIn.Update(msg)
		return m, cmd

	case wCPUs:
		if k == "enter" {
			v := strings.TrimSpace(m.cpusIn.Value())
			n, err := strconv.Atoi(v)
			if err != nil || n < 1 || n > 128 {
				m.wizErr = "enter a whole number of vCPUs (1-128), e.g. 4"
				return m, nil
			}
			m.cpus = n
			m.wizErr = ""
			m.cpusIn.Blur()
			m.step = wMem
			m.memIn.Focus()
			return m, nil
		}
		var cmd tea.Cmd
		m.cpusIn, cmd = m.cpusIn.Update(msg)
		return m, cmd

	case wMem:
		if k == "enter" {
			v := strings.TrimSpace(m.memIn.Value())
			n, err := strconv.Atoi(v)
			if err != nil || n < 1 || n > 1024 {
				m.wizErr = "enter a whole number of GB of RAM (1-1024), e.g. 8"
				return m, nil
			}
			m.memGB = n
			m.wizErr = ""
			m.memIn.Blur()
			m.afterFreshSandboxConfig()
			return m, nil
		}
		var cmd tea.Cmd
		m.memIn, cmd = m.memIn.Update(msg)
		return m, cmd

	case wWSLRootfs:
		if k == "enter" {
			p := strings.TrimSpace(m.wslRootfsIn.Value())
			if p == "" {
				m.wizErr = "enter the path to a Debian rootfs tarball (.tar / .tar.gz / .tar.xz)"
				return m, nil
			}
			m.wslRootfs = p
			m.wizErr = ""
			m.wslRootfsIn.Blur()
			m.afterFreshSandboxConfig()
			return m, nil
		}
		var cmd tea.Cmd
		m.wslRootfsIn, cmd = m.wslRootfsIn.Update(msg)
		return m, cmd

	case wResume:
		n := len(m.engagements) // index of the "import from host" option
		switch k {
		case "up", "k":
			if m.engSel > 0 {
				m.engSel--
			}
		case "down", "j":
			if m.engSel < n {
				m.engSel++
			}
		case "enter":
			if m.engSel == n { // import from host
				m.step = wImport
				m.importIn.Focus()
				return m, nil
			}
			m.engagement = m.engagements[m.engSel]
			// Prefill the scope field with the saved targets as a convenience, but
			// still ask: a reverted lab may have changed the target IPs. QEMU resumes
			// from a VM disk (no host-readable scope file), so prefill only for WSL.
			if m.cands[m.envSel].Backend != sandbox.BackendQEMU {
				if data, err := sandbox.ReadWSLFile(context.Background(), m.selDistro, sandbox.WSLWorkspace+"/"+m.engagement+"/00-scope.md"); err == nil && data != "" {
					m.scopeIn.SetValue(strings.Join(scope.Parse(data).CIDRs(), ", "))
				}
			}
			// If this engagement has saved conversation history, offer to load it or
			// start clean; otherwise there is nothing to load, so go straight on.
			if historyExists(m.engagement) {
				m.histSel = 0
				m.step = wHistory
			} else {
				m.step = wScope
				m.scopeIn.Focus()
			}
		}
		return m, nil

	case wHistory:
		switch k {
		case "up", "k":
			m.histSel = 0
		case "down", "j":
			m.histSel = 1
		case "enter":
			m.loadHist = m.histSel == 0
			m.step = wScope
			m.scopeIn.Focus()
		}
		return m, nil

	case wImport:
		if k == "enter" {
			p := strings.TrimSpace(m.importIn.Value())
			if p == "" {
				m.wizErr = "enter a host path to a folder or .tar.gz"
				return m, nil
			}
			m.engagement = engNameFromPath(p)
			m.importIn.Blur()
			m.step = wScope // still confirm scope (imported scope may be stale)
			m.scopeIn.Focus()
			return m, nil
		}
		var cmd tea.Cmd
		m.importIn, cmd = m.importIn.Update(msg)
		return m, cmd

	case wScope:
		if k == "enter" {
			m.scopeIn.Blur()
			if m.resume {
				// The saved goal is kept; this step takes optional extra instructions.
				m.goalIn.SetValue("")
				m.goalIn.Placeholder = "extra instructions for this resume (optional — Enter to skip)"
			} else {
				m.goalIn.Placeholder = "the engagement goal"
			}
			m.step = wGoal
			m.goalIn.Focus()
			return m, nil
		}
		var cmd tea.Cmd
		m.scopeIn, cmd = m.scopeIn.Update(msg)
		return m, cmd

	case wGoal:
		if k == "enter" {
			m.goalIn.Blur()
			m.step = wVPN
			return m, nil
		}
		var cmd tea.Cmd
		m.goalIn, cmd = m.goalIn.Update(msg)
		return m, cmd

	case wVPN:
		switch k {
		case "up", "k":
			if m.vpnSel > 0 {
				m.vpnSel--
			}
		case "down", "j":
			if m.vpnSel < 3 {
				m.vpnSel++
			}
		case "enter":
			switch m.vpnSel {
			case 0:
				m.vpnMode = "none"
				m.gotoData()
			case 1:
				m.vpnMode = "running"
				m.gotoData()
			case 2:
				m.vpnMode = "envfile"
				m.vpnIn.Placeholder = "path to the .ovpn file inside the environment"
				m.vpnIn.SetValue("")
				m.vpnIn.Focus()
				m.step = wVPNInput
			case 3:
				m.vpnMode = "hostfile"
				m.vpnIn.Placeholder = "host path to the .ovpn file to upload"
				m.vpnIn.SetValue("")
				m.vpnIn.Focus()
				m.step = wVPNInput
			}
		}
		return m, nil

	case wVPNInput:
		if k == "enter" {
			if strings.TrimSpace(m.vpnIn.Value()) == "" {
				m.wizErr = "enter the .ovpn path"
				return m, nil
			}
			m.vpnIn.Blur()
			m.gotoData()
			return m, nil
		}
		var cmd tea.Cmd
		m.vpnIn, cmd = m.vpnIn.Update(msg)
		return m, cmd

	case wData:
		if k == "enter" {
			p := strings.TrimSpace(m.dataDirIn.Value())
			if p == "" { // optional — skip, and never go looking for a folder
				m.dataDir = ""
				m.dataDirIn.Blur()
				m.step = wAuto
				return m, nil
			}
			info, err := os.Stat(p)
			if err != nil || !info.IsDir() {
				m.wizErr = "not a readable folder on this host (leave blank to skip)"
				return m, nil
			}
			m.dataDir = filepath.Clean(p)
			m.wizErr = ""
			m.dataDirIn.Blur()
			m.step = wAuto
			return m, nil
		}
		var cmd tea.Cmd
		m.dataDirIn, cmd = m.dataDirIn.Update(msg)
		return m, cmd

	case wAuto:
		switch k {
		case "up", "k":
			if m.autoSel > 0 {
				m.autoSel--
			}
		case "down", "j":
			if m.autoSel < 2 {
				m.autoSel++
			}
		case "enter":
			return m.startEngagement()
		}
		return m, nil
	}
	return m, nil
}

// buildCatalog gathers every provider/model-id the config knows about, so the
// first-run picker and the editor can offer them as choices.
func (m *model) buildCatalog() {
	m.catalog = m.catalog[:0]
	provs := make([]string, 0, len(m.store.Providers()))
	for name := range m.store.Providers() {
		provs = append(provs, name)
	}
	sort.Strings(provs)
	for _, pn := range provs {
		p := m.store.Providers()[pn]
		for _, md := range p.Models {
			sel := pn + "/" + md.ID
			label := sel
			if md.Name != "" {
				label = md.Name + "  (" + sel + ")"
			}
			m.catalog = append(m.catalog, catalogModel{sel: sel, label: label})
		}
	}
}

// seedRoleSel points each tier's picker at its currently-configured model (if any),
// else leaves it at the first catalog entry.
func (m *model) seedRoleSel() {
	roles := m.store.ModelRoles()
	cur := [3]string{roles.Big, roles.Medium, roles.Small}
	for i := range cur {
		m.roleSel[i] = 0
		for j, c := range m.catalog {
			if c.sel == cur[i] {
				m.roleSel[i] = j
				break
			}
		}
	}
}

// providersNeedingKeys returns the providers used by the three configured tier
// models whose API key is not yet resolvable, in a stable order.
func (m *model) providersNeedingKeys() []string {
	roles := m.store.ModelRoles()
	var out []string
	seen := map[string]bool{}
	for _, sel := range []string{roles.Big, roles.Medium, roles.Small} {
		prov, _, ok := strings.Cut(sel, "/")
		if !ok || prov == "" || seen[prov] {
			continue
		}
		seen[prov] = true
		if _, err := m.store.ResolveKey(prov); err != nil {
			out = append(out, prov)
		}
	}
	return out
}

// gotoKeyOrStart moves to the API-key step for the next provider that needs a key,
// or to the engagement step when every needed key is present.
func (m *model) gotoKeyOrStart() {
	if m.keyQueue == nil || m.step != wKey {
		m.keyQueue = m.providersNeedingKeys()
	}
	if len(m.keyQueue) > 0 {
		m.keyProvider = m.keyQueue[0]
		m.keyIn.SetValue("")
		m.keyIn.Focus()
		m.step = wKey
		return
	}
	m.step = wStart
}

// gotoModelSetup enters the right first-run model-config step: the full
// provider+models form when no provider is configured, otherwise the per-tier
// picker over the existing catalog.
func (m *model) gotoModelSetup() {
	if len(m.store.Providers()) == 0 {
		m.initSetupForm()
		m.step = wSetup
		return
	}
	m.seedRoleSel()
	m.roleFocus = 0
	m.step = wModels
}

// setupRow is one row of the first-run provider+models form (wSetup): a text field
// or a yes/no toggle, optionally preceded by a section header.
type setupRow struct {
	key    string // stable id, e.g. "provider.name", "big.id", "medium.same"
	label  string
	header string // section header shown above this row (empty = none)
	toggle bool
	ti     textinput.Model
	on     bool // toggle state
}

// initSetupForm builds the provider+models form, prefilled with an OpenAI example.
func (m *model) initSetupForm() {
	text := func(key, label, header, val string, mask bool) *setupRow {
		ti := textinput.New()
		ti.SetValue(val)
		ti.Width = 48
		if mask {
			ti.EchoMode = textinput.EchoPassword
		}
		return &setupRow{key: key, label: label, header: header, ti: ti}
	}
	toggle := func(key, label, header string, on bool) *setupRow {
		return &setupRow{key: key, label: label, header: header, toggle: true, on: on}
	}
	// A model's config rows (id, limits, reasoning, prices), shared by every tier.
	modelRows := func(name, header, idPrefill string) []*setupRow {
		return []*setupRow{
			text(name+".id", "Model id", header, idPrefill, false),
			text(name+".context", "Context window (tokens)", "", "128000", false),
			text(name+".maxTokens", "Max output tokens", "", "16384", false),
			toggle(name+".reasoning", "Reasoning model (send a thinking effort)", "", false),
			text(name+".efforts", "Thinking efforts (comma-separated)", "", "low,medium,high", false),
			text(name+".defaultEffort", "Default thinking effort", "", "medium", false),
			text(name+".in", "Price: input $ per 1M tokens", "", "0", false),
			text(name+".out", "Price: output $ per 1M tokens", "", "0", false),
			text(name+".cacheRead", "Price: cache-read $ per 1M tokens", "", "0", false),
			text(name+".cacheWrite", "Price: cache-write $ per 1M tokens", "", "0", false),
		}
	}
	rows := []*setupRow{
		text("provider.name", "Provider name", "Inference provider (OpenAI-compatible)", "openai", false),
		text("provider.baseURL", "Base URL", "", "https://api.openai.com/v1", false),
		text("provider.key", "API key", "", "", true),
	}
	rows = append(rows, modelRows("big", "Big tier model (orchestrator, exploiters, thinkers)", "gpt-4o")...)
	rows = append(rows, toggle("medium.same", "Use the same model as the big tier", "Medium tier model (recon, installer, reporter)", true))
	rows = append(rows, modelRows("medium", "", "gpt-4o-mini")...)
	rows = append(rows, toggle("small.same", "Use the same model as the medium tier", "Small tier model (summaries, context compaction)", true))
	rows = append(rows, modelRows("small", "", "gpt-4o-mini")...)
	m.setupRows = rows
	m.setupFocus = 0
	m.sizeWizardInputs() // fit the just-built form fields to the current terminal width
	m.syncSetupFocus()
}

func (m *model) setupRowByKey(key string) *setupRow {
	for _, r := range m.setupRows {
		if r.key == key {
			return r
		}
	}
	return nil
}

func (m *model) setupToggleOn(key string) bool {
	if r := m.setupRowByKey(key); r != nil {
		return r.on
	}
	return false
}

func (m *model) setupVal(key string) string {
	if r := m.setupRowByKey(key); r != nil {
		return strings.TrimSpace(r.ti.Value())
	}
	return ""
}

// setupRowVisible hides a tier's model fields when it reuses another tier's model,
// and a model's thinking-effort fields when it is not a reasoning model.
func (m *model) setupRowVisible(r *setupRow) bool {
	tierField := func(prefix string) bool { return strings.HasPrefix(r.key, prefix+".") && r.key != prefix+".same" }
	if tierField("medium") && m.setupToggleOn("medium.same") {
		return false
	}
	if tierField("small") && m.setupToggleOn("small.same") {
		return false
	}
	if strings.HasSuffix(r.key, ".efforts") || strings.HasSuffix(r.key, ".defaultEffort") {
		tier := strings.SplitN(r.key, ".", 2)[0]
		if !m.setupToggleOn(tier + ".reasoning") {
			return false
		}
	}
	return true
}

// moveSetupFocus moves focus to the next/previous visible row.
func (m *model) moveSetupFocus(d int) {
	for i := m.setupFocus + d; i >= 0 && i < len(m.setupRows); i += d {
		if m.setupRowVisible(m.setupRows[i]) {
			m.setupFocus = i
			m.syncSetupFocus()
			return
		}
	}
}

// syncSetupFocus keeps focus on a visible row and focuses that row's text input.
func (m *model) syncSetupFocus() {
	if m.setupFocus < 0 || m.setupFocus >= len(m.setupRows) || !m.setupRowVisible(m.setupRows[m.setupFocus]) {
		m.setupFocus = 0
		for i, r := range m.setupRows {
			if m.setupRowVisible(r) {
				m.setupFocus = i
				break
			}
		}
	}
	for _, r := range m.setupRows {
		r.ti.Blur()
	}
	if r := m.setupRows[m.setupFocus]; !r.toggle {
		r.ti.Focus()
	}
}

// submitSetupForm validates the form, writes the provider and its models to the
// user models.yml, saves the API key, assigns the three tiers, and advances.
func (m *model) submitSetupForm() {
	name := m.setupVal("provider.name")
	if name == "" || strings.ContainsAny(name, "/ \t") {
		m.wizErr = "provider name is required and may not contain spaces or '/'"
		return
	}
	baseURL := m.setupVal("provider.baseURL")
	if baseURL == "" {
		m.wizErr = "base URL is required"
		return
	}

	// Resolve each tier to a model config, honouring the "same as" reuse toggles.
	models := map[string]config.ModelDef{}
	var order []string
	tierModel := map[string]string{}
	resolve := func(tier string) error {
		src := tier
		if tier == "medium" && m.setupToggleOn("medium.same") {
			src = "big"
		}
		if tier == "small" && m.setupToggleOn("small.same") {
			src = "medium"
			if m.setupToggleOn("medium.same") {
				src = "big"
			}
		}
		if src != tier { // reuse the already-resolved source tier's model
			tierModel[tier] = tierModel[src]
			return nil
		}
		id := m.setupVal(tier + ".id")
		if id == "" {
			return fmt.Errorf("%s tier model id is required", tier)
		}
		ctxTok, err := setupInt(m.setupVal(tier + ".context"))
		if err != nil {
			return fmt.Errorf("%s context window must be a whole number", tier)
		}
		maxTok, err := setupInt(m.setupVal(tier + ".maxTokens"))
		if err != nil {
			return fmt.Errorf("%s max output tokens must be a whole number", tier)
		}
		prices := map[string]float64{}
		for _, f := range []string{"in", "out", "cacheRead", "cacheWrite"} {
			v, perr := setupFloat(m.setupVal(tier + "." + f))
			if perr != nil {
				return fmt.Errorf("%s prices must be non-negative numbers", tier)
			}
			prices[f] = v
		}
		md := config.ModelDef{ID: id, Name: id, ContextWindow: ctxTok, MaxTokens: maxTok}
		if m.setupToggleOn(tier + ".reasoning") {
			md.Reasoning = true
			var efforts []string
			for _, e := range strings.Split(m.setupVal(tier+".efforts"), ",") {
				if e = strings.TrimSpace(e); e != "" {
					efforts = append(efforts, e)
				}
			}
			if len(efforts) == 0 {
				return fmt.Errorf("%s is a reasoning model but has no thinking efforts", tier)
			}
			md.Thinking = config.ThinkingSpec{Mode: "effort", Efforts: efforts, DefaultLevel: m.setupVal(tier + ".defaultEffort")}
			md.Compat = config.Compat{ThinkingFormat: "openai"}
		}
		md.Cost = config.Cost{Input: prices["in"], Output: prices["out"], CacheRead: prices["cacheRead"], CacheWrite: prices["cacheWrite"]}
		if _, seen := models[id]; !seen {
			models[id] = md
			order = append(order, id)
		}
		tierModel[tier] = id
		return nil
	}
	for _, tier := range tierNames {
		if err := resolve(tier); err != nil {
			m.wizErr = err.Error()
			return
		}
	}

	modelList := make([]config.ModelDef, 0, len(order))
	for _, id := range order {
		modelList = append(modelList, models[id])
	}
	if err := m.store.AddProvider(name, baseURL, "secret:"+name, modelList); err != nil {
		m.wizErr = "save provider: " + err.Error()
		return
	}
	if key := m.setupVal("provider.key"); key != "" {
		if err := m.store.SaveSecret(name, key); err != nil {
			m.wizErr = "save key: " + err.Error()
			return
		}
	}
	for _, tier := range tierNames {
		if err := m.store.SaveModelRole(tier, name+"/"+tierModel[tier]); err != nil {
			m.wizErr = "save model: " + err.Error()
			return
		}
	}
	m.wizErr = ""
	m.buildCatalog()
	m.gotoKeyOrStart() // prompts for the key here if it was left blank
}

// setupInt parses a non-negative whole number; blank is 0.
func setupInt(s string) (int, error) {
	if s == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("invalid")
	}
	return n, nil
}

// setupFloat parses a non-negative number; blank is 0.
func setupFloat(s string) (float64, error) {
	if s == "" {
		return 0, nil
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil || v < 0 {
		return 0, fmt.Errorf("invalid")
	}
	return v, nil
}

// setupBodyView renders the provider+models form, a window of its visible rows
// around the focused one.
func (m *model) setupBodyView() string {
	var vis []*setupRow
	focusPos := 0
	for _, r := range m.setupRows {
		if m.setupRowVisible(r) {
			if r == m.setupRows[m.setupFocus] {
				focusPos = len(vis)
			}
			vis = append(vis, r)
		}
	}
	win := 14
	if m.h > 0 {
		win = clampInt(m.h-12, 6, 26)
	}
	start := 0
	if len(vis) > win {
		start = focusPos - win/2
		if start < 0 {
			start = 0
		}
		if start+win > len(vis) {
			start = len(vis) - win
		}
	}
	end := start + win
	if end > len(vis) {
		end = len(vis)
	}
	var b strings.Builder
	if start > 0 {
		b.WriteString(styleMuted.Render("    ↑ more above") + "\n")
	}
	for i := start; i < end; i++ {
		r := vis[i]
		if r.header != "" {
			b.WriteString("  " + styleAccent.Render(r.header) + "\n")
		}
		marker := "    "
		if r == m.setupRows[m.setupFocus] {
			marker = styleAccent.Render("  ▸ ")
		}
		if r.toggle {
			box := "[ ]"
			if r.on {
				box = styleAccent.Render("[x]")
			}
			b.WriteString(marker + box + " " + r.label + "\n")
		} else {
			b.WriteString(marker + r.label + "\n")
			b.WriteString("      " + r.ti.View() + "\n")
		}
	}
	if end < len(vis) {
		b.WriteString(styleMuted.Render("    ↓ more below") + "\n")
	}
	b.WriteString("\n    " + styleMuted.Render("↑/↓ or Tab move · ←/→ or Space toggle · type to edit · Enter saves & continues · Esc back"))
	return b.String()
}

// engNameFromPath derives an engagement name from an import path.
func engNameFromPath(p string) string {
	base := filepath.Base(strings.TrimRight(p, `/\`))
	base = strings.TrimSuffix(base, ".tar.gz")
	base = strings.TrimSuffix(base, ".tgz")
	if base == "" || base == "." {
		base = "imported-engagement"
	}
	// Sanitize so the name can't traverse host (logs/backups/history) or guest
	// (root-run tar) paths it gets joined into.
	return sandbox.SafeEngagementName(base)
}

func (m *model) reloadForMode() {
	// Reload store+roster if the chosen mode differs from the launch mode.
	fsys := assetsFor(m.mode)
	if st, err := config.Load(m.mode, fsys); err == nil {
		m.store = st
		if r, err := agents.LoadRoster(m.mode, fsys, m.store); err == nil {
			m.roster = r
		}
	}
	// The scope/goal fields are mode-specific; refresh them for the chosen mode.
	m.applyModePlaceholders()
}

// startEngagement validates the choice and kicks off sandbox preparation
// (open/create + egress) off the UI goroutine. beginSession finishes once the
// sandbox is ready. Per-agent egress is mandatory: a failure returns to the
// wizard rather than running agents unconfined.
func (m *model) startEngagement() (tea.Model, tea.Cmd) {
	// The engagement must not run unless the model tiers are configured and usable:
	// all three set, resolving to a known model with a working API key.
	if !m.store.ModelsConfigured() || m.store.ValidateModels() != nil {
		// A direct start (-env ...) skipped the wizard; rather than dead-end on the
		// error, drop into the wizard at the model picker (or the key step, if the
		// models are set but a key is missing) so the user can finish setup.
		if m.direct {
			m.direct = false
			m.buildCatalog()
			if !m.store.ModelsConfigured() {
				m.gotoModelSetup()
			} else {
				m.gotoKeyOrStart()
			}
		}
		if err := m.store.ValidateModels(); err != nil {
			m.wizErr = "models not ready: " + err.Error() + " — configure them in setup or with `oline -edit`."
		}
		return m, nil
	}
	if len(m.cands) == 0 {
		m.wizErr = "no environments found. WSL/QEMU required. Press r to rescan."
		return m, nil
	}
	cand := m.cands[m.envSel]
	if cand.Backend == sandbox.BackendQEMU && !sandbox.QEMUAvailable() {
		m.wizErr = "QEMU not found — install QEMU (on PATH or in Program Files), or pick a WSL distro."
		return m, nil
	}
	m.prepCand = cand
	m.preparing = true
	m.wizErr = ""
	eff := m.store.Effective()
	qopts := sandbox.BootOpts{
		Persist:       !eff.QEMU.Disposable, // QEMU engagements persist by default (opt out with qemu.disposable)
		HostFirewall:  eff.QEMU.HostFirewall && runtime.GOOS == "linux",
		AllowInternet: wire.RosterAllowsInternet(m.mode, m.roster),
		DiskGB:        m.diskGB,       // 0 unless the wDisk step set it (new VM only)
		MemMB:         m.memGB * 1024, // 0 unless the wMem step set it; then backend default (8 GB)
		CPUs:          m.cpus,         // 0 unless the wCPUs step set it; then backend default (4)
	}
	return m, prepareSandboxCmd(sandboxPrep{
		cand:       cand,
		scopeStr:   m.egressScope(),
		allowIPv6:  eff.Network.AllowIPv6,
		qemu:       qopts,
		resume:     m.reuseDisk && cand.Backend == sandbox.BackendQEMU, // reuse the saved disk; a fresh VM with loaded history is not a disk-resume
		engagement: m.engagement,
		createWSL:  m.createWSL,
		newName:    m.newEnvName,
		wslRootfs:  m.wslRootfs,
	})
}

// beginSession builds and launches the engagement once the sandbox handle is
// ready (egress already applied in prepareSandboxCmd).
func (m *model) beginSession(h sandbox.Handle) (tea.Model, tea.Cmd) {
	cand := m.prepCand
	pol := scope.Parse(m.egressScope())

	reg := wire.BuildRegistry(m.store)
	llmFor := wire.NewLLMResolver(m.store)

	// Optional System-1 decision model: enabled only when it is configured, valid, AND
	// reachable (probed now). Otherwise the decision features stay off and the
	// orchestrator is never told they exist.
	decisionEnabled := false
	var decClient engine.Decider
	var decModel string
	var decCtx int
	if sel := strings.TrimSpace(m.store.Effective().ModelRoles.Decision); sel != "" {
		switch {
		case m.store.ValidateDecisionModel() != nil:
			m.writeln(core.Main, styleWarn.Render("decision model not usable — decision features disabled: "+m.store.ValidateDecisionModel().Error()))
		default:
			if cl, mid, ctxTok, err := wire.DecisionClient(m.store, sel); err != nil {
				m.writeln(core.Main, styleWarn.Render("decision model setup failed — decision features disabled: "+err.Error()))
			} else {
				pctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				perr := cl.Ping(pctx)
				cancel()
				if perr != nil {
					m.writeln(core.Main, styleWarn.Render("decision model unreachable — decision features disabled: "+perr.Error()))
				} else {
					decClient, decModel, decCtx = cl, mid, ctxTok
					decisionEnabled = true
				}
			}
		}
	}

	orch, resolver := wire.Build(m.mode, m.store, m.roster, decisionEnabled)

	// Connect operator-configured MCP servers (host-side integrations) and register
	// their tools into the registry, so an agent granted <server>__<tool> can call it.
	// A server that fails to connect is skipped with a warning (non-fatal). Reported to
	// Main's transcript in the startup block below; closed at shutdown.
	var mcpWarns []string
	if cfgs := wire.MCPServerConfigs(m.store); len(cfgs) > 0 {
		var mcpTools []tools.Tool
		m.mcp, mcpTools, mcpWarns = mcp.ConnectAll(context.Background(), cfgs)
		for _, t := range mcpTools {
			reg.Register(t)
		}
	}

	m.events = make(chan event.Event, 4096) // generous buffer; the TUI batch-drains it
	m.sess = engine.NewSession(reg, h, llmFor, resolver, m.events, m.store.Effective().MaxConcurrency)
	m.sess.Mode = runModeFromSel(m.autoSel)
	// Custom tools (config command tools, Go extension tools, and MCP tools) execute,
	// so watcher mode must block them along with bash/bash_bg/revshell.
	execTools := wire.CustomToolNames(m.store)
	if m.mcp != nil {
		for _, n := range m.mcp.ToolNames() {
			execTools[n] = true
		}
	}
	m.sess.ExecTools = execTools
	m.saveReq = make(chan struct{}, 1)
	// Surface config mistakes (unknown tool grants, bad tiers, missing descriptions,
	// bad spawns, malformed custom tools) rather than letting them fail silently.
	for _, w := range wire.Lint(m.store, m.roster, reg) {
		m.writeln(core.Main, styleWarn.Render("config lint: "+w))
	}
	m.sess.Interactive = true // keep Main alive between turns so the user can keep chatting
	// Let update_diagram enforce the LIVE Diagram-pane width (read fresh each call, so
	// a resize is reflected immediately).
	m.sess.DiagramWidth = m.diagramWidth
	m.sess.BashDeny = engine.CompileDeny(bashDenyGlobs(m.store.Effective().Bash.Patterns))
	// Route context auto-compaction through the cheaper `small` model (a big cost
	// saving on long runs); falls back to each agent's own model if it can't resolve.
	if small := m.store.Effective().ModelRoles.Small; small != "" {
		if cl, mid, err := llmFor(small); err == nil {
			m.sess.SummarizerLLM, m.sess.SummarizerModel = cl, mid
			m.sess.SummarizerContextTokens = m.modelContextWindow(small)
		}
	}
	// Goal + target as structured state so engine-side decision calls can use them.
	m.sess.Goal = m.goalIn.Value()
	m.sess.Target = m.scopeIn.Value()
	// Activate the decision model (already probed above).
	if decisionEnabled {
		m.sess.Decider = decClient
		m.sess.DecisionModel = decModel
		m.sess.DecisionContextTokens = decCtx
		m.sess.RouterRoles = routerRolesFromRoster(m.roster)
		m.writeln(core.Main, styleAccent.Render("decision model active ("+decModel+"): finding triage/dedup, judge pre-screen, progress checks, and task routing enabled"))
	}
	m.costCap = m.store.Effective().Budget.MaxCostUSD
	m.summaryCap = m.store.Effective().Budget.MaxSummaryCostUSD
	m.router = m.sess.Router()
	m.resultCh = make(chan engine.Result, 1)
	m.envID = cand.ID
	// A freshly created WSL distro's candidate id is the "create new" sentinel; use
	// the live handle's real oline- name for the header instead.
	if m.createWSL {
		if id := h.Info().ID; id != "" {
			m.envID = id
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	m.cancel = cancel

	m.addAgent(core.Main)
	// Default layout: the orchestrator on the left, and a stacked Summary (top) and
	// Diagram (bottom) in the center. The right-side Options/help pane is separate
	// and togglable (Ctrl-b ?).
	mainLeaf := newLeaf(core.Main)
	center := &pane{dir: splitV, ratio: 0.5, a: newSummaryLeaf(), b: newDiagramLeaf()}
	center.a.parent, center.b.parent = center, center
	root := &pane{dir: splitH, ratio: 0.5, a: mainLeaf, b: center}
	mainLeaf.parent, center.parent = root, root
	m.paneRoot = root
	m.focusPane = mainLeaf
	if m.customizablePanes() {
		// Fully customizable: a single numeric tab with the default panes; the user
		// adds tabs/panes themselves, with no enforcement or auto-layout.
		m.windows = []*windowState{{root: root, focus: mainLeaf, name: "1", kind: tabFree}}
	} else {
		// Enforced: tab 1 "Info" holds the default info panes; tab 2 "Subagents"
		// starts empty and auto-arranges spawned subagents.
		sub := newEmptyLeaf()
		m.windows = []*windowState{
			{root: root, focus: mainLeaf, name: tabInfo.label(), kind: tabInfo},
			{root: sub, focus: sub, name: tabSubagents.label(), kind: tabSubagents},
		}
	}
	m.winIdx = 0
	m.shellCh = make(chan shellMsg, 256)
	m.shells = map[core.AgentID]*shellState{}
	m.termCh = make(chan core.AgentID, 1)
	m.terms = map[core.AgentID]*termState{}
	m.running = true
	m.input.Focus()
	m.resize()
	// A new engagement gets a program-assigned workspace name, so both the
	// orchestrator and the host backup target the same directory.
	if !m.resume && m.engagement == "" {
		m.engagement = "engagement-" + time.Now().Format("20060102-150405")
	}
	// Confine the agents' file writes to this engagement's workspace: file-tool
	// relative paths resolve against it, and write/edit refuse any path outside it
	// (the Privileged installer excepted). Matches the workspace path the
	// orchestrator is told about in composePrompt.
	// Point the sandbox's working directory at this engagement's workspace (creating
	// it) so a relative path resolves there for every agent command, then confine
	// file writes to it. Only confine once the cwd is actually set, so a failure
	// falls back to the default cwd (and the broader workspace-root confinement)
	// rather than a mismatched state.
	engWorkspace := sandbox.WSLWorkspace + "/" + m.engagement
	if wsr, ok := m.sess.Sandbox.(sandbox.WorkspaceSetter); ok {
		if err := wsr.SetWorkspace(ctx, engWorkspace); err != nil {
			m.writeln(core.Main, styleWarn.Render("could not set the engagement workspace directory: "+err.Error()))
		} else {
			m.sess.Workspace = engWorkspace
		}
	}
	// Per-engagement activity log on the host (what every agent does/thinks/runs).
	m.log = openEngLog(m.engagement)
	m.log.setRedact(m.store.KnownSecrets()) // scrub any API keys an agent echoes
	// Per-engagement tamper-evident command audit log on the host (agent + operator
	// command executions, hash-chained). Agents never touch it.
	m.audit = openAuditLog(m.engagement)
	m.sess.SetAudit(m.audit)
	m.log.banner(fmt.Sprintf("%s · %s · env=%s · scope=%q · goal=%q", m.engagement,
		m.sess.Mode.String(), cand.ID, strings.TrimSpace(m.scopeIn.Value()), strings.TrimSpace(m.goalIn.Value())))
	// Resume with history: restore the prior conversation (seed Main live, rebuild
	// every agent's transcript) before the "resuming" notice, so the old
	// conversation appears first. If loading fails, fall back to a clean start.
	if m.resume && m.loadHist {
		if eh, err := loadHistory(m.engagement); err == nil {
			m.restoreHistory(eh.Agents)
			// Recover prior findings + summary from the saved history so a resumed
			// engagement's export bundle still reflects what earlier sessions found.
			if fs, sum := findingsFromHistory(eh.Agents); len(fs) > 0 || sum != "" {
				m.sess.SeedFindings(fs)
				m.sess.SeedSummary(sum)
			}
			// Only the fully-customizable mode restores a saved free-form layout; the
			// enforced tabs (Info / Subagents) are deterministic, so they are kept as
			// built above.
			if m.customizablePanes() {
				m.restoreLayout(eh.Windows, eh.WinIdx)
			}
			m.writeln(core.Main, styleMuted.Render("── restored previous conversation history ──"))
		} else {
			m.writeln(core.Main, styleWarn.Render("could not load saved conversation history: "+err.Error()))
		}
	}

	// Enforced mode on resume: the subagents were restored (no spawn events fire for
	// them), so populate the Subagents tab from the live subagents now.
	if !m.customizablePanes() {
		m.rebuildSubagentsTab()
	}

	start := "engagement starting"
	if m.resume {
		start = "resuming engagement " + m.engagement
	}
	m.writeln(core.Main, styleMuted.Render(start+" in "+cand.ID+" — "+m.sess.Mode.String()))
	if p := m.log.Path(); p != "" {
		m.writeln(core.Main, styleMuted.Render("activity log → "+p))
	}
	if p := m.audit.Path(); p != "" {
		m.writeln(core.Main, styleMuted.Render("audit log (tamper-evident) → "+p+"  (verify with: oline -audit-verify "+m.engagement+")"))
	}
	// MCP servers (host-side integrations): report what connected and what didn't, so
	// the operator knows the exact tool names to grant to agents.
	for _, w := range mcpWarns {
		m.writeln(core.Main, styleWarn.Render("mcp: "+w))
	}
	if m.mcp != nil {
		for _, s := range m.mcp.Servers() {
			if len(s.Tools) == 0 {
				m.writeln(core.Main, styleMuted.Render("mcp server "+s.Name+" connected (no tools exposed)"))
				continue
			}
			m.writeln(core.Main, styleMuted.Render("mcp server "+s.Name+" connected (host-side) → tools: "+strings.Join(s.Tools, ", ")+"  (grant a tool to an agent by name)"))
		}
	}

	// Resume: make sure the engagement's files are in the canonical workspace.
	//   - A brand-new sandbox (fresh WSL distro or fresh QEMU VM) holds none of the
	//     prior workspace, so when loading history, restore it from the host backup
	//     bundle. An explicit host archive/in-env path (the WSL import option) wins.
	//   - A reused WSL distro may already hold the files (or an import points at them).
	//   - A resumed QEMU VM carries its workspace on the persisted disk — nothing to do.
	if m.resume {
		switch {
		case m.freshSandbox(cand):
			if cand.Backend == sandbox.BackendWSL && strings.TrimSpace(m.importIn.Value()) != "" {
				m.placeResumeFiles(cand.ID)
			} else if m.loadHist {
				m.restoreWorkspaceFromBackup(m.engagement)
			}
		case cand.Backend != sandbox.BackendQEMU:
			m.placeResumeFiles(cand.ID)
		}
	}

	// Optional operator-provided data: copy a host folder into the workspace (read-only)
	// so agents can read it. No-op unless a folder was chosen in setup.
	m.copyHostDataToWorkspace()

	// VPN to reach the target, if requested.
	m.setupVPN(cand.ID)

	// Egress was established (fail-closed) above; report the resulting policy.
	if m.mode == config.ModeExploiter {
		m.writeln(core.Main, styleMuted.Render("egress enforced: agents are network-isolated (local target only); internet for installer (download) and judge/skeptic (research) only"))
	} else if pol.Empty() && len(pol.Domains()) == 0 {
		m.writeln(core.Main, styleWarn.Render("no targets parsed from scope — in-scope agents can reach NO network (strict). researcher/installer/judge/skeptic still get internet."))
	} else {
		m.writeln(core.Main, styleMuted.Render(fmt.Sprintf("egress enforced: in-scope IP/CIDR=%v · internet-minus-target for researcher/installer/judge/skeptic", pol.CIDRs())))
	}
	// Domain targets are resolved to IPs at apply time and firewall-enforced like any
	// other target; agents resolve names via the sandbox's DNS resolver (now allowed).
	if len(pol.Domains()) > 0 {
		m.writeln(core.Main, styleMuted.Render(fmt.Sprintf("domain targets %v resolved to IPs and firewall-enforced (a snapshot at apply time — re-apply scope with Ctrl-b e if a target's address changes)", pol.Domains())))
	}

	// Host-side backups (done by the program, not the agents). 0 disables.
	if min := m.store.Effective().Backup.IntervalMinutes; min > 0 {
		// On resume, move any existing bundle aside FIRST so a previous engagement's
		// backup is never overwritten by this session's backups.
		if m.resume {
			m.preserveResumeBackup()
		}
		m.writeln(core.Main, styleMuted.Render(fmt.Sprintf("host backups: every %d min + at end → %s (full engagement export bundle: workspace, transcripts, findings, logs, report when produced; overwritten)",
			min, m.backupBundlePath())))
		go m.backupLoop(ctx, m.engagement, min)
	} else {
		m.writeln(core.Main, styleMuted.Render("host backups: disabled (backup.intervalMinutes = 0)"))
	}

	// Persist the conversation to the host on a timer so it can be reloaded after a
	// restart (program-written, like the activity log; agents never touch it).
	m.writeln(core.Main, styleMuted.Render("conversation history → "+historyPath(m.engagement)+" (reload it on resume)"))
	m.refreshLayoutSnapshot() // seed the layout snapshot so even an early save persists it
	go m.historyLoop(ctx)

	// The operator's reverse-shell catcher pane (Ctrl-b L) needs socat in the
	// sandbox. In pentester mode, best-effort install it if missing so the feature
	// is ready; non-fatal (the catcher otherwise reports socat is needed).
	if m.mode == config.ModePentester {
		go m.ensureSocat(ctx)
	}

	prompt := m.composePrompt()
	go func() { m.resultCh <- m.sess.Run(ctx, orch, prompt) }()

	return m, tea.Batch(listen(m.events), waitResult(m.resultCh), listenShell(m.shellCh), listenTerm(m.termCh), tickCmd(), textinput.Blink)
}

// backupLoop periodically backs up the engagement workspace to the host until
// the engagement context ends.
func (m *model) backupLoop(ctx context.Context, engagement string, minutes int) {
	t := time.NewTicker(time.Duration(minutes) * time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			m.runBackup(engagement)
		}
	}
}

// backupBundlePath is the host path this session writes its backup bundle to. It is
// normally <BackupDir>/<engagement>.tar.gz, but a resume that could not preserve a
// prior engagement's bundle falls back to a distinct name (backupName) so the
// previous engagement's backup is never overwritten.
func (m *model) backupBundlePath() string {
	name := m.backupName
	if strings.TrimSpace(name) == "" {
		name = m.engagement
	}
	return filepath.Join(m.store.BackupDir(), name+".tar.gz")
}

// preserveResumeBackup moves an existing backup bundle aside before a resumed
// engagement starts, so a previous engagement's backup is NEVER overwritten by the
// resume. The live session keeps writing <engagement>.tar.gz (now a complete, merged
// bundle); the pre-resume copy is kept as <engagement>.pre-resume-<timestamp>.tar.gz.
// Best-effort: if the file cannot be moved aside, this session's backups are written
// under a separate name instead, so the original is still left intact.
func (m *model) preserveResumeBackup() {
	if m.engagement == "" {
		return
	}
	cur := filepath.Join(m.store.BackupDir(), m.engagement+".tar.gz")
	info, err := os.Stat(cur)
	if err != nil {
		return // nothing to preserve
	}
	ts := info.ModTime().Format("20060102-150405")
	preserved := filepath.Join(m.store.BackupDir(), m.engagement+".pre-resume-"+ts+".tar.gz")
	for i := 1; ; i++ {
		if _, err := os.Stat(preserved); os.IsNotExist(err) {
			break
		}
		preserved = filepath.Join(m.store.BackupDir(), fmt.Sprintf("%s.pre-resume-%s-%d.tar.gz", m.engagement, ts, i))
	}
	if err := os.Rename(cur, preserved); err != nil {
		m.backupName = m.engagement + ".resumed-" + time.Now().Format("20060102-150405")
		m.writeln(core.Main, styleWarn.Render("could not move the previous backup aside ("+err.Error()+"); this session's backups go to "+m.backupName+".tar.gz so the original is left untouched"))
		return
	}
	m.writeln(core.Main, styleMuted.Render("preserved the previous engagement backup → "+preserved))
}

// runBackup writes THE one compressed backup for the engagement to the host
// (overwriting any previous one) via the sandbox handle — works for any backend
// (WSL now, QEMU when its lifecycle lands) — and reports via a notice.
func (m *model) runBackup(engagement string) {
	if m.sess == nil {
		return
	}
	// Flush the latest conversation to history.json first, so the bundle captures the
	// most recent transcript (saveHistory is serialized by histMu, safe from here).
	m.saveHistory()
	dest := m.backupBundlePath()
	// Pause agent file writes for the duration of the backup so the workspace is never
	// captured mid-write (agents get a "backup in progress, retry" error meanwhile);
	// the closure guarantees writes resume even if the build panics.
	err := func() error {
		m.sess.FreezeWrites()
		defer m.sess.UnfreezeWrites()
		return m.buildExportBundle(engagement, dest)
	}()
	if err != nil {
		m.emitNotice("warn", "backup skipped: "+err.Error())
		return
	}
	m.emitNotice("info", "backed up engagement bundle → "+dest)
}

// emitNotice sends a notice to the TUI from any goroutine (non-blocking).
func (m *model) emitNotice(level, text string) {
	if m.events == nil {
		return
	}
	select {
	case m.events <- event.Notice{Level: level, Text: text}:
	default:
	}
}

// socatEnsureScript checks for socat and, if absent, installs it with whatever
// package manager the sandbox has. Quiet; its last line is one of
// "socat: already present" / "socat: installed" / "socat: install failed".
const socatEnsureScript = `set +e
if command -v socat >/dev/null 2>&1; then echo "socat: already present"; exit 0; fi
if command -v apt-get >/dev/null 2>&1; then DEBIAN_FRONTEND=noninteractive apt-get update -y >/dev/null 2>&1; DEBIAN_FRONTEND=noninteractive apt-get install -y socat >/dev/null 2>&1
elif command -v dnf >/dev/null 2>&1; then dnf install -y socat >/dev/null 2>&1
elif command -v apk >/dev/null 2>&1; then apk add --no-cache socat >/dev/null 2>&1
elif command -v pacman >/dev/null 2>&1; then pacman -Sy --noconfirm socat >/dev/null 2>&1
elif command -v zypper >/dev/null 2>&1; then zypper --non-interactive install socat >/dev/null 2>&1
fi
if command -v socat >/dev/null 2>&1; then echo "socat: installed"; else echo "socat: install failed"; fi`

// ensureSocat best-effort installs socat in the sandbox (as root) so the operator's
// reverse-shell catcher pane (Ctrl-b L) works. Pentester mode only; non-fatal, and
// stays quiet when socat is already present.
func (m *model) ensureSocat(ctx context.Context) {
	if m.sess == nil || m.sess.Sandbox == nil {
		return
	}
	cctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	var out strings.Builder
	_, err := m.sess.Sandbox.Exec(cctx, core.AgentID("setup"),
		sandbox.Cmd{Argv: []string{"bash", "-lc", socatEnsureScript}, Policy: sandbox.Privileged}, &out)
	s := out.String()
	switch {
	case strings.Contains(s, "socat: already present"):
		// already installed; nothing to say
	case strings.Contains(s, "socat: installed"):
		m.emitNotice("info", "installed socat in the sandbox for the reverse-shell catcher (Ctrl-b L)")
	default:
		detail := ""
		if err != nil {
			detail = " (" + err.Error() + ")"
		}
		m.emitNotice("info", "could not auto-install socat"+detail+"; install it in the sandbox to use the reverse-shell catcher (Ctrl-b L)")
	}
}

// applyScopeLive updates the engagement scope. While agents are running it
// re-applies the per-agent firewall ON THE FLY and tells the orchestrator the
// allowed scope changed; otherwise the new scope just takes effect at start.
// egressScope is the scope text that feeds the per-agent egress firewall. In
// exploiter mode "scope" names the application under analysis, not network targets,
// and every exec agent is network-isolated anyway, so it yields no firewall targets.
// In pentester mode it is the in-scope target list.
func (m *model) egressScope() string {
	if m.mode == config.ModeExploiter {
		return ""
	}
	return m.scopeIn.Value()
}

func (m *model) applyScopeLive(raw string) {
	m.scopeIn.SetValue(raw)
	if m.sess == nil || m.sess.Sandbox == nil || m.router == nil {
		return // not running — the new scope is used when the engagement starts
	}
	ec := m.sess.Sandbox.EgressController()
	allowIPv6 := m.store.Effective().Network.AllowIPv6
	if m.mode == config.ModeExploiter {
		// The exploiter target is local; agents stay network-isolated regardless of
		// scope. Treat the edit as a new application description, not a firewall change.
		go func() {
			if err := ec.ApplyBaseline(sandbox.Scope{AllowIPv6: allowIPv6}); err != nil {
				m.emitNotice("error", "live scope update: firewall re-apply failed: "+err.Error())
				return
			}
			m.router.Send(engine.Envelope{From: "User", To: core.Main, Kind: engine.MsgSteer,
				Body: fmt.Sprintf("TARGET UPDATE from the operator: the application under analysis is now %q. Agents remain network-isolated (local target only); never attack a remote host.", strings.TrimSpace(raw))})
		}()
		return
	}
	pol := scope.Parse(raw)
	go func() {
		if err := ec.ApplyBaseline(sandbox.Scope{Targets: append(pol.CIDRs(), pol.Domains()...), AllowIPv6: allowIPv6}); err != nil {
			m.emitNotice("error", "live scope update: firewall re-apply failed: "+err.Error())
			return
		}
		m.emitNotice("info", fmt.Sprintf("scope updated live — firewall now enforces in-scope=%v", pol.CIDRs()))
		m.router.Send(engine.Envelope{From: "User", To: core.Main, Kind: engine.MsgSteer,
			Body: fmt.Sprintf("SCOPE UPDATE from the operator: the authorized in-scope targets are now %q (firewall-enforced IP/CIDR: %v). The per-agent egress firewall has been re-applied to match. From now on treat ONLY these as in scope — stop touching anything removed from scope, and you may engage anything newly added.",
				strings.TrimSpace(raw), pol.CIDRs())})
	}()
}

// placeResumeFiles ensures the resumed engagement's files sit in the canonical
// workspace (/opt/oline/engagements/<name>) so the orchestrator and backups use
// one consistent location.
func (m *model) placeResumeFiles(distro string) {
	ctx := context.Background()
	canonical := sandbox.WSLWorkspace + "/" + m.engagement
	if p := strings.TrimSpace(m.importIn.Value()); p != "" {
		if _, err := os.Stat(p); err == nil {
			// a host path -> import (folder or .tar.gz)
			if err := sandbox.ImportWSLEngagement(ctx, distro, p, m.engagement); err != nil {
				m.writeln(core.Main, styleErr.Render("import failed: "+err.Error()))
			} else {
				m.writeln(core.Main, styleMuted.Render("imported (host) "+p+" → "+canonical))
			}
			return
		}
		// not a host path -> treat it as a path already inside the environment
		if err := sandbox.CopyWSLDirToWorkspace(ctx, distro, p, m.engagement); err != nil {
			m.writeln(core.Main, styleErr.Render("could not use environment path "+p+": "+err.Error()))
		} else {
			m.writeln(core.Main, styleMuted.Render("restored from environment path "+p+" → "+canonical))
		}
		return
	}
	// picked from the list: find its real location and copy into canonical if needed
	src, err := sandbox.ResolveWSLEngagementPath(ctx, distro, m.engagement)
	if err != nil {
		m.writeln(core.Main, styleWarn.Render("note: no existing files found for "+m.engagement+"; the orchestrator will start it fresh"))
		return
	}
	if src != canonical {
		if err := sandbox.CopyWSLDirToWorkspace(ctx, distro, src, m.engagement); err != nil {
			m.writeln(core.Main, styleErr.Render("restore failed: "+err.Error()))
		} else {
			m.writeln(core.Main, styleMuted.Render("restored "+m.engagement+" from "+src+" → "+canonical))
		}
	}
}

// freshSandbox reports whether the chosen environment is being created new for this
// engagement — a QEMU VM on a fresh overlay (not a resumed saved disk), or a newly
// provisioned WSL distro (not a reused one). A fresh sandbox holds none of a resumed
// engagement's prior workspace files, so they must be restored from the host backup.
func (m *model) freshSandbox(cand sandbox.Candidate) bool {
	switch cand.Backend {
	case sandbox.BackendQEMU:
		return !m.reuseDisk
	case sandbox.BackendWSL:
		return m.createWSL
	}
	return false
}

// restoreWorkspaceFromBackup brings a previous engagement's workspace files into a
// brand-new sandbox from the host backup bundle (backups/<engagement>.tar.gz), so a
// resume onto a freshly-created WSL distro or QEMU VM continues with the prior
// artifacts and loot rather than an empty workspace. Best-effort: a missing bundle or
// a copy/unpack failure is reported, and the engagement starts with whatever is there.
func (m *model) restoreWorkspaceFromBackup(engagement string) {
	if m.sess == nil || m.sess.Sandbox == nil || strings.TrimSpace(engagement) == "" {
		return
	}
	bundle := filepath.Join(m.store.BackupDir(), engagement+".tar.gz")
	wsTgz, err := extractWorkspaceTar(bundle)
	if err != nil {
		m.writeln(core.Main, styleWarn.Render("note: no saved workspace to restore for "+engagement+" ("+err.Error()+"); starting with an empty workspace"))
		return
	}
	defer os.Remove(wsTgz)

	ctx := context.Background()
	const guestTgz = "/tmp/.oline-ws-restore.tgz"
	if err := m.sess.Sandbox.CopyIn(ctx, wsTgz, guestTgz); err != nil {
		m.writeln(core.Main, styleErr.Render("workspace restore: could not copy files into the sandbox: "+err.Error()))
		return
	}
	ws := strings.TrimSpace(m.sess.Workspace)
	if ws == "" {
		ws = sandbox.WSLWorkspace + "/" + engagement
	}
	eng := sandbox.SafeEngagementName(engagement)
	// Unpack into a staging dir, then merge its contents into the workspace. oline's
	// WSL backups nest everything under <engagement>/ while QEMU backups are flat;
	// descend into a lone engagement-named directory when present, so either layout
	// lands as workspace-relative files. Runs as root (Privileged) and re-grants the
	// oline group so the unprivileged agents can read/write the restored files.
	script := fmt.Sprintf(`set -e
stage="$(mktemp -d)"
tar --no-same-owner --no-same-permissions -xzf %s -C "$stage"
rm -f %s
mkdir -p %s
src="$stage"
if [ -d "$stage"/%s ] && [ "$(ls -A "$stage" | wc -l)" -eq 1 ]; then src="$stage"/%s; fi
cp -a "$src"/. %s/
rm -rf "$stage"
getent group oline >/dev/null 2>&1 && chown -R root:oline %s 2>/dev/null || true
chmod -R g+rwX %s 2>/dev/null || true
echo RESTORED`,
		shQuote(guestTgz), shQuote(guestTgz), shQuote(ws), shQuote(eng), shQuote(eng), shQuote(ws), shQuote(ws), shQuote(ws))

	var out strings.Builder
	if _, err := m.sess.Sandbox.Exec(ctx, core.AgentID("setup"),
		sandbox.Cmd{Argv: []string{"bash", "-lc", script}, Policy: sandbox.Privileged}, &out); err != nil || !strings.Contains(out.String(), "RESTORED") {
		detail := strings.TrimSpace(out.String())
		if err != nil {
			detail = err.Error() + " " + detail
		}
		m.writeln(core.Main, styleErr.Render("workspace restore failed: "+strings.TrimSpace(detail)))
		return
	}
	m.writeln(core.Main, styleMuted.Render("restored previous workspace files for "+engagement+" → "+ws))
}

// dataSubdirName is the workspace subdirectory the operator-provided data folder is
// copied into: the source folder's base name, sanitized to a safe single path
// segment, falling back to "provided-data".
func dataSubdirName(hostDir string) string {
	base := filepath.Base(filepath.Clean(hostDir))
	var b strings.Builder
	for _, r := range base {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	s := strings.Trim(b.String(), "._-")
	if s == "" {
		return "provided-data"
	}
	return s
}

// copyHostDataToWorkspace copies the operator-provided host folder (m.dataDir) into
// the engagement workspace as a READ-ONLY subdirectory, so agents can read the data
// while the original host files stay completely out of reach — they are never mounted
// or exposed to the guest, so agents cannot write to or modify them. No-op when the
// operator specified no folder (nothing is copied, and no folder is searched for).
// Best-effort: a failure is reported and the engagement continues.
func (m *model) copyHostDataToWorkspace() {
	if m.sess == nil || m.sess.Sandbox == nil || strings.TrimSpace(m.dataDir) == "" {
		return
	}
	tgz, err := tarHostDirToTemp(m.dataDir)
	if err != nil {
		m.writeln(core.Main, styleErr.Render("could not read the provided data folder "+m.dataDir+": "+err.Error()))
		return
	}
	defer os.Remove(tgz)

	ctx := context.Background()
	const guestTgz = "/tmp/.oline-data.tgz"
	if err := m.sess.Sandbox.CopyIn(ctx, tgz, guestTgz); err != nil {
		m.writeln(core.Main, styleErr.Render("provided data: copy into the sandbox failed: "+err.Error()))
		return
	}
	ws := strings.TrimSpace(m.sess.Workspace)
	if ws == "" {
		ws = sandbox.WSLWorkspace + "/" + m.engagement
	}
	dest := ws + "/" + dataSubdirName(m.dataDir)
	// Unpack as root into the workspace, then make the copy read-only to the
	// unprivileged agents (root-owned, world-readable, not group/other-writable): they
	// can READ the reference data but not alter it, and the host originals are untouched.
	script := fmt.Sprintf(`set -e
rm -rf %s
mkdir -p %s
tar --no-same-owner --no-same-permissions -xzf %s -C %s
rm -f %s
chown -R root:root %s 2>/dev/null || true
chmod -R a+rX,go-w %s 2>/dev/null || true
echo COPIED`,
		shQuote(dest), shQuote(dest), shQuote(guestTgz), shQuote(dest), shQuote(guestTgz), shQuote(dest), shQuote(dest))

	var out strings.Builder
	if _, err := m.sess.Sandbox.Exec(ctx, core.AgentID("setup"),
		sandbox.Cmd{Argv: []string{"bash", "-lc", script}, Policy: sandbox.Privileged}, &out); err != nil || !strings.Contains(out.String(), "COPIED") {
		detail := strings.TrimSpace(out.String())
		if err != nil {
			detail = err.Error() + " " + detail
		}
		m.writeln(core.Main, styleErr.Render("provided data: unpack failed: "+strings.TrimSpace(detail)))
		return
	}
	m.writeln(core.Main, styleMuted.Render("copied provided data (read-only) → "+dest))
}

// setupVPN brings up (or confirms) the OpenVPN tunnel per the wizard choice.
func (m *model) setupVPN(distro string) {
	ctx := context.Background()
	switch m.vpnMode {
	case "", "none":
		return
	case "running":
		if up, info := sandbox.VPNStatus(ctx, distro); up {
			m.writeln(core.Main, styleMuted.Render("VPN already up in environment: "+info))
		} else {
			m.writeln(core.Main, styleWarn.Render("no VPN tunnel detected in the environment, but you selected 'already running' — continuing"))
		}
	case "envfile":
		out, err := sandbox.StartWSLVPN(ctx, distro, strings.TrimSpace(m.vpnIn.Value()))
		m.vpnStatusLine(out, err)
	case "hostfile":
		host := strings.TrimSpace(m.vpnIn.Value())
		guest := "/opt/oline/vpn/" + filepath.Base(host)
		if err := sandbox.CopyFileToWSL(ctx, distro, host, guest); err != nil {
			m.writeln(core.Main, styleErr.Render("VPN config upload failed: "+err.Error()))
			return
		}
		m.writeln(core.Main, styleMuted.Render("uploaded VPN config → "+guest))
		out, err := sandbox.StartWSLVPN(ctx, distro, guest)
		m.vpnStatusLine(out, err)
	}
}

func (m *model) vpnStatusLine(out string, err error) {
	if err != nil {
		m.writeln(core.Main, styleErr.Render("VPN: "+err.Error()))
		if out != "" {
			m.writeln(core.Main, styleMuted.Render(out))
		}
		return
	}
	m.writeln(core.Main, styleGreenRender("VPN: "+out))
}

func (m *model) composePrompt() string {
	var b strings.Builder
	b.WriteString("AUTHORIZED ENGAGEMENT. The user has confirmed written authorization to test the target.\n")
	fmt.Fprintf(&b, "Mode: %s.\n", m.sess.Mode.String())
	b.WriteString(m.sandboxBoundaryLine())
	if m.sess.Mode == core.Watcher {
		b.WriteString("WATCHER mode: the operator runs every command themselves in the terminal panes; you do NOT execute anything (the harness blocks bash, bash_bg and revshell). Your job is to WATCH and ADVISE: read and write files in the workspace, track the state of play, and give concise, concrete guidance. The operator shares terminal output with you when they want your read on it; ask (with `ask`) if you need more.\n")
	}
	if m.vpnMode != "" && m.vpnMode != "none" {
		b.WriteString("The in-scope target is reached through a VPN tunnel (a tun/tap interface is up in the sandbox); route to it normally.\n")
	}
	if strings.TrimSpace(m.dataDir) != "" {
		ws := strings.TrimSpace(m.sess.Workspace)
		if ws == "" {
			ws = sandbox.WSLWorkspace + "/" + m.engagement
		}
		fmt.Fprintf(&b, "The operator provided supporting data at %s/%s in the workspace — read-only reference material to consult before and during the work (you cannot modify it).\n", ws, dataSubdirName(m.dataDir))
	}
	if m.resume {
		dir := sandbox.WSLWorkspace + "/" + m.engagement
		fmt.Fprintf(&b, "RESUME the existing engagement %q. Its workspace is at %s.\n", m.engagement, dir)
		b.WriteString("First rebuild the state of play: read 00-scope.md for the GOAL and prior context, and read the registers (assumptions.md, trust-zones.md, go-no-go.md, changes.md) and skim engagement-log.md. Do NOT redo completed work.\n")
		if m.mode == config.ModeExploiter {
			fmt.Fprintf(&b, "CURRENT target application (analyze ONLY this; it may have changed since the saved run and SUPERSEDES any recorded earlier): %s\n", strings.TrimSpace(m.scopeIn.Value()))
			b.WriteString("Update 00-scope.md to reflect this target, then continue toward the goal.\n")
			b.WriteString(exploiterLocalOnly)
		} else {
			fmt.Fprintf(&b, "CURRENT in scope (ONLY these — authoritative; they may have changed since the saved run, so they SUPERSEDE any scope recorded earlier): %s\n", strings.TrimSpace(m.scopeIn.Value()))
			b.WriteString("Update 00-scope.md to reflect this current scope, then continue toward the goal.\n")
			b.WriteString(m.networkPolicyLine())
		}
		if extra := strings.TrimSpace(m.goalIn.Value()); extra != "" {
			fmt.Fprintf(&b, "ADDITIONAL INSTRUCTIONS from the user for this resume (follow these as well as continuing toward the goal): %s\n", extra)
		}
		b.WriteString(m.diagramWidthLine())
		return b.String()
	}
	if m.mode == config.ModeExploiter {
		fmt.Fprintf(&b, "Target application (reverse engineer / analyze ONLY this): %s\n", strings.TrimSpace(m.scopeIn.Value()))
	} else {
		fmt.Fprintf(&b, "In scope (ONLY these — never touch anything else): %s\n", strings.TrimSpace(m.scopeIn.Value()))
	}
	fmt.Fprintf(&b, "Goal: %s\n", strings.TrimSpace(m.goalIn.Value()))
	if m.mode == config.ModeExploiter {
		b.WriteString(exploiterLocalOnly)
	} else {
		b.WriteString(m.networkPolicyLine())
	}
	fmt.Fprintf(&b, "Your engagement workspace is %s/%s/ (also reachable as ~/engagements/%s/). Create it and keep 00-scope.md, the registers, and all artifacts there.\n",
		sandbox.WSLWorkspace, m.engagement, m.engagement)
	b.WriteString(m.diagramWidthLine())
	b.WriteString("All execution happens inside the sandbox. Begin by recording scope, then proceed per your workflow.")
	return b.String()
}

// exploiterLocalOnly tells the orchestrator that the exploiter target is local and
// no agent may reach a remote host; only the installer fetches the app from the net.
const exploiterLocalOnly = "The target runs LOCALLY inside the sandbox. You are NOT authorized to attack, scan, or exploit any remote host or service: every exec agent is network-isolated and can reach only the local machine (loopback). If the application must be obtained from the internet, have the installer (the one agent with internet access) download and install it; every other agent works offline against the local build.\n"

// diagramWidthLine tells the orchestrator the exact current width of the Diagram
// pane so its first map fits. The limit is also enforced by update_diagram (which
// rejects over-wide lines and reports the live width), so this is the starting
// value; it can change if the user resizes the pane.
func (m *model) diagramWidthLine() string {
	w := m.diagramWidth()
	if w <= 0 {
		return ""
	}
	return fmt.Sprintf("Diagram pane: it is currently %d columns wide. Every line of the map you draw with update_diagram must be at most %d columns or the tool will reject it (the pane clips, it does not wrap); update_diagram always reports the current width, which changes if the user resizes the pane.\n", w, w)
}

// networkPolicyLine tells the orchestrator the sandbox's IPv6 egress posture so it
// does not plan around IPv6 reachability that the firewall blocks by default.
func (m *model) networkPolicyLine() string {
	if m.store.Effective().Network.AllowIPv6 {
		return "Network: IPv6 egress is ENABLED for this engagement (the per-agent firewall enforces scope over IPv6 as well as IPv4).\n"
	}
	return "Network: outbound non-loopback IPv6 is DISABLED by default (the sandbox firewall drops it for agents; targets are reached over IPv4). If a target is reachable only over IPv6, tell the user to enable it in config (network.allowIPv6) — do not try to work around the block.\n"
}

// sandboxBoundaryLine states the concrete host boundary for this engagement's
// backend, so the backend-neutral system prompt gets its WSL-vs-QEMU specifics at
// runtime (the orchestrator is told to carry this into every delegation).
func (m *model) sandboxBoundaryLine() string {
	if m.prepCand.Backend == sandbox.BackendQEMU {
		return "Sandbox: a disposable QEMU Linux VM (a separate kernel; the host's drives are NOT mounted in it). Everything runs inside this VM. NEVER attempt to escape the VM or reach the host machine or hypervisor. Keep all artifacts in the engagement workspace at " + sandbox.WSLWorkspace + "/<engagement>/.\n"
	}
	return "Sandbox: a Kali Linux distro inside WSL on a Windows host. Everything runs inside this Linux environment. NEVER escape WSL or touch the Windows host — no cmd.exe/powershell.exe/wsl.exe/explorer.exe, and never read or write under /mnt/ (the mounted Windows drives). Keep all artifacts in the engagement workspace at " + sandbox.WSLWorkspace + "/<engagement>/.\n"
}

// ---- running -------------------------------------------------------------

func (m *model) updateRunning(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case engineBatchMsg:
		for _, ev := range msg.evs {
			m.mutate(ev) // update state without rendering
		}
		m.flushDirty() // render each touched pane exactly once for the whole batch
		return m, listen(m.events)
	case resultMsg:
		m.writeln(core.Main, "")
		m.writeln(core.Main, styleAccent.Render("── engagement finished ──"))
		if msg.res.Err != nil {
			m.writeln(core.Main, styleErr.Render("error: "+msg.res.Err.Error()))
		} else {
			m.writeln(core.Main, msg.res.Text)
		}
		m.status = "finished — Ctrl-b q to quit"
		m.refreshLayoutSnapshot()
		m.saveHistory() // final conversation snapshot so the engagement can be reloaded
		if m.store.Effective().Backup.IntervalMinutes > 0 && m.engagement != "" {
			go m.runBackup(m.engagement) // final backup at end of engagement
		}
		return m, nil
	case tickMsg:
		m.refreshLayoutSnapshot() // UI-goroutine snapshot so the periodic save can persist the layout
		return m, tickCmd()       // re-render each second so the time-left countdowns update
	case shellMsg:
		m.applyShellMsg(msg)
		return m, listenShell(m.shellCh)
	case termRepaintMsg:
		m.applyTermRepaint(core.AgentID(msg))
		return m, listenTerm(m.termCh)
	case tea.MouseMsg:
		m.handleMouse(msg)
		return m, nil
	case tea.KeyMsg:
		return m.handleRunningKey(msg)
	}
	return m, nil
}

// handleMouse routes the wheel to scrolling (the roster sidebar when the cursor is
// over it, otherwise the focused pane) and the left button to pane focus, roster
// selection, and dragging panes to resize or move them.
func (m *model) handleMouse(msg tea.MouseMsg) {
	overRoster := m.showRoster && m.rightW > 0 && msg.X >= m.rosterX
	switch msg.Button {
	case tea.MouseButtonWheelUp:
		switch {
		case m.copyMode:
			m.copyVP.LineUp(3)
		case overRoster:
			m.rosterScroll -= 3
			if m.rosterScroll < 0 {
				m.rosterScroll = 0
			}
		case m.focusPane != nil:
			m.focusPane.vp.LineUp(3)
		}
		return
	case tea.MouseButtonWheelDown:
		switch {
		case m.copyMode:
			m.copyVP.LineDown(3)
		case overRoster:
			m.rosterScroll += 3 // renderRoster clamps to the content height
		case m.focusPane != nil:
			m.focusPane.vp.LineDown(3)
		}
		return
	}
	if m.copyMode || msg.Button != tea.MouseButtonLeft {
		return
	}

	switch msg.Action {
	case tea.MouseActionPress:
		m.mousePress(msg.X, msg.Y, overRoster)
	case tea.MouseActionMotion:
		m.mouseDrag(msg.X, msg.Y)
	case tea.MouseActionRelease:
		m.mouseRelease(msg.X, msg.Y)
	}
}

// mousePress starts a roster selection, a boundary resize, or a potential pane
// move/click depending on where the button went down.
func (m *model) mousePress(x, y int, overRoster bool) {
	m.dragBoundary, m.dragPane, m.dragMoved = nil, nil, false // clear any stale drag
	// The header occupies row 0 (bodyTop is 1): a click on a window tab there switches
	// to that window, so tabs are usable with the mouse and not only Ctrl-b n.
	if y == m.bodyTop-1 {
		for _, s := range m.tabSpans() {
			if x >= s.start && x < s.end {
				m.selectWindow(s.idx)
				return
			}
		}
		return
	}
	if overRoster {
		if id, ok := m.rosterUserPaneRows[y]; ok { // a listed shell: jump to its pane
			m.focusShellByID(id)
			return
		}
		if name, ok := m.rosterChannelRows[y]; ok { // a channel: open/jump to its pane
			m.openRevPane(name)
			return
		}
		if id, ok := m.rosterTermRows[y]; ok { // a terminal: jump to its pane
			m.focusTermByID(id)
			return
		}
		if id, ok := m.rosterAgentRows[y]; ok {
			m.setFocus(id)
		}
		return
	}
	// While zoomed, a single pane fills the area and the split rects are stale, so
	// skip resize/move and treat a press as a plain click.
	if !m.zoom {
		// A press on a split divider begins a resize; a press inside a pane begins a
		// potential move (resolved on release: a drag moves the pane, a plain click
		// focuses it and toggles the tool call under the cursor).
		if n := boundaryAt(m.paneRoot, x, y); n != nil {
			m.dragBoundary = n
			return
		}
	}
	if p := paneAt(m.paneRoot, x, y); p != nil {
		m.focusPane = p
		m.dragPane = p
		m.dragStartX, m.dragStartY = x, y
		m.dragMoved = false
	}
}

// mouseDrag updates a live resize, or flags that a pane move is in progress.
func (m *model) mouseDrag(x, y int) {
	if m.dragBoundary != nil {
		n := m.dragBoundary
		if n.dir == splitH && n.w > 1 {
			n.ratio = clampRatio(float64(x-n.x) / float64(n.w))
		} else if n.dir == splitV && n.h > 1 {
			n.ratio = clampRatio(float64(y-n.y) / float64(n.h))
		}
		m.layoutRectsOnly() // cheap during the drag; full markdown reflow happens on release
		return
	}
	if m.dragPane != nil && (x != m.dragStartX || y != m.dragStartY) {
		m.dragMoved = true
	}
}

// mouseRelease finishes a resize or move, or performs the click (focus + expand the
// tool call under the cursor) when the press did not turn into a drag.
func (m *model) mouseRelease(x, y int) {
	if m.dragBoundary != nil {
		m.dragBoundary = nil
		m.relayout() // reflow content to the final pane sizes now the drag is done
		return
	}
	if m.dragPane == nil {
		return
	}
	src := m.dragPane
	m.dragPane = nil
	if m.dragMoved {
		if dest := paneAt(m.paneRoot, x, y); dest != nil && dest != src {
			m.swapPaneViews(src, dest)
			m.focusPane = dest
			m.relayout()
			return
		}
	}
	// A plain click: toggle the tool call under where the button went down.
	m.toggleToolAt(src, m.dragStartX, m.dragStartY)
}

// swapPaneViews exchanges what two leaves display (agent/shell/summary/diagram), so
// dragging one pane onto another visually swaps them. The shell/info backing state
// is keyed by id, so moving the view is enough; a refresh rebuilds each pane.
func (m *model) swapPaneViews(a, b *pane) {
	a.agent, b.agent = b.agent, a.agent
	a.shell, b.shell = b.shell, a.shell
	a.rev, b.rev = b.rev, a.rev
	a.term, b.term = b.term, a.term
	a.summary, b.summary = b.summary, a.summary
	a.diagram, b.diagram = b.diagram, a.diagram
	a.empty, b.empty = b.empty, a.empty
	m.refreshLeaf(a)
	m.refreshLeaf(b)
	// The swapped-in content is new to each pane, so the old viewport scroll offset
	// is meaningless; reset to each pane's natural edge (top for the info panes,
	// bottom for a transcript that follows live output).
	for _, p := range []*pane{a, b} {
		if p.summary || p.diagram {
			p.vp.GotoTop()
		} else {
			p.vp.GotoBottom()
		}
	}
}

// clampRatio keeps a split ratio within sane bounds so neither child collapses.
func clampRatio(r float64) float64 {
	if r < 0.1 {
		return 0.1
	}
	if r > 0.9 {
		return 0.9
	}
	return r
}

// toggleToolAt expands/collapses the tool-output segment under a click in pane p,
// mapping the screen row to a transcript content line via the viewport's scroll.
func (m *model) toggleToolAt(p *pane, x, y int) {
	t := m.transcripts[p.agent]
	if t == nil {
		return
	}
	// A segment's startLine/nLines are shared transcript state, set by whichever
	// pane rendered it last. When the same agent is shown in more than one pane
	// (e.g. after a split), those positions may belong to a DIFFERENT pane's width
	// and scroll, so a click here would hit the wrong block and reflow the other
	// pane. Re-render THIS pane first so the positions match the pane that was
	// actually clicked.
	m.refreshLeaf(p)
	contentY0 := p.y + 2 // pane top border + header line
	vpH := p.h - 3
	if y < contentY0 || y >= contentY0+vpH {
		return
	}
	line := p.vp.YOffset + (y - contentY0)
	for _, s := range t.segs {
		if s.kind == segTool && s.nLines > 0 && line >= s.startLine && line < s.startLine+s.nLines {
			s.expanded = !s.expanded
			// Re-render every pane showing this agent so they stay consistent (the
			// expanded flag is shared across panes of the same agent).
			m.refreshFor(p.agent)
			return
		}
	}
}

func (m *model) handleRunningKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.copyMode {
		switch msg.String() {
		case "esc", "q", "ctrl+b":
			m.copyMode = false
			m.status = "exited copy mode"
		case "up", "k":
			m.copyVP.LineUp(1)
		case "down", "j":
			m.copyVP.LineDown(1)
		case "pgup":
			m.copyVP.HalfViewUp()
		case "pgdown", " ":
			m.copyVP.HalfViewDown()
		case "home", "g":
			m.copyVP.GotoTop()
		case "end", "G":
			m.copyVP.GotoBottom()
		}
		return m, nil
	}
	if m.prefix {
		m.prefix = false
		focus := m.focusedAgent()
		// tmux-style double prefix: Ctrl-b Ctrl-b sends a literal Ctrl-b to a focused
		// terminal pane (so the child can receive it), instead of an oline command.
		if msg.String() == "ctrl+b" {
			if ts := m.focusTerm(); ts != nil {
				ts.write([]byte{0x02})
				return m, nil
			}
		}
		switch msg.String() {
		// pane management (tmux-style)
		case "%", "|":
			m.doSplit(splitH)
		case "\"", "-":
			m.doSplit(splitV)
		case "z":
			m.zoom = !m.zoom
			m.relayout()
		case "x":
			m.closePaneCmd()
		case "o":
			m.cyclePane()
		case "$":
			m.openShellPane()
		case "T":
			m.openTermPane()
		case "L":
			m.startListenInput()
		case "[":
			m.enterCopyMode()
		case "g":
			if m.focusPane != nil {
				m.focusPane.vp.GotoTop()
				m.status = "jumped to top"
			}
		case "G":
			if m.focusPane != nil {
				m.focusPane.vp.GotoBottom()
				m.status = "jumped to bottom"
			}
		// window (tab) management (tmux-style)
		case "c":
			m.newWindow()
		case "n":
			m.nextWindow()
		case "&":
			m.killWindow()
		case "1", "2", "3", "4", "5", "6", "7", "8", "9":
			m.selectWindow(int(msg.String()[0] - '1')) // '1' -> window index 0
		case "0":
			m.selectWindow(9) // window 10
		case "left", "h":
			m.navPane('h')
		case "right", "l":
			m.navPane('l')
		case "up", "k":
			m.navPane('k')
		case "down", "j":
			m.navPane('j')
		// agent + view controls
		case "p":
			if m.focusCommandPane() {
				m.status = "this pane is a command pane, not an agent"
			} else {
				m.router.Pause(focus)
				m.status = "paused " + string(focus)
			}
		case "P":
			m.router.PauseAll()
			m.status = "paused ALL agents"
		case "r":
			if m.focusCommandPane() {
				m.status = "this pane is a command pane, not an agent"
			} else {
				m.router.Resume(focus)
				m.status = "resumed " + string(focus)
			}
		case "R":
			m.router.ResumeAll()
			m.status = "resumed ALL agents"
		case "K":
			// Kill the focused command pane's backing process (a terminal's PTY
			// session, a reverse-shell channel's listener, or a shell pane's command)
			// and close the pane; otherwise kill the focused agent.
			m.killFocused(focus)
		case "S":
			// Stop the running command(s): a shell pane's command, or the focused
			// agent's foreground/background commands (without pausing/killing it).
			if sh := m.focusShell(); sh != nil {
				if sh.stop() {
					m.status = "stopped the shell command"
				} else {
					m.status = "no shell command is running"
				}
			} else if m.focusRev() != nil {
				m.status = "a reverse-shell channel's listener is managed by agents (revshell close)"
			} else {
				n := m.router.StopCommands(focus)
				m.status = fmt.Sprintf("stopped %d running command(s) for %s", n, focus)
			}
		case "s":
			m.sendSummary()
			m.status = "requested summary + diagram refresh from orchestrator"
		case "d":
			m.sendDiagram()
			m.status = "requested diagram from orchestrator"
		case "t":
			m.showThoughts = !m.showThoughts
			m.relayout()
			if m.showThoughts {
				m.status = "showing agent thoughts"
			} else {
				m.status = "hiding agent thoughts"
			}
		case "?":
			// Toggle the WHOLE right-side Options pane (roster + help), so the main
			// panes reflow to use the reclaimed width. resize() recomputes widths and
			// re-lays-out the panes.
			m.showRoster = !m.showRoster
			m.resize()
			if m.showRoster {
				m.status = "showing the Options pane"
			} else {
				m.status = "hiding the Options pane (Ctrl-b ? shows it)"
			}
		case "b":
			// Set / change / remove the focused agent's time budget.
			if m.focusCommandPane() {
				m.status = "this pane is a command pane, not an agent"
			} else if focus == core.Main {
				m.status = "the orchestrator (Main) has no time budget"
			} else {
				m.budgetInput = true
				m.budgetTarget = focus
				m.input.SetValue("")
				m.status = "time budget for " + string(focus) + ": minutes (or 'none' to remove) · Esc cancels"
			}
		case "e":
			m.openEditor()
		case "m":
			m.cycleMode()
		case "a":
			m.sharePaneToAI()
		case "q":
			m.shutdown()
			return m, tea.Quit
		}
		return m, nil
	}

	// Budget-entry mode: the command line captures a minutes value (or "none").
	if m.budgetInput {
		switch msg.String() {
		case "esc":
			m.budgetInput = false
			m.input.SetValue("")
			m.status = "budget change cancelled"
			return m, nil
		case "enter":
			m.applyBudgetInput()
			return m, nil
		}
		var cmd tea.Cmd
		m.input, cmd = m.input.Update(msg)
		return m, cmd
	}

	// Listen-port entry mode: the command line captures a port for a fully-emulated
	// reverse-shell catcher terminal.
	if m.listenInput {
		switch msg.String() {
		case "esc":
			m.listenInput = false
			m.input.SetValue("")
			m.status = "reverse-shell catcher cancelled"
			return m, nil
		case "enter":
			m.applyListenInput()
			return m, nil
		}
		var cmd tea.Cmd
		m.input, cmd = m.input.Update(msg)
		return m, cmd
	}

	// A focused terminal pane is fully emulated: every key except the Ctrl-b prefix
	// goes straight to the child as raw bytes.
	if ts := m.focusTerm(); ts != nil {
		if msg.String() == "ctrl+b" {
			m.prefix = true
			return m, nil
		}
		if b := keyToBytes(msg); len(b) > 0 {
			ts.write(b)
		}
		return m, nil
	}

	switch msg.String() {
	case "ctrl+b":
		m.prefix = true
		return m, nil
	case "up":
		m.focusRosterDelta(-1)
		return m, nil
	case "down":
		m.focusRosterDelta(1)
		return m, nil
	case "pgup":
		if m.focusPane != nil {
			m.focusPane.vp.HalfViewUp()
		}
		return m, nil
	case "pgdown":
		if m.focusPane != nil {
			m.focusPane.vp.HalfViewDown()
		}
		return m, nil
	case "enter":
		// A focused shell pane runs the typed line as a command in the sandbox,
		// rather than sending it to an agent.
		if sh := m.focusShell(); sh != nil {
			cmd := strings.TrimSpace(m.input.Value())
			m.input.SetValue("")
			if cmd != "" {
				m.runShell(sh, cmd)
			}
			return m, nil
		}
		// A focused reverse-shell pane sends the typed line to the connected shell
		// (through the channel's input FIFO), rather than to an agent.
		if rs := m.focusRev(); rs != nil {
			cmd := strings.TrimSpace(m.input.Value())
			m.input.SetValue("")
			if cmd != "" {
				m.sendRev(rs, cmd)
			}
			return m, nil
		}
		// The Summary / Findings panes are read-only views, not agents: typing into
		// them would message a non-existent agent. Route a message to the orchestrator
		// instead and tell the user.
		if m.focusInfoPane() || (m.focusPane != nil && m.focusPane.empty) {
			if strings.TrimSpace(m.input.Value()) != "" {
				m.status = "nothing to message here — focus an agent (↑/↓) to send a message"
			}
			return m, nil
		}
		text := strings.TrimSpace(m.input.Value())
		if text != "" {
			to := m.focusedAgent()
			// If an agent is waiting on a question (e.g. an `ask`), the next message
			// ANSWERS it — route it as a user-answer so
			// the asking agent's `ask` unblocks, not as a steer it would ignore.
			kind := engine.MsgSteer
			if ans := m.answerTarget(to); ans != "" {
				to, kind = ans, engine.MsgUserAnswer
				delete(m.awaitingAnswer, to)
				// Clear the "question from X" cue now that it is answered; mention any
				// others still waiting so a stale prompt never lingers on the status bar.
				if n := len(m.awaitingAnswer); n > 0 {
					m.status = fmt.Sprintf("answer sent to %s — %d still waiting for an answer", to, n)
				} else {
					m.status = "answer sent to " + string(to)
				}
			}
			m.router.Send(engine.Envelope{From: "User", To: to, Kind: kind, Body: text})
			m.log.userMsg(to, text)
			m.tr(to).addStyled("you → "+string(to)+": "+text, styleAccent)
			m.refreshFor(to)
			m.input.SetValue("")
		}
		return m, nil
	}
	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	return m, cmd
}

func (m *model) doSplit(d paneDir) {
	// In enforced mode the Info and Subagents tabs are auto-managed and can't be split
	// by hand (activeKind returns tabFree in customizable mode, so splits stay allowed
	// there and on Shell/free tabs). Open panes on a Shell tab instead (Ctrl-b c).
	if k := m.activeKind(); k == tabInfo || k == tabSubagents {
		m.status = "can't split panes on the " + k.label() + " tab in enforced mode (open a Shell tab with Ctrl-b c, or turn on Customizable Panes)"
		return
	}
	if m.paneRoot == nil || m.focusPane == nil {
		return
	}
	m.zoom = false
	m.paneRoot, m.focusPane = splitPane(m.paneRoot, m.focusPane, d, m.focusedAgent())
	m.relayout()
}

func (m *model) closePaneCmd() {
	if m.paneRoot == nil || m.focusPane == nil {
		return
	}
	m.reapShell(m.focusPane)
	m.reapRev(m.focusPane)
	m.reapTerm(m.focusPane)
	m.paneRoot, m.focusPane = closePane(m.paneRoot, m.focusPane)
	m.zoom = false
	m.relayout()
}

// killFocused (Ctrl-b K) kills whatever the focused pane is backed by: a terminal
// pane's PTY session (so the shell / socat listener / caught reverse shell dies), a
// reverse-shell channel's listener (closing the channel), or a shell pane's running
// command — closing the command pane in each case. On an ordinary agent pane it
// kills the agent, as before.
func (m *model) killFocused(focus core.AgentID) {
	switch {
	case m.focusTerm() != nil:
		id := m.focusPane.agent
		m.closePaneCmd() // reapTerm closes the PTY session (shell/listener/reverse shell)
		m.status = "killed terminal " + string(id)
	case m.focusRev() != nil:
		name := m.focusRev().name
		if m.sess != nil {
			m.sess.CloseChannel(name) // kill the channel's listener
		}
		m.closePaneCmd() // stop the pane's output tail and close it
		m.status = "killed reverse-shell channel " + name
	case m.focusShell() != nil:
		id := m.focusPane.agent
		m.closePaneCmd() // reapShell stops the running command and closes the pane
		m.status = "killed shell " + string(id)
	default:
		m.router.Kill(focus)
		m.status = "killed " + string(focus)
	}
}

// reapShell cancels and forgets the shell backing pane p (if it is a shell), so
// closing a shell pane or window doesn't leave a command running detached.
func (m *model) reapShell(p *pane) {
	if p == nil || !p.shell {
		return
	}
	if sh := m.shells[p.agent]; sh != nil {
		sh.stop()
		delete(m.shells, p.agent)
	}
	if m.sess != nil {
		m.sess.ForgetOperator(string(p.agent)) // drop it from the agent-facing `terminal` list
	}
	// Also forget the shell's transcript and bookkeeping — a closed shell never
	// reopens under the same synthetic id, so keeping these would leak memory and
	// leave a stale "agent" in m.order for the engagement's lifetime.
	delete(m.transcripts, p.agent)
	delete(m.states, p.agent)
	delete(m.dirty, p.agent)
	for i, id := range m.order {
		if id == p.agent {
			m.order = append(m.order[:i], m.order[i+1:]...)
			break
		}
	}
}

// reapShells reaps every shell leaf in the subtree rooted at p.
func (m *model) reapShells(p *pane) {
	for _, l := range leaves(p) {
		m.reapShell(l)
		m.reapRev(l)
		m.reapTerm(l)
	}
}

// ---- windows (tabs) ------------------------------------------------------

// ensureWindows lazily creates the window list from the current pane state (so
// direct-start / tests that set paneRoot without beginSession still work).
func (m *model) ensureWindows() {
	if len(m.windows) == 0 {
		m.windows = []*windowState{{root: m.paneRoot, focus: m.focusPane, zoom: m.zoom, name: "1"}}
		m.winIdx = 0
	}
}

// saveWindow copies the live active-window state back into the window list.
func (m *model) saveWindow() {
	m.ensureWindows()
	w := m.windows[m.winIdx]
	w.root, w.focus, w.zoom = m.paneRoot, m.focusPane, m.zoom
}

// activateWindow makes window i the active one, loading its state and relaying.
func (m *model) activateWindow(i int) {
	if i < 0 || i >= len(m.windows) {
		return
	}
	m.winIdx = i
	w := m.windows[i]
	m.paneRoot, m.focusPane, m.zoom = w.root, w.focus, w.zoom
	m.relayout()
}

// newWindow opens a fresh window (tab) and switches to it. In customizable mode it
// is a numeric free tab showing the orchestrator; in enforced mode it is a "Shell"
// tab that starts with one shell pane.
func (m *model) newWindow() {
	m.saveWindow()
	var w *windowState
	if m.customizablePanes() {
		root := newLeaf(core.Main)
		w = &windowState{root: root, focus: root, name: strconv.Itoa(len(m.windows) + 1), kind: tabFree}
	} else {
		leaf := m.createShellLeaf() // a Shell tab is usable immediately
		w = &windowState{root: leaf, focus: leaf, name: tabShell.label(), kind: tabShell}
	}
	m.windows = append(m.windows, w)
	m.activateWindow(len(m.windows) - 1)
	m.status = "new window " + m.windows[m.winIdx].name
}

// needNewCommandTab reports whether a shell/terminal must open in a fresh Shell
// tab instead of the active one. Only the Info and Subagents tabs can't hold
// command panes; a Shell tab or a free (customizable-mode) tab holds them fine.
func (m *model) needNewCommandTab() bool {
	m.ensureWindows()
	k := m.windows[m.winIdx].kind
	return k == tabInfo || k == tabSubagents
}

// openLeafInNewShellTab appends a new Shell tab whose sole pane is leaf and
// activates it (relayout gives the leaf a size). Used when the operator opens a
// shell/terminal (Ctrl-b $ / Ctrl-b T) from a non-Shell tab in enforced mode.
func (m *model) openLeafInNewShellTab(leaf *pane) {
	m.saveWindow()
	m.zoom = false
	w := &windowState{root: leaf, focus: leaf, name: tabShell.label(), kind: tabShell}
	m.windows = append(m.windows, w)
	m.activateWindow(len(m.windows) - 1)
}

// renumberTabs refreshes tab names: numeric for free tabs, the kind label for
// enforced tabs. Called after the window set changes.
func (m *model) renumberTabs() {
	for j, w := range m.windows {
		if w.kind == tabFree {
			w.name = strconv.Itoa(j + 1)
		} else {
			w.name = w.kind.label()
		}
	}
}

// nextWindow cycles to the next window.
func (m *model) nextWindow() {
	m.ensureWindows()
	if len(m.windows) < 2 {
		m.status = "only one window — Ctrl-b c opens another"
		return
	}
	m.saveWindow()
	m.activateWindow((m.winIdx + 1) % len(m.windows))
	m.status = "window " + m.windows[m.winIdx].name
}

// selectWindow switches to window index i (0-based) if it exists.
func (m *model) selectWindow(i int) {
	m.ensureWindows()
	if i == m.winIdx || i < 0 || i >= len(m.windows) {
		return
	}
	m.saveWindow()
	m.activateWindow(i)
	m.status = "window " + m.windows[m.winIdx].name
}

// killWindow closes the active window (never the last one) and renumbers the rest.
func (m *model) killWindow() {
	m.ensureWindows()
	if len(m.windows) < 2 {
		m.status = "can't close the last window (Ctrl-b x closes a pane)"
		return
	}
	// In enforced mode the Info and Subagents tabs are fixed fixtures and can't be
	// closed (only Shell tabs can). Turn on Customizable Panes to close any tab.
	if k := m.windows[m.winIdx].kind; k == tabInfo || k == tabSubagents {
		m.status = "the " + k.label() + " tab can't be closed in enforced mode (Ctrl-b x closes a pane)"
		return
	}
	i := m.winIdx
	m.reapShells(m.paneRoot) // cancel any shells living in the window being closed
	m.windows = append(m.windows[:i], m.windows[i+1:]...)
	m.renumberTabs()
	if i >= len(m.windows) {
		i = len(m.windows) - 1
	}
	m.activateWindow(i)
	m.status = "closed window; now on " + m.windows[m.winIdx].name
}

// renderTabs returns a compact tab strip for the header, shown only when more than
// one window exists. The tabs are PLAIN text (no per-tab styling): the header is one
// uniformly-styled title bar, and a foreground-only style here would emit a reset
// that drops the title bar's background to black for the rest of the line. The
// active window is marked by brackets alone.
func (m *model) renderTabs() string {
	if len(m.windows) < 2 {
		return ""
	}
	var b strings.Builder
	b.WriteString(tabStripLead)
	for i := range m.windows {
		b.WriteString(m.tabPart(i))
	}
	return b.String()
}

// tabStripLead is the separator the tab strip starts with, after the env id.
const tabStripLead = "· "

// tabPart is the header label for window i: the active one is bracketed, the rest
// are space-padded (so both are the same width for a single-character name).
func (m *model) tabPart(i int) string {
	if i == m.winIdx {
		return "[" + m.windows[i].name + "]"
	}
	return " " + m.windows[i].name + " "
}

// tabSpan is one window tab's clickable column range [start,end) in the header row.
type tabSpan struct {
	idx        int
	start, end int
}

// tabSpans returns the clickable column range of each window tab in the header,
// matching renderTabs' layout exactly, so a left-click on a tab can switch to it.
// It is nil when the tab strip is hidden (fewer than two windows).
func (m *model) tabSpans() []tabSpan {
	if len(m.windows) < 2 {
		return nil
	}
	// The header is " oline · <mode> · <envID> " followed by the tab strip.
	col := lipgloss.Width(fmt.Sprintf(" oline · %s · %s ", m.mode, m.envID)) + lipgloss.Width(tabStripLead)
	spans := make([]tabSpan, 0, len(m.windows))
	for i := range m.windows {
		w := lipgloss.Width(m.tabPart(i))
		spans = append(spans, tabSpan{idx: i, start: col, end: col + w})
		col += w
	}
	return spans
}

// answerTarget returns the agent a typed message should ANSWER (because it is
// blocked on an `ask`), or "" to treat the message as a normal steer. It prefers
// the focused agent; failing that, if exactly one agent is waiting, that one.
func (m *model) answerTarget(focused core.AgentID) core.AgentID {
	if m.awaitingAnswer[focused] {
		return focused
	}
	if len(m.awaitingAnswer) == 1 {
		for a := range m.awaitingAnswer {
			return a
		}
	}
	return ""
}

// applyBudgetInput parses the command line as a time budget for m.budgetTarget:
// a positive number of minutes, or "none"/"off"/"0" to remove the limit.
func (m *model) applyBudgetInput() {
	raw := strings.TrimSpace(m.input.Value())
	target := m.budgetTarget
	m.budgetInput = false
	m.input.SetValue("")
	switch strings.ToLower(raw) {
	case "none", "off", "0", "remove", "unlimited":
		m.router.SetBudget(target, 0)
		m.status = "removed time budget for " + string(target)
		return
	}
	mins, err := strconv.Atoi(raw)
	if err != nil || mins <= 0 {
		m.status = "invalid budget — enter minutes, or 'none' to remove"
		return
	}
	m.router.SetBudget(target, time.Duration(mins)*time.Minute)
	m.status = fmt.Sprintf("time budget for %s set to %dm", target, mins)
}

func (m *model) cyclePane() {
	ls := leaves(m.paneRoot)
	for i, l := range ls {
		if l == m.focusPane {
			m.focusPane = ls[(i+1)%len(ls)]
			return
		}
	}
}

func (m *model) navPane(dir byte) {
	if n := navigate(m.paneRoot, m.focusPane, dir); n != nil {
		m.focusPane = n
	}
}

func (m *model) sendSummary() {
	m.router.Send(engine.Envelope{
		From: "User", To: core.Main, Kind: engine.MsgSteer,
		Body: "REFRESH BOTH LIVE PANELS NOW: first call `update_summary` with a concise, plain-language state-of-play of the engagement so far (what's done, what's confirmed, what's in progress, what's next), then call `update_diagram` with an up-to-date, legible ASCII map of the engagement (pentester: the network — hosts, footholds vs. root, connections, key services; exploiter: the software's main components and how they interact), then continue your work.",
	})
}

func (m *model) sendDiagram() {
	m.router.Send(engine.Envelope{
		From: "User", To: core.Main, Kind: engine.MsgSteer,
		Body: "REFRESH THE DIAGRAM PANEL NOW: call `update_diagram` with an up-to-date, legible ASCII map of the engagement (pentester: the network — hosts, footholds vs. root, connections, key services; exploiter: the software's main components and how they interact), then continue your work.",
	})
}

// setFocus points the focused pane at a view, identified by id: a real agent id
// (including Main), or the synthetic summaryPaneID / diagramPaneID for the two
// info views. This is how the roster click and the up/down navigation switch a pane
// between agents and info views (and how a closed Summary/Diagram view is
// reopened — just navigate a pane onto it). A live shell pane is never clobbered:
// a different non-shell pane is retargeted instead.
func (m *model) setFocus(id core.AgentID) {
	// Enforced mode restricts which views a tab may show; ignore a retarget to a view
	// that isn't allowed on the active tab (the roster won't offer those anyway).
	if !m.viewAllowedHere(id) {
		return
	}
	p := m.focusPane
	if p != nil && (p.shell || p.rev || p.term) {
		// A command pane (shell / reverse-shell) can't just be retargeted — its live
		// state is tied to the leaf. Prefer retargeting a different, ordinary leaf so
		// the command pane is preserved. But when the command pane is the only pane in
		// the window (e.g. the auto-opened Shell tab), there is nothing else to point
		// at, so convert this pane in place: reap its live state and let it show the
		// selected view. Without this, selecting a view does nothing and the tab is
		// stuck on the shell.
		if alt := m.firstViewableLeaf(); alt != nil {
			p = alt
			m.focusPane = p
		} else {
			m.reapShell(p)
			m.reapRev(p)
			m.reapTerm(p)
			p.shell, p.rev, p.term = false, false, false
		}
	}
	if p == nil {
		return
	}
	p.summary = id == summaryPaneID
	p.diagram = id == diagramPaneID
	p.agent = id // for an info view this is the synthetic id (used for refresh/hit-testing)
	m.refreshLeaf(p)
}

// firstViewableLeaf returns the first leaf in the current window that is an ordinary
// agent/info pane (not a live shell or reverse-shell pane, which can't be retargeted
// without destroying their state), or nil when every pane is a command pane.
func (m *model) firstViewableLeaf() *pane {
	for _, l := range leaves(m.paneRoot) {
		if !l.shell && !l.rev && !l.term {
			return l
		}
	}
	return nil
}

// navOrder is the ordered list of selectable views the roster renders and that
// up/down cycles through: the info panes first (Main, the Summary view, the
// Diagram view), then the active subagents (running/paused), then the idle ones.
// The two info views use their synthetic ids so setFocus can switch a pane onto
// them.
func (m *model) navOrder() []core.AgentID {
	info := []core.AgentID{core.Main, summaryPaneID, diagramPaneID}
	var active, idle []core.AgentID
	for _, id := range m.order {
		if id == core.Main {
			continue // listed under info panes, not as a subagent
		}
		if s := m.states[id]; s == core.Running || s == core.Paused {
			active = append(active, id)
		} else {
			idle = append(idle, id)
		}
	}
	subs := append(active, idle...)
	// Enforced mode cycles only the active tab's category; customizable cycles all.
	switch m.activeKind() {
	case tabInfo:
		return info
	case tabSubagents:
		return subs
	case tabShell:
		return nil // command panes aren't cycled here; open them with Ctrl-b $/T/L
	default:
		return append(info, subs...)
	}
}

func indexOfAgent(list []core.AgentID, id core.AgentID) int {
	for i, x := range list {
		if x == id {
			return i
		}
	}
	return -1
}

// focusRosterDelta moves the focused pane to the next/previous view in navOrder
// (info panes and subagents alike).
func (m *model) focusRosterDelta(delta int) {
	order := m.navOrder()
	if len(order) == 0 {
		return
	}
	i := indexOfAgent(order, m.focusedAgent())
	if i < 0 {
		i = 0
	} else {
		i = (i + delta + len(order)) % len(order)
	}
	m.setFocus(order[i])
	m.rosterFollow = true // scroll the Options pane so the new selection is visible
	m.status = "focus → " + navLabel(order[i])
}

// navLabel is the human label for a view id (info views get friendly names).
func navLabel(id core.AgentID) string {
	switch id {
	case summaryPaneID:
		return "Summary"
	case diagramPaneID:
		return "Diagram"
	default:
		return string(id)
	}
}

// focusNextAgent cycles the focused pane to the next agent (tmux-style Ctrl-b n).
func (m *model) focusNextAgent() { m.focusRosterDelta(1) }

// enterCopyMode shows the focused pane's transcript as plain fullscreen text, so
// the terminal's own selection copies just that pane's lines (no other panes, no
// styling/borders).
func (m *model) enterCopyMode() {
	id := m.focusedAgent()
	m.copyAgent = id
	vp := viewport.New(maxInt(m.w, 1), maxInt(m.h-2, 1))
	vp.SetContent(m.tr(id).plainText(m.showThoughts))
	vp.GotoBottom()
	m.copyVP = vp
	m.copyMode = true
	m.status = "copy mode: " + string(id)
}

func (m *model) viewCopyMode() string {
	hdr := titleBar.Render(fmt.Sprintf(" copy: %s — drag to select · ↑↓/wheel scroll · Esc exits ", m.copyAgent))
	return lipgloss.JoinVertical(lipgloss.Left, hdr, m.copyVP.View())
}

func (m *model) shutdown() {
	m.refreshLayoutSnapshot()
	m.saveHistory() // persist the conversation so it can be reloaded on resume
	if m.cancel != nil {
		m.cancel()
	}
	for _, sh := range m.shells { // cancel any user shell commands still running
		sh.stop()
	}
	// Shut the sandbox down after agents are cancelled (above): QEMU powers off the
	// VM; WSL tears down the per-agent firewall rules (the distro keeps running, so
	// the rules would otherwise linger). Done last so nothing is running under the
	// rules when they are removed.
	if m.sess != nil && m.sess.Sandbox != nil {
		if sd, ok := m.sess.Sandbox.(sandbox.Shutdowner); ok {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			_ = sd.Shutdown(ctx)
			cancel()
		}
	}
	if m.log != nil {
		m.log.banner("engagement session ended")
		m.log.close()
		m.log = nil
	}
	if m.audit != nil {
		_ = m.audit.Close()
		m.audit = nil
	}
	if m.mcp != nil {
		m.mcp.Close() // terminate the external MCP server processes
		m.mcp = nil
	}
}

// ---- event application ---------------------------------------------------

// apply mutates state for one event and renders the affected pane. Used for a
// single event (and by tests); the live loop uses mutate + flushDirty to batch.
func (m *model) apply(ev event.Event) {
	m.mutate(ev)
	m.flushDirty()
}

// markDirty records that an agent's pane needs re-rendering.
func (m *model) markDirty(id core.AgentID) {
	if m.dirty == nil {
		m.dirty = map[core.AgentID]bool{}
	}
	m.dirty[id] = true
}

// flushDirty re-renders every pane marked dirty since the last flush (once each).
func (m *model) flushDirty() {
	for id := range m.dirty {
		m.refreshFor(id)
		delete(m.dirty, id)
	}
}

// mutate applies one engine event to the model WITHOUT rendering (it marks the
// touched agent dirty). flushDirty then renders each dirty pane once.
func (m *model) mutate(ev event.Event) {
	m.log.log(ev) // tee every agent event to the host activity log
	switch e := ev.(type) {
	case event.TokenDelta:
		t := m.tr(e.Agent)
		if e.Text != "" {
			t.appendText(e.Text)
		}
		if e.Reasoning != "" {
			t.appendThink(e.Reasoning)
		}
		m.markDirty(e.Agent)
	case event.ToolStarted:
		// A clear space separates the gear glyph from the tool name; the output
		// (if any) is collapsed under this one-line header until clicked.
		header := "⚙  " + e.Tool
		if strings.TrimSpace(e.ArgsPreview) != "" {
			header += "  " + e.ArgsPreview
		}
		// full = the whole command (untruncated), shown when the call is expanded.
		full := "⚙  " + e.Tool
		if a := strings.TrimSpace(e.Args); a != "" {
			full += "  " + a
		}
		m.tr(e.Agent).startTool(header, full)
		m.markDirty(e.Agent)
	case event.ToolOutput:
		m.tr(e.Agent).appendToolOut(e.Chunk)
		m.markDirty(e.Agent)
	case event.AgentState:
		m.tr(e.Agent)
		prev := m.states[e.Agent] // capture before the overwrite, to detect transitions
		m.states[e.Agent] = e.State
		if e.State == core.Killed {
			delete(m.awaitingAnswer, e.Agent) // a terminated agent is no longer waiting on the user
			// Don't leave a stale "question from X" cue if the asking agent is gone.
			if len(m.awaitingAnswer) == 0 && strings.HasPrefix(m.status, "question from ") {
				m.status = ""
			}
		}
		if e.State == core.Idle || e.State == core.Killed {
			m.transcripts[e.Agent].finalizeOpen()
			m.markDirty(e.Agent)
			m.requestSave() // an agent finishing is a durable milestone
		}
		// Drive the Subagents-tab auto-layout on a subagent's live<->not-live
		// transitions: open a pane when it (re)starts running, close it when it goes
		// idle/killed (close gated by the config toggle). Main has no auto pane.
		if e.Agent != core.Main {
			switch {
			case e.State == core.Running && prev != core.Running && prev != core.Paused:
				m.autoAdmitSubagent(e.Agent) // first run / reprompt: open a pane if < 4
			case (e.State == core.Idle || e.State == core.Killed) && m.closeIdleSubagentPanes():
				m.autoRemoveSubagent(e.Agent) // free the slot
			}
		}
	case event.Spawned:
		m.addAgent(e.Child)
		m.autoPlaceSpawn(e.Child) // open/arrange its pane on the auto-layout tab
		m.requestSave()           // persist the new agent promptly
		m.tr(core.Main).addRich(styleAccent.Render("→ spawned ") + string(e.Child) + styleMuted.Render(" ("+e.Role+")"))
		m.markDirty(core.Main)
		// Put the orchestrator's task prompt at the TOP of the new subagent's pane so
		// the user can scroll up to see exactly what it was told to do.
		if strings.TrimSpace(e.Prompt) != "" {
			t := m.tr(e.Child)
			t.addStyled("task from the orchestrator:", styleAccent)
			// Render the prompt body as markdown (segText → glamour), like assistant
			// text, so an orchestrator's markdown instructions aren't shown as raw text.
			t.appendText(e.Prompt)
			t.finalizeOpen()
			m.markDirty(e.Child)
		}
	case event.UserQuestion:
		// Track this agent as awaiting an answer. Additive (not a reset) so that
		// several subagents can have open questions at once — each entry is cleared
		// when that agent is answered (see the enter handler) or is terminated.
		m.awaitingAnswer[e.From] = true
		q := "❓ " + e.Text
		if len(e.Options) > 0 {
			q += "\n   options: " + strings.Join(e.Options, " · ")
		}
		m.tr(e.From).addStyled(q, styleWarn)
		m.markDirty(e.From)
		m.status = "question from " + string(e.From) + " — type an answer and press Enter"
	case event.Notice:
		st := styleMuted
		if e.Level == "error" {
			st = styleErr
		} else if e.Level == "warn" {
			st = styleWarn
		}
		to := e.Agent
		if to == "" {
			to = core.Main
		}
		m.tr(to).addStyled("["+e.Level+"] "+e.Text, st)
		m.markDirty(to)
	case event.Usage:
		in, out, cacheRead := m.costFor(e.Model)
		// Prompt tokens served from the provider's cache are billed at the cheaper
		// cache-read rate, not the full input rate; only the uncached remainder is
		// billed at `in`. Billing everything at `in` overcounts heavily when large
		// system prompts are reused across turns.
		cached := e.CachedPromptTokens
		if cached > e.PromptTokens {
			cached = e.PromptTokens
		}
		uncached := e.PromptTokens - cached
		cost := float64(uncached)/1e6*in +
			float64(cached)/1e6*cacheRead +
			float64(e.CompletionTokens)/1e6*out
		m.totalCost += cost
		if e.Summarizer {
			m.summaryCost += cost
		}
		m.enforceCostCap()
		m.enforceSummaryCap()
	case event.Finding:
		// There is no findings pane anymore; the finding is logged and surfaced in the
		// recording agent's transcript (and so into saved history for the reporter).
		line := "✔ finding [" + strings.ToUpper(e.Severity) + "] " + e.Title
		if e.Target != "" {
			line += " — " + e.Target
		}
		m.tr(e.Agent).addStyled(line, severityStyle(e.Severity))
		m.markDirty(e.Agent)
		m.requestSave() // a confirmed finding is worth persisting immediately
	case event.Summary:
		m.summary = e.Text
		m.markDirty(summaryPaneID) // refresh the live Summary pane
	case event.Diagram:
		m.diagram = e.Text
		m.markDirty(diagramPaneID) // refresh the live Diagram pane
	case event.Channel:
		m.applyChannel(e.Name, e.Port, e.Dir, e.Open)
	}
}

// enforceCostCap pauses the whole fleet the first time the running spend reaches the
// configured cap, and tells the operator. It fires once (a circuit breaker): after
// that the user can raise budget.maxCostUSD, or resume with Ctrl-b R to continue
// past the cap for this run.
func (m *model) enforceCostCap() {
	if m.costCap <= 0 || m.costCapHit || m.totalCost < m.costCap {
		return
	}
	m.costCapHit = true
	if m.router != nil {
		m.router.PauseAll()
	}
	m.emitNotice("error", fmt.Sprintf("SPEND CAP reached: $%.2f of $%.2f — all agents paused. Raise budget.maxCostUSD in config, or resume with Ctrl-b R to continue past the cap.", m.totalCost, m.costCap))
	m.status = fmt.Sprintf("spend cap $%.2f reached — agents paused (Ctrl-b R resumes)", m.costCap)
}

// enforceSummaryCap is the same circuit breaker for the summarizer (compaction)
// model: the first time its cumulative spend reaches the ceiling, pause the fleet
// and tell the operator (a misconfigured expensive `small` model is the usual
// cause). Fires once.
func (m *model) enforceSummaryCap() {
	if m.summaryCap <= 0 || m.summaryCapHit || m.summaryCost < m.summaryCap {
		return
	}
	m.summaryCapHit = true
	if m.router != nil {
		m.router.PauseAll()
	}
	m.emitNotice("error", fmt.Sprintf("SUMMARIZER COST CAP reached: $%.2f of $%.2f spent on context compaction — all agents paused. Point the `small` tier at a cheaper model or raise budget.maxSummaryCostUSD, then resume with Ctrl-b R.", m.summaryCost, m.summaryCap))
	m.status = fmt.Sprintf("summarizer cost cap $%.2f reached — agents paused (Ctrl-b R resumes)", m.summaryCap)
}

// costFor returns the per-1M-token input / output / cache-read USD rates for a
// model id, exactly as configured. Prices are literal: a rate of 0 means $0 (free /
// untracked), including cache-read — set a cache-read rate explicitly if cached
// tokens should be billed.
func (m *model) costFor(modelID string) (in, out, cacheRead float64) {
	for _, p := range m.store.Effective().Providers {
		for _, md := range p.Models {
			if md.ID == modelID {
				return md.Cost.Input, md.Cost.Output, md.Cost.CacheRead
			}
		}
	}
	return 0, 0, 0
}

// modelContextWindow returns the declared context window for a "provider/model-id"
// selector, or 0 if unknown.
func (m *model) modelContextWindow(sel string) int {
	prov, id, ok := strings.Cut(sel, "/")
	if !ok {
		return 0
	}
	for _, md := range m.store.Providers()[prov].Models {
		if md.ID == id {
			return md.ContextWindow
		}
	}
	return 0
}

// routerRolesFromRoster builds the decision-model router's role list from the current
// mode's roster: each spawnable subagent's name + a one-line description. It is scoped
// to this roster, so the router can only ever pick a role that exists in this mode.
func routerRolesFromRoster(r *agents.Roster) []engine.RoleInfo {
	out := make([]engine.RoleInfo, 0, len(r.Agents))
	for _, name := range r.Names() {
		desc := r.Agents[name].Description
		if i := strings.IndexByte(desc, '\n'); i >= 0 {
			desc = desc[:i]
		}
		out = append(out, engine.RoleInfo{Name: name, Description: strings.TrimSpace(desc)})
	}
	return out
}

func (m *model) addAgent(id core.AgentID) {
	if _, ok := m.transcripts[id]; ok {
		return
	}
	m.transcripts[id] = &transcript{}
	m.order = append(m.order, id)
	if _, ok := m.states[id]; !ok {
		m.states[id] = core.Running
	}
}

// tr returns an agent's transcript, creating it (and the agent) if needed.
func (m *model) tr(id core.AgentID) *transcript {
	t := m.transcripts[id]
	if t == nil {
		m.addAgent(id)
		t = m.transcripts[id]
	}
	return t
}

// writeln appends a pre-styled harness line to an agent's transcript.
func (m *model) writeln(id core.AgentID, pre string) {
	m.tr(id).addRich(pre)
	m.refreshFor(id)
}

// ---- view ----------------------------------------------------------------

func (m *model) View() string {
	if m.w == 0 {
		return "starting oline…"
	}
	var s string
	switch {
	case m.editing:
		s = m.viewEditor()
	case m.running:
		s = m.viewRunning()
	default:
		s = m.viewWizard()
	}
	// Paint the whole screen pure black so the logo and every pane sit on one
	// seamless background (MaxHeight clips rather than growing past the screen).
	return screenStyle.Width(m.w).Height(m.h).MaxHeight(m.h).Render(s)
}

// logoBlock renders the embedded ANSI logo centered for the start screen. It is
// skipped when the terminal is too small to spare the vertical/horizontal room.
func (m *model) logoBlock() string {
	if m.hideLogo {
		return "" // the Sixel splash already showed the banner
	}
	art := strings.TrimRight(assets.Logo, "\n")
	if art == "" {
		return ""
	}
	lines := strings.Split(art, "\n")
	// Visible width ignores the ANSI color codes in each cell. The caller
	// (viewWizard) decides whether there is vertical room; here we only require
	// the width to fit and a basic height floor.
	cols := lipgloss.Width(lines[0])
	if cols == 0 || m.w < cols+4 || m.h < len(lines)+2 {
		return ""
	}
	prefix := strings.Repeat(" ", (m.w-cols)/2)
	var sb strings.Builder
	sb.WriteString("\n")
	for _, ln := range lines {
		sb.WriteString(prefix + ln + "\n")
	}
	return sb.String()
}

func (m *model) viewWizard() string {
	var b strings.Builder
	b.WriteString(titleBar.Render("oline — setup") + "\n\n")
	step := func(active bool, label, body string) {
		head := "  "
		if active {
			head = styleAccent.Render("▶ ")
		}
		b.WriteString(head + lipgloss.NewStyle().Bold(active).Render(label) + "\n")
		if active && body != "" {
			b.WriteString(body + "\n")
		}
		b.WriteString("\n")
	}
	radio := func(sel int, opts ...string) string {
		var sb strings.Builder
		for i, o := range opts {
			cur := "  ○ "
			if i == sel {
				cur = styleAccent.Render("  ● ")
			}
			sb.WriteString(cur + o + "\n")
		}
		return sb.String()
	}

	step(m.step == wMode, "1. Mode", radio(m.modeSel, "Pentester", "Exploiter"))
	if m.step == wSetup {
		step(true, "Models — set up your provider (first-run setup)", m.setupBodyView())
	}
	if m.step == wModels {
		step(true, "Models — pick one per tier (first-run setup)", m.modelsBodyView())
	}
	if m.step == wKey {
		step(true, "API key — "+m.keyProvider+" (not set)", "    "+m.keyIn.View()+"\n    "+styleMuted.Render("no key in $ENV or secrets.yml — stored to ~/oline/config/secrets.yml (git-ignored)"))
	}
	step(m.step == wStart, "2. Engagement", radio(m.startSel, "New engagement", "Resume a previous engagement"))
	step(m.step == wEnv, "3. Environment", m.envBodyView())
	if m.step == wDisk {
		step(true, "   Disk size for the new VM (GB)",
			"    "+m.diskIn.View()+"\n    "+styleMuted.Render("how much disk the VM may use; installing tools fills a small disk fast (default 25 GB)"))
	}
	if m.step == wCPUs {
		step(true, "   Virtual CPUs for the new VM",
			"    "+m.cpusIn.View()+"\n    "+styleMuted.Render("how many vCPUs the VM gets (default 4)"))
	}
	if m.step == wMem {
		step(true, "   RAM for the new VM (GB)",
			"    "+m.memIn.View()+"\n    "+styleMuted.Render("how much memory the VM gets (default 8 GB)"))
	}
	if m.step == wWSLRootfs {
		step(true, "   Debian rootfs tarball for the new WSL distro",
			"    "+m.wslRootfsIn.View()+"\n    "+styleMuted.Render("your WSL is too old to install by name — point to a Debian rootfs .tar(.gz/.xz) to import as "+m.newEnvName))
	}

	scopeWord := "Scope"
	scopeResumeLabel := "5. Scope (confirm — targets may have changed)"
	if m.mode == config.ModeExploiter {
		scopeWord = "Application"
		scopeResumeLabel = "5. Application (confirm — it may have changed)"
	}
	if m.resume {
		step(m.step == wResume, "4. Previous engagement", m.resumeBodyView(radio))
		if m.step == wImport {
			step(true, "   Path (host folder/.tar.gz, or a path in the environment)", "    "+m.importIn.View())
		}
		if m.step == wHistory {
			step(true, "   Conversation history", radio(m.histSel,
				"Load the previous conversation history", "Start from a clean slate (no history)"))
		}
		step(m.step == wScope, scopeResumeLabel, "    "+m.scopeIn.View())
		step(m.step == wGoal, "6. Additional instructions (optional)", "    "+m.goalIn.View())
	} else {
		step(m.step == wScope, "4. "+scopeWord, "    "+m.scopeIn.View())
		step(m.step == wGoal, "5. Goal", "    "+m.goalIn.View())
	}

	step(m.step == wVPN, "6. VPN (optional)", radio(m.vpnSel,
		"No VPN",
		"Use a VPN already running in the environment",
		"OpenVPN config already in the environment",
		"Upload an OpenVPN config from the host"))
	if m.step == wVPNInput {
		step(true, "   OpenVPN config path", "    "+m.vpnIn.View())
	}

	step(m.step == wData, "7. Copy a host folder into the workspace (optional)",
		"    "+m.dataDirIn.View()+"\n    "+styleMuted.Render("agents read a copy in the workspace; the original host files are never exposed and cannot be changed"))

	step(m.step == wAuto, "8. Automation", radio(m.autoSel,
		"Automatic (AI runs end-to-end)",
		"Planner (AI proposes, you approve each step)",
		"Watcher (you run everything; AI only watches and advises)"))

	if m.preparing {
		msg := "  preparing environment…"
		switch {
		case m.createWSL:
			msg = "  creating the WSL distro (installing Debian + the egress toolchain — this can take a few minutes)…"
		case m.prepCand.Backend == sandbox.BackendQEMU:
			msg = "  booting QEMU VM (first run downloads the image — this can take a few minutes)…"
		}
		b.WriteString(styleAccent.Render(msg) + "\n")
	}
	if m.wizErr != "" {
		b.WriteString(styleErr.Render("  "+m.wizErr) + "\n")
	}
	if m.status != "" {
		b.WriteString(styleAccent.Render("  "+m.status) + "\n")
	}
	back := ""
	if len(m.wizHistory) > 0 {
		back = " · Esc back"
	}
	b.WriteString(styleMuted.Render("\n  ↑/↓ choose · type to edit · Enter next" + back + " · Ctrl-c twice to quit"))
	// The logo is rendered outside the padded body so its own centering (and raw
	// truecolor cells) aren't shifted or reflowed by lipgloss padding.
	body := lipgloss.NewStyle().Padding(1, 2).Render(b.String())
	logo := m.logoBlock()
	// Only prepend the logo when it AND the wizard body both fit the screen; a
	// taller window never overflows (which was making the window misbehave).
	if logo != "" {
		if strings.Count(logo, "\n")+strings.Count(body, "\n")+2 > m.h {
			logo = ""
		}
	}
	return logo + body
}

// modelsBodyView renders the first-run model picker: one row per tier, the focused
// row cycling through the catalog with left/right.
func (m *model) modelsBodyView() string {
	if len(m.catalog) == 0 {
		return styleWarn.Render("    no models found in models.yml — add a provider/model there first")
	}
	labels := [3]string{"big   ", "medium", "small "}
	hints := [3]string{"exploiters, thinkers, critiques, orchestrator", "balanced, in-between work", "summarizers + context compaction"}
	var sb strings.Builder
	for i := 0; i < 3; i++ {
		cur := "    "
		if i == m.roleFocus {
			cur = styleAccent.Render("  ▶ ")
		}
		sel := m.catalog[m.roleSel[i]].label
		sb.WriteString(cur + lipgloss.NewStyle().Bold(i == m.roleFocus).Render(labels[i]) + "  " + sel + "\n")
		sb.WriteString("        " + styleMuted.Render(hints[i]) + "\n")
	}
	sb.WriteString(styleMuted.Render("    ↑/↓ choose tier · ←/→ change model · Enter confirms all three · Esc back"))
	return sb.String()
}

func (m *model) envBodyView() string {
	if m.step != wEnv {
		if m.selDistro != "" {
			return "    " + styleMuted.Render(m.selDistro)
		}
		return ""
	}
	if !m.scanned {
		return "    scanning environments…"
	}
	if len(m.cands) == 0 {
		return styleWarn.Render("    no WSL/QEMU environments found — press r to rescan")
	}
	var sb strings.Builder
	anyResumable := false
	for i, c := range m.cands {
		cur := "  ○ "
		if i == m.envSel {
			cur = styleAccent.Render("  ● ")
		}
		tag := ""
		if c.Managed {
			tag = styleMuted.Render("  [oline-managed]")
		}
		name := c.ID
		if c.NeedsCreate {
			name = c.Name // the synthetic "create new …" entries read as a sentence
		}
		extra := ""
		if c.Engagement != "" {
			extra = styleMuted.Render("  (saved VM — resumes its disk & tooling)")
		}
		// A reusable existing environment (a saved QEMU VM or an existing WSL distro)
		// is something the user can resume into; a "create new …" entry is not.
		if c.Engagement != "" || (c.Backend == sandbox.BackendWSL && !c.NeedsCreate) {
			anyResumable = true
		}
		sb.WriteString(fmt.Sprintf("%s%s  %s%s%s\n", cur, name, styleMuted.Render(string(c.Backend)), tag, extra))
	}
	// When the user asked to resume but nothing here can be resumed, say so plainly
	// rather than leaving an empty "previous engagement" selection.
	if m.resume && !anyResumable {
		sb.WriteString(styleWarn.Render("    No previous engagement is available to resume — every entry above starts a NEW one.\n"))
	}
	sb.WriteString(styleMuted.Render("    (r to rescan)"))
	return sb.String()
}

func (m *model) resumeBodyView(radio func(int, ...string) string) string {
	if m.step != wResume {
		if m.engagement != "" {
			return "    " + styleMuted.Render(m.engagement)
		}
		return ""
	}
	opts := append([]string{}, m.engagements...)
	opts = append(opts, "Provide a path (host folder/.tar.gz, or a path in the environment)…")
	var sb strings.Builder
	sb.WriteString(radio(m.engSel, opts...))
	if len(m.engagements) == 0 {
		sb.WriteString(styleMuted.Render("    (no saved engagements found in " + m.selDistro + " — choose Import)"))
	}
	return sb.String()
}

func (m *model) viewRunning() string {
	if m.copyMode {
		return m.viewCopyMode()
	}
	// Cap the header to a single row of the terminal width: a wide header (long env
	// id + tab strip) would otherwise wrap to two rows, making the whole frame taller
	// than the screen and clipping the bottom pane.
	header := titleBar.MaxWidth(m.w).MaxHeight(1).Render(fmt.Sprintf(" oline · %s · %s %s", m.mode, m.envID, m.renderTabs()))

	var main string
	if m.zoom && m.focusPane != nil {
		main = m.renderLeaf(m.focusPane)
	} else if m.paneRoot != nil {
		main = m.renderPane(m.paneRoot)
	}
	// The Options pane is hidden with Ctrl-b ?, giving the main panes the full width.
	body := main
	if m.showRoster {
		roster := m.renderRoster(m.rightW, m.bodyH)
		body = lipgloss.JoinHorizontal(lipgloss.Top, main, " ", roster)
	}

	running := 0
	for _, s := range m.states {
		if s == core.Running {
			running++
		}
	}
	mode := "automatic"
	if m.sess != nil {
		mode = m.sess.Mode.String()
	}
	th := "thoughts:on"
	if !m.showThoughts {
		th = "thoughts:off"
	}
	pfx := ""
	if m.prefix {
		pfx = styleAccent.Render(" PREFIX ")
	}
	// MaxHeight(1) so a long status can never wrap to a second line and push the
	// view past the screen height.
	status := statusBar.Width(m.w).MaxHeight(1).Render(fmt.Sprintf(" %s · focus:%s · %d/%d running · %s%s · %s",
		mode, m.focusedAgent(), running, m.store.Effective().MaxConcurrency, th, pfx, m.status))
	// When an agent is waiting for an answer, the prompt turns into a cue so the
	// user knows their next message answers it (not a steer). The roster names the
	// agent; the input prompt is kept short so the line never overflows.
	inPrompt := styleAccent.Render(" > ")
	switch {
	case m.budgetInput:
		inPrompt = styleWarn.Render(" ⏱ ")
	case m.focusShell() != nil:
		inPrompt = styleAccent.Render(" $ ")
	case m.focusRev() != nil:
		inPrompt = styleAccent.Render(" ⇄ ")
	case m.focusTerm() != nil:
		inPrompt = styleAccent.Render(" ⌨ ")
	case m.listenInput:
		inPrompt = styleWarn.Render(" ⌨ ")
	case m.answerTarget(m.focusedAgent()) != "":
		inPrompt = styleWarn.Render(" ❓ ")
	}
	input := inPrompt + m.input.View()

	return lipgloss.JoinVertical(lipgloss.Left, header, body, status, input)
}

// renderPane recursively renders the split tree.
func (m *model) renderPane(p *pane) string {
	if p.leaf {
		return m.renderLeaf(p)
	}
	if p.dir == splitH {
		return lipgloss.JoinHorizontal(lipgloss.Top, m.renderPane(p.a), m.renderPane(p.b))
	}
	return lipgloss.JoinVertical(lipgloss.Left, m.renderPane(p.a), m.renderPane(p.b))
}

func (m *model) renderLeaf(p *pane) string {
	innerW := p.w - 2
	if innerW < 1 {
		innerW = 1
	}
	innerH := p.h - 2
	if innerH < 1 {
		innerH = 1
	}
	border := paneBorder
	label := string(p.agent)
	switch {
	case p.shell:
		label = string(p.agent) + "  $ shell"
	case p.rev:
		label = string(p.agent) + "  ⇄ reverse shell"
	case p.term:
		label = string(p.agent) + "  ⌨ terminal"
		if ts := m.terms[p.agent]; ts != nil && ts.title != "" {
			label += "  " + styleMuted.Render(ts.title)
		}
	case p.summary:
		label = "Summary"
	case p.diagram:
		label = "Diagram"
	case p.empty:
		label = "Subagents (auto-layout)"
	}
	title := styleMuted.Render(label)
	if p == m.focusPane {
		border = paneBorderFocused
		title = styleAccent.Render(label)
	}
	switch {
	case p.shell:
		if sh := m.shells[p.agent]; sh != nil && sh.running() {
			title += "  " + styleWarn.Render("running… (Ctrl-b S stops)")
		}
	case p.summary || p.diagram:
		// no state badge for the info panes
	default:
		if s, ok := m.states[p.agent]; ok {
			title += "  " + stateStyle(s.String()).Render(s.String())
		}
	}
	head := lipgloss.NewStyle().Width(innerW).MaxHeight(1).Render(title)
	body := p.vp.View()
	switch {
	case p.term:
		body = m.renderTerm(p, innerW, innerH-1, p == m.focusPane)
	case p.empty:
		hint := "No subagents yet.\n\nPanes open here automatically as the\norchestrator spawns subagents."
		body = lipgloss.NewStyle().Width(innerW).Height(innerH-1).
			Align(lipgloss.Center, lipgloss.Center).Foreground(styleMuted.GetForeground()).Render(hint)
	}
	return border.Width(innerW).Height(innerH).Render(head + "\n" + body)
}

func (m *model) renderRoster(w, h int) string {
	m.rosterAgentRows = map[int]core.AgentID{}
	m.rosterUserPaneRows = map[int]core.AgentID{}
	m.rosterChannelRows = map[int]string{}
	m.rosterTermRows = map[int]core.AgentID{}
	focused := m.focusedAgent()

	// Build the full content as a list of lines, with parallel maps from content line
	// index -> the view it selects (for scroll-aware click hit-testing): rowID for
	// agent/info views (click retargets the focused pane) and shellIdx for opened
	// shells (click jumps to that shell's pane).
	var rows []string
	rowID := map[int]core.AgentID{}
	shellIdx := map[int]core.AgentID{}
	focusedLine := -1
	add := func(s string) { rows = append(rows, s) }
	addSel := func(s string, id core.AgentID) {
		if id == focused {
			focusedLine = len(rows)
		}
		rowID[len(rows)] = id
		rows = append(rows, s)
	}
	addShell := func(s string, id core.AgentID) {
		if id == focused {
			focusedLine = len(rows)
		}
		shellIdx[len(rows)] = id
		rows = append(rows, s)
	}
	channelIdx := map[int]string{}
	addChannel := func(s, name string) {
		if revID(name) == focused {
			focusedLine = len(rows)
		}
		channelIdx[len(rows)] = name
		rows = append(rows, s)
	}
	termIdx := map[int]core.AgentID{}
	addTerm := func(s string, id core.AgentID) {
		if id == focused {
			focusedLine = len(rows)
		}
		termIdx[len(rows)] = id
		rows = append(rows, s)
	}

	add(styleAccent.Render("Pane Options") + "   " + styleMuted.Render(fmt.Sprintf("spent $%.4f", m.totalCost)))
	// Decision-model cue: whether this engagement is using the optional System-1
	// decision model (finding triage, judge pre-screen, progress checks, routing).
	if m.sess != nil {
		if m.sess.Decider != nil {
			add(styleGreenRender("● decision model: on"))
		} else {
			add(styleMuted.Render("○ decision model: off"))
		}
	}
	add(styleMuted.Render("agent · state · ⏱ · ctx%"))
	if n := len(m.awaitingAnswer); n > 0 {
		add(styleWarn.Render(fmt.Sprintf("❓ %d waiting for your answer", n)))
	}

	// Subagents split active (running/paused) first, then idle; Main is listed under
	// "info panes" below, not among the subagents.
	var active, idle []core.AgentID
	for _, id := range m.order {
		if id == core.Main {
			continue
		}
		if s := m.states[id]; s == core.Running || s == core.Paused {
			active = append(active, id)
		} else {
			idle = append(idle, id)
		}
	}

	renderRow := func(id core.AgentID) {
		st := m.states[id].String()
		cursor := "  "
		if id == focused {
			cursor = styleAccent.Render("▸ ")
		}
		badge := ""
		if m.awaitingAnswer[id] {
			badge += " " + styleWarn.Render("❓")
		}
		if id != core.Main && m.router != nil && m.states[id] == core.Running {
			if rem, ok := m.router.Remaining(id); ok {
				badge += " " + styleMuted.Render(fmtRemaining(rem))
			}
		}
		if m.router != nil {
			if pct, ok := m.router.ContextUsage(id); ok {
				badge += " " + styleMuted.Render(fmt.Sprintf("%d%%", pct))
			}
		}
		addSel(fmt.Sprintf("%s%-10s %s%s", cursor, truncate(string(id), 10), stateStyle(st).Render(st), badge), id)
	}

	renderInfoRow := func(id core.AgentID, label string) {
		cursor := "  "
		if id == focused {
			cursor = styleAccent.Render("▸ ")
		}
		addSel(cursor+label, id)
	}

	section := func(label string, ids []core.AgentID) {
		add(styleMuted.Render(fmt.Sprintf("%s (%d)", label, len(ids))))
		if len(ids) == 0 {
			add(styleMuted.Render("  —"))
			return
		}
		for _, id := range ids {
			renderRow(id)
		}
	}

	// Command panes: the shells the user has opened (via Ctrl-b $) and the
	// reverse-shell channels the agents have set up (via the `revshell` tool). Shown
	// ONLY when at least one exists. Each row is click-only — a shell row jumps to its
	// pane; a channel row opens (or jumps to) a pane that streams that channel and
	// sends what you type to its connected shell. These rows are not part of the
	// up/down view cycle.
	// The roster shows only the options for the active tab's kind; a free tab (and
	// customizable mode, where every tab is free) shows everything.
	kind := m.activeKind()
	showInfo := kind == tabFree || kind == tabInfo
	showSubs := kind == tabFree || kind == tabSubagents
	showCmd := kind == tabFree || kind == tabShell

	shells := m.shellIDsSorted()
	chans := m.channelsSorted()
	termsList := m.termIDsSorted()
	n := len(shells) + len(chans) + len(termsList)
	if showCmd && n == 0 && kind == tabShell {
		add(styleMuted.Render("command panes (0)"))
		add(styleMuted.Render("  open with Ctrl-b $ shell / T terminal / L revshell"))
	}
	if showCmd && n > 0 {
		add(styleMuted.Render(fmt.Sprintf("command panes (%d)", n)))
		for _, sid := range shells {
			cursor := "  "
			if sid == focused {
				cursor = styleAccent.Render("▸ ")
			}
			label := cursor + string(sid)
			if sh := m.shells[sid]; sh != nil && sh.running() {
				label += "  " + styleWarn.Render("running…")
			}
			addShell(label, sid)
		}
		for _, c := range chans {
			cursor := "  "
			if revID(c.name) == focused {
				cursor = styleAccent.Render("▸ ")
			}
			label := cursor + "⇄ " + c.name + styleMuted.Render(fmt.Sprintf(" :%d", c.port))
			if _, open := m.revs[revID(c.name)]; !open {
				label += "  " + styleMuted.Render("(click to open)")
			}
			addChannel(label, c.name)
		}
		for _, tid := range termsList {
			cursor := "  "
			if tid == focused {
				cursor = styleAccent.Render("▸ ")
			}
			label := cursor + "⌨ " + string(tid)
			if ts := m.terms[tid]; ts != nil && ts.title != "" {
				label += "  " + styleMuted.Render(ts.title)
			}
			addTerm(label, tid)
		}
	}

	// Info panes: the orchestrator plus the two live views. Selecting any of these
	// (click, or up/down) shows it in the focused pane — which is also how you reopen
	// a Summary or Diagram pane you closed. (Open a shell with Ctrl-b $.)
	if showInfo {
		add(styleMuted.Render("info panes"))
		renderRow(core.Main)
		renderInfoRow(summaryPaneID, "Summary")
		renderInfoRow(diagramPaneID, "Diagram")
	}

	if showSubs {
		section("active subagents", active)
		section("idle subagents", idle)
	}

	add("")
	add(styleAccent.Render("Ctrl-b Commands"))
	for _, kl := range strings.Split(keyHelp(), "\n") {
		add(styleMuted.Render(kl))
	}

	// The bordered box is exactly w × h; its inner content area is (w-2) × (h-2).
	innerW, innerH := w-2, h-2
	if innerW < 1 {
		innerW = 1
	}
	if innerH < 1 {
		innerH = 1
	}

	total := len(rows)
	// When the content overflows, reserve the last inner row for a scroll hint so the
	// user knows the pane scrolls (mouse wheel). Otherwise everything fits.
	visH := innerH
	overflow := total > innerH
	if overflow {
		visH = innerH - 1
		if visH < 1 {
			visH = 1
		}
	}
	maxScroll := total - visH
	if maxScroll < 0 {
		maxScroll = 0
	}
	// Keyboard navigation asks (via rosterFollow) to scroll the selected row into view.
	if m.rosterFollow && focusedLine >= 0 {
		if focusedLine < m.rosterScroll {
			m.rosterScroll = focusedLine
		} else if focusedLine >= m.rosterScroll+visH {
			m.rosterScroll = focusedLine - visH + 1
		}
		m.rosterFollow = false
	}
	if m.rosterScroll > maxScroll {
		m.rosterScroll = maxScroll
	}
	if m.rosterScroll < 0 {
		m.rosterScroll = 0
	}

	// Slice the visible window and map each visible selectable row to its screen Y.
	var b strings.Builder
	for i := 0; i < visH; i++ {
		ci := m.rosterScroll + i
		if ci >= total {
			break
		}
		screenY := m.bodyTop + 1 + i
		if id, ok := rowID[ci]; ok {
			m.rosterAgentRows[screenY] = id
		}
		if id, ok := shellIdx[ci]; ok {
			m.rosterUserPaneRows[screenY] = id
		}
		if name, ok := channelIdx[ci]; ok {
			m.rosterChannelRows[screenY] = name
		}
		if id, ok := termIdx[ci]; ok {
			m.rosterTermRows[screenY] = id
		}
		b.WriteString(rows[ci] + "\n")
	}
	if overflow {
		up, down := "  ", "  "
		if m.rosterScroll > 0 {
			up = "▲ "
		}
		if m.rosterScroll < maxScroll {
			down = "▼ "
		}
		b.WriteString(styleMuted.Render(fmt.Sprintf("%s%smore — scroll wheel", up, down)))
	}
	return paneBorder.Width(innerW).Height(innerH).MaxHeight(h).Render(strings.TrimRight(b.String(), "\n"))
}

// summaryContent is the body of the Summary pane: the orchestrator's live
// state-of-play (update_summary), wrapped to width w. Empty until the orchestrator
// sets one.
func (m *model) summaryContent(w int) string {
	if w < 1 {
		w = 1
	}
	if strings.TrimSpace(m.summary) == "" {
		// Wrap the placeholder to the pane width so it doesn't overflow and get clipped.
		return styleMuted.Width(w).Render("(no summary yet — the orchestrator updates this as the engagement progresses; Ctrl-b s asks it to refresh)")
	}
	// The orchestrator writes the summary in Markdown (update_summary); render it as
	// Markdown like the agent transcripts, not raw text. Hard-wrap the result to w so a
	// long token or wide table can't overflow the pane into the next one.
	return wrapPlain(renderMarkdown(m.getMD(w), strings.TrimSpace(m.summary)), w)
}

// diagramContent is the body of the Diagram pane: the orchestrator's live ASCII map
// (update_diagram), rendered VERBATIM in monospace — not as Markdown — so the art's
// column alignment is preserved. Each line is truncated (never wrapped) to width w so
// an over-wide map keeps its row structure; zoom the pane (Ctrl-b z) to see more.
// diagramWidth returns the Diagram pane's current content width in columns (0 if it
// has not been laid out yet). Safe to call from any goroutine; the engine's
// update_diagram reads it to reject an over-wide map.
func (m *model) diagramWidth() int { return int(atomic.LoadInt64(&m.diagramW)) }

func (m *model) diagramContent(w int) string {
	if w < 1 {
		w = 1
	}
	if strings.TrimSpace(m.diagram) == "" {
		// Wrap the placeholder to the pane width so it doesn't overflow and get clipped.
		return styleMuted.Width(w).Render("(no diagram yet — the orchestrator draws a network/software map here as the engagement progresses; Ctrl-b d asks it to refresh)")
	}
	var b strings.Builder
	for i, line := range strings.Split(strings.Trim(m.diagram, "\n"), "\n") {
		if i > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(ansi.Truncate(line, w, "")) // clip, don't wrap, to keep rows aligned
	}
	return b.String()
}

// renderMarkdown renders src through r, trimming the trailing newline glamour adds;
// it falls back to the raw text if the renderer is unavailable or rendering fails.
func renderMarkdown(r *glamour.TermRenderer, src string) string {
	if r == nil {
		return src
	}
	out, err := r.Render(src)
	if err != nil {
		return src
	}
	return strings.TrimRight(out, "\n")
}

// keyHelp returns the prefix-key cheat sheet body (without a header; the roster
// prints an accent-styled "Ctrl-b Commands" heading above it).
func keyHelp() string {
	return ` % / |   split across
 " / -   split down
 ←↑↓→ / hjkl  move
 z   zoom pane
 x   close pane
 o   cycle panes
 g/G top/bottom
 $   shell pane
 T   terminal pane
 L   revshell catcher
 [   copy mode
 c   new window
 n   next window
 1-9 window N
 &   close window
 p   pause agent
 P   pause all
 r   resume agent
 R   resume all
 K   kill agent / pane
 S   stop commands
 s   summary+diagram
 d   diagram
 t   toggle thoughts
 m   cycle mode (auto/planner/watcher)
 a   share pane with the AI
 b   time budget
 e   editor
 ?   show/hide pane
 q   quit`
}

// ---- helpers -------------------------------------------------------------

// fmtRemaining formats a remaining time budget as m:ss (e.g. 29:05). Minutes are
// uncapped so a budget over an hour still reads correctly. It FLOORS to whole
// seconds (not round-to-nearest): a render can land at any sub-second offset, and
// flooring makes the shown value depend only on the whole seconds left, so the
// countdown decrements by one each second instead of flickering between two values
// as renders land either side of a half-second boundary.
func fmtRemaining(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	secs := int(d.Seconds()) // truncate toward zero == floor for d >= 0
	return fmt.Sprintf("%d:%02d", secs/60, secs%60)
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	if n <= 0 {
		return ""
	}
	return string(r[:n-1]) + "…"
}

// runModeFromSel maps the wizard's automation selection (0/1/2) to a RunMode.
func runModeFromSel(sel int) core.RunMode {
	switch sel {
	case 1:
		return core.Planner
	case 2:
		return core.Watcher
	default:
		return core.Automatic
	}
}

func managedFirst(cs []sandbox.Candidate) []sandbox.Candidate {
	var managed, other []sandbox.Candidate
	for _, c := range cs {
		if c.Managed {
			managed = append(managed, c)
		} else {
			other = append(other, c)
		}
	}
	return append(managed, other...)
}

func assetsFor(mode config.Mode) fs.FS {
	if mode == config.ModeExploiter {
		return assets.ExploiterFS
	}
	return assets.PentesterFS
}
