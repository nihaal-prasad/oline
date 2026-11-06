package decide

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// flapServer answers the first len(statuses) requests with those (error) status
// codes, then 200 + okBody for every request after — modeling a provider that is
// briefly unavailable (cold/flapping) and then recovers. It returns the client and a
// getter for how many requests arrived.
func flapServer(t *testing.T, statuses []int, okBody string) (*Client, func() int) {
	t.Helper()
	var n int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		i := int(atomic.AddInt32(&n, 1)) - 1
		if i < len(statuses) {
			w.WriteHeader(statuses[i])
			_, _ = w.Write([]byte(`{"error":"flapping"}`))
			return
		}
		w.WriteHeader(200)
		_, _ = w.Write([]byte(okBody))
	}))
	t.Cleanup(srv.Close)
	c := New(srv.URL, "k", "m")
	c.backoff = 5 * time.Millisecond // keep the test fast
	return c, func() int { return int(atomic.LoadInt32(&n)) }
}

// TestRetryThenSucceed: two 503s followed by a 200 must succeed within the retry
// budget (maxAttempts=3) — a brief outage recovers transparently, not as an error.
func TestRetryThenSucceed(t *testing.T) {
	c, count := flapServer(t, []int{503, 503}, `{"answers":[{"type":"predicate","name":"q","probability":1}]}`)
	if err := c.Ping(context.Background()); err != nil {
		t.Fatalf("a flap that recovers within the retry budget must succeed, got %v", err)
	}
	if n := count(); n != 3 {
		t.Errorf("expected 3 attempts (2 failures + 1 success), got %d", n)
	}
}

// TestRetryExhaustedIsTransient: more failures than the retry budget leaves a
// transient error (so the caller enables the feature optimistically), not a definitive
// one.
func TestRetryExhaustedIsTransient(t *testing.T) {
	c, count := flapServer(t, []int{503, 503, 503, 503}, `{"answers":[]}`)
	err := c.Ping(context.Background())
	if err == nil || !IsTransient(err) {
		t.Fatalf("exhausting retries on 5xx must be transient, got %v (transient=%v)", err, IsTransient(err))
	}
	if n := count(); n != c.maxAttempts {
		t.Errorf("expected exactly maxAttempts=%d requests, got %d", c.maxAttempts, n)
	}
}

// TestTransientOn429: a rate-limit response is retryable and, when it persists,
// transient (its own branch, distinct from 5xx).
func TestTransientOn429(t *testing.T) {
	c := serve(t, 429, `{"error":"slow down"}`)
	err := c.Ping(context.Background())
	if err == nil || !IsTransient(err) {
		t.Errorf("a 429 must be transient, got %v (transient=%v)", err, IsTransient(err))
	}
}

// TestMalformedIsDefinitive: a 200 whose body is not a decisions response is a
// DEFINITIVE failure (the endpoint is not a decisions API) — it must not be retried or
// classified transient, so the feature stays disabled instead of being optimistically
// enabled against a provider that cannot serve it.
func TestMalformedIsDefinitive(t *testing.T) {
	c, count := flapServer(t, nil, `not a decisions response`)
	err := c.Ping(context.Background())
	if err == nil {
		t.Fatal("a malformed 200 body must error")
	}
	if IsTransient(err) {
		t.Errorf("a malformed body must be definitive (not transient), got %v", err)
	}
	if n := count(); n != 1 {
		t.Errorf("a definitive failure must not be retried, got %d requests", n)
	}
}

// TestTransientOnNetworkError: a provider that cannot be reached at all (connection
// refused) is a transient/network failure after the retries are spent.
func TestTransientOnNetworkError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close() // nothing listens here now

	c := New(url, "k", "m")
	c.maxAttempts = 2
	c.backoff = 5 * time.Millisecond
	err := c.Ping(context.Background())
	if err == nil || !IsTransient(err) {
		t.Errorf("a connection-refused probe must be transient, got %v (transient=%v)", err, IsTransient(err))
	}
}

// TestCancelDuringRetryIsTransient: when the context is cancelled while waiting to
// retry a flapping provider, the call returns promptly with a transient error (never
// wedging on the backoff).
func TestCancelDuringRetryIsTransient(t *testing.T) {
	c, _ := flapServer(t, []int{503, 503, 503, 503, 503}, `{}`)
	c.backoff = 500 * time.Millisecond // long enough that the ctx fires during backoff

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- c.Ping(ctx) }()
	select {
	case err := <-done:
		if err == nil || !IsTransient(err) {
			t.Errorf("a ctx cancel mid-retry must be transient, got %v (transient=%v)", err, IsTransient(err))
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Ping did not return after ctx cancel during retry backoff — wedged on backoff")
	}
}
