package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nihaal-prasad/oline/internal/core"
	"github.com/nihaal-prasad/oline/internal/event"
)

func TestEngLogCapturesActivity(t *testing.T) {
	t.Setenv("OLINE_HOME", t.TempDir())

	l := openEngLog("shadow-supply")
	if l == nil {
		t.Fatal("openEngLog returned nil")
	}
	l.banner("start")
	// A realistic burst: think -> text -> tool call -> output -> state.
	l.log(event.TokenDelta{Agent: "recon#1", Reasoning: "planning a quick scan"})
	l.log(event.TokenDelta{Agent: "recon#1", Text: "Running an nmap sweep."})
	l.log(event.ToolStarted{Agent: "recon#1", Tool: "bash", ArgsPreview: `{"cmd":"nmap -sn 10.0.0.0/24"}`})
	l.log(event.ToolOutput{Agent: "recon#1", Chunk: "Host 10.0.0.5 is up\n"})
	l.log(event.AgentState{Agent: "recon#1", State: core.Idle})
	l.log(event.Spawned{Parent: core.Main, Child: "recon#1", Role: "recon"})
	l.userMsg(core.Main, "focus on the fileserver")
	l.close()

	data, err := os.ReadFile(filepath.Join(os.Getenv("OLINE_HOME"), "logs", "shadow-supply.log"))
	if err != nil {
		t.Fatalf("log file not written: %v", err)
	}
	s := string(data)
	for _, want := range []string{
		"planning a quick scan",        // thinking
		"Running an nmap sweep.",       // text
		"call", "nmap -sn 10.0.0.0/24", // tool call
		"Host 10.0.0.5 is up", // tool output
		"state", "idle",       // state change
		"spawn", "recon#1 (role recon)", // spawn
		"user→", "focus on the fileserver", // user message
	} {
		if !strings.Contains(s, want) {
			t.Errorf("activity log missing %q\n---\n%s", want, s)
		}
	}
}

func TestEngLogNilSafe(t *testing.T) {
	var l *englog // nil
	l.banner("x")
	l.log(event.Notice{Level: "info", Text: "y"})
	l.userMsg(core.Main, "z")
	l.close()
	if l.Path() != "" {
		t.Error("nil logger should have empty path")
	}
}
