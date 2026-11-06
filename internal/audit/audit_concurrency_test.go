package audit

import (
	"fmt"
	"path/filepath"
	"sync"
	"testing"
)

// TestConcurrentAppendsKeepChainIntact fires many agents at one audit log at once —
// the real multi-agent case, where several subagents log commands simultaneously —
// and then verifies the hash chain end to end. Because Append is mutex-guarded, the
// chain must stay valid (every record's hash, Prev link, and +1 sequence), every
// record must be present, and nothing may be interleaved or dropped.
func TestConcurrentAppendsKeepChainIntact(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.log")
	l, err := Open(path, "eng")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	const workers, each = 12, 200
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < each; i++ {
				l.Command(fmt.Sprintf("agent#%d", w), "bash", "", fmt.Sprintf("cmd-%d-%d", w, i))
			}
		}(w)
	}
	wg.Wait()
	if err := l.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	res, err := Verify(path)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if !res.OK {
		t.Fatalf("hash chain broken after concurrent appends at record %d: %s", res.BrokenAt, res.Reason)
	}
	if want := workers * each; res.Entries != want {
		t.Errorf("log has %d entries, want %d (a concurrent append was dropped or interleaved)", res.Entries, want)
	}
}

// TestConcurrentAppendsThenResumeVerifies confirms the chain survives a reopen after
// concurrent writes: Open recovers the head, more concurrent appends extend it, and
// the whole chain still verifies — so a resumed engagement's audit trail stays sound.
func TestConcurrentAppendsThenResumeVerifies(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.log")

	writeBurst := func(tag string) {
		l, err := Open(path, "eng")
		if err != nil {
			t.Fatalf("Open(%s): %v", tag, err)
		}
		var wg sync.WaitGroup
		for w := 0; w < 8; w++ {
			wg.Add(1)
			go func(w int) {
				defer wg.Done()
				for i := 0; i < 100; i++ {
					l.Command(fmt.Sprintf("%s-agent#%d", tag, w), "bash", "", fmt.Sprintf("%s-%d-%d", tag, w, i))
				}
			}(w)
		}
		wg.Wait()
		if err := l.Close(); err != nil {
			t.Fatalf("Close(%s): %v", tag, err)
		}
	}

	writeBurst("a")
	writeBurst("b") // reopen: recovers seq/head, extends the same chain

	res, err := Verify(path)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if !res.OK {
		t.Fatalf("chain broken after a resumed concurrent burst at record %d: %s", res.BrokenAt, res.Reason)
	}
	if want := 2 * 8 * 100; res.Entries != want {
		t.Errorf("log has %d entries, want %d", res.Entries, want)
	}
}
