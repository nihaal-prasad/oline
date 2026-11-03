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
	overlay string // transient CoW disk (removed on Shutdown)
	seedISO string // cloud-init seed (removed on Shutdown)
	sshPort int
	signer  ssh.Signer
	cmd     *exec.Cmd // the qemu-system process
	client  *ssh.Client
	egress  *sshEgress
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

	guestDir := WSLWorkspace + "/" + sanitizeHostname(engagement)
	inner := "tar czf - -C " + sshQuote(guestDir) + " ."
	// stderr to a separate buffer: tar's warnings must not land inside the .tar.gz.
	var errBuf bytes.Buffer
	code, rerr := runSSH(ctx, h.client, "sudo -n bash -lc "+sshQuote(inner), nil, tmp, &errBuf)
	if cerr := tmp.Close(); cerr != nil && rerr == nil {
		rerr = cerr
	}
	if rerr != nil || code != 0 {
		return fmt.Errorf("sandbox/qemu: backup: %v (exit %d): %s", rerr, code, strings.TrimSpace(errBuf.String()))
	}
	return os.Rename(tmpName, destPath)
}

func (h *qemuHandle) EgressController() EgressController {
	if h.egress != nil {
		return h.egress
	}
	return noopEgress{}
}

// SetupEgress provisions the per-policy unprivileged users and root-owned
// firewall rules inside the VM (over SSH), then routes Exec through them.
func (h *qemuHandle) SetupEgress(ctx context.Context, scope Scope) error {
	e := newSSHEgress(h)
	if err := e.ensureUsers(ctx); err != nil {
		return err
	}
	if err := e.ApplyBaseline(scope); err != nil {
		return err
	}
	h.egress = e
	return nil
}

// Shutdown powers the VM off (best-effort graceful), kills the process if it
// lingers, and removes the transient overlay and seed. The base image and any
// user-supplied image are left intact for reuse.
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
	if h.overlay != "" {
		os.Remove(h.overlay)
	}
	if h.seedISO != "" {
		os.Remove(h.seedISO)
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

// boot creates a disposable overlay + cloud-init seed on top of base, launches
// the VM headless, and waits for SSH. The returned handle owns the VM.
func (q *qemuSandbox) boot(ctx context.Context, base, id string) (Handle, error) {
	if qemuBinary() == "" {
		return nil, qemuUnavailable()
	}
	signer, authLine, err := genSSHKey()
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
	port, err := freePort()
	if err != nil {
		os.Remove(seed)
		os.Remove(overlay)
		return nil, err
	}

	cmd := exec.Command(qemuBinary(), BuildQemuArgs(overlay, seed, 2048, 2, port)...)
	if err := cmd.Start(); err != nil {
		os.Remove(seed)
		os.Remove(overlay)
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
		os.Remove(seed)
		os.Remove(overlay)
		return nil, fmt.Errorf("sandbox/qemu: %w", err)
	}
	// SSH comes up mid-boot; wait for cloud-init to finish provisioning (user,
	// packages) so egress setup doesn't race a half-configured guest.
	waitCloudInit(dialCtx, client)
	return &qemuHandle{
		id: id, overlay: overlay, seedISO: seed, sshPort: port,
		signer: signer, cmd: cmd, client: client,
	}, nil
}
