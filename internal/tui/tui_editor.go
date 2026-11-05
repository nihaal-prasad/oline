package tui

import (
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"

	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/nihaal-prasad/oline/internal/config"
	"github.com/nihaal-prasad/oline/internal/core"
)

// eItem is one editable entity in the model/provider editor.
type eItem struct {
	kind string // "scope" | "orchestrator" | "role" | "provider" | "agent"
	name string
}

// openEditor builds the editor item list and enters editing mode.
func (m *model) openEditor() {
	m.eItems = m.eItems[:0]
	// Scope can be changed live while agents run (re-applies the firewall and tells
	// the orchestrator). With `oline -edit` there is no engagement, so it is omitted.
	if !m.editorOnly {
		scopeName := "in-scope targets"
		if m.mode == config.ModeExploiter {
			scopeName = "target application"
		}
		m.eItems = append(m.eItems, eItem{kind: "scope", name: scopeName})
	}
	// TUI preferences (terminal-UI, not engagement behavior) — one section.
	m.eItems = append(m.eItems, eItem{kind: "tui", name: "preferences"})
	// Main: the orchestrator (primary agent). One section holding its model tier,
	// thinking level, max context, its system prompt, and the mode personality.
	m.eItems = append(m.eItems, eItem{kind: "main", name: "Primary Agent Modifications"})
	// The three model tiers: every agent runs on one of these, and compaction uses
	// small. Configuring them here is the same as the first-run setup.
	m.eItems = append(m.eItems,
		eItem{kind: "role", name: "big"},
		eItem{kind: "role", name: "medium"},
		eItem{kind: "role", name: "small"})
	provs := make([]string, 0)
	for name := range m.store.Providers() {
		provs = append(provs, name)
	}
	sort.Strings(provs)
	for _, p := range provs {
		m.eItems = append(m.eItems, eItem{kind: "provider", name: p})
	}
	for _, name := range m.roster.Names() {
		m.eItems = append(m.eItems, eItem{kind: "agent", name: name})
	}
	m.editing = true
	m.eFocus = -1
	m.eSel = 0
	m.eStatus = ""
}

// defaultPromptH is the initial height (rows) of the resizable system-prompt box.
const defaultPromptH = 12

// buildFields creates the inputs for the selected item: zero or more single-line
// text fields plus zero or more resizable multi-line prompt boxes (m.ePrompts).
func (m *model) buildFields() {
	m.eFields = nil
	m.ePrompts = nil
	m.ePromptLabels = nil
	if m.eSel < 0 || m.eSel >= len(m.eItems) {
		return
	}
	it := m.eItems[m.eSel]
	mk := func(placeholder, value string, mask bool) textinput.Model {
		ti := textinput.New()
		ti.Placeholder = placeholder
		ti.SetValue(value)
		ti.Width = 56
		if mask {
			ti.EchoMode = textinput.EchoPassword
		}
		return ti
	}
	addPrompt := func(label, content string) {
		m.ePrompts = append(m.ePrompts, m.newPromptArea(content))
		m.ePromptLabels = append(m.ePromptLabels, label)
	}
	boolVal := func(b bool) string {
		if b {
			return "true"
		}
		return "false"
	}
	switch it.kind {
	case "scope":
		label := "in-scope targets (IPs / CIDRs / domains)"
		if m.mode == config.ModeExploiter {
			label = "target application (local path, or URL/name to fetch)"
		}
		m.eFields = []textinput.Model{
			mk(label, m.scopeIn.Value(), false),
		}
	case "tui":
		eff := m.store.Effective().TUI
		m.eFields = []textinput.Model{
			mk("customizable panes (true|false)", boolVal(eff.CustomizablePanes), false),
			mk("close idle subagent panes (true|false)", boolVal(eff.CloseIdleSubagentPanes), false),
		}
	case "role":
		// A tier's model, plus that model's per-1M-token prices (which feed the live
		// cost readout and spend caps). The prices belong to the model the selector
		// names, so they are seeded from it.
		cur := m.roleModel(it.name)
		prov, id, _ := strings.Cut(cur, "/")
		c := m.modelCost(prov, id)
		m.eFields = []textinput.Model{
			mk("model (provider/model-id)", cur, false),
			mk("USD per 1M input tokens", costStr(c.Input), false),
			mk("USD per 1M output tokens", costStr(c.Output), false),
			mk("USD per 1M cached-read tokens", costStr(c.CacheRead), false),
			mk("USD per 1M cache-write tokens", costStr(c.CacheWrite), false),
		}
	case "provider":
		p := m.store.Providers()[it.name]
		m.eFields = []textinput.Model{
			mk("baseURL", p.BaseURL, false),
			mk("API key (leave blank to keep current)", "", true),
		}
	case "agent":
		cfg, _ := m.resolveAgent(it.name)
		ctxVal := ""
		if cfg.maxContext > 0 {
			ctxVal = strconv.Itoa(cfg.maxContext)
		}
		m.eFields = []textinput.Model{
			mk("model tier (Big|Medium|Small)", capitalFirst(cfg.tier), false),
			mk("thinking-level (Low|Medium|High)", capitalFirst(cfg.effort), false),
			mk("max context tokens (blank = auto)", ctxVal, false),
		}
		addPrompt("system prompt", m.roster.Agents[it.name].Prompt)
	case "main":
		// The orchestrator (primary agent), edited like a subagent: tier / thinking /
		// max-context fields + the system prompt AND the mode personality.
		oc := m.store.Orchestrator()
		tier := oc.Tier
		if tier == "" {
			tier = "medium"
		}
		ctxVal := ""
		if oc.MaxContextTokens > 0 {
			ctxVal = strconv.Itoa(oc.MaxContextTokens)
		}
		m.eFields = []textinput.Model{
			mk("model tier (Big|Medium|Small)", capitalFirst(tier), false),
			mk("thinking-level (Low|Medium|High)", capitalFirst(oc.ThinkingLevel), false),
			mk("max context tokens (blank = auto)", ctxVal, false),
		}
		addPrompt("system prompt", m.roster.Orchestrator.SystemTemplate)
		addPrompt("personality", m.roster.Orchestrator.Personality)
	}
	if len(m.eFields) > 0 {
		m.eFields[0].Focus()
	}
}

// newPromptArea builds the multi-line system-prompt editor seeded with content.
func (m *model) newPromptArea(content string) *textarea.Model {
	ta := textarea.New()
	ta.CharLimit = 0 // system prompts are large; never truncate
	ta.ShowLineNumbers = false
	ta.Prompt = "" // no per-line gutter so the full width is usable
	ta.SetValue(content)
	if m.ePromptH <= 0 {
		m.ePromptH = defaultPromptH
	}
	ta.SetHeight(m.ePromptH)
	return &ta
}

// modelCost returns the current cost (USD per 1M tokens) of a provider's model,
// or a zero Cost if the model isn't found.
func (m *model) modelCost(prov, id string) config.Cost {
	for _, md := range m.store.Providers()[prov].Models {
		if md.ID == id {
			return md.Cost
		}
	}
	return config.Cost{}
}

// roleModel returns the "provider/model-id" selector currently assigned to a tier.
func (m *model) roleModel(tier string) string {
	r := m.store.ModelRoles()
	switch tier {
	case "big":
		return r.Big
	case "small":
		return r.Small
	default:
		return r.Medium
	}
}

// costStr formats a per-1M-token price for a text field: the shortest exact
// decimal (e.g. 0.3, 0.006), and empty for an unset (zero) price.
func costStr(v float64) string {
	if v == 0 {
		return ""
	}
	return strconv.FormatFloat(v, 'f', -1, 64)
}

// costFromFields parses the four price inputs (input, output, cache-read,
// cache-write, in that order) into a Cost. A blank field is 0; a negative or
// non-numeric value is an error.
func (m *model) costFromFields(fields []textinput.Model) (config.Cost, error) {
	var c config.Cost
	dst := []*float64{&c.Input, &c.Output, &c.CacheRead, &c.CacheWrite}
	for i := range dst {
		if i >= len(fields) {
			break
		}
		raw := strings.TrimSpace(fields[i].Value())
		if raw == "" {
			continue // blank = 0 (e.g. no separate cache rate)
		}
		v, err := strconv.ParseFloat(raw, 64)
		if err != nil || v < 0 {
			return config.Cost{}, fmt.Errorf("prices must be non-negative numbers (USD per 1M tokens)")
		}
		*dst[i] = v
	}
	return c, nil
}

// promptAt returns the index into m.ePrompts for focus index i (a prompt box sits
// after the text fields), or -1 when i is not a prompt box.
func (m *model) promptAt(i int) int {
	j := i - len(m.eFields)
	if j >= 0 && j < len(m.ePrompts) {
		return j
	}
	return -1
}

// focusedPrompt returns the prompt box currently focused, or nil.
func (m *model) focusedPrompt() *textarea.Model {
	if j := m.promptAt(m.eFocus); j >= 0 {
		return m.ePrompts[j]
	}
	return nil
}

// numFields is the number of focusable inputs: the text fields plus the prompt boxes.
func (m *model) numFields() int { return len(m.eFields) + len(m.ePrompts) }

// setEFocus moves focus to focusable i (a text field or a prompt box), blurring the rest.
func (m *model) setEFocus(i int) {
	for j := range m.eFields {
		m.eFields[j].Blur()
	}
	for j := range m.ePrompts {
		m.ePrompts[j].Blur()
	}
	m.eFocus = i
	switch {
	case m.promptAt(i) >= 0:
		m.ePrompts[m.promptAt(i)].Focus()
	case i >= 0 && i < len(m.eFields):
		m.eFields[i].Focus()
	}
}

// resizePrompt grows (d>0) or shrinks (d<0) the prompt boxes; the chosen height
// persists across items. viewEditor clamps the rendered height to the box.
func (m *model) resizePrompt(d int) {
	if len(m.ePrompts) == 0 {
		return
	}
	m.ePromptH = clampInt(m.ePromptH+d, 3, 60)
	for _, ta := range m.ePrompts {
		ta.SetHeight(m.ePromptH)
	}
}

// capitalFirst upper-cases the first letter of a word (for display: Big, Medium,
// High, …). Empty stays empty.
func capitalFirst(s string) string {
	if s == "" {
		return ""
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// resolveAgent returns the current model, effort, and max-context-tokens override
// for an agent (via the roster). maxContext is 0 when it uses the derived default.
func (m *model) resolveAgent(name string) (struct {
	tier, effort string
	maxContext   int
}, bool) {
	var out struct {
		tier, effort string
		maxContext   int
	}
	d, ok := m.roster.Agents[name]
	if !ok {
		return out, false
	}
	out.tier = d.Tier
	if out.tier == "" {
		out.tier = "medium"
	}
	out.effort = d.ThinkingLevel
	out.maxContext = d.MaxContextTokens
	return out, true
}

func (m *model) updateEditor(msg tea.Msg) (tea.Model, tea.Cmd) {
	if mm, ok := msg.(tea.MouseMsg); ok {
		return m, m.handleEditorMouse(mm)
	}
	if done, ok := msg.(editorDoneMsg); ok {
		m.applyExternalEdit(done)
		return m, nil
	}
	key, ok := msg.(tea.KeyMsg)
	if !ok {
		return m, nil
	}
	switch key.String() {
	case "ctrl+c":
		m.shutdown()
		return m, tea.Quit
	case "esc":
		if m.eFocus >= 0 {
			m.blurFields()
			m.eFocus = -1
			return m, nil
		}
		m.editing = false
		if m.editorOnly {
			return m, tea.Quit
		}
		return m, nil
	}

	if m.eFocus == -1 {
		// navigating the item list
		switch key.String() {
		case "up", "k":
			if m.eSel > 0 {
				m.eSel--
			}
		case "down", "j":
			if m.eSel < len(m.eItems)-1 {
				m.eSel++
			}
		case "enter":
			if len(m.eItems) == 0 {
				return m, nil
			}
			m.buildFields()
			if m.numFields() == 0 {
				return m, nil
			}
			m.setEFocus(0)
		}
		return m, nil
	}

	// editing fields
	if m.eFocus < 0 || m.eFocus >= m.numFields() {
		m.eFocus = -1
		return m, nil
	}
	// Universal keys (work on any field): Ctrl+S saves; Tab cycles fields.
	switch key.String() {
	case "ctrl+s":
		m.saveItem()
		return m, nil
	case "tab":
		m.cycleField(1)
		return m, nil
	case "shift+tab":
		m.cycleField(-1)
		return m, nil
	}
	if ta := m.focusedPrompt(); ta != nil {
		// The multi-line prompt box: arrows/Enter edit text; alt+↑/↓ resize it;
		// Ctrl+E opens the body in the user's $EDITOR (vim) for full modal editing.
		switch key.String() {
		case "alt+up", "ctrl+up":
			m.resizePrompt(1)
			return m, nil
		case "alt+down", "ctrl+down":
			m.resizePrompt(-1)
			return m, nil
		case "ctrl+e":
			return m, m.editPromptInEditor()
		}
		var cmd tea.Cmd
		*ta, cmd = ta.Update(msg)
		return m, cmd
	}
	// A single-line text field: ↑/↓ switch fields, Enter saves.
	switch key.String() {
	case "up":
		m.cycleField(-1)
		return m, nil
	case "down":
		m.cycleField(1)
		return m, nil
	case "enter":
		m.saveItem()
		return m, nil
	}
	var cmd tea.Cmd
	m.eFields[m.eFocus], cmd = m.eFields[m.eFocus].Update(msg)
	return m, cmd
}

// handleEditorMouse maps a click in the settings editor to the same actions the
// keyboard offers: click a left-list item to open it, click a right-pane field to
// focus it, and wheel over the prompt box to scroll it. Coordinates are matched
// against the anchors viewEditor recorded for the last render.
func (m *model) handleEditorMouse(msg tea.MouseMsg) tea.Cmd {
	// Wheel: scroll the focused prompt box when the pointer is over the right pane.
	if msg.Button == tea.MouseButtonWheelUp || msg.Button == tea.MouseButtonWheelDown {
		if ta := m.focusedPrompt(); ta != nil && msg.X >= m.eRightLeft {
			var cmd tea.Cmd
			*ta, cmd = ta.Update(msg)
			return cmd
		}
		return nil
	}
	if msg.Button != tea.MouseButtonLeft || msg.Action != tea.MouseActionPress {
		return nil
	}
	// Left item list: select (and open) the clicked item.
	if msg.X < m.eLeftWidth {
		if idx := msg.Y - m.eLeftItemTop; idx >= 0 && idx < len(m.eItems) {
			m.eSel = idx
			m.eFocus = -1
			m.eStatus = ""
			m.buildFields()
			if m.numFields() > 0 {
				m.setEFocus(0)
			}
		}
		return nil
	}
	// Right pane: focus the clicked field/prompt (only once an item is open).
	if msg.X >= m.eRightLeft && m.eFocus >= 0 {
		for j := 0; j < len(m.ePrompts) && j < len(m.ePromptTops); j++ {
			if msg.Y >= m.ePromptTops[j] && msg.Y <= m.ePromptBots[j] {
				m.setEFocus(len(m.eFields) + j)
				return nil
			}
		}
		for i := 0; i < len(m.eFields) && i < len(m.eFieldRows); i++ {
			if r := m.eFieldRows[i]; msg.Y == r || msg.Y == r-1 { // the input row or its label
				m.setEFocus(i)
				return nil
			}
		}
	}
	return nil
}

func (m *model) cycleField(d int) {
	n := m.numFields()
	if n == 0 {
		return
	}
	m.setEFocus(((m.eFocus+d)%n + n) % n)
}

func (m *model) blurFields() {
	for i := range m.eFields {
		m.eFields[i].Blur()
	}
	for i := range m.ePrompts {
		m.ePrompts[i].Blur()
	}
}

func (m *model) saveItem() {
	if m.eSel < 0 || m.eSel >= len(m.eItems) {
		return
	}
	it := m.eItems[m.eSel]
	switch it.kind {
	case "main":
		tier := strings.ToLower(strings.TrimSpace(m.eFields[0].Value()))
		if tier != "" && tier != "big" && tier != "medium" && tier != "small" {
			m.eStatus = styleErr.Render("model tier must be Big, Medium, or Small")
			return
		}
		eff := strings.ToLower(strings.TrimSpace(m.eFields[1].Value()))
		if eff != "" && eff != "low" && eff != "medium" && eff != "high" {
			m.eStatus = styleErr.Render("thinking-level must be Low, Medium, or High")
			return
		}
		oc := config.OrchestratorConfig{Tier: tier, ThinkingLevel: eff}
		if raw := strings.TrimSpace(m.eFields[2].Value()); raw != "" {
			n, err := strconv.Atoi(raw)
			if err != nil || n <= 0 {
				m.eStatus = styleErr.Render("max context tokens must be a positive whole number (or blank for auto)")
				return
			}
			oc.MaxContextTokens = n
		}
		if err := m.store.SaveOrchestrator(oc); err != nil {
			m.eStatus = styleErr.Render("save: " + err.Error())
			return
		}
		// Persist the system prompt (prompt box 0) and the personality (box 1).
		sysPrompt, personality := m.ePrompts[0].Value(), m.ePrompts[1].Value()
		if err := m.store.SaveModeFile("SYSTEM_TEMPLATE.md", sysPrompt); err != nil {
			m.eStatus = styleErr.Render("save system prompt: " + err.Error())
			return
		}
		if err := m.store.SaveModeFile("PERSONALITY.md", personality); err != nil {
			m.eStatus = styleErr.Render("save personality: " + err.Error())
			return
		}
		m.roster.Orchestrator.SystemTemplate = sysPrompt
		m.roster.Orchestrator.Personality = personality
		m.eStatus = styleGreenRender("saved Main (model config applies to the next engagement)")
	case "tui":
		parseBool := func(s string) (bool, bool) {
			switch strings.ToLower(strings.TrimSpace(s)) {
			case "true", "1", "yes", "on":
				return true, true
			case "false", "0", "no", "off":
				return false, true
			}
			return false, false
		}
		cp, ok1 := parseBool(m.eFields[0].Value())
		cip, ok2 := parseBool(m.eFields[1].Value())
		if !ok1 || !ok2 {
			m.eStatus = styleErr.Render("both settings must be true or false")
			return
		}
		if err := m.store.SaveTUICustomizablePanes(cp); err != nil {
			m.eStatus = styleErr.Render("save: " + err.Error())
			return
		}
		if err := m.store.SaveTUICloseIdleSubagentPanes(cip); err != nil {
			m.eStatus = styleErr.Render("save: " + err.Error())
			return
		}
		m.eStatus = styleGreenRender("saved TUI preferences (applies to the next engagement)")
	case "scope":
		raw := strings.TrimSpace(m.eFields[0].Value())
		m.applyScopeLive(raw)
		if m.sess != nil {
			m.eStatus = styleGreenRender("scope updated — re-applying firewall & notifying the orchestrator")
		} else {
			m.eStatus = styleGreenRender("scope set (applies when the engagement starts)")
		}
	case "role":
		sel := strings.TrimSpace(m.eFields[0].Value())
		prevSel := strings.TrimSpace(m.roleModel(it.name))
		prov, id, hasSlash := strings.Cut(sel, "/")
		// If the selector names a model the provider's catalog doesn't list yet, add
		// it, so a tier can point at a new model without being rejected at start as
		// "not in the model list". (A truly unknown provider can't be auto-added; the
		// tier still saves and start-time validation reports the bad provider.)
		added := false
		if hasSlash {
			if a, err := m.store.EnsureModel(prov, id); err == nil {
				added = a
			}
		}
		note := ""
		if added {
			note = " · added it to " + prov + " (set its prices below)"
		}
		if err := m.store.SaveModelRole(it.name, sel); err != nil {
			m.eStatus = styleErr.Render("save model: " + err.Error())
			return
		}
		// If the model itself changed, the price fields still show the PREVIOUS
		// model's prices — re-seed them for the newly chosen model rather than
		// writing stale prices onto it. The user edits and saves again to change them.
		if sel != prevSel {
			m.buildFields()
			m.setEFocus(0)
			m.eStatus = styleGreenRender("saved " + it.name + " model → " + sel + note + " · prices shown below — edit and save again to change them")
			return
		}
		// Model unchanged: also persist any edited prices for that model.
		if hasSlash {
			c, perr := m.costFromFields(m.eFields[1:])
			if perr != nil {
				m.eStatus = styleErr.Render(perr.Error())
				return
			}
			if err := m.store.SaveModelCost(prov, id, c); err != nil {
				// The tier saved fine; report only that the price part didn't.
				m.eStatus = styleGreenRender("saved " + it.name + " model → " + sel + " (prices not saved: " + err.Error() + ")")
				return
			}
		}
		m.eStatus = styleGreenRender("saved " + it.name + " model → " + sel + note + " and its prices (applies to newly spawned agents)")
	case "provider":
		base := strings.TrimSpace(m.eFields[0].Value())
		if err := m.store.SaveProvider(it.name, base); err != nil {
			m.eStatus = styleErr.Render("save baseURL: " + err.Error())
			return
		}
		if key := strings.TrimSpace(m.eFields[1].Value()); key != "" {
			if err := m.store.SaveSecret(it.name, key); err != nil {
				m.eStatus = styleErr.Render("save key: " + err.Error())
				return
			}
			m.eFields[1].SetValue("")
		}
		m.eStatus = styleGreenRender("saved provider " + it.name + " → ~/oline/config")
	case "agent":
		tier := strings.ToLower(strings.TrimSpace(m.eFields[0].Value()))
		if tier != "" && tier != "big" && tier != "medium" && tier != "small" {
			m.eStatus = styleErr.Render("model tier must be Big, Medium, or Small")
			return
		}
		eff := strings.ToLower(strings.TrimSpace(m.eFields[1].Value()))
		if eff != "" && eff != "low" && eff != "medium" && eff != "high" {
			m.eStatus = styleErr.Render("thinking-level must be Low, Medium, or High")
			return
		}
		ov := config.AgentOverride{
			Tier:          tier,
			ThinkingLevel: eff,
		}
		ctxTokens := 0
		if raw := strings.TrimSpace(m.eFields[2].Value()); raw != "" {
			n, err := strconv.Atoi(raw)
			if err != nil || n <= 0 {
				m.eStatus = styleErr.Render("max context tokens must be a positive whole number (or blank for auto)")
				return
			}
			ctxTokens = n
			ov.MaxContextTokens = &n
		}
		if err := m.store.SaveAgent(it.name, ov); err != nil {
			m.eStatus = styleErr.Render("save: " + err.Error())
			return
		}
		// reflect immediately in the in-memory roster
		if d, ok := m.roster.Agents[it.name]; ok {
			d.Tier = ov.Tier
			d.ThinkingLevel = ov.ThinkingLevel
			d.MaxContextTokens = ctxTokens
			m.roster.Agents[it.name] = d
		}
		// Persist the (possibly edited) system prompt, preserving the frontmatter
		// so tier/tools/etc. in the file are untouched.
		if len(m.ePrompts) > 0 {
			d := m.roster.Agents[it.name]
			body := m.ePrompts[0].Value()
			full := "---\n" + d.Frontmatter + "---\n\n" + body
			if !strings.HasSuffix(full, "\n") {
				full += "\n"
			}
			if err := m.store.SaveModeFile("agents/"+it.name+".md", full); err != nil {
				m.eStatus = styleErr.Render("save prompt: " + err.Error())
				return
			}
			d.Prompt = body
			m.roster.Agents[it.name] = d
		}
		m.eStatus = styleGreenRender("saved agent " + it.name + " (applies to newly spawned agents)")
	}
}

func styleGreenRender(s string) string { return lipgloss.NewStyle().Foreground(colGreen).Render(s) }

func (m *model) viewEditor() string {
	var left strings.Builder
	left.WriteString(styleAccent.Render("Model & provider editor") + "\n\n")
	for i, it := range m.eItems {
		cur := "  "
		if i == m.eSel {
			cur = styleAccent.Render("▸ ")
		}
		left.WriteString(fmt.Sprintf("%s%-10s %s\n", cur, displayKind(it.kind), displayName(it)))
	}

	// Size the left box to its widest line (plus the border and a column of slack)
	// so entries like "scope  in-scope targets" never wrap; give the rest to the
	// right box. On a narrow terminal, cap the left box so the right box (which holds
	// a 56-wide input) keeps room — there the left lines may wrap, as before. Sizing
	// happens before the right box is built so the prompt box can be fitted to it.
	leftTotal := lipgloss.Width(left.String()) + 3 // +2 rounded border, +1 slack
	if m.w > 0 && leftTotal > m.w-22 {
		leftTotal = maxInt(m.w-22, 12)
	}
	rightTotal := maxInt(m.w-leftTotal-2, 24)
	boxH := 18
	if m.h > 0 {
		boxH = clampInt(m.h-4, 6, 40)
	}

	// Record mouse hit-testing anchors (see the model fields). Content starts on the
	// box's first inner row: terminal row 3 (header row 0, blank row 1, border row 2).
	// The left list prints a title + blank line first, so item 0 is on row 5.
	const contentTop = 3
	m.eLeftItemTop = contentTop + 2
	m.eLeftWidth = leftTotal
	m.eRightLeft = leftTotal + 2 // leftBox (width leftTotal) + the "  " separator
	m.eFieldRows = m.eFieldRows[:0]
	m.ePromptTops = m.ePromptTops[:0]
	m.ePromptBots = m.ePromptBots[:0]

	var right strings.Builder
	if m.eSel >= 0 && m.eSel < len(m.eItems) && m.eFocus >= 0 {
		it := m.eItems[m.eSel]
		// Per-prompt height: split the room left after the fixed lines between the
		// prompt boxes, honouring the user's chosen size but never overflowing.
		ph := 0
		if np := len(m.ePrompts); np > 0 {
			statusLines := 0
			if m.eStatus != "" {
				statusLines = 2
			}
			fixed := 2 + 3*len(m.eFields) + 2*np + 1 + statusLines // header+blank, fields, per-prompt label+gap, hint, status
			ph = clampInt((boxH-2-fixed)/np, 3, m.ePromptH)
		}
		row := contentTop // absolute terminal row of the next line written to the right pane
		right.WriteString(styleMuted.Render("editing "+displayKind(it.kind)+" "+it.name) + "\n\n")
		row += 2
		labels := m.fieldLabels(it)
		for i, f := range m.eFields {
			marker := "  "
			if i == m.eFocus {
				marker = styleAccent.Render("▸ ")
			}
			lbl := ""
			if i < len(labels) {
				lbl = labels[i]
			}
			m.eFieldRows = append(m.eFieldRows, row+1) // the input line sits below its label
			right.WriteString(marker + lbl + "\n  " + f.View() + "\n\n")
			row += 3
		}
		for j, ta := range m.ePrompts {
			ta.SetWidth(maxInt(rightTotal-4, 16))
			ta.SetHeight(ph)
			marker := "  "
			if m.eFocus == len(m.eFields)+j {
				marker = styleAccent.Render("▸ ")
			}
			lbl := "prompt"
			if j < len(m.ePromptLabels) {
				lbl = m.ePromptLabels[j]
			}
			m.ePromptTops = append(m.ePromptTops, row)
			m.ePromptBots = append(m.ePromptBots, row+ph)
			right.WriteString(marker + lbl + "\n" + ta.View() + "\n\n")
			row += 1 + ph + 1
		}
		right.WriteString(styleMuted.Render(m.editHint()))
	} else {
		right.WriteString(styleMuted.Render("↑/↓ choose an item · Enter to edit · Esc to " + exitLabel(m.editorOnly)))
	}
	if m.eStatus != "" {
		right.WriteString("\n\n" + m.eStatus)
	}

	leftBox := paneBorder.Width(leftTotal - 2).Height(boxH - 2).MaxHeight(boxH).Render(left.String())
	rightBox := paneBorderFocused.Width(rightTotal - 2).Height(boxH - 2).MaxHeight(boxH).Render(right.String())
	body := lipgloss.JoinHorizontal(lipgloss.Top, leftBox, "  ", rightBox)
	header := titleBar.Render(" oline · settings ")
	return lipgloss.JoinVertical(lipgloss.Left, header, "", body)
}

func (m *model) fieldLabels(it eItem) []string {
	switch it.kind {
	case "provider":
		return []string{"baseURL", "API key"}
	case "scope":
		return []string{"in-scope targets"}
	case "tui":
		return []string{"customizable panes (true|false)", "close idle subagent panes (true|false)"}
	case "role":
		return []string{"model (provider/model-id)", "input $/1M", "output $/1M", "cache-read $/1M", "cache-write $/1M"}
	}
	// "agent" and "main": tier / thinking / max-context.
	return []string{"model tier (Big|Medium|Small)", "thinking-level", "max context tokens"}
}

// displayKind is the capitalized section label shown in the left list.
func displayKind(kind string) string {
	switch kind {
	case "tui":
		return "TUI"
	case "main":
		return "Main"
	default:
		return capitalFirst(kind)
	}
}

// displayName is the capitalized right-hand detail shown for an item in the left list.
func displayName(it eItem) string {
	switch it.kind {
	case "agent":
		return core.CanonRole(it.name) // subagent names capitalized (config key stays lower-case)
	case "role":
		return capitalFirst(it.name) // Big / Medium / Small
	default:
		return it.name
	}
}

// editHint is the footer cheat-line for the field editor; it adapts to whether a
// multi-line prompt box is focused (there Enter inserts a newline, so saving is
// Ctrl+S, the box resizes with alt+↑/↓, and Ctrl+E opens the body in $EDITOR/vim).
func (m *model) editHint() string {
	if len(m.ePrompts) == 0 {
		return "Tab/↑↓ switch field · Enter save · Esc back"
	}
	if m.focusedPrompt() != nil {
		return "editing prompt · Enter = newline · Ctrl+E vim · Ctrl+S save · alt+↑/↓ resize · Tab leave · Esc back"
	}
	return "Tab switch field · Enter or Ctrl+S save · Esc back"
}

func exitLabel(editorOnly bool) string {
	if editorOnly {
		return "quit"
	}
	return "return"
}

// editorDoneMsg is delivered after the external editor (vim) process exits; it
// carries the temp file to read back and which prompt box to load it into.
type editorDoneMsg struct {
	path string
	j    int // index into m.ePrompts to reload
	err  error
}

// editPromptInEditor writes the focused prompt box to a temp file and opens it in
// the user's editor ($VISUAL, $EDITOR, else vim) for full modal editing, suspending
// the TUI until it exits. The edited file is read back into the box (not yet saved
// to disk — the user still presses Ctrl+S). This is the "vim" path for large bodies.
func (m *model) editPromptInEditor() tea.Cmd {
	ta := m.focusedPrompt()
	if ta == nil {
		return nil
	}
	j := m.promptAt(m.eFocus)
	f, err := os.CreateTemp("", "oline-edit-*.md")
	if err != nil {
		m.eStatus = styleErr.Render("open editor: " + err.Error())
		return nil
	}
	path := f.Name()
	_, werr := f.WriteString(ta.Value())
	if cerr := f.Close(); werr != nil || cerr != nil {
		os.Remove(path)
		m.eStatus = styleErr.Render("open editor: could not write the temp file")
		return nil
	}
	name, args := editorCommand()
	cmd := exec.Command(name, append(args, path)...) // #nosec G204 — operator's own editor
	return tea.ExecProcess(cmd, func(err error) tea.Msg {
		return editorDoneMsg{path: path, j: j, err: err}
	})
}

// applyExternalEdit folds the result of an external editor session back into the
// prompt box (and always removes the temp file).
func (m *model) applyExternalEdit(done editorDoneMsg) {
	defer os.Remove(done.path)
	if done.err != nil {
		m.eStatus = styleErr.Render("editor exited with an error: " + done.err.Error())
		return
	}
	data, err := os.ReadFile(done.path)
	if err != nil {
		m.eStatus = styleErr.Render("could not read back the edited file: " + err.Error())
		return
	}
	if done.j < 0 || done.j >= len(m.ePrompts) {
		return // the selection changed while the editor was open
	}
	m.ePrompts[done.j].SetValue(string(data))
	m.eStatus = styleGreenRender("loaded edits from your editor — press Ctrl+S to save")
}

// editorCommand resolves the external editor: $VISUAL, then $EDITOR, else vim.
func editorCommand() (string, []string) {
	ed := strings.TrimSpace(os.Getenv("VISUAL"))
	if ed == "" {
		ed = strings.TrimSpace(os.Getenv("EDITOR"))
	}
	if ed == "" {
		ed = "vim"
	}
	parts := strings.Fields(ed)
	return parts[0], parts[1:]
}
