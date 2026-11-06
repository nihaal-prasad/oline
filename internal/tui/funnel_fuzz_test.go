package tui

import (
	"strconv"
	"testing"
	"time"

	"github.com/nihaal-prasad/oline/internal/event"
)

// FuzzFunnel hammers the event funnel's core contract under concurrency: a producer
// emits n events (0..20000) onto the source channel while a consumer calls take()
// in a loop at a fuzz-chosen cadence. The funnel must deliver every event exactly
// once, in order, and must report ok=false only after the source is closed AND the
// buffer is fully drained. A hang (take never returning) trips the test timeout.
//
// This is the most concurrency-dense primitive in the TUI: a backing-array aliasing
// bug or a missed wake here would silently drop or reorder lifecycle events, which is
// exactly the agent-freeze class of bug this harness has had to chase.
func FuzzFunnel(f *testing.F) {
	f.Add(0, 0)
	f.Add(10, 3)
	f.Add(1000, 0)
	f.Add(2049, 1) // several maxFunnelBatch backlogs
	f.Fuzz(func(t *testing.T, n, takeDelayUs int) {
		if n < 0 {
			n = -n
		}
		if n > 20000 {
			n = 20000
		}
		if takeDelayUs < 0 {
			takeDelayUs = -takeDelayUs
		}
		takeDelayUs %= 50 // keep the test fast; just vary consumer timing

		src := make(chan event.Event) // unbuffered: producer and consumer truly race
		fn := newFunnel(src)

		go func() {
			for i := 0; i < n; i++ {
				src <- event.Notice{Text: strconv.Itoa(i)}
			}
			close(src)
		}()

		got := make([]int, 0, n)
		for {
			batch, ok := fn.take()
			if !ok {
				break
			}
			if len(batch) == 0 {
				t.Fatal("take returned ok with an empty batch")
			}
			if len(batch) > maxFunnelBatch {
				t.Fatalf("batch of %d exceeds maxFunnelBatch %d", len(batch), maxFunnelBatch)
			}
			for _, e := range batch {
				v, err := strconv.Atoi(e.(event.Notice).Text)
				if err != nil {
					t.Fatalf("corrupt event payload %q: %v", e.(event.Notice).Text, err)
				}
				got = append(got, v)
			}
			if takeDelayUs > 0 {
				time.Sleep(time.Duration(takeDelayUs) * time.Microsecond)
			}
		}

		if len(got) != n {
			t.Fatalf("delivered %d events, want %d (loss or duplication)", len(got), n)
		}
		for i, v := range got {
			if v != i {
				t.Fatalf("event %d delivered out of order (got %d)", i, v)
			}
		}
	})
}
