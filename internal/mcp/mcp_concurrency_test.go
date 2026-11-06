package mcp

import (
	"context"
	"io"
	"sync"
	"testing"
	"time"
)

// drainReader reads and discards everything from r until EOF, so a client write
// (writeLoop's Encode into an unbuffered pipe) never blocks for lack of a reader.
// Returns when r is closed.
func drainReader(r io.Reader) {
	buf := make([]byte, 4096)
	for {
		if _, err := r.Read(buf); err != nil {
			return
		}
	}
}

// TestClientConcurrentCallsUnblockOnServerDeath verifies that when the server dies
// mid-flight (its stdout hits EOF) with many CallTool requests outstanding, EVERY
// caller unblocks promptly with an error — none hangs — and both transport goroutines
// (readLoop/writeLoop) exit. This is the "a spawned MCP server crashed" path: a leak
// here would wedge the agents that were mid-tool-call and leak goroutines per restart.
func TestClientConcurrentCallsUnblockOnServerDeath(t *testing.T) {
	aR, aW := io.Pipe() // client -> server (server reads)
	bR, bW := io.Pipe() // server -> client (server writes)
	go drainReader(aR)  // accept the clients' request writes, but never answer

	c := newClient("dying", aW, bR)

	const N = 16
	var wg sync.WaitGroup
	errs := make(chan error, N)
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_, _, err := c.CallTool(ctx, "echo", nil)
			errs <- err
		}()
	}

	time.Sleep(50 * time.Millisecond) // let the calls get in flight
	bW.Close()                        // server stdout EOF -> readLoop.fail() -> unblock everyone

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(8 * time.Second):
		t.Fatal("concurrent CallTool did not all return after the server died — hang or leak")
	}
	close(errs)
	for err := range errs {
		if err == nil {
			t.Error("a CallTool returned a nil error after the server died")
		}
	}

	// Both transport goroutines must have exited (no per-crash goroutine leak).
	select {
	case <-c.readDone:
	case <-time.After(2 * time.Second):
		t.Error("readLoop did not exit after the server died")
	}
	select {
	case <-c.closed:
	default:
		t.Error("connection was not marked closed after the server died")
	}
}

// TestClientCloseDuringConcurrentCalls verifies that calling Close() while many
// CallTool are outstanding unblocks all of them (via c.closed) rather than wedging —
// the orderly-shutdown counterpart to the server-death path above.
func TestClientCloseDuringConcurrentCalls(t *testing.T) {
	aR, aW := io.Pipe()
	bR, bW := io.Pipe()
	defer bW.Close()
	go drainReader(aR)

	c := newClient("closing", aW, bR)

	const N = 16
	var wg sync.WaitGroup
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			c.CallTool(ctx, "echo", nil)
		}()
	}

	time.Sleep(50 * time.Millisecond)
	_ = c.Close() // must unblock every in-flight caller

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(8 * time.Second):
		t.Fatal("Close did not unblock in-flight CallTool callers")
	}

	// A call started AFTER Close returns immediately, never hangs.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, _, err := c.CallTool(ctx, "echo", nil); err == nil {
		t.Error("a call on a closed client should fail")
	}
}
