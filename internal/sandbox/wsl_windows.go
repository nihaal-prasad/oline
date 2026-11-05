//go:build windows

package sandbox

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/nihaal-prasad/oline/internal/core"
)

// wslSandbox drives Windows Subsystem for Linux distros via wsl.exe.
//
// Scan/Open/Exec/Copy run against an existing distro; SetupEgress provisions the
// per-agent unprivileged users and root-owned iptables rules (see wslEgress).
// CreateDistro imports or installs a fresh oline- distro and hardens it (no
// Windows drive automount / interop); Destroy unregisters an oline- distro. Both
// refuse to touch a distro without the oline- Prefix. Cancellation kills the
// wsl.exe child (a later pass adds an explicit in-guest PID kill so no remote
// process orphans).
type wslSandbox struct{}

// NewWSL returns a WSL-backed Sandbox.
func NewWSL() Sandbox { return &wslSandbox{} }

// WSLAvailable reports whether wsl.exe is on PATH, so the TUI can offer the WSL
// backend (and the "create a new distro" entry).
func WSLAvailable() bool {
	_, err := exec.LookPath("wsl.exe")
	return err == nil
}

func wslEnv() []string { return append(os.Environ(), "WSL_UTF8=1") }

func (w *wslSandbox) Scan(ctx context.Context) ([]Candidate, error) {
	cmd := exec.CommandContext(ctx, "wsl.exe", "--list", "--verbose")
	cmd.Env = wslEnv()
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("sandbox/wsl: listing distros: %w", err)
	}
	var cands []Candidate
	lines := strings.Split(strings.ReplaceAll(string(out), "\r\n", "\n"), "\n")
	for i, line := range lines {
		line = strings.TrimSpace(strings.ReplaceAll(line, "\x00", ""))
		if line == "" {
			continue
		}
		if i == 0 && strings.HasPrefix(strings.ToUpper(line), "NAME") {
			continue // header
		}
		line = strings.TrimPrefix(line, "*")
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		name, state := fields[0], fields[1]
		cands = append(cands, Candidate{
			ID:      name,
			Name:    name,
			Backend: BackendWSL,
			Running: strings.EqualFold(state, "Running"),
			Managed: strings.HasPrefix(name, Prefix),
		})
	}
	return cands, nil
}

func (w *wslSandbox) Open(ctx context.Context, id string) (Handle, error) {
	cands, err := w.Scan(ctx)
	if err != nil {
		return nil, err
	}
	for _, c := range cands {
		if c.ID == id {
			return &wslHandle{distro: id}, nil
		}
	}
	return nil, fmt.Errorf("sandbox/wsl: distro %q not found", id)
}

// Create provisions a fresh oline-managed Debian distro named spec.Name (the
// oline- Prefix is added if missing) by installing the official Debian distro
// under that name. For the tarball-import path use CreateDistro directly.
func (w *wslSandbox) Create(ctx context.Context, spec Spec) (Handle, error) {
	return w.CreateDistro(ctx, prefixed(spec.Name), "")
}

// CreateDistro provisions a fresh oline-managed Debian distro and returns a live
// handle. When rootfsTarball is non-empty it is imported with `wsl --import`;
// otherwise the official Debian distro is installed under name with
// `wsl --install Debian --name`. The distro is hardened afterward (no Windows
// drive automount / interop) and the per-agent egress toolchain is installed.
func (w *wslSandbox) CreateDistro(ctx context.Context, name, rootfsTarball string) (Handle, error) {
	name = prefixed(name)
	if !WSLAvailable() {
		return nil, errors.New("sandbox/wsl: wsl.exe not found — install WSL to create a distro")
	}
	// Never clobber an existing distro (oline-managed or not).
	if cands, err := w.Scan(ctx); err == nil {
		for _, c := range cands {
			if strings.EqualFold(c.ID, name) {
				return nil, fmt.Errorf("sandbox/wsl: a distro named %q already exists — reuse it instead of creating a new one", name)
			}
		}
	}

	if rootfsTarball != "" {
		st, err := os.Stat(rootfsTarball)
		if err != nil || st.IsDir() {
			return nil, fmt.Errorf("sandbox/wsl: rootfs tarball %q not found", rootfsTarball)
		}
		installDir := wslInstallDir(name)
		if err := os.MkdirAll(installDir, 0o755); err != nil {
			return nil, fmt.Errorf("sandbox/wsl: create install dir: %w", err)
		}
		c := exec.CommandContext(ctx, "wsl.exe", "--import", name, installDir, rootfsTarball)
		c.Env = wslEnv()
		if out, err := c.CombinedOutput(); err != nil {
			os.RemoveAll(installDir)
			return nil, fmt.Errorf("sandbox/wsl: import %q: %w: %s", name, err, strings.TrimSpace(string(out)))
		}
	} else {
		// Install the official Debian distro directly under the oline- name. WSL
		// manages its storage location, and --unregister removes it on Destroy.
		c := exec.CommandContext(ctx, "wsl.exe", "--install", "Debian", "--name", name, "--no-launch")
		c.Env = wslEnv()
		if out, err := c.CombinedOutput(); err != nil {
			return nil, fmt.Errorf("sandbox/wsl: install Debian as %q (if your WSL is too old for --name, create from a rootfs tarball instead): %w: %s",
				name, err, strings.TrimSpace(string(out)))
		}
	}

	if err := w.hardenDistro(ctx, name); err != nil {
		_ = w.Destroy(context.Background(), name) // don't leave a half-provisioned distro behind
		return nil, err
	}
	return &wslHandle{distro: name}, nil
}

// InstallSupportsName reports whether `wsl --install` accepts --name on this host
// (added in WSL ~2.5.9, Nov 2024). When it does not, the TUI collects a Debian
// rootfs tarball and imports instead.
func (w *wslSandbox) InstallSupportsName(ctx context.Context) bool {
	if !WSLAvailable() {
		return false
	}
	c := exec.CommandContext(ctx, "wsl.exe", "--install", "--help")
	c.Env = wslEnv()
	if out, _ := c.CombinedOutput(); strings.Contains(string(out), "--name") {
		return true
	}
	c2 := exec.CommandContext(ctx, "wsl.exe", "--help")
	c2.Env = wslEnv()
	out2, _ := c2.CombinedOutput()
	return strings.Contains(string(out2), "--name")
}

// hardenDistro confines a freshly created distro (no Windows drive automount, no
// interop/PATH bleed), creates the engagement workspace root, and installs the
// per-agent egress toolchain (iptables) if it is missing. It then terminates the
// distro so /etc/wsl.conf takes effect on the next start.
func (w *wslSandbox) hardenDistro(ctx context.Context, distro string) error {
	if err := waitWSLReady(ctx, distro); err != nil {
		return err
	}
	script := `set -e
cat > /etc/wsl.conf <<'EOF'
[automount]
enabled = false
[interop]
enabled = false
appendWindowsPath = false
EOF
mkdir -p /opt/oline/engagements
chmod 2775 /opt/oline/engagements 2>/dev/null || true
export DEBIAN_FRONTEND=noninteractive
if ! command -v iptables >/dev/null 2>&1; then
  apt-get update -y 2>/dev/null && apt-get install -y --no-install-recommends iptables iproute2 sudo procps ca-certificates 2>/dev/null || true
fi
echo HARDENED`
	out, err := rootRun(ctx, distro, script)
	if err != nil || !strings.Contains(out, "HARDENED") {
		return fmt.Errorf("sandbox/wsl: harden %q: %w: %s", distro, err, strings.TrimSpace(out))
	}
	_ = exec.CommandContext(ctx, "wsl.exe", "--terminate", distro).Run()
	return nil
}

// waitWSLReady blocks until the distro answers a trivial root command, or times out.
func waitWSLReady(ctx context.Context, distro string) error {
	deadline := time.Now().Add(90 * time.Second)
	for {
		c := exec.CommandContext(ctx, "wsl.exe", "-d", distro, "-u", "root", "--", "true")
		c.Env = wslEnv()
		if err := c.Run(); err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("sandbox/wsl: distro %q did not become ready in time", distro)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

// Destroy unregisters an oline-managed distro and removes any install directory
// oline created for it (imported distros). It refuses anything without the
// oline- Prefix, so a distro the user keeps for other work is never touched.
func (w *wslSandbox) Destroy(ctx context.Context, id string) error {
	if !strings.HasPrefix(id, Prefix) {
		return fmt.Errorf("sandbox/wsl: refusing to destroy %q — not an oline-managed distro", id)
	}
	c := exec.CommandContext(ctx, "wsl.exe", "--unregister", id)
	c.Env = wslEnv()
	if out, err := c.CombinedOutput(); err != nil {
		return fmt.Errorf("sandbox/wsl: unregister %q: %w: %s", id, err, strings.TrimSpace(string(out)))
	}
	_ = os.RemoveAll(wslInstallDir(id))
	return nil
}

// wslInstallDir is where oline stores an imported distro's files on the host.
func wslInstallDir(name string) string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "oline", "wsl", name)
}

// prefixed ensures a name carries the oline- Prefix exactly once.
func prefixed(name string) string {
	if strings.HasPrefix(name, Prefix) {
		return name
	}
	return Prefix + name
}

// wslHandle is a live WSL distro. user == "" uses the distro's default user
// (unprivileged on a standard Kali install). Once SetupEgress runs, commands run
// as the per-policy oline user instead.
type wslHandle struct {
	distro  string
	user    string
	egress  *wslEgress
	workdir string // default guest cwd for agent commands (the engagement workspace)
}

func (h *wslHandle) Info() EnvInfo { return EnvInfo{ID: h.distro, Backend: BackendWSL} }

// Backup archives the engagement workspace to the host (program-driven).
func (h *wslHandle) Backup(ctx context.Context, engagement, destPath string) error {
	return BackupWSLEngagement(ctx, h.distro, engagement, destPath)
}

func (h *wslHandle) EgressController() EgressController {
	if h.egress != nil {
		return h.egress
	}
	return noopEgress{}
}

// SetupEgress provisions the per-policy unprivileged users and root-owned
// firewall rules for this distro, then routes subsequent Exec calls through them.
func (h *wslHandle) SetupEgress(ctx context.Context, scope Scope) error {
	e := newWSLEgress(h.distro)
	if err := e.ensureUsers(ctx); err != nil {
		return err
	}
	if err := e.ApplyBaseline(scope); err != nil {
		return err
	}
	h.egress = e
	return nil
}

// Shutdown removes the per-agent firewall rules when the engagement ends. Unlike
// QEMU (where the whole VM is destroyed), a WSL distro keeps running after oline
// exits, so without this the OLINE_OUT chain would linger in the running distro.
// It tears the firewall down completely (deletes the OLINE_OUT chain and its OUTPUT
// hook, IPv4 and IPv6); the oline-* users and the engagement workspace are left in
// place. The TUI calls this on exit, after agents are cancelled, so nothing is
// running under the rules when they are removed.
func (h *wslHandle) Shutdown(ctx context.Context) error {
	if h.egress == nil {
		return nil
	}
	_, err := h.egress.rootExec(ctx, buildEgressTeardown())
	h.egress.applied = false
	return err
}

// Compile-time check: a WSL handle is a Shutdowner, so the TUI tears its firewall
// down on exit (see the shutdown path).
var _ Shutdowner = (*wslHandle)(nil)

// SetWorkspace creates the engagement workspace (group-writable by the oline users)
// and makes it the default working directory for later agent commands. It runs the
// creation from an existing directory (/opt/oline) so the command does not depend on
// the workspace it is about to create.
func (h *wslHandle) SetWorkspace(ctx context.Context, dir string) error {
	script := "mkdir -p " + shellQuote(dir) + " && chmod 2775 " + shellQuote(dir)
	if _, err := h.Exec(ctx, "setup", Cmd{Argv: []string{"bash", "-lc", script}, Dir: "/opt/oline"}, io.Discard); err != nil {
		return err
	}
	h.workdir = dir
	return nil
}

var _ WorkspaceSetter = (*wslHandle)(nil)

// execUser resolves which guest user a given agent's command runs as.
func (h *wslHandle) execUser(agent core.AgentID, policy EgressPolicy) string {
	if h.egress != nil {
		if np, err := h.egress.PathFor(agent, policy); err == nil && np.User != "" {
			return np.User
		}
	}
	return h.user
}

// baseArgs builds the wsl.exe prefix. It ALWAYS pins the guest working
// directory with --cd to a WSL-native path (default: the user's home), so a
// command never inherits the Windows cwd (which wsl.exe would otherwise map to
// /mnt/c/... — off-limits for this harness).
func (h *wslHandle) baseArgs(dir, user string) []string {
	if dir == "" {
		dir = h.workdir // the engagement workspace, once set
	}
	if dir == "" {
		dir = "~"
	}
	args := []string{"-d", h.distro, "--cd", dir}
	if user != "" {
		args = append(args, "-u", user)
	}
	return args
}

func (h *wslHandle) Exec(ctx context.Context, agent core.AgentID, cmd Cmd, out io.Writer) (ExitStatus, error) {
	if len(cmd.Argv) == 0 {
		return ExitStatus{}, errors.New("sandbox/wsl: empty command")
	}
	args := append(h.baseArgs(cmd.Dir, h.execUser(agent, cmd.Policy)), "--")
	args = append(args, cmd.Argv...)

	c := exec.CommandContext(ctx, "wsl.exe", args...)
	c.Env = wslEnv()
	c.Stdout = out
	c.Stderr = out

	err := c.Run()
	code := -1
	if c.ProcessState != nil {
		code = c.ProcessState.ExitCode()
	}
	if ctx.Err() != nil {
		return ExitStatus{Code: code}, ctx.Err()
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ExitStatus{Code: code}, nil // a non-zero exit is a result, not an infra error
	}
	if err != nil {
		return ExitStatus{Code: code}, fmt.Errorf("sandbox/wsl: exec: %w", err)
	}
	return ExitStatus{Code: code}, nil
}

// OpenTerminal runs cmd under a pseudo-terminal inside WSL so full-screen programs
// work as in a real terminal. wsl.exe gives us plain pipes, not a console, so we
// allocate the PTY on the Linux side with util-linux `script`: it spawns the target
// on a fresh pty whose stdin/stdout we drive through the pipes. The initial size is
// set with `stty` inside that pty. Live resize is not supported on WSL (script's pty
// size is fixed at spawn); the QEMU backend resizes live.
func (h *wslHandle) OpenTerminal(ctx context.Context, agent core.AgentID, cmd Cmd, rows, cols int, out io.Writer) (PTYSession, error) {
	if len(cmd.Argv) == 0 {
		return nil, errors.New("sandbox/wsl: empty terminal command")
	}
	if rows <= 0 {
		rows = 24
	}
	if cols <= 0 {
		cols = 80
	}
	target := quoteArgv(cmd.Argv)
	// Inside the pty: set its size, export a sensible TERM, then exec the target.
	inner := fmt.Sprintf("stty rows %d cols %d 2>/dev/null; export TERM=xterm-256color; exec %s", rows, cols, target)
	// `script -qfc <inner> /dev/null` allocates the pty; -f flushes so output streams.
	outer := "exec script -qfc " + shellQuote(inner) + " /dev/null"

	cctx, cancel := context.WithCancel(ctx)
	args := append(h.baseArgs(cmd.Dir, h.execUser(agent, cmd.Policy)), "--", "bash", "-lc", outer)
	c := exec.CommandContext(cctx, "wsl.exe", args...)
	c.Env = wslEnv()
	c.Stdout = out
	c.Stderr = out
	stdin, err := c.StdinPipe()
	if err != nil {
		cancel()
		return nil, fmt.Errorf("sandbox/wsl: terminal stdin: %w", err)
	}
	if err := c.Start(); err != nil {
		cancel()
		return nil, fmt.Errorf("sandbox/wsl: start terminal: %w", err)
	}
	go func() { _ = c.Wait(); cancel() }() // reap and release the context when it exits
	return &wslPTY{stdin: stdin, cancel: cancel}, nil
}

// wslPTY is a PTYSession backed by a wsl.exe process running `script`.
type wslPTY struct {
	stdin  io.WriteCloser
	cancel context.CancelFunc
}

func (p *wslPTY) Write(b []byte) (int, error) { return p.stdin.Write(b) }

// Resize is a no-op on WSL: the `script`-allocated pty's size is fixed at open.
func (p *wslPTY) Resize(rows, cols int) error { return nil }

func (p *wslPTY) Close() error {
	_ = p.stdin.Close()
	p.cancel()
	return nil
}

func (h *wslHandle) CopyIn(ctx context.Context, localPath, guestPath string) error {
	f, err := os.Open(localPath)
	if err != nil {
		return err
	}
	defer f.Close()
	args := append(h.baseArgs("~", h.execUser("", InScopeOnly)), "--", "bash", "-lc", "cat > "+shellQuote(guestPath))
	c := exec.CommandContext(ctx, "wsl.exe", args...)
	c.Env = wslEnv()
	c.Stdin = f
	if out, err := c.CombinedOutput(); err != nil {
		return fmt.Errorf("sandbox/wsl: CopyIn: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func (h *wslHandle) CopyOut(ctx context.Context, guestPath, localPath string) error {
	f, err := os.Create(localPath)
	if err != nil {
		return err
	}
	defer f.Close()
	args := append(h.baseArgs("~", h.execUser("", InScopeOnly)), "--", "bash", "-lc", "cat "+shellQuote(guestPath))
	c := exec.CommandContext(ctx, "wsl.exe", args...)
	c.Env = wslEnv()
	c.Stdout = f
	if err := c.Run(); err != nil {
		return fmt.Errorf("sandbox/wsl: CopyOut: %w", err)
	}
	return nil
}

// shellQuote single-quotes a string for safe use inside a bash -lc command.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// quoteArgv joins argv into a single shell command string, quoting each element.
func quoteArgv(argv []string) string {
	parts := make([]string, len(argv))
	for i, a := range argv {
		parts[i] = shellQuote(a)
	}
	return strings.Join(parts, " ")
}

var _ Interactive = (*wslHandle)(nil)
