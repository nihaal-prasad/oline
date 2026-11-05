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
// drives aren't present). It boots a copy-on-write overlay of a base image and
// drives it over SSH (Exec/Copy/Backup), satisfying the same sandbox.Handle
// interface as WSL. The overlay is disposable by default; with qemu.persist on it
// is kept per engagement so the engagement can be resumed (see boot / bootResume).
//
// Egress: per-agent egress is enforced with root-owned IN-GUEST iptables
// (buildEgressRules, applied over SSH), the same as WSL. On a Linux host, with
// qemu.hostFirewall on, the guest is additionally pinned to a host-owned tap whose
// traffic is filtered by host nftables (BuildHostNftRules) — a whole-guest cap the
// guest cannot change even as root. SSH stays on a user-mode (SLIRP) NIC.
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

// BootOpts carries the per-boot options the TUI selects from config: whether to
// keep the disk for resume, and whether to pin the guest behind a host tap +
// nftables (Linux only). allowInternet tells the host firewall to permit general
// internet egress for the whole guest (the roster has internet agents); the
// in-guest per-agent rules still split scope / internet / isolated.
type BootOpts struct {
	Persist       bool
	HostFirewall  bool
	AllowInternet bool
	// DiskGB, when > 0, grows the created VM's disk (the CoW overlay) to this many
	// gigabytes so tool installs don't fill a small image. The guest's cloud-init
	// grows the root filesystem to use it on first boot. 0 keeps the base image size.
	DiskGB int
	// MemMB and CPUs size the VM at boot. Each <= 0 uses the backend default
	// (DefaultMemMB / DefaultCPUs). They are not baked into the disk, so they apply to
	// a fresh boot and to a resume alike (a resumed VM can be given more/less).
	MemMB int
	CPUs  int
}

// Default VM sizing when BootOpts leaves MemMB/CPUs unset (0). The setup wizard
// seeds these same values, but they also cover the direct-start (-env) path and a
// resume, which skip the sizing steps.
const (
	DefaultMemMB = 8192 // 8 GB
	DefaultCPUs  = 4
)

// qemuResources resolves the VM's memory (MB) and vCPU count from opts, falling
// back to the package defaults when a field is unset.
func qemuResources(opts BootOpts) (memMB, cpus int) {
	memMB, cpus = opts.MemMB, opts.CPUs
	if memMB <= 0 {
		memMB = DefaultMemMB
	}
	if cpus <= 0 {
		cpus = DefaultCPUs
	}
	return memMB, cpus
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
	return q.boot(ctx, base, debianImageID, BootOpts{})
}

// Open boots an existing image (its qcow2 lives at <imagesDir>/<id>.qcow2) on a
// fresh overlay and returns a live handle.
func (q *qemuSandbox) Open(ctx context.Context, id string) (Handle, error) {
	if qemuBinary() == "" {
		return nil, qemuUnavailable()
	}
	base := q.baseImagePath(id)
	if st, err := os.Stat(base); err != nil || st.IsDir() {
		return nil, fmt.Errorf("sandbox/qemu: image %q not found at %s", id, base)
	}
	return q.boot(ctx, base, id, BootOpts{})
}

// OpenWith boots a base image with explicit boot options (persist / host
// firewall). The TUI uses it for a new QEMU engagement when qemu.persist or
// qemu.hostFirewall is set.
func (q *qemuSandbox) OpenWith(ctx context.Context, id string, opts BootOpts) (Handle, error) {
	if qemuBinary() == "" {
		return nil, qemuUnavailable()
	}
	var base string
	if id == debianImageID {
		b, err := q.ensureBaseImage(ctx)
		if err != nil {
			return nil, err
		}
		base = b
	} else {
		base = q.baseImagePath(id)
		if st, err := os.Stat(base); err != nil || st.IsDir() {
			return nil, fmt.Errorf("sandbox/qemu: image %q not found at %s", id, base)
		}
	}
	return q.boot(ctx, base, id, opts)
}

// Resume brings a persisted engagement's VM back (see bootResume). The disk and
// SSH key must still exist under the engagement's state dir.
func (q *qemuSandbox) Resume(ctx context.Context, engagement string, opts BootOpts) (Handle, error) {
	return q.bootResume(ctx, engagement, opts)
}

// QEMUControl is the QEMU backend's extra capability surface the TUI uses beyond
// the generic Sandbox interface: boot with options (persist / host firewall),
// resume a persisted engagement, and list the resumable ones. NewQEMU's result
// implements it; the TUI type-asserts for it.
type QEMUControl interface {
	OpenWith(ctx context.Context, id string, opts BootOpts) (Handle, error)
	Resume(ctx context.Context, engagement string, opts BootOpts) (Handle, error)
	ResumableEngagements() []string
	DestroyEngagement(ctx context.Context, engagement string) error
}

// Destroy removes a managed (oline-) base image file. It never touches a non-oline
// image the user supplied. Persisted engagement disks under .state are separate
// destroy targets (see DestroyEngagement / the -destroy menu) and are left intact.
func (q *qemuSandbox) Destroy(ctx context.Context, id string) error {
	if !strings.HasPrefix(id, Prefix) {
		return fmt.Errorf("sandbox/qemu: refusing to destroy %q — not an oline-managed image", id)
	}
	if err := os.Remove(q.baseImagePath(id)); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// DestroyEngagement removes a persisted engagement's QEMU state (its disk overlay
// and SSH key under .state/<engagement>). It only ever removes a directory inside
// the managed images/.state tree, so it cannot touch anything oline did not create.
func (q *qemuSandbox) DestroyEngagement(ctx context.Context, engagement string) error {
	dir := q.stateDir(engagement)
	base := filepath.Join(q.imagesDir, ".state")
	if rel, err := filepath.Rel(base, dir); err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("sandbox/qemu: refusing to remove engagement state %q — outside the managed state dir", engagement)
	}
	return os.RemoveAll(dir)
}

func qemuUnavailable() error {
	return errors.New("sandbox/qemu: qemu-system not found — install QEMU (and ensure it's on PATH or in Program Files) to use this backend")
}

// BuildQemuArgs constructs the headless launch command line.
//
// n0 is always a user-mode (SLIRP) NIC with a loopback SSH hostfwd: the exec
// transport, on 127.0.0.1 and so unaffected by any egress firewall. When tap is
// non-empty, n1 is a second NIC bound to that host tap device; agent egress is
// routed out it (the guest's default route) so host nftables can filter traffic
// the guest cannot change even as root. seedISO is the cloud-init CD-ROM on first
// boot; pass "" on resume (the disk is already provisioned).
func BuildQemuArgs(overlay, seedISO string, memMB, cpus, sshPort int, tap string) []string {
	args := []string{
		"-machine", "accel=whpx:kvm:hvf:tcg",
		"-m", strconv.Itoa(memMB),
		"-smp", strconv.Itoa(cpus),
		"-drive", "file=" + overlay + ",if=virtio,format=qcow2",
	}
	if seedISO != "" {
		args = append(args, "-drive", "file="+seedISO+",media=cdrom")
	}
	args = append(args,
		"-netdev", fmt.Sprintf("user,id=n0,hostfwd=tcp:127.0.0.1:%d-:22", sshPort),
		"-device", "virtio-net-pci,netdev=n0",
	)
	if tap != "" {
		args = append(args,
			"-netdev", fmt.Sprintf("tap,id=n1,ifname=%s,script=no,downscript=no", tap),
			"-device", "virtio-net-pci,netdev=n1",
		)
	}
	return append(args, "-display", "none", "-serial", "null")
}

// BuildHostNftRules builds the host-side nftables ruleset for the guest's tap: a
// default-drop forward chain plus NAT masquerade for the guest's /30 source, so
// every packet the guest sends off-box traverses rules root owns on the HOST and
// the guest cannot change. In-scope target CIDRs are always accepted; when
// allowInternet is set (the roster has internet agents) the rest of the internet
// is accepted too and the per-agent split is left to the in-guest iptables,
// otherwise everything but the targets is dropped. guestCIDR is the tap /30 (e.g.
// 172.16.231.0/30). The add/delete/add idiom makes the script idempotent.
func BuildHostNftRules(tap, guestCIDR string, scope Scope, allowInternet bool) []string {
	rules := []string{
		"add table inet oline",
		"delete table inet oline",
		"add table inet oline",
		"add chain inet oline fwd { type filter hook forward priority 0 ; policy drop ; }",
		fmt.Sprintf("add rule inet oline fwd iifname %q ct state established,related accept", tap),
		fmt.Sprintf("add rule inet oline fwd oifname %q ct state established,related accept", tap),
	}
	for _, c := range sanitizeCIDRs(scope.Targets) {
		rules = append(rules, fmt.Sprintf("add rule inet oline fwd iifname %q ip daddr %s accept", tap, c))
	}
	if allowInternet {
		rules = append(rules, fmt.Sprintf("add rule inet oline fwd iifname %q accept", tap))
	}
	// else: the default drop policy caps the guest to loopback + the targets.

	// NAT the guest /30 so the accepted destinations are reachable off-box.
	rules = append(rules,
		"add table ip oline_nat",
		"delete table ip oline_nat",
		"add table ip oline_nat",
		"add chain ip oline_nat post { type nat hook postrouting priority 100 ; policy accept ; }",
		fmt.Sprintf("add rule ip oline_nat post ip saddr %s oifname != %q masquerade", sanitizeOneCIDR(guestCIDR), tap),
	)
	return rules
}

// buildHostNftTeardown removes both oline host tables (idempotent).
func buildHostNftTeardown() []string {
	return []string{
		"add table inet oline", "delete table inet oline",
		"add table ip oline_nat", "delete table ip oline_nat",
	}
}
