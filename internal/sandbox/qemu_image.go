package sandbox

import (
	"bytes"
	"context"
	"crypto/sha512"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/kdomanski/iso9660"
)

// The auto-downloaded base image: Debian 13 (trixie) generic cloud qcow2. It
// ships cloud-init, so oline's seed ISO can inject an SSH key and provisioning
// user on first boot. A user-supplied qcow2 dropped in the images dir is used
// as-is instead (it must also support cloud-init NoCloud).
const (
	debianImageURL = "https://cloud.debian.org/images/cloud/trixie/latest/debian-13-genericcloud-amd64.qcow2"
	debianSumsURL  = "https://cloud.debian.org/images/cloud/trixie/latest/SHA512SUMS"
	debianImageID  = Prefix + "debian13" // managed id; appears in Scan for reuse

	sshProvisionUser = "oline" // the sudo-capable user cloud-init creates; agents run under it via sudo -u
)

// baseImagePath returns the on-disk path of the base qcow2 for an env id.
func (q *qemuSandbox) baseImagePath(id string) string {
	return filepath.Join(q.imagesDir, id+".qcow2")
}

// runDir holds the transient per-boot overlay and seed ISO.
func (q *qemuSandbox) runDir() string { return filepath.Join(q.imagesDir, ".run") }

// State (persistent-resume) layout: each resumable engagement keeps its disk
// overlay and the SSH key it was provisioned with under .state/<engagement>/.
const (
	stateDiskName = "disk.qcow2"
	stateKeyName  = "key.pem"
)

// stateDir is where a persisted engagement's disk + key live.
func (q *qemuSandbox) stateDir(engagement string) string {
	return filepath.Join(q.imagesDir, ".state", sanitizeHostname(engagement))
}

// ResumableEngagements lists engagements with a persisted QEMU disk (sorted), so
// the TUI can offer them on the resume step.
func (q *qemuSandbox) ResumableEngagements() []string {
	entries, err := os.ReadDir(filepath.Join(q.imagesDir, ".state"))
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if st, err := os.Stat(filepath.Join(q.imagesDir, ".state", e.Name(), stateDiskName)); err == nil && !st.IsDir() {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out
}

// ensureBaseImage returns the Debian base image path, downloading and verifying
// it (against the published SHA512SUMS) on first use.
func (q *qemuSandbox) ensureBaseImage(ctx context.Context) (string, error) {
	path := q.baseImagePath(debianImageID)
	if st, err := os.Stat(path); err == nil && st.Size() > 0 {
		return path, nil
	}
	if err := os.MkdirAll(q.imagesDir, 0o755); err != nil {
		return "", err
	}
	tmp := path + ".download"
	if err := downloadFile(ctx, debianImageURL, tmp); err != nil {
		return "", fmt.Errorf("sandbox/qemu: download base image: %w", err)
	}
	// Verify against SHA512SUMS; a base image we boot MUST match the published
	// hash. A failure to fetch the sums is fatal — we never boot an unverified
	// image (and never cache one), rather than silently skipping the check.
	want, err := fetchSum(ctx, debianSumsURL, "debian-13-genericcloud-amd64.qcow2")
	if err != nil {
		os.Remove(tmp)
		return "", fmt.Errorf("sandbox/qemu: cannot verify base image (checksum fetch failed): %w", err)
	}
	got, herr := sha512File(tmp)
	if herr != nil {
		os.Remove(tmp)
		return "", herr
	}
	if !strings.EqualFold(got, want) {
		os.Remove(tmp)
		return "", fmt.Errorf("sandbox/qemu: base image checksum mismatch (got %s… want %s…)", got[:16], want[:16])
	}
	if err := os.Rename(tmp, path); err != nil {
		return "", err
	}
	return path, nil
}

// makeOverlay creates a qcow2 copy-on-write overlay backed by base, so the base
// stays pristine and each boot is disposable. Returns the overlay path.
func (q *qemuSandbox) makeOverlay(ctx context.Context, base, id string) (string, error) {
	if err := os.MkdirAll(q.runDir(), 0o755); err != nil {
		return "", err
	}
	overlay := filepath.Join(q.runDir(), fmt.Sprintf("%s-%d.qcow2", id, time.Now().UnixNano()))
	img := qemuImg()
	if img == "" {
		return "", fmt.Errorf("sandbox/qemu: qemu-img not found (needed to build the disk overlay)")
	}
	cmd := exec.CommandContext(ctx, img, "create", "-f", "qcow2",
		"-b", base, "-F", "qcow2", overlay)
	if out, err := cmd.CombinedOutput(); err != nil {
		os.Remove(overlay) // don't leave a partial overlay behind
		return "", fmt.Errorf("sandbox/qemu: qemu-img create overlay: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return overlay, nil
}

// makeSeedISO writes a cloud-init NoCloud seed ISO (volume label CIDATA) that
// creates the sudo-capable provisioning user with authLine as its key.
func (q *qemuSandbox) makeSeedISO(id, authLine string) (string, error) {
	if err := os.MkdirAll(q.runDir(), 0o755); err != nil {
		return "", err
	}
	userData := cloudInitUserData(authLine)
	metaData := "instance-id: " + id + "\nlocal-hostname: " + sanitizeHostname(id) + "\n"

	w, err := iso9660.NewWriter()
	if err != nil {
		return "", err
	}
	defer w.Cleanup()
	if err := w.AddFile(strings.NewReader(userData), "user-data"); err != nil {
		return "", err
	}
	if err := w.AddFile(strings.NewReader(metaData), "meta-data"); err != nil {
		return "", err
	}
	seed := filepath.Join(q.runDir(), fmt.Sprintf("%s-seed-%d.iso", id, time.Now().UnixNano()))
	f, err := os.Create(seed)
	if err != nil {
		return "", err
	}
	if err := w.WriteTo(f, "CIDATA"); err != nil {
		f.Close()
		os.Remove(seed) // don't leave a partial ISO behind
		return "", err
	}
	if err := f.Close(); err != nil {
		os.Remove(seed)
		return "", err
	}
	return seed, nil
}

// cloudInitUserData is the NoCloud user-data that creates the sudo-capable
// provisioning user with authLine as its only SSH key (password login off).
func cloudInitUserData(authLine string) string {
	return "#cloud-config\n" +
		"users:\n" +
		"  - name: " + sshProvisionUser + "\n" +
		"    sudo: 'ALL=(ALL) NOPASSWD:ALL'\n" +
		"    shell: /bin/bash\n" +
		"    lock_passwd: true\n" +
		"    ssh_authorized_keys:\n" +
		"      - " + authLine + "\n" +
		"ssh_pwauth: false\n"
}

// downloadFile streams url to path.
func downloadFile(ctx context.Context, url, path string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := io.Copy(f, resp.Body); err != nil {
		return err
	}
	return nil
}

// fetchSum fetches a SHA512SUMS file and returns the hex digest for filename.
func fetchSum(ctx context.Context, url, filename string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(body), "\n") {
		f := strings.Fields(line)
		if len(f) == 2 && f[1] == filename {
			return f[0], nil
		}
	}
	return "", fmt.Errorf("checksum for %q not found", filename)
}

// sha512File returns the hex SHA-512 of a file.
func sha512File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha512.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// sanitizeHostname makes an env id safe as a Linux hostname.
func sanitizeHostname(id string) string {
	var b bytes.Buffer
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	h := strings.Trim(b.String(), "-")
	if h == "" {
		return "oline"
	}
	return h
}
