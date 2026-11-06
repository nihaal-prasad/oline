package engine_test

import (
	"context"
	"testing"
	"time"

	"github.com/nihaal-prasad/oline/internal/core"
	"github.com/nihaal-prasad/oline/internal/engine"
	"github.com/nihaal-prasad/oline/internal/event"
	"github.com/nihaal-prasad/oline/internal/tools"
)

// FuzzActorControl runs a small engagement (an orchestrator that spawns two
// subagents and exchanges messages) while a fuzzer-derived sequence of control
// operations (pause / resume / kill / steer / stop / pause-all / resume-all) is
// applied CONCURRENTLY to random agents. It asserts the actor model tolerates any
// interleaving: no panic, and Session.Run always returns (no deadlock, no agent left
// wedged). The engagement context is bounded, so a paused-and-never-resumed agent
// still terminates; a true deadlock is caught by the outer watchdog.
//
// Run with `go test` (seed corpus) or `go test -fuzz=FuzzActorControl`.
func FuzzActorControl(f *testing.F) {
	f.Add([]byte{0, 1, 2, 3})
	f.Add([]byte{2, 2, 2})       // kill, kill, kill
	f.Add([]byte{0, 0, 5, 6, 1}) // pause, pause, pause-all, resume-all, resume
	f.Add([]byte{3, 4, 3, 4})    // steer, stop, steer, stop

	f.Fuzz(func(t *testing.T, ops []byte) {
		mainMock := &mockLLM{steps: []step{
			{toolID: "t1", toolName: "task", toolArgs: `{"agent":"probe","prompt":"a"}`},
			{toolID: "t2", toolName: "task", toolArgs: `{"agent":"probe","prompt":"b"}`},
			{toolID: "w1", toolName: "wait", toolArgs: `{}`},
			{text: "FINAL: done"},
		}}
		probeMock := &mockLLM{steps: []step{{text: "probe done"}}}
		llmFor := func(sel string) (engine.LLM, string, error) {
			if sel == "mock/main" {
				return mainMock, "m", nil
			}
			return probeMock, "p", nil
		}
		cfgFor := func(role string) (engine.AgentConfig, bool) {
			if role == "probe" {
				return engine.AgentConfig{Role: "probe", Model: "mock/probe", Tools: []string{"wait"}}, true
			}
			return engine.AgentConfig{}, false
		}

		events := make(chan event.Event, 4096)
		drained := make(chan struct{})
		go func() { // keep Events drained so nothing back-pressures
			for range events {
			}
			close(drained)
		}()

		s := engine.NewSession(tools.NewRegistry(), mockSB{}, llmFor, cfgFor, events, 5)
		mainCfg := engine.AgentConfig{Role: "Main", Model: "mock/main", Tools: []string{"task", "wait", "send_message"}, IsOrchestrator: true}

		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()

		done := make(chan struct{})
		go func() { s.Run(ctx, mainCfg, "go"); close(done) }()

		// Apply the fuzzed control ops concurrently against the live router.
		r := s.Router()
		targets := []core.AgentID{core.Main, "Probe#1", "Probe#2", "nope#9"}
		opDone := make(chan struct{})
		go func() {
			defer close(opDone)
			for i, b := range ops {
				tgt := targets[int(b)%len(targets)]
				switch b % 7 {
				case 0:
					r.Pause(tgt)
				case 1:
					r.Resume(tgt)
				case 2:
					r.Kill(tgt)
				case 3:
					r.Send(engine.Envelope{From: "User", To: tgt, Kind: engine.MsgSteer, Body: "x"})
				case 4:
					r.StopCommands(tgt)
				case 5:
					r.PauseAll()
				case 6:
					r.ResumeAll()
				}
				if i%3 == 0 {
					time.Sleep(time.Millisecond)
				}
			}
		}()

		select {
		case <-done:
		case <-time.After(8 * time.Second):
			t.Fatal("Session.Run did not return — possible deadlock under concurrent control ops")
		}
		<-opDone
		close(events)
		<-drained
	})
}
