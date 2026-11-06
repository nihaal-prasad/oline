package tui

import (
	"testing"
	"time"

	"github.com/nihaal-prasad/oline/internal/core"
	"github.com/nihaal-prasad/oline/internal/event"
)

// TestFunnelDecouplesProducer is the whole point: the producer must not block on a
// slow/idle consumer. With an UNBUFFERED source and no take() calls at all, every
// send must still complete, because the drain goroutine receives them into the
// in-memory buffer. If draining were coupled to the consumer, these sends would
// block and the test would time out.
func TestFunnelDecouplesProducer(t *testing.T) {
	src := make(chan event.Event) // unbuffered: a coupled drain would block the sender
	f := newFunnel(src)

	const n = 1000
	sent := make(chan struct{})
	go func() {
		for i := 0; i < n; i++ {
			src <- event.TokenDelta{Agent: core.Main, Text: "x"}
		}
		close(src)
		close(sent)
	}()

	select {
	case <-sent:
	case <-time.After(3 * time.Second):
		t.Fatal("producer blocked — draining is still coupled to the consumer")
	}

	// Now drain everything the consumer side would see; nothing may be lost.
	got := 0
	for {
		batch, ok := f.take()
		if !ok {
			break
		}
		if len(batch) > maxFunnelBatch {
			t.Fatalf("take returned %d events, over the %d cap", len(batch), maxFunnelBatch)
		}
		got += len(batch)
	}
	if got != n {
		t.Errorf("drained %d events, want %d (events were lost)", got, n)
	}
}

// TestFunnelOrderAndClose checks events come out in order and that, once the source
// is closed and drained, take reports ok=false exactly once there is nothing left.
func TestFunnelOrderAndClose(t *testing.T) {
	src := make(chan event.Event, 8)
	f := newFunnel(src)
	for i := 0; i < 5; i++ {
		src <- event.Notice{Agent: core.Main, Text: string(rune('a' + i))}
	}
	close(src)

	var out []string
	for {
		batch, ok := f.take()
		if !ok {
			break
		}
		for _, e := range batch {
			out = append(out, e.(event.Notice).Text)
		}
	}
	want := []string{"a", "b", "c", "d", "e"}
	if len(out) != len(want) {
		t.Fatalf("got %v, want %v", out, want)
	}
	for i := range want {
		if out[i] != want[i] {
			t.Fatalf("order wrong at %d: got %v, want %v", i, out, want)
		}
	}
}
