package sandbox

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"
)

// mockRoot stands in for the in-guest root transport so the egress state machine can
// be exercised without a live sandbox. It answers the three script shapes egressCore
// issues — user provisioning, rule application, and the EnsureApplied health check —
// and counts rule applications so a test can tell a re-apply from a no-op.
type mockRoot struct {
	mu       sync.Mutex
	uidsLine string // what ensureUsers parses (SCOPE=/NET=/ISO=)
	chainOK  bool   // EnsureApplied health check: true => "OK", false => "GONE"
	applies  int    // number of rule-apply scripts run
}

func (m *mockRoot) exec(_ context.Context, script string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	switch {
	case strings.Contains(script, "useradd"):
		return m.uidsLine, nil
	case strings.Contains(script, "echo APPLIED"):
		m.applies++
		return "APPLIED", nil
	case strings.Contains(script, "echo OK"): // the EnsureApplied chain-present probe
		if m.chainOK {
			return "OK", nil
		}
		return "GONE", nil
	default:
		return "", nil // teardown, etc.
	}
}

func (m *mockRoot) applyCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.applies
}

func goodScope() Scope { return Scope{Targets: []string{"10.0.0.0/24"}} } // IP-only: no DNS

// TestEgressApplyTeardownTransitions covers the applied/torn-down lifecycle: a clean
// apply flips Applied() true, Teardown flips it back and is idempotent.
func TestEgressApplyTeardownTransitions(t *testing.T) {
	m := &mockRoot{uidsLine: "SCOPE=1001 NET=1002 ISO=1003", chainOK: true}
	e := newEgressCore(m.exec)

	if e.Applied() {
		t.Fatal("a fresh core must not report applied")
	}
	if err := e.ApplyBaseline(goodScope()); err != nil {
		t.Fatalf("ApplyBaseline with good uids: %v", err)
	}
	if !e.Applied() {
		t.Error("Applied() should be true after a successful ApplyBaseline")
	}
	if m.applyCount() != 1 {
		t.Errorf("expected exactly one rule apply, got %d", m.applyCount())
	}
	if err := e.Teardown(); err != nil {
		t.Errorf("Teardown: %v", err)
	}
	if e.Applied() {
		t.Error("Applied() should be false after Teardown")
	}
	if err := e.Teardown(); err != nil { // idempotent no-op once torn down
		t.Errorf("second Teardown should be a no-op, got %v", err)
	}
}

// TestEgressApplyBaselineBadUidsFailClosed proves ApplyBaseline refuses to emit rules
// when the oline users did not resolve to distinct non-root uids — a root (uid 0) or a
// collision would cut off or conflate egress classes, so it must fail closed.
func TestEgressApplyBaselineBadUidsFailClosed(t *testing.T) {
	for _, tc := range []struct {
		name, uids string
	}{
		{"root-uid", "SCOPE=0 NET=1002 ISO=1003"},
		{"collision", "SCOPE=1001 NET=1001 ISO=1003"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := &mockRoot{uidsLine: tc.uids, chainOK: true}
			e := newEgressCore(m.exec)
			err := e.ApplyBaseline(goodScope())
			if err == nil || !strings.Contains(err.Error(), "distinct non-root uids") {
				t.Fatalf("expected a fail-closed uid error, got %v", err)
			}
			if e.Applied() {
				t.Error("Applied() must stay false when ApplyBaseline fails closed")
			}
			if m.applyCount() != 0 {
				t.Errorf("no rules may be applied when uids are bad, got %d applies", m.applyCount())
			}
		})
	}
}

// TestEgressEnsureAppliedReappliesWhenGone covers the distro-restart recovery: once
// applied, if the health check reports the chain GONE, EnsureApplied re-applies the
// stored baseline; if it reports OK, it does nothing; and it is throttled so it checks
// at most once per interval.
func TestEgressEnsureAppliedReappliesWhenGone(t *testing.T) {
	m := &mockRoot{uidsLine: "SCOPE=1001 NET=1002 ISO=1003", chainOK: true}
	e := newEgressCore(m.exec)
	if err := e.ApplyBaseline(goodScope()); err != nil {
		t.Fatalf("ApplyBaseline: %v", err)
	}

	// Throttled right after apply (lastVerify is fresh): no check, no re-apply.
	e.EnsureApplied()
	if m.applyCount() != 1 {
		t.Errorf("EnsureApplied re-applied while throttled: applies=%d", m.applyCount())
	}

	// Chain vanished and the throttle window has passed: must re-apply.
	m.mu.Lock()
	m.chainOK = false
	m.mu.Unlock()
	e.mu.Lock()
	e.lastVerify = time.Now().Add(-time.Minute)
	e.mu.Unlock()
	e.EnsureApplied()
	if m.applyCount() != 2 {
		t.Errorf("EnsureApplied did not re-apply after the chain vanished: applies=%d", m.applyCount())
	}
	if !e.Applied() {
		t.Error("Applied() should remain true after a successful re-apply")
	}

	// Chain present and window passed: a check, but no re-apply.
	m.mu.Lock()
	m.chainOK = true
	m.mu.Unlock()
	e.mu.Lock()
	e.lastVerify = time.Now().Add(-time.Minute)
	e.mu.Unlock()
	e.EnsureApplied()
	if m.applyCount() != 2 {
		t.Errorf("EnsureApplied re-applied even though the chain was present: applies=%d", m.applyCount())
	}
}

// TestEgressEnsureAppliedNoopBeforeApply proves EnsureApplied does nothing (no in-guest
// call) until the baseline has been applied at least once.
func TestEgressEnsureAppliedNoopBeforeApply(t *testing.T) {
	m := &mockRoot{uidsLine: "SCOPE=1001 NET=1002 ISO=1003", chainOK: false}
	e := newEgressCore(m.exec)
	e.EnsureApplied()
	if m.applyCount() != 0 {
		t.Errorf("EnsureApplied acted before any apply: applies=%d", m.applyCount())
	}
	if e.Applied() {
		t.Error("Applied() must be false before the first ApplyBaseline")
	}
}

// TestEgressCoreConcurrent hammers the state machine from many goroutines (apply /
// teardown / ensure / applied) to confirm the mutex discipline holds — no panic, no
// deadlock.
func TestEgressCoreConcurrent(t *testing.T) {
	m := &mockRoot{uidsLine: "SCOPE=1001 NET=1002 ISO=1003", chainOK: true}
	e := newEgressCore(m.exec)
	if err := e.ApplyBaseline(goodScope()); err != nil {
		t.Fatalf("seed ApplyBaseline: %v", err)
	}

	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				switch (g + i) % 4 {
				case 0:
					_ = e.ApplyBaseline(goodScope())
				case 1:
					e.EnsureApplied()
				case 2:
					_ = e.Teardown()
				case 3:
					_ = e.Applied()
				}
			}
		}(g)
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("egressCore state ops deadlocked under concurrency")
	}
}
