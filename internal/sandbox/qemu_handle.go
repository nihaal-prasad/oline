package sandbox

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/nihaal-prasad/oline/internal/core"
)

// bootTimeout bounds how long we wait for the VM to boot and SSH to come up
// (first boot runs cloud-init, which can take a while).
const bootTimeout = 3 * time.Minute

// qemuHandle is a live QEMU VM. Commands run over SSH: as the sudo-capable
// provisioning user before egress is set up, and afterward as the per-policy
// unprivileged oline user (via sudo -u), so the in-guest owner-match firewall
// confines each agent exactly as on WSL.
type qemuHandle struct {
	id      string
	overlay string // CoW disk; removed on Shutdown unless persist is set (then kept for resume)
	seedISO string // cloud-init seed (removed on Shutdown; empty on a resumed boot)
	sshPort int
	signer  ssh.Signer
	cmd     *exec.Cmd // the qemu-system process
	client  *ssh.Client
	egress  EgressController
	workdir string // default guest cwd for agent commands (the engagement workspace)

	imagesDir     string  // host images root, for persisting the disk on Shutdown
	persist       bool    // keep the disk (and SSH key) for resume instead of discarding
	keyPEM        []byte  // the VM's SSH private key, persisted so resume can reuse it
	hostNet       hostNet // host tap + nftables boundary (Linux, qemu.hostFirewall); nil otherwise
	allowInternet bool    // host firewall: permit general internet egress for the guest
}

func (h *qemuHandle) Info() EnvInfo { return EnvInfo{ID: h.id, Backend: BackendQEMU} }

// execUser resolves which guest user a command runs as. Empty means "run as the
// provisioning user without sudo" (only before SetupEgress, e.g. dev/test).
func (h *qemuHandle) execUser(policy EgressPolicy) string {
	if h.egress != nil {
		if np, err := h.egress.PathFor("", policy); err == nil {
			return np.User
		}
	}
	return ""
}

// wrap builds the remote command: the agent's script, run in the engagement
// workspace, as the policy's guest user.
func (h *qemuHandle) wrap(cmd Cmd) string {
	dir := cmd.Dir
	if dir == "" {
		dir = h.workdir // the engagement workspace, once set
	}
	if dir == "" {
		dir = "/opt/oline"
	}
	var script string
	if len(cmd.Argv) == 3 && cmd.Argv[0] == "bash" && cmd.Argv[1] == "-lc" {
		script = cmd.Argv[2]
	} else {
		script = sshJoin(cmd.Argv)
	}
	// Fail loudly if the workspace dir is missing rather than silently running in
	// the wrong directory (which would scatter relative-path artifacts).
	full := "cd " + sshQuote(dir) + " || exit 1; " + script
	run := "bash -lc " + sshQuote(full)
	switch u := h.execUser(cmd.Policy); u {
	case "":
		return run
	case "root":
		return "sudo -n " + run
	default:
		return "sudo -n -u " + u + " " + run
	}
}

func (h *qemuHandle) Exec(ctx context.Context, agent core.AgentID, cmd Cmd, out io.Writer) (ExitStatus, error) {
	if len(cmd.Argv) == 0 {
		return ExitStatus{}, errors.New("sandbox/qemu: empty command")
	}
	code, err := runSSHCombined(ctx, h.client, h.wrap(cmd), nil, out)
	if err != nil {
		if ctx.Err() != nil {
			return ExitStatus{Code: code}, ctx.Err()
		}
		return ExitStatus{Code: code}, fmt.Errorf("sandbox/qemu: exec: %w", err)
	}
	return ExitStatus{Code: code}, nil
}

// OpenTerminal starts cmd under a pseudo-terminal over SSH (RequestPty), so
// full-screen programs work exactly as in a real terminal. Output streams to out;
// the returned session carries the operator's keystrokes to the child's stdin and
// can resize the PTY live (SSH WindowChange).
func (h *qemuHandle) OpenTerminal(ctx context.Context, agent core.AgentID, cmd Cmd, rows, cols int, out io.Writer) (PTYSession, error) {
	if len(cmd.Argv) == 0 {
		return nil, errors.New("sandbox/qemu: empty terminal command")
	}
	if rows <= 0 {
		rows = 24
	}
	if cols <= 0 {
		cols = 80
	}
	sess, err := h.client.NewSession()
	if err != nil {
		return nil, fmt.Errorf("sandbox/qemu: open terminal: %w", err)
	}
	// A normal cooked PTY, exactly like a real terminal: the guest line discipline
	// echoes typed input and does line editing (ECHO + ICANON on), and the TUI's VT
	// emulator renders the result. Full-screen programs (vim, htop, gdb -tui) switch
	// the PTY to raw mode themselves. ISIG on so Ctrl-C reaches programs as SIGINT.
	// (Without ECHO here the operator sees nothing they type — the emulator does not
	// echo locally.)
	modes := ssh.TerminalModes{
		ssh.ECHO:          1,
		ssh.ICANON:        1,
		ssh.ISIG:          1,
		ssh.TTY_OP_ISPEED: 115200,
		ssh.TTY_OP_OSPEED: 115200,
	}
	if err := sess.RequestPty("xterm-256color", rows, cols, modes); err != nil {
		sess.Close()
		return nil, fmt.Errorf("sandbox/qemu: request pty: %w", err)
	}
	stdin, err := sess.StdinPipe()
	if err != nil {
		sess.Close()
		return nil, fmt.Errorf("sandbox/qemu: terminal stdin: %w", err)
	}
	sess.Stdout = out
	sess.Stderr = out
	if err := sess.Start(h.wrap(cmd)); err != nil {
		sess.Close()
		return nil, fmt.Errorf("sandbox/qemu: start terminal: %w", err)
	}
	p := &sshPTY{sess: sess, stdin: stdin}
	go func() { // reap the session and close it when the caller's ctx ends
		done := make(chan struct{})
		go func() { _ = sess.Wait(); close(done) }()
		select {
		case <-ctx.Done():
		case <-done:
		}
		p.Close()
	}()
	return p, nil
}

// sshPTY is a PTYSession backed by one SSH session with a PTY attached.
type sshPTY struct {
	sess  *ssh.Session
	stdin io.WriteCloser
}

func (p *sshPTY) Write(b []byte) (int, error) { return p.stdin.Write(b) }

func (p *sshPTY) Resize(rows, cols int) error {
	if rows <= 0 || cols <= 0 {
		return nil
	}
	return p.sess.WindowChange(rows, cols)
}

func (p *sshPTY) Close() error {
	_ = p.stdin.Close()
	return p.sess.Close()
}

func (h *qemuHandle) CopyIn(ctx context.Context, localPath, guestPath string) error {
	f, err := os.Open(localPath)
	if err != nil {
		return err
	}
	defer f.Close()
	inner := `mkdir -p "$(dirname -- ` + sshQuote(guestPath) + `)" && cat > ` + sshQuote(guestPath)
	var out bytes.Buffer
	code, err := runSSHCombined(ctx, h.client, "sudo -n bash -lc "+sshQuote(inner), f, &out)
	if err != nil || code != 0 {
		return fmt.Errorf("sandbox/qemu: CopyIn: %v: %s", err, out.String())
	}
	return nil
}

func (h *qemuHandle) CopyOut(ctx context.Context, guestPath, localPath string) error {
	// Stream to a temp file and rename on success, so a dropped connection or a
	// mid-stream error never leaves a truncated file at localPath. stderr goes to
	// a separate buffer so it can't corrupt the copied bytes.
	tmp, err := os.CreateTemp(filepath.Dir(localPath), ".oline-copy-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	inner := "cat -- " + sshQuote(guestPath)
	var errBuf bytes.Buffer
	code, rerr := runSSH(ctx, h.client, "sudo -n bash -lc "+sshQuote(inner), nil, tmp, &errBuf)
	if cerr := tmp.Close(); cerr != nil && rerr == nil {
		rerr = cerr
	}
	if rerr != nil || code != 0 {
		return fmt.Errorf("sandbox/qemu: CopyOut: %v (exit %d): %s", rerr, code, strings.TrimSpace(errBuf.String()))
	}
	return os.Rename(tmpName, localPath)
}

// Backup streams a gzipped tar of the engagement workspace out of the guest to
// destPath on the host, written atomically (temp + rename) so there is at most
// one backup file per engagement.
func (h *qemuHandle) Backup(ctx context.Context, engagement, destPath string) error {
	if err := os.MkdirAll(filepath.Dir(destPath), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(destPath), ".oline-bak-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	guestDir := WSLWorkspace + "/" + SafeEngagementName(engagement) // same canonical name as the workspace/backups
	// --warning flags quiet tar's benign notes so they don't look like failures:
	// IPC sockets/fifos in the workspace (which tar can't archive and skips) and
	// files the live agents change mid-read. Those still make tar exit 1; see
	// tarExitIsFatal for why that is kept rather than discarded.
	inner := "tar --warning=no-file-ignored --warning=no-file-changed -czf - -C " + sshQuote(guestDir) + " ."
	// stderr to a separate buffer: tar's warnings must not land inside the .tar.gz.
	var errBuf bytes.Buffer
	code, rerr := runSSH(ctx, h.client, "sudo -n bash -lc "+sshQuote(inner), nil, tmp, &errBuf)
	if cerr := tmp.Close(); cerr != nil && rerr == nil {
		rerr = cerr
	}
	if rerr != nil || tarExitIsFatal(code) {
		return fmt.Errorf("sandbox/qemu: backup: %v (exit %d): %s", rerr, code, strings.TrimSpace(errBuf.String()))
	}
	return os.Rename(tmpName, destPath)
}

// tarExitIsFatal reports whether a tar exit code means the archive is unusable.
// GNU tar exits 1 for non-fatal differences — files that changed while being read
// (expected on a live workspace the agents are still writing to) or members it
// skipped such as sockets and fifos — and still writes a complete, valid archive,
// so exit 1 is kept. Only exit 2 and up (tar aborted, leaving a truncated stream)
// is fatal; our own precondition scripts likewise signal real failures with exit 2.
func tarExitIsFatal(code int) bool { return code != 0 && code != 1 }

func (h *qemuHandle) EgressController() EgressController {
	if h.egress != nil {
		return h.egress
	}
	return noopEgress{}
}

// SetupEgress provisions the per-policy unprivileged users and root-owned
// firewall rules inside the VM (over SSH), then routes Exec through them. When a
// host tap boundary is active (qemu.hostFirewall on Linux), it also applies the
// host-side nftables scope rules and wraps both so a live scope change updates
// each.
func (h *qemuHandle) SetupEgress(ctx context.Context, scope Scope) error {
	e := newSSHEgress(h)
	if err := e.ensureUsers(ctx); err != nil {
		return err
	}
	if err := e.ApplyBaseline(scope); err != nil {
		return err
	}
	if h.hostNet != nil {
		if err := h.hostNet.applyScope(scope, h.allowInternet); err != nil {
			return fmt.Errorf("sandbox/qemu: host firewall: %w", err)
		}
		h.egress = &compositeEgress{in: e, host: h.hostNet, allowInternet: h.allowInternet}
	} else {
		h.egress = e
	}
	return nil
}

// SetWorkspace creates the engagement workspace (group-writable by the oline users)
// and makes it the default working directory for later agent commands. It runs the
// creation from an existing directory (/opt/oline) so the command does not depend on
// the workspace it is about to create.
func (h *qemuHandle) SetWorkspace(ctx context.Context, dir string) error {
	script := "mkdir -p " + sshQuote(dir) + " && chmod 2775 " + sshQuote(dir)
	if _, err := h.Exec(ctx, "setup", Cmd{Argv: []string{"bash", "-lc", script}, Dir: "/opt/oline"}, io.Discard); err != nil {
		return err
	}
	h.workdir = dir
	return nil
}

var _ WorkspaceSetter = (*qemuHandle)(nil)
var _ Interactive = (*qemuHandle)(nil)

// Shutdown powers the VM off (best-effort graceful), kills the process if it
// lingers, tears down the host tap boundary (if any), and either keeps the disk
// for resume (persist) or removes it. The base/user image is always left intact.
func (h *qemuHandle) Shutdown(ctx context.Context) error {
	if h.client != nil {
		var b bytes.Buffer
		poweroffCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		_, _ = runSSHCombined(poweroffCtx, h.client, "sudo -n poweroff", nil, &b)
		cancel()
		_ = h.client.Close()
	}
	if h.cmd != nil && h.cmd.Process != nil {
		done := make(chan struct{})
		go func() { _, _ = h.cmd.Process.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(8 * time.Second):
			_ = h.cmd.Process.Kill()
			<-done
		}
	}
	// Remove the host tap + nftables now that the VM is down (nothing to filter).
	if h.hostNet != nil {
		_ = h.hostNet.teardown()
	}
	// Persist the disk (and key) for resume on a clean shutdown; otherwise discard.
	if h.persist {
		if err := h.persistDisk(); err == nil {
			if h.seedISO != "" {
				os.Remove(h.seedISO) // the seed is first-boot only; a resume doesn't use it
			}
			return nil
		}
		// fall through to cleanup if persisting failed (don't leak the overlay)
	}
	if h.overlay != "" {
		os.Remove(h.overlay)
	}
	if h.seedISO != "" {
		os.Remove(h.seedISO)
	}
	return nil
}

// persistDisk moves the VM's overlay and SSH key under the engagement's state
// directory so bootResume can bring it back. The engagement is taken from the
// workspace path SetWorkspace recorded.
func (h *qemuHandle) persistDisk() error {
	if h.overlay == "" || h.imagesDir == "" {
		return fmt.Errorf("sandbox/qemu: cannot persist: no overlay/images dir")
	}
	eng := filepath.Base(h.workdir)
	if h.workdir == "" || eng == "." || eng == string(filepath.Separator) {
		return fmt.Errorf("sandbox/qemu: cannot persist: engagement workspace unknown")
	}
	dir := filepath.Join(h.imagesDir, ".state", SafeEngagementName(eng))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	disk := filepath.Join(dir, stateDiskName)
	if h.overlay != disk { // first persist: move the overlay into place (already there on resume)
		if err := os.Rename(h.overlay, disk); err != nil {
			return err
		}
		h.overlay = disk
	}
	if len(h.keyPEM) > 0 {
		if err := os.WriteFile(filepath.Join(dir, stateKeyName), h.keyPEM, 0o600); err != nil {
			return err
		}
	}
	return nil
}

// sshEgress supplies the SSH transport for the shared egressCore.
type sshEgress struct {
	*egressCore
	h *qemuHandle
}

func newSSHEgress(h *qemuHandle) *sshEgress {
	e := &sshEgress{h: h}
	e.egressCore = newEgressCore(e.rootExec)
	return e
}

func (e *sshEgress) rootExec(ctx context.Context, script string) (string, error) {
	var buf bytes.Buffer
	code, err := runSSHCombined(ctx, e.h.client, "sudo -n bash -s", strings.NewReader(script), &buf)
	if err != nil {
		return buf.String(), err
	}
	if code != 0 {
		return buf.String(), fmt.Errorf("exit %d", code)
	}
	return buf.String(), nil
}

// waitCloudInit blocks until cloud-init has finished its run (so the provisioning
// user and any packages are ready). Best-effort and bounded by ctx; an image
// without cloud-init returns immediately.
func waitCloudInit(ctx context.Context, client *ssh.Client) {
	var b bytes.Buffer
	_, _ = runSSHCombined(ctx, client,
		"command -v cloud-init >/dev/null 2>&1 && sudo -n cloud-init status --wait >/dev/null 2>&1 || true",
		nil, &b)
}

// boot creates a CoW overlay + cloud-init seed on top of base, launches the VM
// headless (with a host tap when opts.HostFirewall), and waits for SSH. The
// returned handle owns the VM; it persists its disk on Shutdown when opts.Persist.
func (q *qemuSandbox) boot(ctx context.Context, base, id string, opts BootOpts) (Handle, error) {
	if qemuBinary() == "" {
		return nil, qemuUnavailable()
	}
	signer, authLine, keyPEM, err := genSSHKey()
	if err != nil {
		return nil, err
	}
	seed, err := q.makeSeedISO(id, authLine)
	if err != nil {
		return nil, err
	}
	overlay, err := q.makeOverlay(ctx, base, id)
	if err != nil {
		os.Remove(seed)
		return nil, err
	}
	// Grow the writable overlay to the requested size so tool installs don't fill a
	// small base image; the guest's cloud-init grows the root filesystem to match on
	// first boot. The base image stays pristine. 0 = keep the base size.
	if err := q.resizeOverlay(ctx, overlay, opts.DiskGB); err != nil {
		os.Remove(seed)
		os.Remove(overlay)
		return nil, err
	}
	hn, tap, err := maybeHostNet(opts)
	if err != nil {
		os.Remove(seed)
		os.Remove(overlay)
		return nil, err
	}
	port, err := freePort()
	if err != nil {
		cleanupBoot(hn, seed, overlay)
		return nil, err
	}

	memMB, cpus := qemuResources(opts)
	cmd := exec.Command(qemuBinary(), BuildQemuArgs(overlay, seed, memMB, cpus, port, tap)...)
	if err := cmd.Start(); err != nil {
		cleanupBoot(hn, seed, overlay)
		return nil, fmt.Errorf("sandbox/qemu: launch: %w", err)
	}

	// Derive the boot-wait deadline from the caller's ctx so an abort (user quit)
	// stops waiting instead of hanging for the full bootTimeout.
	dialCtx, cancel := context.WithTimeout(ctx, bootTimeout)
	defer cancel()
	client, err := dialSSH(dialCtx, port, sshConfig(sshProvisionUser, signer))
	if err != nil {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait() // reap so we don't leak a zombie / process handle
		cleanupBoot(hn, seed, overlay)
		return nil, fmt.Errorf("sandbox/qemu: %w", err)
	}
	// SSH comes up mid-boot; wait for cloud-init to finish provisioning (user,
	// packages) so egress setup doesn't race a half-configured guest.
	waitCloudInit(dialCtx, client)
	if hn != nil {
		if err := configureGuestTap(dialCtx, client, hn); err != nil {
			_ = client.Close()
			_ = cmd.Process.Kill()
			_, _ = cmd.Process.Wait()
			cleanupBoot(hn, seed, overlay)
			return nil, err
		}
	}
	return &qemuHandle{
		id: id, overlay: overlay, seedISO: seed, sshPort: port,
		signer: signer, cmd: cmd, client: client,
		imagesDir: q.imagesDir, persist: opts.Persist, keyPEM: keyPEM,
		hostNet: hn, allowInternet: opts.AllowInternet,
	}, nil
}

// bootResume brings a persisted engagement back: it boots the kept disk (no seed;
// cloud-init already ran) with the SSH key the VM was provisioned with, so the
// workspace and installed tooling return intact. Re-establishes the host tap when
// opts.HostFirewall is set.
func (q *qemuSandbox) bootResume(ctx context.Context, engagement string, opts BootOpts) (Handle, error) {
	if qemuBinary() == "" {
		return nil, qemuUnavailable()
	}
	dir := q.stateDir(engagement)
	disk := filepath.Join(dir, stateDiskName)
	if st, err := os.Stat(disk); err != nil || st.IsDir() {
		return nil, fmt.Errorf("sandbox/qemu: no resumable disk for %q at %s", engagement, disk)
	}
	keyPEM, err := os.ReadFile(filepath.Join(dir, stateKeyName))
	if err != nil {
		return nil, fmt.Errorf("sandbox/qemu: resume key: %w", err)
	}
	signer, err := loadSSHKey(keyPEM)
	if err != nil {
		return nil, fmt.Errorf("sandbox/qemu: resume key: %w", err)
	}
	hn, tap, err := maybeHostNet(opts)
	if err != nil {
		return nil, err
	}
	port, err := freePort()
	if err != nil {
		cleanupBoot(hn, "", "")
		return nil, err
	}
	// MemMB/CPUs apply to this boot (opts may differ from the first boot); DiskGB is
	// deliberately NOT honored on resume — the disk keeps the size it was created with,
	// and cloud-init's growpart/resize_rootfs only runs on first boot, so re-growing
	// the qcow2 now would not expand the guest filesystem anyway.
	memMB, cpus := qemuResources(opts)
	cmd := exec.Command(qemuBinary(), BuildQemuArgs(disk, "", memMB, cpus, port, tap)...)
	if err := cmd.Start(); err != nil {
		cleanupBoot(hn, "", "")
		return nil, fmt.Errorf("sandbox/qemu: launch: %w", err)
	}
	dialCtx, cancel := context.WithTimeout(ctx, bootTimeout)
	defer cancel()
	client, err := dialSSH(dialCtx, port, sshConfig(sshProvisionUser, signer))
	if err != nil {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
		cleanupBoot(hn, "", "")
		return nil, fmt.Errorf("sandbox/qemu: %w", err)
	}
	if hn != nil {
		if err := configureGuestTap(dialCtx, client, hn); err != nil {
			_ = client.Close()
			_ = cmd.Process.Kill()
			_, _ = cmd.Process.Wait()
			cleanupBoot(hn, "", "")
			return nil, err
		}
	}
	return &qemuHandle{
		id: engagement, overlay: disk, seedISO: "", sshPort: port,
		signer: signer, cmd: cmd, client: client,
		imagesDir: q.imagesDir, persist: true, keyPEM: keyPEM,
		hostNet: hn, allowInternet: opts.AllowInternet,
		workdir: WSLWorkspace + "/" + SafeEngagementName(engagement),
	}, nil
}

// maybeHostNet creates the host tap boundary when opts.HostFirewall is set
// (Linux only), returning the controller and the tap device name for the launch
// args. With the flag off it returns (nil, "", nil).
func maybeHostNet(opts BootOpts) (hostNet, string, error) {
	if !opts.HostFirewall {
		return nil, "", nil
	}
	hn, err := newHostNet()
	if err != nil {
		return nil, "", err
	}
	return hn, hn.tap(), nil
}

// cleanupBoot removes transient boot artifacts and tears down the host tap on a
// failed launch. Empty paths are skipped (resume has no seed/overlay to remove).
func cleanupBoot(hn hostNet, seed, overlay string) {
	if hn != nil {
		_ = hn.teardown()
	}
	if seed != "" {
		os.Remove(seed)
	}
	if overlay != "" {
		os.Remove(overlay)
	}
}

// configureGuestTap makes the guest route agent egress out the host tap (its
// default route) while keeping the SLIRP NIC for the SSH control channel.
func configureGuestTap(ctx context.Context, client *ssh.Client, hn hostNet) error {
	script := guestTapRouteScript(hn.guestIP(), hn.hostIP())
	var b bytes.Buffer
	code, err := runSSHCombined(ctx, client, "sudo -n bash -s", strings.NewReader(script), &b)
	if err != nil || code != 0 {
		return fmt.Errorf("sandbox/qemu: configure guest tap route (exit %d): %v: %s", code, err, strings.TrimSpace(b.String()))
	}
	return nil
}

// guestTapRouteScript assigns guestIP to the guest's tap NIC (the ethernet NIC
// that is NOT the SLIRP 10.0.2.0/24 one) and makes hostIP its default route. The
// SLIRP NIC keeps its address so the loopback SSH hostfwd still works.
func guestTapRouteScript(guestIP, hostIP string) string {
	return strings.Join([]string{
		"set -e",
		`slirp=$(ip -o -4 addr show | awk '/inet 10\.0\.2\./{print $2; exit}')`,
		`tap=$(ip -o link show | awk -F': ' '{print $2}' | grep -E '^(en|eth)' | grep -v "^${slirp}$" | head -1)`,
		`[ -n "$tap" ] || { echo "no secondary NIC for the host tap" >&2; exit 1; }`,
		fmt.Sprintf("ip addr replace %s/30 dev \"$tap\"", guestIP),
		`ip link set "$tap" up`,
		fmt.Sprintf("ip route replace default via %s dev \"$tap\"", hostIP),
	}, "\n")
}

// hostNet is the host-side tap + nftables boundary for a QEMU guest (Linux only).
// Nothing it configures lives inside the guest, so a root agent cannot change it.
type hostNet interface {
	tap() string       // tap device name, for the QEMU -netdev
	guestIP() string   // IP the guest assigns to its tap NIC
	hostIP() string    // the tap's host-side IP (the guest's default gateway)
	guestCIDR() string // the /30 spanning host + guest
	// applyScope (re)applies the forward-filter + NAT rules for scope. allowInternet
	// permits general egress (roster has internet agents); the in-guest iptables
	// still does the per-agent split.
	applyScope(scope Scope, allowInternet bool) error
	teardown() error
}

// compositeEgress applies BOTH the in-guest per-agent rules and the host-side
// whole-guest cap on each scope change, while routing Exec via the in-guest users.
type compositeEgress struct {
	in            *sshEgress
	host          hostNet
	allowInternet bool
}

func (c *compositeEgress) ApplyBaseline(scope Scope) error {
	if err := c.in.ApplyBaseline(scope); err != nil {
		return err
	}
	if c.host != nil {
		return c.host.applyScope(scope, c.allowInternet)
	}
	return nil
}

func (c *compositeEgress) PathFor(agent core.AgentID, policy EgressPolicy) (NetPath, error) {
	return c.in.PathFor(agent, policy)
}

func (c *compositeEgress) Teardown() error {
	var hostErr error
	if c.host != nil {
		hostErr = c.host.teardown()
	}
	if err := c.in.Teardown(); err != nil {
		return err
	}
	return hostErr
}
