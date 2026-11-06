package engine

import (
	"sync"
	"testing"

	"github.com/nihaal-prasad/oline/internal/core"
)

// FuzzStateMachine drives a random sequence of lifecycle operations (from several
// goroutines at once) against a stateMachine and asserts it never panics — the key
// risk being a double-close of the resume channel across concurrent kill/resume. It
// also checks the state never lands outside the known set. Run with `go test` (the
// seed corpus) or `go test -fuzz=FuzzStateMachine`.
func FuzzStateMachine(f *testing.F) {
	f.Add([]byte{0, 1, 2, 3, 4, 5})
	f.Add([]byte{4, 4, 2, 1}) // kill, kill, resume, pause
	f.Add([]byte{0, 4, 1, 0}) // pause, kill, resume, pause

	f.Fuzz(func(t *testing.T, ops []byte) {
		if len(ops) > 512 {
			ops = ops[:512] // bound the work per input so a huge corpus entry isn't flagged "slow"
		}
		sm := newStateMachine()
		apply := func(op byte) {
			switch op % 6 {
			case 0:
				sm.pause()
			case 1:
				sm.resumeNow()
			case 2:
				sm.setIdle()
			case 3:
				sm.setRunning()
			case 4:
				sm.kill()
			case 5:
				st, ch := sm.snapshot()
				if ch == nil {
					t.Fatalf("snapshot returned a nil resume channel in state %v", st)
				}
			}
		}
		// Apply the sequence concurrently from three goroutines: a double-close of the
		// resume channel (the real hazard) panics the test deterministically.
		var wg sync.WaitGroup
		for g := 0; g < 3; g++ {
			wg.Add(1)
			go func(shift int) {
				defer wg.Done()
				for i := 0; i < len(ops); i++ {
					apply(ops[(i+shift)%len(ops)])
				}
			}(g)
		}
		if len(ops) == 0 {
			wg.Wait()
			return
		}
		wg.Wait()

		switch st := sm.State(); st {
		case core.Running, core.Paused, core.Idle, core.Killed:
		default:
			t.Fatalf("state machine reached an unknown state %v", st)
		}
	})
}
