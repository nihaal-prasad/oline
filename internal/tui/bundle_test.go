package tui

import (
	"archive/tar"
	"compress/gzip"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/nihaal-prasad/oline/internal/core"
	"github.com/nihaal-prasad/oline/internal/engine"
	"github.com/nihaal-prasad/oline/internal/llm"
)

// writeTestWorkspaceTar writes a gzipped tar with the given {path: {content, mtime}}
// entries, mimicking a workspace backup, and returns its path.
func writeTestWorkspaceTar(t *testing.T, files map[string]struct {
	body  string
	mtime time.Time
}) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "workspace.tar.gz")
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	for name, e := range files {
		hdr := &tar.Header{Name: name, Mode: 0o644, Size: int64(len(e.body)), ModTime: e.mtime, Typeflag: tar.TypeReg}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(e.body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestExtractReportsNewestWins checks that when the reporter ran more than once
// (an interim report, then a newer one), the MOST RECENT report of each kind is the
// one surfaced at the bundle root, and that a PDF rendering is picked up too.
func TestExtractReportsNewestWins(t *testing.T) {
	old := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)
	newer := time.Date(2026, 11, 5, 15, 0, 0, 0, time.UTC)
	type e = struct {
		body  string
		mtime time.Time
	}
	// Two REPORT.md at the workspace root (an overwrite would normally leave one, but
	// an interim could live elsewhere); newest must win. Plus a rendered PDF.
	tarPath := writeTestWorkspaceTar(t, map[string]e{
		"./REPORT.md":         {"FINAL REPORT", newer},
		"./interim/REPORT.md": {"INTERIM REPORT", old},
		"./REPORT.pdf":        {"%PDF-final", newer},
		"./recon/notes.txt":   {"not a report", newer},
	})
	got := extractReportsFromWorkspace(tarPath)
	byMember := map[string]extractedReport{}
	for _, r := range got {
		byMember[r.member] = r
	}
	if r, ok := byMember["report.md"]; !ok || string(r.data) != "FINAL REPORT" {
		t.Errorf("report.md should be the newest (FINAL REPORT), got %q (present=%v)", string(r.data), ok)
	}
	if r, ok := byMember["report.pdf"]; !ok || string(r.data) != "%PDF-final" {
		t.Errorf("report.pdf should be extracted, got %q (present=%v)", string(r.data), ok)
	}
	if len(got) != 2 {
		t.Errorf("expected exactly report.md + report.pdf, got %d entries", len(got))
	}
}

// TestExtractReportsExploiterSubmission checks the exploiter's submission/description.md
// is recognized and surfaced as report.md.
func TestExtractReportsExploiterSubmission(t *testing.T) {
	now := time.Now().UTC()
	type e = struct {
		body  string
		mtime time.Time
	}
	tarPath := writeTestWorkspaceTar(t, map[string]e{
		"submission/description.md": {"SUBMISSION", now},
	})
	got := extractReportsFromWorkspace(tarPath)
	if len(got) != 1 || got[0].member != "report.md" || string(got[0].data) != "SUBMISSION" {
		t.Errorf("exploiter submission should surface as report.md, got %+v", got)
	}
}

// TestExtractReportsNoneWhenAbsent checks a workspace with no report yields nothing.
func TestExtractReportsNoneWhenAbsent(t *testing.T) {
	type e = struct {
		body  string
		mtime time.Time
	}
	tarPath := writeTestWorkspaceTar(t, map[string]e{
		"./recon/scan.txt": {"nmap output", time.Now()},
	})
	if got := extractReportsFromWorkspace(tarPath); len(got) != 0 {
		t.Errorf("no report should be extracted when none exists, got %+v", got)
	}
}

// TestFindingsFromHistory checks that findings and the latest summary are recovered
// from saved agent histories, so a resumed engagement's export bundle still reflects
// what earlier sessions recorded.
func TestFindingsFromHistory(t *testing.T) {
	recs := []engine.AgentRecord{{
		ID: core.AgentID("recon#1"),
		History: []llm.Message{
			{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{
				{Name: "record_finding", Arguments: `{"title":"Open Redis","severity":"High","target":"10.0.0.5:6379","detail":"no auth"}`},
			}},
			{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{
				{Name: "update_summary", Arguments: `{"summary":"foothold on 10.0.0.5"}`},
			}},
			{Role: llm.RoleUser, Content: "keep going"},
		},
	}}
	fs, sum := findingsFromHistory(recs)
	if len(fs) != 1 {
		t.Fatalf("expected 1 finding, got %d (%+v)", len(fs), fs)
	}
	if fs[0].Title != "Open Redis" || fs[0].Severity != "high" || fs[0].Target != "10.0.0.5:6379" {
		t.Errorf("finding not parsed correctly: %+v", fs[0])
	}
	if fs[0].Agent != "recon#1" {
		t.Errorf("finding should be attributed to the agent, got %q", fs[0].Agent)
	}
	if sum != "foothold on 10.0.0.5" {
		t.Errorf("summary = %q, want the last update_summary", sum)
	}
}

// TestRenderFindingsMarkdown checks the bundle's findings.md is severity-ordered and
// handles the empty case.
func TestRenderFindingsMarkdown(t *testing.T) {
	if md := renderFindingsMarkdown("e1", nil); md == "" {
		t.Fatal("empty findings should still render a section")
	}
	fs := []engine.Finding{
		{Severity: "low", Title: "Verbose errors"},
		{Severity: "critical", Title: "RCE"},
	}
	md := renderFindingsMarkdown("e1", fs)
	ci, li := indexOf(md, "RCE"), indexOf(md, "Verbose errors")
	if ci < 0 || li < 0 || ci > li {
		t.Errorf("critical finding should render before the low one (ci=%d li=%d)", ci, li)
	}
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
