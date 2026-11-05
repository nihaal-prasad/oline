package tui

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/nihaal-prasad/oline/internal/audit"
	"github.com/nihaal-prasad/oline/internal/config"
	"github.com/nihaal-prasad/oline/internal/engine"
	"github.com/nihaal-prasad/oline/internal/llm"
)

// Version is the build version string, set from main at startup so it can be
// recorded in the engagement export bundle's metadata.
var Version string

// The engagement export bundle.
//
// Each host backup is a single self-contained .tar.gz per engagement, holding
// everything needed to review, resume, or hand off the engagement — the sandbox
// workspace (artifacts/loot), the conversation transcripts, the activity log, the
// tamper-evident audit log, the structured findings, the latest summary, the
// reporter's report when one has been produced, and engagement metadata. Items that
// do not exist yet (e.g. the report, before the reporter has run) are simply
// omitted. The bundle is the single source for the "export bundle" idea, produced
// automatically on the backup timer and at engagement end.

// auditPath is the host path of an engagement's audit log, sanitized like the
// activity log and history so an illegal engagement name can't break the save.
func auditPath(engagement string) string {
	return filepath.Join(config.AuditDir(), safeFileName(engagement)+".audit.jsonl")
}

// AuditPath returns the host path of an engagement's audit log. Exported so the
// `-audit-verify` CLI resolves the same path the running engagement writes to.
func AuditPath(engagement string) string { return auditPath(engagement) }

// openAuditLog opens (or extends, on resume) the tamper-evident command audit log
// for an engagement. Returns nil on any failure (logging then no-ops).
func openAuditLog(engagement string) *audit.Log {
	if strings.TrimSpace(engagement) == "" {
		return nil
	}
	l, err := audit.Open(auditPath(engagement), engagement)
	if err != nil {
		return nil
	}
	return l
}

// auditUser records one command the OPERATOR ran (in a shell/terminal pane) to the
// audit log. Nil-safe via the log.
func (m *model) auditUser(action, target, detail string) {
	m.audit.Command("User", action, target, detail)
}

// bundleMeta is the engagement metadata written into the bundle.
type bundleMeta struct {
	Engagement string    `json:"engagement"`
	Mode       string    `json:"mode"`
	Scope      string    `json:"scope,omitempty"`
	Goal       string    `json:"goal,omitempty"`
	EnvID      string    `json:"env,omitempty"`
	CreatedAt  time.Time `json:"createdAt"`
	Findings   int       `json:"findings"`
	Reports    []string  `json:"reports,omitempty"` // report files surfaced at the bundle root (if any)
	OlineVer   string    `json:"olineVersion,omitempty"`
}

// buildExportBundle writes the engagement's full export bundle to dest atomically
// (temp + rename). It is called with agent writes frozen (see runBackup), so the
// sandbox workspace is captured consistently.
func (m *model) buildExportBundle(engagement, dest string) error {
	if m.sess == nil || m.sess.Sandbox == nil {
		return fmt.Errorf("no sandbox attached")
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}

	// The sandbox workspace (artifacts/loot) is tarred out of the guest to a temp
	// file, then embedded in the bundle as workspace.tar.gz.
	wsTmp, err := os.CreateTemp(filepath.Dir(dest), ".oline-ws-*.tgz")
	if err != nil {
		return err
	}
	wsName := wsTmp.Name()
	_ = wsTmp.Close()
	defer os.Remove(wsName)
	workspaceOK := true
	if err := m.sess.Sandbox.Backup(context.Background(), engagement, wsName); err != nil {
		// A missing or unreadable workspace shouldn't abort the whole bundle — the
		// host-side transcripts/findings/logs are still worth capturing. Note and go on.
		workspaceOK = false
		m.emitNotice("warn", "backup: workspace capture failed ("+err.Error()+"); bundling host-side state only")
	}

	tmp, err := os.CreateTemp(filepath.Dir(dest), ".oline-bundle-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	gz := gzip.NewWriter(tmp)
	tw := tar.NewWriter(gz)

	add := func(name string, data []byte) error {
		hdr := &tar.Header{Name: name, Mode: 0o600, Size: int64(len(data)), ModTime: time.Now()}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		_, err := tw.Write(data)
		return err
	}
	addFile := func(name, src string) {
		if src == "" {
			return
		}
		data, err := os.ReadFile(src)
		if err != nil {
			return // a not-yet-produced or unreadable file is simply left out
		}
		_ = add(name, data)
	}

	// 1. Sandbox workspace (artifacts/loot), and the reporter's report surfaced at the
	// bundle root when one exists (most recent of each kind — see extractReports…).
	var reportNames []string
	if workspaceOK {
		addFile("workspace.tar.gz", wsName)
		for _, r := range extractReportsFromWorkspace(wsName) {
			if add(r.member, r.data) == nil {
				reportNames = append(reportNames, r.member)
			}
		}
		sort.Strings(reportNames)
	}
	// 2. Conversation transcripts, 3. activity log, 4. tamper-evident audit log.
	addFile("history.json", historyPath(engagement))
	addFile("activity.log", m.log.Path())
	addFile("audit.jsonl", m.audit.Path())

	// 5. Structured findings (JSON + a readable Markdown rendering).
	findings := m.sess.Findings()
	if data, err := json.MarshalIndent(findings, "", "  "); err == nil {
		_ = add("findings.json", data)
	}
	_ = add("findings.md", []byte(renderFindingsMarkdown(engagement, findings)))

	// 6. Latest engagement summary, if any.
	if s := strings.TrimSpace(m.sess.Summary()); s != "" {
		_ = add("summary.md", []byte("# Engagement summary — "+engagement+"\n\n"+s+"\n"))
	}
	// 7. Metadata.
	meta := bundleMeta{
		Engagement: engagement,
		Mode:       string(m.mode),
		Scope:      strings.TrimSpace(m.scopeIn.Value()),
		Goal:       strings.TrimSpace(m.goalIn.Value()),
		EnvID:      m.envID,
		CreatedAt:  time.Now(),
		Findings:   len(findings),
		Reports:    reportNames,
		OlineVer:   Version,
	}
	if data, err := json.MarshalIndent(meta, "", "  "); err == nil {
		_ = add("metadata.json", data)
	}

	if err := tw.Close(); err != nil {
		_ = gz.Close()
		_ = tmp.Close()
		return err
	}
	if err := gz.Close(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, dest)
}

// extractWorkspaceTar pulls the nested workspace.tar.gz member out of an engagement's
// host backup bundle (backups/<engagement>.tar.gz) to a temp file on the host,
// returning its path (the caller removes it). It is the inverse of the workspace
// capture in buildExportBundle, used to seed a brand-new sandbox with a resumed
// engagement's prior workspace. Errors if the bundle or the workspace member is
// missing (e.g. backups were disabled, or no backup ran before the engagement ended).
func extractWorkspaceTar(bundlePath string) (string, error) {
	f, err := os.Open(bundlePath)
	if err != nil {
		return "", err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return "", err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", err
		}
		if hdr.Typeflag != tar.TypeReg || path.Base(path.Clean(hdr.Name)) != "workspace.tar.gz" {
			continue
		}
		out, err := os.CreateTemp(filepath.Dir(bundlePath), ".oline-ws-restore-*.tgz")
		if err != nil {
			return "", err
		}
		if _, err := io.Copy(out, tr); err != nil {
			_ = out.Close()
			_ = os.Remove(out.Name())
			return "", err
		}
		if err := out.Close(); err != nil {
			_ = os.Remove(out.Name())
			return "", err
		}
		return out.Name(), nil
	}
	return "", fmt.Errorf("no workspace captured in the backup bundle")
}

// renderFindingsMarkdown renders the findings as a severity-ordered report section
// for the bundle (the start of the eventual full report).
func renderFindingsMarkdown(engagement string, fs []engine.Finding) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Findings — %s\n\n", engagement)
	if len(fs) == 0 {
		b.WriteString("_No findings recorded yet._\n")
		return b.String()
	}
	for _, want := range []string{"critical", "high", "medium", "low", "info"} {
		for _, f := range fs {
			if strings.ToLower(f.Severity) != want {
				continue
			}
			fmt.Fprintf(&b, "## [%s] %s\n", strings.ToUpper(f.Severity), f.Title)
			if f.Target != "" {
				fmt.Fprintf(&b, "- **Target:** %s\n", f.Target)
			}
			if f.Agent != "" {
				fmt.Fprintf(&b, "- **Found by:** %s\n", f.Agent)
			}
			if !f.Time.IsZero() {
				fmt.Fprintf(&b, "- **When:** %s\n", f.Time.Format(time.RFC3339))
			}
			if f.Detail != "" {
				fmt.Fprintf(&b, "\n%s\n", f.Detail)
			}
			b.WriteByte('\n')
		}
	}
	return b.String()
}

// reportBasenames are the report artifacts the reporter subagents produce by
// default: pentester writes REPORT.md/.pdf at the workspace root; exploiter writes
// submission/description.md/.pdf. A matching file, when present, is surfaced at the
// bundle root (as report.md / report.pdf) so the report is easy to find without
// unpacking workspace.tar.gz. A report saved under a custom name is not surfaced
// here but is still inside workspace.tar.gz.
var reportBasenames = map[string]bool{
	"report.md": true, "report.pdf": true,
	"description.md": true, "description.pdf": true,
}

const maxReportBytes = 32 << 20 // 32 MiB cap per extracted report file

// extractedReport is one report artifact pulled from the workspace tar.
type extractedReport struct {
	member  string // bundle member name: report.md or report.pdf
	source  string // original path inside the workspace
	modTime time.Time
	data    []byte
}

// extractReportsFromWorkspace scans the workspace tar.gz for the reporter's output
// and returns the MOST RECENT report of each kind (Markdown and PDF), by the file's
// modification time recorded in the tar. The reporter can be run more than once (an
// interim report, then a final one); this always picks the latest, so the bundle's
// report.md/report.pdf are never a stale earlier draft.
func extractReportsFromWorkspace(tarGzPath string) []extractedReport {
	f, err := os.Open(tarGzPath)
	if err != nil {
		return nil
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return nil
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	best := map[string]extractedReport{} // keyed by member (report.md / report.pdf)
	for {
		hdr, err := tr.Next()
		if err != nil {
			break // EOF or a malformed archive: return whatever we found so far
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		clean := path.Clean(strings.TrimPrefix(hdr.Name, "./"))
		base := strings.ToLower(path.Base(clean))
		if !reportBasenames[base] {
			continue
		}
		member := "report" + path.Ext(base) // report.md or report.pdf
		// Keep only the newest of each kind; an older or equal-dated entry is skipped
		// without reading its body (tar advances past it on the next Next()).
		if cur, ok := best[member]; ok && !hdr.ModTime.After(cur.modTime) {
			continue
		}
		data, err := io.ReadAll(io.LimitReader(tr, maxReportBytes))
		if err != nil {
			continue
		}
		best[member] = extractedReport{member: member, source: clean, modTime: hdr.ModTime, data: data}
	}
	out := make([]extractedReport, 0, len(best))
	for _, r := range best {
		out = append(out, r)
	}
	return out
}

// findingsFromHistory extracts confirmed findings and the latest summary from saved
// agent histories (record_finding / update_summary tool calls), so a resumed
// engagement's bundle still reflects what earlier sessions found. Returns the
// findings and the last summary seen.
func findingsFromHistory(recs []engine.AgentRecord) ([]engine.Finding, string) {
	var out []engine.Finding
	summary := ""
	for _, r := range recs {
		for _, msg := range r.History {
			if msg.Role != llm.RoleAssistant {
				continue
			}
			for _, c := range msg.ToolCalls {
				switch c.Name {
				case "record_finding":
					var a struct {
						Title, Severity, Target, Detail string
					}
					if json.Unmarshal([]byte(c.Arguments), &a) == nil && strings.TrimSpace(a.Title) != "" {
						out = append(out, engine.Finding{
							Agent:    string(r.ID),
							Severity: strings.ToLower(strings.TrimSpace(a.Severity)),
							Title:    strings.TrimSpace(a.Title),
							Target:   strings.TrimSpace(a.Target),
							Detail:   strings.TrimSpace(a.Detail),
						})
					}
				case "update_summary":
					var a struct{ Summary string }
					if json.Unmarshal([]byte(c.Arguments), &a) == nil && strings.TrimSpace(a.Summary) != "" {
						summary = strings.TrimSpace(a.Summary)
					}
				}
			}
		}
	}
	return out, summary
}
