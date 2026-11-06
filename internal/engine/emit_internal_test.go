package engine

import (
	"context"
	"testing"
	"time"

	"github.com/nihaal-prasad/oline/internal/core"
	"github.com/nihaal-prasad/oline/internal/event"
)

// TestEmitDeliversLifecycleDropsStream verifies the emit policy that fixes the
// "agent stuck showing running" bug: when the event channel is full, streaming
// deltas are dropped (never block the engine) but lifecycle AgentState events are
// delivered reliably — yet never block past the engagement context ending.
func TestEmitDeliversLifecycleDropsStream(t *testing.T) {
	ch := make(chan event.Event) // unbuffered: a send needs a ready reader
	s := &Session{Events: ch}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.setBase(ctx)

	// A stream delta with no reader is dropped (emit returns immediately).
	done := make(chan struct{})
	go func() { s.emit(event.TokenDelta{Agent: "a", Text: "x"}); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("emit blocked on a droppable stream delta")
	}

	// A lifecycle AgentState IS delivered: emit blocks until the reader takes it.
	go func() { s.emit(event.AgentState{Agent: "a", State: core.Idle}) }()
	select {
	case ev := <-ch:
		if as, ok := ev.(event.AgentState); !ok || as.State != core.Idle {
			t.Fatalf("got %#v, want AgentState Idle", ev)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("lifecycle AgentState was not delivered")
	}

	// Once the engagement context ends, even a lifecycle emit must not block.
	cancel()
	done2 := make(chan struct{})
	go func() { s.emit(event.AgentState{Agent: "a", State: core.Idle}); close(done2) }()
	select {
	case <-done2:
	case <-time.After(time.Second):
		t.Fatal("emit blocked after the engagement context ended")
	}
}
