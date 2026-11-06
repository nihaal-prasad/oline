package tui

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/nihaal-prasad/oline/internal/config"
	"github.com/nihaal-prasad/oline/internal/core"
	"github.com/nihaal-prasad/oline/internal/engine"
	"github.com/nihaal-prasad/oline/internal/llm"
	"github.com/nihaal-prasad/oline/internal/sandbox"
)

// writeFakeBundle writes a backup bundle at path containing a workspace.tar.gz entry,
// which is what extractWorkspaceTar looks for when restoring a resumed engagement.
func writeFakeBundle(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	body := []byte("fake workspace tar")
	_ = tw.WriteHeader(&tar.Header{Name: "workspace.tar.gz", Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg})
	_, _ = tw.Write(body)
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
}

func configureTestModels(m *model) {
	m.store.DefineProvider("t", config.Provider{
		BaseURL: "http://127.0.0.1:1", API: "openai", APIKeyRef: "env:T_KEY",
		Models: []config.ModelDef{{ID: "m1", Name: "m1", ContextWindow: 8000, MaxTokens: 1024}},
	})
	m.store.SetModelRoles("t/m1", "t/m1", "t/m1")
}

// fakeLLM returns one line and finishes, so the orchestrator goes idle (no network).
type fakeLLM struct{}

func (fakeLLM) Stream(ctx context.Context, _ llm.ChatReq, ch chan<- llm.Delta) error {
	defer close(ch)
	select {
	case ch <- llm.Delta{TextDelta: "ready"}:
	case <-ctx.Done():
		return ctx.Err()
	}
	ch <- llm.Delta{Finish: "stop"}
	return nil
}

// fakeHandle is a no-op sandbox.Handle (+ WorkspaceSetter) for exercising the
// engagement launch sequence without a real WSL/QEMU sandbox. It records the calls
// the off-thread workspace preparation makes.
type fakeHandle struct {
	mu      sync.Mutex
	wsSet   string
	copyIns int
	execs   int
}

func (h *fakeHandle) Exec(_ context.Context, _ core.AgentID, _ sandbox.Cmd, out io.Writer) (sandbox.ExitStatus, error) {
	h.mu.Lock()
	h.execs++
	h.mu.Unlock()
	// Satisfy the restore/copy helpers' success sentinels if they run.
	io.WriteString(out, "RESTORED COPIED\n")
	return sandbox.ExitStatus{Code: 0}, nil
}
func (h *fakeHandle) CopyIn(_ context.Context, _, _ string) error {
	h.mu.Lock()
	h.copyIns++
	h.mu.Unlock()
	return nil
}
func (h *fakeHandle) CopyOut(_ context.Context, _, _ string) error         { return nil }
func (h *fakeHandle) Backup(_ context.Context, _, _ string) error          { return nil }
func (h *fakeHandle) EgressController() sandbox.EgressController           { return nil }
func (h *fakeHandle) SetupEgress(_ context.Context, _ sandbox.Scope) error { return nil }
func (h *fakeHandle) Info() sandbox.EnvInfo                                { return sandbox.EnvInfo{ID: "fake"} }
func (h *fakeHandle) SetWorkspace(_ context.Context, dir string) error {
	h.mu.Lock()
	h.wsSet = dir
	h.mu.Unlock()
	return nil
}

// TestStartupOffThread verifies the three-phase engagement launch: beginSession
// builds the session (fast, on the UI thread), prepareWorkspaceCmd does the workspace
// I/O OFF the UI thread (here against a fake sandbox), and workspaceReadyMsg launches
// the orchestrator. The model's provider points at a closed port, so the first LLM
// turn fails fast and Run returns — confirming the whole launch path completes with no
// panic or deadlock.
func TestStartupOffThread(t *testing.T) {
	t.Setenv("T_KEY", "test")
	m, _ := tempHomeModel(t)
	configureTestModels(m)

	var tm tea.Model = m
	tm, _ = tm.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	m = tm.(*model)
	m.engagement = "startup-test"
	m.prepCand = sandbox.Candidate{ID: "fake", Backend: sandbox.BackendWSL}
	m.cands = []sandbox.Candidate{m.prepCand}

	h := &fakeHandle{}

	// Phase 1: build the session on the UI thread (decision/MCP unconfigured, so the
	// off-thread prep1 would be a no-op; pass an empty sessionPrep1 directly).
	tmodel, _ := m.beginSession(sessionPrep1{h: h})
	m = tmodel.(*model)
	if m.sess == nil || !m.running {
		t.Fatal("beginSession did not build a running session")
	}
	// Swap in a fake LLM so the orchestrator does no network on launch; it goes idle
	// (interactive) and unwinds when the engagement context is cancelled below.
	m.sess.LLMFor = func(string) (engine.LLM, string, error) { return fakeLLM{}, "m", nil }

	// Phase 2: the workspace I/O runs off-thread. Execute the command and confirm it
	// hit the (fake) sandbox and returned workspaceReadyMsg.
	msg := m.prepareWorkspaceCmd(m.engCtx, m.prepCand)()
	if _, ok := msg.(workspaceReadyMsg); !ok {
		t.Fatalf("prepareWorkspaceCmd returned %T, want workspaceReadyMsg", msg)
	}
	h.mu.Lock()
	ws := h.wsSet
	h.mu.Unlock()
	if !strings.Contains(ws, "startup-test") {
		t.Errorf("SetWorkspace was not called off-thread with the engagement workspace (got %q)", ws)
	}
	if m.sess.Workspace == "" {
		t.Error("m.sess.Workspace was not set by the off-thread preparation")
	}

	// Phase 3: workspaceReadyMsg launches the orchestrator. Cancel the engagement so
	// the first (closed-port) LLM turn aborts promptly, then confirm Run returned.
	tmodel, _ = m.Update(workspaceReadyMsg{})
	m = tmodel.(*model)
	m.cancel() // abort the engagement context so the idle orchestrator unwinds
	select {
	case <-m.resultCh:
	case <-time.After(10 * time.Second):
		t.Fatal("orchestrator Run did not launch/return after workspaceReadyMsg (possible deadlock)")
	}
	// Release the host log/audit files so the temp HOME can be cleaned up.
	if m.log != nil {
		m.log.close()
	}
	if m.audit != nil {
		_ = m.audit.Close()
	}
}

// TestStartupResumeRestoreOffThread exercises the resume path's off-thread workspace
// restore against a fake prior engagement (a backup bundle) and a fake sandbox: the
// restore runs inside prepareWorkspaceCmd (copying into the sandbox), not on the UI
// goroutine, and returns workspaceReadyMsg without hanging.
func TestStartupResumeRestoreOffThread(t *testing.T) {
	t.Setenv("T_KEY", "test")
	m, _ := tempHomeModel(t)
	configureTestModels(m)

	var tm tea.Model = m
	tm, _ = tm.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	m = tm.(*model)

	// A fake prior engagement: a backup bundle to restore from, and resume settings.
	m.engagement = "prior-eng"
	writeFakeBundle(t, filepath.Join(m.store.BackupDir(), "prior-eng.tar.gz"))
	m.resume = true
	m.loadHist = true
	m.createWSL = true // a freshly created distro => restore from the host bundle
	m.prepCand = sandbox.Candidate{ID: "fake", Backend: sandbox.BackendWSL}
	m.cands = []sandbox.Candidate{m.prepCand}

	h := &fakeHandle{}
	tmodel, _ := m.beginSession(sessionPrep1{h: h})
	m = tmodel.(*model)
	m.sess.LLMFor = func(string) (engine.LLM, string, error) { return fakeLLM{}, "m", nil }

	msg := m.prepareWorkspaceCmd(m.engCtx, m.prepCand)()
	if _, ok := msg.(workspaceReadyMsg); !ok {
		t.Fatalf("prepareWorkspaceCmd returned %T, want workspaceReadyMsg", msg)
	}
	h.mu.Lock()
	copyIns, execs := h.copyIns, h.execs
	h.mu.Unlock()
	if copyIns == 0 || execs == 0 {
		t.Errorf("resume restore did not run off-thread against the sandbox (copyIns=%d execs=%d)", copyIns, execs)
	}

	tmodel, _ = m.Update(workspaceReadyMsg{})
	m = tmodel.(*model)
	m.cancel()
	select {
	case <-m.resultCh:
	case <-time.After(10 * time.Second):
		t.Fatal("orchestrator Run did not launch/return on resume (possible deadlock)")
	}
	if m.log != nil {
		m.log.close()
	}
	if m.audit != nil {
		_ = m.audit.Close()
	}
}
