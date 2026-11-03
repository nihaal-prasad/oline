package engine

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/nihaal-prasad/oline/internal/core"
	"github.com/nihaal-prasad/oline/internal/event"
	"github.com/nihaal-prasad/oline/internal/sandbox"
	"github.com/nihaal-prasad/oline/internal/tools"
)

// resSB is a sandbox stub whose Exec writes canned output and records the command
// it was asked to run, so the resources probe can be asserted without a real VM.
type resSB struct {
	out    string
	policy sandbox.EgressPolicy
	argv   []string
}

func (m *resSB) Exec(_ context.Context, _ core.AgentID, cmd sandbox.Cmd, out io.Writer) (sandbox.ExitStatus, error) {
	m.policy = cmd.Policy
	m.argv = cmd.Argv
	io.WriteString(out, m.out)
	return sandbox.ExitStatus{Code: 0}, nil
}

func (*resSB) CopyIn(context.Context, string, string) error     { return nil }
func (*resSB) CopyOut(context.Context, string, string) error    { return nil }
func (*resSB) Backup(context.Context, string, string) error     { return nil }
func (*resSB) EgressController() sandbox.EgressController       { return nil }
func (*resSB) SetupEgress(context.Context, sandbox.Scope) error { return nil }
func (*resSB) Info() sandbox.EnvInfo                            { return sandbox.EnvInfo{} }

// TestResourcesSnapshot verifies the orchestrator's resources tool returns the
// sandbox probe output and, crucially, runs the probe privileged (as root) so it
// can see every user's processes for the per-agent-class breakdown.
func TestResourcesSnapshot(t *testing.T) {
	events := make(chan event.Event, 64)
	go func() {
		for range events {
		}
	}()
	sb := &resSB{out: "## Sandbox resources\nCPU: 4 cores | load 0.10 0.05 0.01 (1/5/15 min) | procs 2\n"}
	s := NewSession(tools.NewRegistry(), sb, nil, func(string) (AgentConfig, bool) { return AgentConfig{}, false }, events, 5)
	main := newAgent(core.Main, AgentConfig{Role: "Main", Tools: []string{"resources"}}, nil, "m", s, "", "")

	out, isErr := main.doResources(context.Background())
	if isErr {
		t.Fatalf("doResources returned an error result: %s", out)
	}
	if !strings.Contains(out, "Sandbox resources") {
		t.Errorf("want the probe snapshot in the result, got: %q", out)
	}
	if sb.policy != sandbox.Privileged {
		t.Errorf("the probe must run privileged (root) for full process visibility, got policy %v", sb.policy)
	}
	if len(sb.argv) < 3 || sb.argv[0] != "bash" || sb.argv[1] != "-lc" {
		t.Errorf("the probe should run as `bash -lc <script>`, got argv %v", sb.argv)
	}

	// resources is orchestrator-only: the dispatch gate refuses it to an agent that
	// was not granted it.
	if allowed([]string{"bash", "read"}, "resources") {
		t.Error("resources must not be available to an agent without the grant")
	}
}
