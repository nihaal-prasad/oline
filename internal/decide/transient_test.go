package decide

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestTransientOnTimeout: a probe that exceeds its deadline (e.g. a cold model that
// never answers in time) must classify as transient, so a caller can enable the
// feature optimistically rather than disabling it for the whole engagement.
func TestTransientOnTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond) // outlast the context below
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"answers":[{"type":"predicate","name":"q","probability":1}]}`))
	}))
	defer srv.Close()
	c := New(srv.URL, "k", "m")
	c.maxAttempts = 1
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	err := c.Ping(ctx)
	if err == nil {
		t.Fatal("expected a timeout error")
	}
	if !IsTransient(err) {
		t.Errorf("a timeout must be transient, got non-transient: %v", err)
	}
}

// TestTransientOn5xx: an exhausted retry on a 5xx is transient.
func TestTransientOn5xx(t *testing.T) {
	c := serve(t, 503, `{"error":"unavailable"}`)
	err := c.Ping(context.Background())
	if err == nil || !IsTransient(err) {
		t.Errorf("a 5xx must be transient, got %v (transient=%v)", err, IsTransient(err))
	}
}

// TestDefinitiveOn4xx: a 404 (provider does not serve /decisions) or other 4xx is a
// definitive rejection, NOT transient — the feature should stay disabled.
func TestDefinitiveOn4xx(t *testing.T) {
	for _, status := range []int{400, 401, 403, 404} {
		c := serve(t, status, `{"error":"nope"}`)
		err := c.Ping(context.Background())
		if err == nil {
			t.Fatalf("status %d: expected an error", status)
		}
		if IsTransient(err) {
			t.Errorf("status %d must be definitive (not transient), got transient: %v", status, err)
		}
	}
}
