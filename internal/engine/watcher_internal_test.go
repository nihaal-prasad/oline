package engine

import (
	"context"
	"strings"
	"testing"

	"github.com/nihaal-prasad/oline/internal/core"
	"github.com/nihaal-prasad/oline/internal/llm"
)

func TestWatcherBlocks(t *testing.T) {
	cases := []struct {
		name string
		args string
		want bool
	}{
		{"bash", `{"cmd":"id"}`, true},
		{"bash_bg", `{"cmd":"nc -lvnp 4444"}`, true},
		{"revshell", `{"action":"open","name":"c","port":4444}`, true},
		{"revshell", `{"action":"send","name":"c","data":"id"}`, true},
		{"revshell", `{"action":"read","name":"c"}`, false},
		{"revshell", `{"action":"list"}`, false},
		{"read", `{"path":"x"}`, false},
		{"write", `{"path":"x"}`, false},
		{"task", `{"agent":"recon","prompt":"x"}`, false},
	}
	for _, c := range cases {
		if got := watcherBlocks(c.name, c.args); got != c.want {
			t.Errorf("watcherBlocks(%q,%s) = %v, want %v", c.name, c.args, got, c.want)
		}
	}
}

func TestWatcherModeBlocksBashInDispatch(t *testing.T) {
	sb := &revSB{}
	a, s, _, cancel := newRevAgent(t, sb) // agent has bash + revshell
	defer cancel()
	s.Mode = core.Watcher

	out, isErr := a.dispatch(context.Background(), llm.ToolCall{Name: "bash", Arguments: `{"cmd":"id"}`})
	if !isErr || !strings.Contains(out, "watcher mode") {
		t.Errorf("bash should be blocked in watcher mode, got (isErr=%v): %s", isErr, out)
	}

	// A read-only revshell action is NOT blocked by watcher (it proceeds to its own
	// handling, which errors only because no channel exists — not the watcher block).
	out, _ = a.dispatch(context.Background(), llm.ToolCall{Name: "revshell", Arguments: `{"action":"list"}`})
	if strings.Contains(out, "watcher mode is active") {
		t.Errorf("revshell list should not be blocked by watcher mode, got: %s", out)
	}

	// Back in automatic mode, bash is not watcher-blocked.
	s.Mode = core.Automatic
	out, _ = a.dispatch(context.Background(), llm.ToolCall{Name: "bash", Arguments: `{"cmd":"true"}`})
	if strings.Contains(out, "watcher mode") {
		t.Errorf("bash must not be watcher-blocked in automatic mode, got: %s", out)
	}
}

func TestWatcherBlocksCustomExecTool(t *testing.T) {
	sb := &revSB{}
	a, s, _, cancel := newRevAgent(t, sb)
	defer cancel()
	s.Mode = core.Watcher
	s.ExecTools = map[string]bool{"my_custom_tool": true}
	out, isErr := a.dispatch(context.Background(), llm.ToolCall{Name: "my_custom_tool", Arguments: `{}`})
	if !isErr || !strings.Contains(out, "watcher mode") {
		t.Errorf("a custom exec tool should be blocked in watcher mode, got (isErr=%v): %s", isErr, out)
	}
}
