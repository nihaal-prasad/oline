package engine

import (
	"context"
	"fmt"
	"math/rand"
	"sync"
	"testing"

	"github.com/nihaal-prasad/oline/internal/core"
	"github.com/nihaal-prasad/oline/internal/event"
	"github.com/nihaal-prasad/oline/internal/llm"
	"github.com/nihaal-prasad/oline/internal/tools"
)

// TestRevshellChannelConcurrency hammers the reverse-shell channel lifecycle from
// several goroutines at once: open / send / read / list / close / acquire / release /
// grant, all racing on a small set of channel names across two subagents. The channel
// map and the per-channel write token are guarded by Session.chMu; this asserts no
// interleaving panics or deadlocks, that the listener goroutines (registered on the
// emitter wg via the drain-gated tracker) all unwind on teardown, and that the channel
// map is empty afterwards (no leaked channel). Reuses revSB from the revshell tests.
func TestRevshellChannelConcurrency(t *testing.T) {
	events := make(chan event.Event, 4096)
	drainStop := make(chan struct{})
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		for {
			select {
			case <-events:
			case <-drainStop:
				return
			}
		}
	}()

	llmFor := func(string) (LLM, string, error) { return &scriptLLM{}, "m", nil }
	cfgFor := func(string) (AgentConfig, bool) { return AgentConfig{}, false }
	s := NewSession(tools.NewRegistry(), &revSB{readOut: "ok\n"}, llmFor, cfgFor, events, 5)
	s.Workspace = "/opt/oline/engagements/test"

	ctx, cancel := context.WithCancel(context.Background())
	s.setBase(ctx)

	mk := func(id core.AgentID) *Agent {
		a := newAgent(id, AgentConfig{Role: string(id), Tools: []string{"revshell"}}, nil, "m", s, core.Main, "")
		s.router.register(a)
		return a
	}
	agents := []*Agent{mk("worker#1"), mk("worker#2")}
	ids := []core.AgentID{agents[0].id, agents[1].id}
	names := []string{"c0", "c1", "c2"}
	actions := []string{"open", "send", "read", "list", "close", "acquire", "release", "grant"}

	rev := func(a *Agent, action, name, target string) {
		args := fmt.Sprintf(`{"action":%q,"name":%q,"port":4444,"data":"id","bytes":64,"agent":%q}`, action, name, target)
		a.doRevshell(llm.ToolCall{Name: "revshell", Arguments: args})
	}

	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			rnd := rand.New(rand.NewSource(int64(g) + 1))
			for i := 0; i < 400; i++ {
				a := agents[rnd.Intn(len(agents))]
				rev(a, actions[rnd.Intn(len(actions))], names[rnd.Intn(len(names))], string(ids[rnd.Intn(len(ids))]))
			}
		}(g)
	}
	wg.Wait()

	// Close whatever is still open, then cancel and drain the listener goroutines.
	for _, n := range names {
		s.CloseChannel(n)
	}
	cancel()
	s.router.drainEmitters()

	s.chMu.Lock()
	left := len(s.channels)
	s.chMu.Unlock()
	if left != 0 {
		t.Errorf("channel map not empty after close + drain: %d leaked", left)
	}

	close(drainStop)
	<-drained
}
