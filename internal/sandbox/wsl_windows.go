//go:build windows

package sandbox

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	"github.com/nihaal-prasad/oline/internal/core"
)

// wslSandbox drives Windows Subsystem for Linux distros via wsl.exe.
//
// Scan/Open/Exec/Copy run against an existing distro; SetupEgress provisions the
// per-agent unprivileged users and root-owned iptables rules (see wslEgress).
// Create/Destroy (importing/removing a hardened oline- distro, including
// disabling drive automount) are not implemented yet, so until then oline opens
// a user-provided distro. Cancellation kills the wsl.exe child (a later pass adds
// an explicit in-guest PID kill so no remote process orphans).
type wslSandbox struct{}

// NewWSL returns a WSL-backed Sandbox.
func NewWSL() Sandbox { return &wslSandbox{} }

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

func (w *wslSandbox) Create(ctx context.Context, spec Spec) (Handle, error) {
	return nil, errors.New("sandbox/wsl: Create (import hardened oline- distro) lands in Phase 4")
}

func (w *wslSandbox) Destroy(ctx context.Context, id string) error {
	if !strings.HasPrefix(id, Prefix) {
		return fmt.Errorf("sandbox/wsl: refusing to destroy %q — not an oline-managed distro", id)
	}
	return errors.New("sandbox/wsl: Destroy lands in Phase 4")
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
