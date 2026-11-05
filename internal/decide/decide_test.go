package decide

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// serve stands up a decisions endpoint that returns the given JSON body for
// POST /decisions, and returns a Client pointed at it.
func serve(t *testing.T, status int, body string) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/decisions" {
			t.Errorf("want POST /decisions, got %s %s", r.Method, r.URL.Path)
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	c := New(srv.URL, "k", "m")
	c.maxAttempts = 1
	return c
}

func TestPredicate(t *testing.T) {
	c := serve(t, 200, `{"answers":[{"type":"predicate","name":"q","probability":0.87}]}`)
	p, err := c.Predicate(context.Background(), "doc", "true?")
	if err != nil || p != 0.87 {
		t.Fatalf("Predicate = %v, %v; want 0.87", p, err)
	}
}

func TestChoice(t *testing.T) {
	c := serve(t, 200, `{"answers":[{"type":"choice","name":"q","choice":"high","confidence":0.9,
		"probabilities":[{"value":"low","probability":0.1},{"value":"high","probability":0.8}]}]}`)
	val, prob, err := c.Choice(context.Background(), "doc", "severity?",
		[]Choice{{Value: "low"}, {Value: "high"}})
	if err != nil || val != "high" || prob != 0.8 {
		t.Fatalf("Choice = %q, %v, %v; want high, 0.8", val, prob, err)
	}
}

func TestScore(t *testing.T) {
	// 4 levels, weighted index 0.6 -> idx 1 (round), grade 0.6/3 = 0.2.
	c := serve(t, 200, `{"answers":[{"type":"score","name":"q","score":0.6,"confidence":0.7}]}`)
	idx, grade, err := c.Score(context.Background(), "doc", "progress?",
		[]string{"stuck", "little", "steady", "strong"})
	if err != nil || idx != 1 {
		t.Fatalf("Score idx = %d, %v; want 1", idx, err)
	}
	if grade < 0.19 || grade > 0.21 {
		t.Errorf("grade = %v; want ~0.2", grade)
	}
}

func TestChoiceBoolValue(t *testing.T) {
	// A provider may echo a bare boolean choice value; rawToString handles it.
	c := serve(t, 200, `{"answers":[{"type":"choice","name":"q","choice":true,
		"probabilities":[{"value":true,"probability":0.95},{"value":false,"probability":0.05}]}]}`)
	val, prob, err := c.Choice(context.Background(), "doc", "?", []Choice{{Value: "true"}, {Value: "false"}})
	if err != nil || val != "true" || prob != 0.95 {
		t.Fatalf("Choice(bool) = %q, %v, %v; want true, 0.95", val, prob, err)
	}
}

func TestErrorStatus(t *testing.T) {
	c := serve(t, 500, `oops`)
	if _, err := c.Predicate(context.Background(), "d", "?"); err == nil {
		t.Error("want error on 500 (so gates can fail open)")
	}
}

func TestMalformedBody(t *testing.T) {
	c := serve(t, 200, `not json`)
	if _, err := c.Predicate(context.Background(), "d", "?"); err == nil {
		t.Error("want error on malformed body")
	}
}

func TestNoAnswers(t *testing.T) {
	c := serve(t, 200, `{"answers":[]}`)
	if _, err := c.Predicate(context.Background(), "d", "?"); err == nil {
		t.Error("want error when no answers returned")
	}
}

func TestRefusal(t *testing.T) {
	c := serve(t, 200, `{"answers":[{"type":"refusal","name":"q"}]}`)
	if _, err := c.Predicate(context.Background(), "d", "?"); err == nil {
		t.Error("want error on refusal")
	}
}
