package engine_test

import (
	"context"
	"testing"
	"time"

	"github.com/nihaal-prasad/oline/internal/engine"
	"github.com/nihaal-prasad/oline/internal/event"
	"github.com/nihaal-prasad/oline/internal/tools"
)

func (c *collector) finding() (event.Finding, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, e := range c.ev {
		if f, ok := e.(event.Finding); ok {
			return f, true
		}
	}
	return event.Finding{}, false
}

func (c *collector) summary() (event.Summary, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, e := range c.ev {
		if s, ok := e.(event.Summary); ok {
			return s, true
		}
	}
	return event.Summary{}, false
}

func (c *collector) diagram() (event.Diagram, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, e := range c.ev {
		if d, ok := e.(event.Diagram); ok {
			return d, true
		}
	}
	return event.Diagram{}, false
}

// TestRecordFindingEmitsFinding checks record_finding emits a normalized Finding.
func TestRecordFindingEmitsFinding(t *testing.T) {
	mock := &mockLLM{steps: []step{
		{toolID: "f1", toolName: "record_finding", toolArgs: `{"title":"Auth bypass on /admin","severity":"CRITICAL","target":"10.0.0.5","detail":"empty password accepted"}`},
		{text: "FINAL: done"},
	}}
	llmFor := func(string) (engine.LLM, string, error) { return mock, "m", nil }
	cfgFor := func(string) (engine.AgentConfig, bool) { return engine.AgentConfig{}, false }

	events := make(chan event.Event, 128)
	c := newCollector()
	go drain(events, c)

	s := engine.NewSession(tools.NewRegistry(), mockSB{}, llmFor, cfgFor, events, 5)
	main := engine.AgentConfig{Role: "Main", Model: "mock/main", Tools: []string{"record_finding"}, IsOrchestrator: true}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if res := s.Run(ctx, main, "go"); res.Err != nil {
		t.Fatalf("Run error: %v", res.Err)
	}
	close(events)
	<-c.done

	f, ok := c.finding()
	if !ok {
		t.Fatal("no Finding event emitted")
	}
	if f.Severity != "critical" {
		t.Errorf("severity = %q, want normalized %q", f.Severity, "critical")
	}
	if f.Title != "Auth bypass on /admin" || f.Target != "10.0.0.5" {
		t.Errorf("finding fields wrong: %+v", f)
	}
}

// TestUpdateSummaryEmitsSummary checks update_summary (orchestrator-granted) emits a
// Summary event with the text.
func TestUpdateSummaryEmitsSummary(t *testing.T) {
	mock := &mockLLM{steps: []step{
		{toolID: "s1", toolName: "update_summary", toolArgs: `{"summary":"recon done; exploiting SQLi next"}`},
		{text: "FINAL"},
	}}
	llmFor := func(string) (engine.LLM, string, error) { return mock, "m", nil }
	cfgFor := func(string) (engine.AgentConfig, bool) { return engine.AgentConfig{}, false }

	events := make(chan event.Event, 128)
	c := newCollector()
	go drain(events, c)

	s := engine.NewSession(tools.NewRegistry(), mockSB{}, llmFor, cfgFor, events, 5)
	main := engine.AgentConfig{Role: "Main", Model: "mock/main", Tools: []string{"update_summary"}, IsOrchestrator: true}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if res := s.Run(ctx, main, "go"); res.Err != nil {
		t.Fatalf("Run error: %v", res.Err)
	}
	close(events)
	<-c.done

	sum, ok := c.summary()
	if !ok {
		t.Fatal("no Summary event emitted")
	}
	if sum.Text != "recon done; exploiting SQLi next" {
		t.Errorf("summary text = %q", sum.Text)
	}
}

// TestUpdateSummaryDeniedToSubagent checks update_summary is orchestrator-only: a
// non-orchestrator agent without it in its allow-list is refused, emitting no
// Summary event.
func TestUpdateSummaryDeniedToSubagent(t *testing.T) {
	mock := &mockLLM{steps: []step{
		{toolID: "s1", toolName: "update_summary", toolArgs: `{"summary":"nope"}`},
		{text: "done"},
	}}
	llmFor := func(string) (engine.LLM, string, error) { return mock, "m", nil }
	cfgFor := func(string) (engine.AgentConfig, bool) { return engine.AgentConfig{}, false }

	events := make(chan event.Event, 128)
	c := newCollector()
	go drain(events, c)

	s := engine.NewSession(tools.NewRegistry(), mockSB{}, llmFor, cfgFor, events, 5)
	// A directly-run subagent (not an orchestrator) without update_summary granted.
	sub := engine.AgentConfig{Role: "worker", Model: "mock/worker", Tools: []string{"record_finding"}}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s.Run(ctx, sub, "go")
	close(events)
	<-c.done

	if _, ok := c.summary(); ok {
		t.Error("a non-orchestrator agent must not be able to emit a Summary via update_summary")
	}
}

// TestUpdateDiagramEmitsDiagram checks update_diagram (orchestrator-granted) emits a
// Diagram event with the ASCII map, trimming surrounding blank lines but preserving
// the interior art verbatim.
func TestUpdateDiagramEmitsDiagram(t *testing.T) {
	mock := &mockLLM{steps: []step{
		{toolID: "d1", toolName: "update_diagram", toolArgs: `{"diagram":"\n[attacker]---[web *root*]\n   |\n[db (foothold)]\n"}`},
		{text: "FINAL"},
	}}
	llmFor := func(string) (engine.LLM, string, error) { return mock, "m", nil }
	cfgFor := func(string) (engine.AgentConfig, bool) { return engine.AgentConfig{}, false }

	events := make(chan event.Event, 128)
	c := newCollector()
	go drain(events, c)

	s := engine.NewSession(tools.NewRegistry(), mockSB{}, llmFor, cfgFor, events, 5)
	main := engine.AgentConfig{Role: "Main", Model: "mock/main", Tools: []string{"update_diagram"}, IsOrchestrator: true}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if res := s.Run(ctx, main, "go"); res.Err != nil {
		t.Fatalf("Run error: %v", res.Err)
	}
	close(events)
	<-c.done

	d, ok := c.diagram()
	if !ok {
		t.Fatal("no Diagram event emitted")
	}
	want := "[attacker]---[web *root*]\n   |\n[db (foothold)]"
	if d.Text != want {
		t.Errorf("diagram text = %q, want %q", d.Text, want)
	}
}

// TestUpdateDiagramRejectsTooWide checks update_diagram refuses a map wider than the
// live pane width, does NOT change the panel, and tells the orchestrator the exact
// reason (the limit and which line overflowed).
func TestUpdateDiagramRejectsTooWide(t *testing.T) {
	wide := "this_is_one_very_long_line_well_over_twenty_columns_wide"
	mock := &mockLLM{steps: []step{
		{toolID: "d1", toolName: "update_diagram", toolArgs: `{"diagram":"` + wide + `"}`},
		{text: "FINAL"},
	}}
	llmFor := func(string) (engine.LLM, string, error) { return mock, "m", nil }
	cfgFor := func(string) (engine.AgentConfig, bool) { return engine.AgentConfig{}, false }

	events := make(chan event.Event, 128)
	c := newCollector()
	go drain(events, c)

	s := engine.NewSession(tools.NewRegistry(), mockSB{}, llmFor, cfgFor, events, 5)
	s.DiagramWidth = func() int { return 20 } // pane is 20 cols
	main := engine.AgentConfig{Role: "Main", Model: "mock/main", Tools: []string{"update_diagram"}, IsOrchestrator: true}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s.Run(ctx, main, "go")
	close(events)
	<-c.done

	if _, ok := c.diagram(); ok {
		t.Error("an over-wide diagram must NOT be applied (no Diagram event)")
	}
	if !c.sawToolOutput("REJECTED") {
		t.Error("the tool reply should say the update was rejected")
	}
	if !c.sawToolOutput("20 columns wide") {
		t.Error("the reply should state the exact current pane width")
	}
	if !c.sawToolOutput("line 1 is") {
		t.Error("the reply should name the offending line and its width")
	}
}

// TestUpdateDiagramAcceptsWithinWidth checks a map whose every line fits the pane is
// applied, and the reply reports the current width.
func TestUpdateDiagramAcceptsWithinWidth(t *testing.T) {
	mock := &mockLLM{steps: []step{
		{toolID: "d1", toolName: "update_diagram", toolArgs: `{"diagram":"[a]--[b]\n |\n[c]"}`},
		{text: "FINAL"},
	}}
	llmFor := func(string) (engine.LLM, string, error) { return mock, "m", nil }
	cfgFor := func(string) (engine.AgentConfig, bool) { return engine.AgentConfig{}, false }

	events := make(chan event.Event, 128)
	c := newCollector()
	go drain(events, c)

	s := engine.NewSession(tools.NewRegistry(), mockSB{}, llmFor, cfgFor, events, 5)
	s.DiagramWidth = func() int { return 40 }
	main := engine.AgentConfig{Role: "Main", Model: "mock/main", Tools: []string{"update_diagram"}, IsOrchestrator: true}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s.Run(ctx, main, "go")
	close(events)
	<-c.done

	d, ok := c.diagram()
	if !ok {
		t.Fatal("a within-width diagram should be applied")
	}
	if d.Text != "[a]--[b]\n |\n[c]" {
		t.Errorf("diagram text = %q", d.Text)
	}
	if !c.sawToolOutput("40 columns wide") {
		t.Error("the success reply should report the current pane width")
	}
}

// TestUpdateDiagramDeniedToSubagent checks update_diagram is orchestrator-only.
func TestUpdateDiagramDeniedToSubagent(t *testing.T) {
	mock := &mockLLM{steps: []step{
		{toolID: "d1", toolName: "update_diagram", toolArgs: `{"diagram":"nope"}`},
		{text: "done"},
	}}
	llmFor := func(string) (engine.LLM, string, error) { return mock, "m", nil }
	cfgFor := func(string) (engine.AgentConfig, bool) { return engine.AgentConfig{}, false }

	events := make(chan event.Event, 128)
	c := newCollector()
	go drain(events, c)

	s := engine.NewSession(tools.NewRegistry(), mockSB{}, llmFor, cfgFor, events, 5)
	sub := engine.AgentConfig{Role: "worker", Model: "mock/worker", Tools: []string{"record_finding"}}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s.Run(ctx, sub, "go")
	close(events)
	<-c.done

	if _, ok := c.diagram(); ok {
		t.Error("a non-orchestrator agent must not be able to emit a Diagram via update_diagram")
	}
}
