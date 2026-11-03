package llm

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// a minimal SSE body the client will parse to completion.
const oneChunk = "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"

// TestStreamRetriesTransient confirms a 503 then 200 succeeds after one retry, and
// that the streamed content is delivered exactly once (no duplication from retry).
func TestStreamRetriesTransient(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			fmt.Fprint(w, "overloaded")
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, oneChunk)
	}))
	defer srv.Close()

	c := New(srv.URL, "")
	c.backoffBase = time.Millisecond // keep the test fast

	ch := make(chan Delta, 16)
	err := c.Stream(context.Background(), ChatReq{Model: "m"}, ch)
	if err != nil {
		t.Fatalf("Stream after retry: %v", err)
	}
	var text string
	for d := range ch {
		text += d.TextDelta
	}
	if text != "hi" {
		t.Fatalf("content = %q, want %q (retry must not duplicate output)", text, "hi")
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("server hits = %d, want 2 (one 503, one 200)", got)
	}
}

// TestStreamGivesUpAfterMaxAttempts confirms persistent 500s exhaust the retries
// and the final error is returned (and the server was hit maxAttempts times).
func TestStreamGivesUpAfterMaxAttempts(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, "boom")
	}))
	defer srv.Close()

	c := New(srv.URL, "")
	c.maxAttempts = 3
	c.backoffBase = time.Millisecond

	ch := make(chan Delta, 4)
	err := c.Stream(context.Background(), ChatReq{Model: "m"}, ch)
	if err == nil {
		t.Fatal("expected an error after exhausting retries")
	}
	if got := atomic.LoadInt32(&calls); got != 3 {
		t.Fatalf("server hits = %d, want 3 (maxAttempts)", got)
	}
}

// TestStreamDoesNotRetryClientError confirms a 400 is returned immediately (a client
// error is not transient), with a single server hit.
func TestStreamDoesNotRetryClientError(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, "bad")
	}))
	defer srv.Close()

	c := New(srv.URL, "")
	c.backoffBase = time.Millisecond
	ch := make(chan Delta, 4)
	if err := c.Stream(context.Background(), ChatReq{Model: "m"}, ch); err == nil {
		t.Fatal("expected an error for a 400")
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("server hits = %d, want 1 (no retry on 4xx)", got)
	}
}

// TestAssembleSplitsNewIDAtSameIndex confirms two sequential calls sharing stream
// index 0 but with distinct ids become two tool calls, not one concatenated blob.
func TestAssembleSplitsNewIDAtSameIndex(t *testing.T) {
	frags := []ToolCallDelta{
		{Index: 0, ID: "a", Name: "one", ArgsFragment: `{"x":1}`},
		{Index: 0, ID: "b", Name: "two", ArgsFragment: `{"y":2}`},
	}
	got := AssembleToolCalls(frags)
	if len(got) != 2 {
		t.Fatalf("got %d calls, want 2: %+v", len(got), got)
	}
	if got[0].ID != "a" || got[0].Arguments != `{"x":1}` {
		t.Fatalf("call 0 = %+v", got[0])
	}
	if got[1].ID != "b" || got[1].Arguments != `{"y":2}` {
		t.Fatalf("call 1 = %+v", got[1])
	}
}

// TestAssembleConcatenatesSameIndexNoID confirms the normal streaming case (one call
// fragmented across deltas, id only on the first) still assembles into one call.
func TestAssembleConcatenatesSameIndexNoID(t *testing.T) {
	frags := []ToolCallDelta{
		{Index: 0, ID: "a", Name: "one", ArgsFragment: `{"x":`},
		{Index: 0, ArgsFragment: `1}`},
	}
	got := AssembleToolCalls(frags)
	if len(got) != 1 || got[0].Arguments != `{"x":1}` {
		t.Fatalf("got %+v, want one call with full args", got)
	}
}
