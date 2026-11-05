package engine_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/nihaal-prasad/oline/internal/decide"
	"github.com/nihaal-prasad/oline/internal/engine"
	"github.com/nihaal-prasad/oline/internal/event"
	"github.com/nihaal-prasad/oline/internal/tools"
)

// fakeDecider is a canned engine.Decider for the gate tests. It dispatches by the
// question instructions so one fake serves severity/dup/routing/judge/real questions.
type fakeDecider struct {
	severity  string  // answer to the severity choice
	dupValue  string  // answer to the duplicate choice ("none" or an index)
	dupConf   float64 // confidence for the duplicate choice
	isReal    float64 // predicate probability the finding is real
	judgeIdx  int     // score index for the judge gate
	routeRole string  // answer to the routing choice
	err       error   // when set, every call fails (fail-open tests)
}

func (f fakeDecider) Predicate(_ context.Context, _, _ string) (float64, error) {
	if f.err != nil {
		return 0, f.err
	}
	return f.isReal, nil
}

func (f fakeDecider) Choice(_ context.Context, _, instr string, _ []decide.Choice) (string, float64, error) {
	if f.err != nil {
		return "", 0, f.err
	}
	switch {
	case strings.Contains(instr, "severity"):
		return f.severity, 1, nil
	case strings.Contains(instr, "specialist"):
		return f.routeRole, 1, nil
	default: // duplicate
		return f.dupValue, f.dupConf, nil
	}
}

func (f fakeDecider) Score(_ context.Context, _, _ string, levels []string) (int, float64, error) {
	if f.err != nil {
		return 0, 0, f.err
	}
	grade := 0.0
	if len(levels) > 1 {
		grade = float64(f.judgeIdx) / float64(len(levels)-1)
	}
	return f.judgeIdx, grade, nil
}

// recordFindingSession builds a session whose Main records one finding then stops.
func recordFindingSession(t *testing.T, dec engine.Decider, args string) (*collector, engine.Result) {
	t.Helper()
	mainMock := &mockLLM{steps: []step{
		{toolID: "f1", toolName: "record_finding", toolArgs: args},
		{text: "done"},
	}}
	llmFor := func(string) (engine.LLM, string, error) { return mainMock, "m", nil }
	cfgFor := func(string) (engine.AgentConfig, bool) { return engine.AgentConfig{}, false }
	events := make(chan event.Event, 256)
	c := newCollector()
	go drain(events, c)
	s := engine.NewSession(tools.NewRegistry(), mockSB{}, llmFor, cfgFor, events, 5)
	s.Decider = dec
	mainCfg := engine.AgentConfig{Role: "Main", Model: "mock/main", Tools: []string{"record_finding"}, IsOrchestrator: true}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	res := s.Run(ctx, mainCfg, "go")
	close(events)
	<-c.done
	return c, res
}

func TestFindingGateSeverityOverride(t *testing.T) {
	c, _ := recordFindingSession(t,
		fakeDecider{severity: "critical", dupValue: "none", isReal: 0.9},
		`{"title":"Auth bypass","severity":"low","target":"t","detail":"d"}`)
	f, ok := c.finding()
	if !ok {
		t.Fatal("finding should be recorded")
	}
	if f.Severity != "critical" {
		t.Errorf("severity = %q, want the decision model's %q (overriding the agent's 'low')", f.Severity, "critical")
	}
}

func TestFindingGateDropsNoise(t *testing.T) {
	c, _ := recordFindingSession(t,
		fakeDecider{severity: "info", dupValue: "none", isReal: 0.05}, // below realThreshold
		`{"title":"maybe something","severity":"high","target":"t","detail":"a hunch"}`)
	if _, ok := c.finding(); ok {
		t.Error("a finding scored as noise must be dropped (not recorded)")
	}
}

func TestFindingGateMergesDuplicate(t *testing.T) {
	mainMock := &mockLLM{steps: []step{
		{toolID: "f1", toolName: "record_finding", toolArgs: `{"title":"SQLi on /login","severity":"high","target":"t","detail":"second observation"}`},
		{text: "done"},
	}}
	llmFor := func(string) (engine.LLM, string, error) { return mainMock, "m", nil }
	cfgFor := func(string) (engine.AgentConfig, bool) { return engine.AgentConfig{}, false }
	events := make(chan event.Event, 256)
	c := newCollector()
	go drain(events, c)
	s := engine.NewSession(tools.NewRegistry(), mockSB{}, llmFor, cfgFor, events, 5)
	s.SeedFindings([]engine.Finding{{Severity: "high", Title: "SQLi on /login", Target: "t", Detail: "first observation"}})
	s.Decider = fakeDecider{severity: "high", dupValue: "0", dupConf: 0.95, isReal: 0.9} // duplicate of finding 0
	mainCfg := engine.AgentConfig{Role: "Main", Model: "mock/main", Tools: []string{"record_finding"}, IsOrchestrator: true}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s.Run(ctx, mainCfg, "go")
	close(events)
	<-c.done

	fs := s.Findings()
	if len(fs) != 1 {
		t.Fatalf("a duplicate must not add a new finding: got %d findings, want 1", len(fs))
	}
	if !strings.Contains(fs[0].Detail, "second observation") {
		t.Errorf("the duplicate's evidence should be merged into the existing finding, got %q", fs[0].Detail)
	}
}

func TestFindingGateFailOpen(t *testing.T) {
	c, _ := recordFindingSession(t,
		fakeDecider{err: errors.New("provider down")},
		`{"title":"real bug","severity":"high","target":"t","detail":"d"}`)
	f, ok := c.finding()
	if !ok {
		t.Fatal("on a decision error the finding must still be recorded (fail open)")
	}
	if f.Severity != "high" {
		t.Errorf("fail-open severity = %q, want the agent's 'high'", f.Severity)
	}
}

// taskSession builds a session whose Main issues one routed task (prompt only) then
// stops, with a resolver that knows recon + judge. Returns the collector.
func taskSession(t *testing.T, dec engine.Decider, prompt string) *collector {
	t.Helper()
	mainMock := &mockLLM{steps: []step{
		{toolID: "t1", toolName: "task", toolArgs: `{"prompt":"` + prompt + `"}`},
		{text: "done"},
	}}
	subMock := &mockLLM{steps: []step{{text: "sub done"}}}
	llmFor := func(sel string) (engine.LLM, string, error) {
		if sel == "mock/main" {
			return mainMock, "m", nil
		}
		return subMock, "s", nil
	}
	cfgFor := func(role string) (engine.AgentConfig, bool) {
		switch strings.ToLower(role) {
		case "recon":
			return engine.AgentConfig{Role: "recon", Model: "mock/recon"}, true
		case "judge":
			return engine.AgentConfig{Role: "judge", Model: "mock/judge"}, true
		}
		return engine.AgentConfig{}, false
	}
	events := make(chan event.Event, 256)
	c := newCollector()
	go drain(events, c)
	s := engine.NewSession(tools.NewRegistry(), mockSB{}, llmFor, cfgFor, events, 5)
	s.Decider = dec
	s.RouterRoles = []engine.RoleInfo{{Name: "recon", Description: "reconnaissance"}, {Name: "judge", Description: "verifies exploits"}}
	mainCfg := engine.AgentConfig{Role: "Main", Model: "mock/main", Tools: []string{"task", "wait"}, IsOrchestrator: true}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s.Run(ctx, mainCfg, "go")
	close(events)
	<-c.done
	return c
}

func TestRoutingPicksRole(t *testing.T) {
	c := taskSession(t, fakeDecider{routeRole: "recon"}, "look at the host")
	if !c.sawSpawn("Recon") {
		t.Error("routed task should spawn the role the decision model chose (recon)")
	}
}

func TestJudgeGatePass(t *testing.T) {
	// routed to judge, strong evidence (idx 2 >= judgeThreshold) -> judge spawns.
	c := taskSession(t, fakeDecider{routeRole: "judge", judgeIdx: 2}, "verify the rce")
	if !c.sawSpawn("Judge") {
		t.Error("a verification that clears the gate should spawn the judge")
	}
}

func TestJudgeGateReject(t *testing.T) {
	// routed to judge, weak evidence (idx 0 < judgeThreshold) -> judge is NOT spawned.
	c := taskSession(t, fakeDecider{routeRole: "judge", judgeIdx: 0}, "verify the rce")
	if c.sawSpawn("Judge") {
		t.Error("a verification below the evidence bar must not spawn the judge")
	}
}
