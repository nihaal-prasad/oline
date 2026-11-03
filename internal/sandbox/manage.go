package sandbox

import (
	"archive/tar"
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// SafeEngagementName strips anything that could escape the workspace from an
// engagement name (path separators and "..") and limits it to a safe charset, so
// it can be joined into a root-run path without traversal. Returns "engagement"
// if nothing usable remains.
func SafeEngagementName(name string) string {
	name = strings.TrimSpace(name)
	if i := strings.LastIndexAny(name, `/\`); i >= 0 {
		name = name[i+1:] // drop any directory part
	}
	name = strings.ReplaceAll(name, "..", "")
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			b.WriteRune(r)
		}
	}
	s := strings.Trim(b.String(), ".")
	if s == "" {
		return "engagement"
	}
	return s
}

// WSLWorkspace is the shared, WSL-native engagement root inside a managed distro
// (the oline users' home). Engagements live at WSLWorkspace/<name>. You can also
// drop engagement files here directly, or point a resume at an existing one.
const WSLWorkspace = "/opt/oline/engagements"

// tarDirToTemp packs the CONTENTS of dir into a gzipped tar in the host's temp
// dir and returns its path. The caller copies it into the sandbox and removes
// this host-side temp file. (The tarball is transient host scaffolding, never a
// sandbox engagement artifact, so the host temp dir is fine here.)
func tarDirToTemp(dir string) (string, error) {
	f, err := os.CreateTemp("", "oline-import-*.tgz")
	if err != nil {
		return "", err
	}
	defer f.Close()
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)

	walkErr := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		hdr, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		hdr.Name = filepath.ToSlash(rel)
		if info.IsDir() {
			hdr.Name += "/"
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if info.Mode().IsRegular() {
			src, err := os.Open(path)
			if err != nil {
				return err
			}
			defer src.Close()
			if _, err := io.Copy(tw, src); err != nil {
				return err
			}
		}
		return nil
	})
	if walkErr != nil {
		_ = tw.Close()
		_ = gz.Close()
		os.Remove(f.Name())
		return "", walkErr
	}
	if err := tw.Close(); err != nil {
		return "", err
	}
	if err := gz.Close(); err != nil {
		return "", err
	}
	return f.Name(), nil
}

// isArchive reports whether a path looks like a tar.gz/tgz archive.
func isArchive(p string) bool {
	p = strings.ToLower(p)
	return strings.HasSuffix(p, ".tgz") || strings.HasSuffix(p, ".tar.gz")
}
