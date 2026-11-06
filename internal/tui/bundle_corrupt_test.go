package tui

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"testing"

	"github.com/nihaal-prasad/oline/internal/engine"
	"github.com/nihaal-prasad/oline/internal/event"
)

// goodBundleBytes builds a valid backup bundle (gzip tar) containing a nested
// workspace.tar.gz member, returned as bytes so a test can corrupt/truncate it.
func goodBundleBytes(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, body := range map[string][]byte{
		"metadata.json":    []byte(`{"engagement":"x"}`),
		"workspace.tar.gz": []byte("inner-workspace-bytes"),
	} {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(body); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// gzipOf wraps raw bytes in a gzip stream (a valid gzip whose content is NOT a tar).
func gzipOf(t *testing.T, raw []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	if _, err := gz.Write(raw); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// TestExtractWorkspaceTarCorruptInputs: every malformed bundle must return an error
// (never a partial or bogus workspace) AND must leave no leftover temp file in the
// bundle directory — the restore then degrades gracefully to an empty workspace.
func TestExtractWorkspaceTarCorruptInputs(t *testing.T) {
	good := goodBundleBytes(t)
	cases := []struct {
		name string
		data []byte
	}{
		{"empty", nil},
		{"non-gzip-garbage", []byte("this is not a gzip stream at all, just text")},
		{"gzip-of-non-tar", gzipOf(t, bytes.Repeat([]byte{0xA5}, 4096))},
		{"truncated-bundle", good[:len(good)/2]},
		{"one-byte", []byte{0x1f}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			p := filepath.Join(dir, "corrupt.tar.gz")
			if err := os.WriteFile(p, tc.data, 0o644); err != nil {
				t.Fatal(err)
			}
			got, err := extractWorkspaceTar(p)
			if err == nil {
				os.Remove(got)
				t.Fatalf("expected an error for a %s bundle, got path %q", tc.name, got)
			}
			// No half-written restore temp may survive an error path.
			leftovers, _ := filepath.Glob(filepath.Join(dir, ".oline-ws-restore-*"))
			if len(leftovers) != 0 {
				t.Errorf("%s left %d temp file(s) behind: %v", tc.name, len(leftovers), leftovers)
			}
		})
	}

	// Sanity: the uncorrupted bundle still extracts cleanly and leaves no stray temp.
	dir := t.TempDir()
	p := filepath.Join(dir, "good.tar.gz")
	if err := os.WriteFile(p, good, 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := extractWorkspaceTar(p)
	if err != nil {
		t.Fatalf("good bundle failed to extract: %v", err)
	}
	data, _ := os.ReadFile(got)
	os.Remove(got)
	if string(data) != "inner-workspace-bytes" {
		t.Errorf("extracted workspace = %q, want the inner bytes", data)
	}
}

// TestRestoreWorkspaceFromBackupFailSafe: a corrupt or missing backup bundle must
// NOT crash or block the resume — restoreWorkspaceFromBackup warns and starts with an
// empty workspace, never touching the sandbox (no CopyIn / Exec).
func TestRestoreWorkspaceFromBackupFailSafe(t *testing.T) {
	for _, tc := range []struct {
		name  string
		write bool
		data  []byte
	}{
		{"corrupt", true, []byte("not a valid gzip bundle")},
		{"missing", false, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, _ := tempHomeModel(t)
			m.events = make(chan event.Event, 16)
			h := &fakeHandle{}
			m.sess = &engine.Session{Sandbox: h, Workspace: "/opt/oline/engagements/eng"}

			if tc.write {
				if err := os.MkdirAll(m.store.BackupDir(), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(m.store.BackupDir(), "eng.tar.gz"), tc.data, 0o644); err != nil {
					t.Fatal(err)
				}
			}

			m.restoreWorkspaceFromBackup("eng") // must not panic

			h.mu.Lock()
			copyIns, execs := h.copyIns, h.execs
			h.mu.Unlock()
			if copyIns != 0 || execs != 0 {
				t.Errorf("a %s bundle must not touch the sandbox (copyIns=%d execs=%d)", tc.name, copyIns, execs)
			}

			var warned bool
			for {
				select {
				case ev := <-m.events:
					if n, ok := ev.(event.Notice); ok && n.Level == "warn" {
						warned = true
					}
					continue
				default:
				}
				break
			}
			if !warned {
				t.Errorf("a %s bundle should warn that it is starting with an empty workspace", tc.name)
			}
		})
	}
}
