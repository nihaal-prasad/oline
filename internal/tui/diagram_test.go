package tui

import (
	"strconv"
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/nihaal-prasad/oline/internal/core"
	"github.com/nihaal-prasad/oline/internal/event"
)

// TestCostCapFiresOnce checks the spend cap trips once when the running total
// reaches it (setting costCapHit) and does not re-trip on further spend.
func TestCostCapFiresOnce(t *testing.T) {
	m := testModel(t)
	withFireworks(m.store)
	m.costCap = 0.0001 // tiny cap so a single Usage crosses it
	const id = "accounts/fireworks/models/deepseek-v4p1-flash"

	m.apply(event.Usage{Model: id, PromptTokens: 1_000_000, CompletionTokens: 1_000_000})
	if !m.costCapHit {
		t.Fatalf("cost cap should have tripped (total=%v cap=%v)", m.totalCost, m.costCap)
	}
	hit := m.costCapHit
	m.apply(event.Usage{Model: id, PromptTokens: 1_000_000, CompletionTokens: 0})
	if m.costCapHit != hit {
		t.Error("cost cap should stay tripped (fire once), not reset")
	}
}

// TestNoCostCapWhenUnset checks a zero cap never trips.
func TestNoCostCapWhenUnset(t *testing.T) {
	m := testModel(t)
	m.costCap = 0
	const id = "accounts/fireworks/models/deepseek-v4p1-flash"
	m.apply(event.Usage{Model: id, PromptTokens: 10_000_000, CompletionTokens: 10_000_000})
	if m.costCapHit {
		t.Error("cost cap must not trip when disabled (0)")
	}
}

// TestFindingSurfacedInTranscript checks a Finding event (there is no findings pane
// anymore) is surfaced in the recording agent's transcript so it is preserved.
func TestFindingSurfacedInTranscript(t *testing.T) {
	m := testModel(t)
	m.apply(event.Finding{Agent: core.AgentID("recon#1"), Severity: "high", Title: "Open Redis", Target: "10.0.0.9:6379"})
	txt := transcriptText(m.transcripts["recon#1"])
	if !strings.Contains(txt, "Open Redis") {
		t.Errorf("finding not surfaced in the agent transcript: %q", txt)
	}
	if !strings.Contains(strings.ToUpper(txt), "HIGH") {
		t.Errorf("finding severity not surfaced in the agent transcript: %q", txt)
	}
}

// flatten strips ANSI styling and collapses whitespace/newlines, so assertions on
// Markdown-rendered pane content aren't broken by styling or soft-wrapping.
func flatten(s string) string {
	return strings.Join(strings.Fields(ansi.Strip(s)), " ")
}

// TestSummaryContentIsMarkdown checks the Summary pane renders the live summary AS
// Markdown (content run through the renderer; compare after stripping styling).
func TestSummaryContentIsMarkdown(t *testing.T) {
	m := testModel(t)
	m.w, m.h = 120, 40
	m.summary = "Recon complete; two hosts up; exploiting the web app next."
	if sc := flatten(m.summaryContent(60)); !strings.Contains(sc, "exploiting the web app next") {
		t.Errorf("summary pane content missing the summary text: %q", sc)
	}
}

// TestDiagramContentVerbatim checks the Diagram pane renders the orchestrator's
// ASCII map VERBATIM (monospace, not Markdown) so column alignment is preserved, and
// clips (does not wrap) lines wider than the pane so each row stays one line.
func TestDiagramContentVerbatim(t *testing.T) {
	m := testModel(t)
	m.w, m.h = 120, 40
	m.diagram = "" +
		"[attacker] --- [10.0.0.1 fw]\n" +
		"                 |\n" +
		"   +-------------+-------------+\n" +
		"   [web *root*]        [db (foothold)]\n"

	out := ansi.Strip(m.diagramContent(80))
	// Verbatim: the exact art lines survive (not reflowed into Markdown).
	for _, want := range []string{"[attacker] --- [10.0.0.1 fw]", "[web *root*]", "[db (foothold)]"} {
		if !strings.Contains(out, want) {
			t.Errorf("diagram pane dropped/altered an art line %q:\n%s", want, out)
		}
	}
	// No line may exceed the pane width (wide rows are clipped, not wrapped).
	narrow := ansi.Strip(m.diagramContent(20))
	for _, line := range strings.Split(narrow, "\n") {
		if lipgloss.Width(line) > 20 {
			t.Errorf("diagram line exceeds width 20 (%d): %q", lipgloss.Width(line), line)
		}
	}
	// Clipping must not introduce extra rows (one source line -> one output line).
	if got, wantN := len(strings.Split(narrow, "\n")), len(strings.Split(strings.Trim(m.diagram, "\n"), "\n")); got != wantN {
		t.Errorf("clipping changed the row count: got %d lines, want %d", got, wantN)
	}
}

// TestInfoPanePlaceholdersWrap checks the empty-state placeholder text in the
// Summary and Diagram panes wraps to the pane width instead of overflowing (so it
// isn't clipped by the pane border).
func TestInfoPanePlaceholdersWrap(t *testing.T) {
	m := testModel(t)
	const w = 24 // a narrow pane: the placeholder is far longer than this
	for name, got := range map[string]string{
		"summary": m.summaryContent(w), // no summary set
		"diagram": m.diagramContent(w), // no diagram set
	} {
		if !strings.Contains(got, "\n") {
			t.Errorf("%s placeholder did not wrap at width %d: %q", name, w, got)
		}
		for _, line := range strings.Split(got, "\n") {
			if lipgloss.Width(line) > w {
				t.Errorf("%s placeholder line exceeds width %d (%d): %q", name, w, lipgloss.Width(line), line)
			}
		}
	}
}

// TestDiagramWidthPublishedOnLayout checks the Diagram pane's content width is
// published (for the engine's update_diagram check) when the pane is laid out, that
// it matches the pane's viewport width, tracks a resize, and feeds the kickoff line.
func TestDiagramWidthPublishedOnLayout(t *testing.T) {
	m := testModel(t)
	m.w, m.h = 120, 40
	mainLeaf := newLeaf(core.Main)
	center := &pane{dir: splitV, ratio: 0.5, a: newSummaryLeaf(), b: newDiagramLeaf()}
	center.a.parent, center.b.parent = center, center
	root := &pane{dir: splitH, ratio: 0.5, a: mainLeaf, b: center}
	mainLeaf.parent, center.parent = root, root
	m.paneRoot, m.focusPane = root, mainLeaf
	m.running = true
	m.resize() // lays out and refreshes the panes

	var diag *pane
	for _, l := range leaves(m.paneRoot) {
		if l.diagram {
			diag = l
		}
	}
	if diag == nil {
		t.Fatal("no diagram leaf in the layout")
	}
	if got, want := m.diagramWidth(), diag.vp.Width; got != want || got <= 0 {
		t.Fatalf("diagramWidth() = %d, want the diagram pane's vp width %d (>0)", got, want)
	}
	// The kickoff line states that exact width.
	if line := m.diagramWidthLine(); !strings.Contains(line, strconv.Itoa(m.diagramWidth())) {
		t.Errorf("diagramWidthLine should state the current width %d: %q", m.diagramWidth(), line)
	}

	// A resize to a wider terminal republishes a larger width.
	before := m.diagramWidth()
	m.w = 200
	m.resize()
	if after := m.diagramWidth(); after <= before {
		t.Errorf("diagram width should grow with the terminal: %d -> %d", before, after)
	}
}

// TestDefaultLayoutHasThreePanes checks a started engagement opens Main plus the
// Summary and Diagram panes.
func TestDefaultLayoutHasThreePanes(t *testing.T) {
	m := testModel(t)
	m.w, m.h = 120, 40
	// Simulate the pane tree beginSession builds (without a live sandbox).
	mainLeaf := newLeaf(core.Main)
	center := &pane{dir: splitV, ratio: 0.5, a: newSummaryLeaf(), b: newDiagramLeaf()}
	center.a.parent, center.b.parent = center, center
	root := &pane{dir: splitH, ratio: 0.5, a: mainLeaf, b: center}
	mainLeaf.parent, center.parent = root, root
	m.paneRoot = root

	var hasMain, hasSummary, hasDiagram bool
	for _, l := range leaves(m.paneRoot) {
		switch {
		case l.summary:
			hasSummary = true
		case l.diagram:
			hasDiagram = true
		case l.agent == core.Main:
			hasMain = true
		}
	}
	if !hasMain || !hasSummary || !hasDiagram {
		t.Errorf("default layout panes: main=%v summary=%v diagram=%v", hasMain, hasSummary, hasDiagram)
	}
}
