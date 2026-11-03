package sandbox

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

// qemuSandbox is the cross-platform QEMU backend. Its isolation is the strongest
// available (a separate kernel — no shared-kernel escape, and the host /mnt
// drives aren't present). It boots a disposable copy-on-write overlay of a base
// image and drives it over SSH (Exec/Copy/Backup), satisfying the same
// sandbox.Handle interface as WSL.
//
// Egress: the active path enforces per-agent egress with root-owned IN-GUEST
// iptables (buildEgressRules, applied over SSH), the same as WSL. The stronger
// host-side tap + nftables filter (BuildNftRules) — which the guest cannot touch
// even as root — is implemented and unit-tested but not yet wired into the
// runtime launch path; use it on a Linux host when you need that guarantee.
type qemuSandbox struct{ imagesDir string }

// NewQEMU returns the QEMU-backed Sandbox. Images live under ~/oline/images.
func NewQEMU() Sandbox {
	home, _ := os.UserHomeDir()
	return &qemuSandbox{imagesDir: filepath.Join(home, "oline", "images")}
}

// QEMUAvailable reports whether a qemu-system binary was found (PATH or a
// well-known install dir), so the TUI can offer the QEMU backend.
func QEMUAvailable() bool { return qemuBinary() != "" }

// QEMUDebianID is the id of the auto-downloaded Debian base image; a synthetic
// "download & create" candidate carries it with NeedsCreate set.
const QEMUDebianID = debianImageID

// qemuDirs are well-known install locations searched in addition to PATH (the
// Windows QEMU installer does not add itself to PATH).
func qemuDirs() []string {
	if runtime.GOOS == "windows" {
		return []string{`C:\Program Files\qemu`, `C:\Program Files (x86)\qemu`}
	}
	return []string{"/usr/bin", "/usr/local/bin", "/opt/homebrew/bin"}
}

// findTool resolves a tool by name on PATH, then in the well-known dirs.
func findTool(names ...string) string {
	for _, n := range names {
		if p, err := exec.LookPath(n); err == nil {
			return p
		}
	}
	ext := ""
	if runtime.GOOS == "windows" {
		ext = ".exe"
	}
	for _, d := range qemuDirs() {
		for _, n := range names {
			p := filepath.Join(d, n+ext)
			if st, err := os.Stat(p); err == nil && !st.IsDir() {
				return p
			}
		}
	}
	return ""
}

// qemuBinary returns the first qemu-system binary found, or "".
func qemuBinary() string { return findTool("qemu-system-x86_64", "qemu-system-aarch64") }

// qemuImg returns the qemu-img binary (for base/overlay disks), or "".
func qemuImg() string { return findTool("qemu-img") }

// Scan lists oline QEMU images (*.qcow2 under the images dir). If qemu is not
// installed it returns no candidates (not an error) so the TUI can still offer WSL.
func (q *qemuSandbox) Scan(ctx context.Context) ([]Candidate, error) {
	if qemuBinary() == "" {
		return nil, nil
	}
	entries, err := os.ReadDir(q.imagesDir)
	if err != nil {
		return nil, nil // no images dir yet
	}
	var cands []Candidate
	for _, e := range entries {
		// A comma in the filename would inject extra -drive sub-options when the
		// launch args are built (file=<name>,if=virtio), so skip such images.
		if strings.HasSuffix(e.Name(), ".qcow2") && !strings.Contains(e.Name(), ",") {
			id := strings.TrimSuffix(e.Name(), ".qcow2")
			cands = append(cands, Candidate{ID: id, Name: id, Backend: BackendQEMU, Managed: strings.HasPrefix(id, Prefix)})
		}
	}
	return cands, nil
}

// Create downloads (if needed) and boots the managed Debian base image. A
// user-supplied qcow2 is used instead by dropping it in the images dir and
// opening it by id (see Open).
func (q *qemuSandbox) Create(ctx context.Context, spec Spec) (Handle, error) {
	if qemuBinary() == "" {
		return nil, qemuUnavailable()
	}
	base, err := q.ensureBaseImage(ctx)
	if err != nil {
		return nil, err
	}
	return q.boot(ctx, base, debianImageID)
}

// Open boots an existing image (its qcow2 lives at <imagesDir>/<id>.qcow2) on a
// disposable overlay and returns a live handle.
func (q *qemuSandbox) Open(ctx context.Context, id string) (Handle, error) {
	if qemuBinary() == "" {
		return nil, qemuUnavailable()
	}
	base := q.baseImagePath(id)
	if st, err := os.Stat(base); err != nil || st.IsDir() {
		return nil, fmt.Errorf("sandbox/qemu: image %q not found at %s", id, base)
	}
	return q.boot(ctx, base, id)
}

// Destroy removes a managed (oline-) base image and its on-disk state. It never
// touches a non-oline image the user supplied.
func (q *qemuSandbox) Destroy(ctx context.Context, id string) error {
	if !strings.HasPrefix(id, Prefix) {
		return fmt.Errorf("sandbox/qemu: refusing to destroy %q — not an oline-managed image", id)
	}
	if err := os.Remove(q.baseImagePath(id)); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func qemuUnavailable() error {
	return errors.New("sandbox/qemu: qemu-system not found — install QEMU (and ensure it's on PATH or in Program Files) to use this backend")
}

// BuildQemuArgs constructs the headless launch command line. User-mode (SLIRP)
// networking gives the guest NAT'd outbound, so per-agent egress is enforced by
// the in-guest root-owned iptables owner rules (allow/deny by destination); the
// SSH hostfwd is the exec transport, and the cloud-init seed is a CD-ROM. (On a
// Linux host you can instead pin the guest to a tap and filter with host
// nftables — BuildNftRules — for a boundary the guest cannot touch even as root.)
func BuildQemuArgs(overlay, seedISO string, memMB, cpus, sshPort int) []string {
	return []string{
		"-machine", "accel=whpx:kvm:hvf:tcg",
		"-m", strconv.Itoa(memMB),
		"-smp", strconv.Itoa(cpus),
		"-drive", "file=" + overlay + ",if=virtio,format=qcow2",
		"-drive", "file=" + seedISO + ",media=cdrom",
		"-netdev", fmt.Sprintf("user,id=n0,hostfwd=tcp:127.0.0.1:%d-:22", sshPort),
		"-device", "virtio-net-pci,netdev=n0",
		"-display", "none",
		"-serial", "null",
	}
}

// BuildNftRules builds host nftables rules for the guest's tap interface: a
// default-deny OUTPUT with ACCEPT only for the in-scope CIDRs. Applied by root
// on the HOST, so nothing inside the guest can change them. (The internet-minus-
// target path for the researcher is a second ruleset on a second tap/source.)
func BuildNftRules(tap string, scope Scope) []string {
	rules := []string{
		fmt.Sprintf("add table inet oline"),
		fmt.Sprintf("add chain inet oline fwd { type filter hook forward priority 0 ; policy drop ; }"),
		fmt.Sprintf("add rule inet oline fwd iifname %q ct state established,related accept", tap),
	}
	for _, c := range sanitizeCIDRs(scope.Targets) {
		rules = append(rules, fmt.Sprintf("add rule inet oline fwd iifname %q ip daddr %s accept", tap, c))
	}
	rules = append(rules, fmt.Sprintf("add rule inet oline fwd iifname %q drop", tap))
	return rules
}
