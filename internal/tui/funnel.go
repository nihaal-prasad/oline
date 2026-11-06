package tui

import (
	"sync"

	"github.com/nihaal-prasad/oline/internal/event"
)

// funnel decouples draining the engine's event channel from the Bubble Tea Update
// loop. A dedicated goroutine reads every event off the channel the instant it is
// produced and parks it in an in-memory buffer; the UI then takes events from that
// buffer at its own pace. Without this, draining was coupled to Update throughput —
// the `listen` command only ran again AFTER Update finished rendering the previous
// batch — so a slow render (many panes, glamour markdown) let the engine's channel
// fill to its cap. A full channel makes the engine block on every authoritative
// lifecycle emit (an AgentState transition, a spawn), which strands agents mid-turn:
// Running, producing no output, waiting on the UI. The drain goroutine here keeps
// the channel essentially empty, so the engine never blocks on back-pressure.
//
// The buffer is unbounded by design: absorbing a burst in memory is always better
// than back-pressuring the engine onto the agents. It drains in bounded batches so a
// large backlog never starves input handling, and it holds the events in order and
// loses none (lifecycle correctness is preserved regardless of UI speed).
type funnel struct {
	mu     sync.Mutex
	buf    []event.Event
	notify chan struct{} // buffered(1): "there may be events" — coalesced
	done   chan struct{} // closed once the source channel is closed
	closed bool          // source drained and closed (guarded by mu)
}

// maxFunnelBatch caps how many events one take returns, so a backlog is applied over
// several Update passes (keeping the UI responsive to input between them) rather than
// one giant render. The remainder stays buffered and is taken immediately next.
const maxFunnelBatch = 512

// newFunnel starts the drain goroutine reading from src and returns the funnel. The
// goroutine exits when src is closed (which, in normal operation, only happens at
// shutdown); until then it lives for the engagement.
func newFunnel(src <-chan event.Event) *funnel {
	f := &funnel{
		notify: make(chan struct{}, 1),
		done:   make(chan struct{}),
	}
	go func() {
		for ev := range src {
			f.mu.Lock()
			f.buf = append(f.buf, ev)
			f.mu.Unlock()
			// Coalesced wake: one pending slot means "non-empty"; take() drains all.
			select {
			case f.notify <- struct{}{}:
			default:
			}
		}
		f.mu.Lock()
		f.closed = true
		f.mu.Unlock()
		close(f.done)
	}()
	return f
}

// take blocks until there are buffered events (or the source closed and drained),
// then returns up to maxFunnelBatch of them in order. ok is false only once the
// source is closed AND every event has been handed out — the signal to stop pumping.
func (f *funnel) take() (batch []event.Event, ok bool) {
	for {
		select {
		case <-f.notify:
		case <-f.done:
		}
		f.mu.Lock()
		n := len(f.buf)
		if n > maxFunnelBatch {
			n = maxFunnelBatch
		}
		batch = f.buf[:n]
		f.buf = f.buf[n:]
		more := len(f.buf) > 0
		if !more {
			f.buf = nil // release the backing array once fully drained
		}
		closed := f.closed
		f.mu.Unlock()

		if len(batch) > 0 {
			if more {
				// Re-arm so the next take grabs the remainder without waiting.
				select {
				case f.notify <- struct{}{}:
				default:
				}
			}
			return batch, true
		}
		if closed {
			return nil, false
		}
		// Spurious wake (an extra notify after we already drained): wait again.
	}
}
