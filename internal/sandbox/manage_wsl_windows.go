//go:build windows

package sandbox

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// RunAsRoot runs a script as root in the distro, returning combined output. It
// is the general privileged-exec helper used by setup/teardown and tests.
func RunAsRoot(ctx context.Context, distro, script string) (string, error) {
	return rootRun(ctx, distro, script)
}

// rootRun runs a script as root in a distro and returns combined output.
func rootRun(ctx context.Context, distro, script string) (string, error) {
	c := exec.CommandContext(ctx, "wsl.exe", "-d", distro, "-u", "root", "--", "bash", "-s")
	c.Env = append(os.Environ(), "WSL_UTF8=1")
	c.Stdin = strings.NewReader(script)
	var buf bytes.Buffer
	c.Stdout, c.Stderr = &buf, &buf
	err := c.Run()
	return buf.String(), err
}

// engagementRoots are the sandbox directories scanned for existing engagements,
// canonical first, plus per-user/root oline engagement dirs. To resume files
// that live anywhere else in the environment, use the "point to a path in the
// environment" option on the resume step.
const engagementRoots = `/opt/oline/engagements /home/*/engagements /root/engagements`

// ListWSLEngagements returns the names of existing engagements in the distro,
// scanning the canonical workspace plus legacy/per-user locations.
func ListWSLEngagements(ctx context.Context, distro string) ([]string, error) {
	out, err := rootRun(ctx, distro, `for r in `+engagementRoots+`; do for d in "$r"/*/; do [ -d "$d" ] && basename "$d"; done; done 2>/dev/null | sort -u`)
	if err != nil {
		return nil, fmt.Errorf("sandbox/wsl: list engagements: %w", err)
	}
	var names []string
	for _, line := range strings.Split(strings.ReplaceAll(out, "\r\n", "\n"), "\n") {
		if s := strings.TrimSpace(line); s != "" {
			names = append(names, s)
		}
	}
	return names, nil
}

// ResolveWSLEngagementPath returns the actual directory of an existing engagement
// by name (searching the engagement roots, canonical first).
func ResolveWSLEngagementPath(ctx context.Context, distro, name string) (string, error) {
	script := "name=" + shellQuote(name) + "\nfor r in " + engagementRoots +
		"; do p=\"$r/$name\"; [ -d \"$p\" ] && { echo \"$p\"; exit 0; }; done\nexit 1"
	out, err := rootRun(ctx, distro, script)
	if err != nil {
		return "", fmt.Errorf("sandbox/wsl: engagement %q not found in the environment", name)
	}
	return strings.TrimSpace(out), nil
}

// CopyWSLDirToWorkspace copies an existing engagement directory (already inside
// the sandbox) into the canonical workspace under name, so the orchestrator and
// backups use one consistent location. A no-op if src is already canonical.
func CopyWSLDirToWorkspace(ctx context.Context, distro, src, name string) error {
	name = SafeEngagementName(name)
	canonical := WSLWorkspace + "/" + name
	script := "src=" + shellQuote(src) + "\ncanon=" + shellQuote(canonical) + `
[ "$src" = "$canon" ] && { echo SAME; exit 0; }
[ -d "$src" ] || { echo "source not found: $src" >&2; exit 2; }
mkdir -p "$canon"
cp -a "$src"/. "$canon"/
getent group oline >/dev/null 2>&1 && chown -R root:oline "$canon" 2>/dev/null || true
chmod -R g+rwX "$canon" 2>/dev/null || true
echo PLACED`
	out, err := rootRun(ctx, distro, script)
	if err != nil || !(strings.Contains(out, "PLACED") || strings.Contains(out, "SAME")) {
		return fmt.Errorf("sandbox/wsl: place engagement %q: %w: %s", name, err, strings.TrimSpace(out))
	}
	return nil
}

// ReadWSLFile reads a file from the distro as root.
func ReadWSLFile(ctx context.Context, distro, path string) (string, error) {
	out, err := rootRun(ctx, distro, "cat -- "+shellQuote(path))
	if err != nil {
		return "", fmt.Errorf("sandbox/wsl: read %s: %w", path, err)
	}
	return out, nil
}

// CopyFileToWSL copies a host file into the distro at guestPath (as root).
func CopyFileToWSL(ctx context.Context, distro, localPath, guestPath string) error {
	f, err := os.Open(localPath)
	if err != nil {
		return err
	}
	defer f.Close()
	c := exec.CommandContext(ctx, "wsl.exe", "-d", distro, "-u", "root", "--", "bash", "-lc",
		"mkdir -p \"$(dirname -- "+shellQuote(guestPath)+")\" && cat > "+shellQuote(guestPath))
	c.Env = append(os.Environ(), "WSL_UTF8=1")
	c.Stdin = f
	if out, err := c.CombinedOutput(); err != nil {
		return fmt.Errorf("sandbox/wsl: copy to %s: %w: %s", guestPath, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// ImportWSLEngagement imports an engagement from the host into the distro's
// workspace: localPath may be a directory or a .tar.gz/.tgz archive. The result
// lives at WSLWorkspace/<name>.
func ImportWSLEngagement(ctx context.Context, distro, localPath, name string) error {
	name = SafeEngagementName(name) // never let a name escape the workspace (root-run tar)
	info, err := os.Stat(localPath)
	if err != nil {
		return fmt.Errorf("sandbox/wsl: import source: %w", err)
	}
	var tgz string
	if info.IsDir() {
		tgz, err = tarDirToTemp(localPath)
		if err != nil {
			return fmt.Errorf("sandbox/wsl: pack %s: %w", localPath, err)
		}
		defer os.Remove(tgz)
	} else if isArchive(localPath) {
		tgz = localPath
	} else {
		return fmt.Errorf("sandbox/wsl: import source must be a directory or a .tar.gz/.tgz archive")
	}

	guestTgz := WSLWorkspace + "/.import.tgz"
	if err := CopyFileToWSL(ctx, distro, tgz, guestTgz); err != nil {
		return err
	}
	dest := WSLWorkspace + "/" + name
	script := fmt.Sprintf(`set -e
mkdir -p %s
tar --no-same-owner --no-same-permissions -xzf %s -C %s
rm -f %s
getent group oline >/dev/null 2>&1 && chown -R root:oline %s 2>/dev/null || true
chmod -R g+rwX %s 2>/dev/null || true
echo IMPORTED`, shellQuote(dest), shellQuote(guestTgz), shellQuote(dest), shellQuote(guestTgz), shellQuote(dest), shellQuote(dest))
	out, err := rootRun(ctx, distro, script)
	if err != nil || !strings.Contains(out, "IMPORTED") {
		return fmt.Errorf("sandbox/wsl: extract engagement: %w: %s", err, strings.TrimSpace(out))
	}
	return nil
}

// StartWSLVPN starts OpenVPN (as root, daemonized) with the given in-guest
// config and waits for a tunnel interface to come up. Returns a status line.
func StartWSLVPN(ctx context.Context, distro, guestConfig string) (string, error) {
	// The config path is bound to a shell variable once (shell-quoted); every use
	// references "$cfg", so a path containing $(...) / backticks can't execute.
	script := fmt.Sprintf(`set -e
cfg=%s
command -v openvpn >/dev/null || { echo "openvpn not installed"; exit 1; }
[ -r "$cfg" ] || { echo "config not readable: $cfg"; exit 1; }
mkdir -p /opt/oline
# already up?
if ip -o link show 2>/dev/null | grep -qE ': (tun|tap)[0-9]'; then echo "VPN already up"; exit 0; fi
openvpn --config "$cfg" --daemon oline-vpn --writepid /opt/oline/vpn.pid --log /opt/oline/vpn.log --connect-timeout 15 || true
for i in $(seq 1 20); do
  if grep -q "Initialization Sequence Completed" /opt/oline/vpn.log 2>/dev/null; then echo "VPN up"; ip -o -4 addr show | grep -E ' (tun|tap)' || true; exit 0; fi
  if ip -o link show 2>/dev/null | grep -qE ': (tun|tap)[0-9]'; then echo "VPN interface up"; exit 0; fi
  sleep 1
done
echo "VPN did not come up in time; last log:"; tail -n 8 /opt/oline/vpn.log 2>/dev/null
exit 1`, shellQuote(guestConfig))
	out, err := rootRun(ctx, distro, script)
	out = strings.TrimSpace(out)
	if err != nil {
		return out, fmt.Errorf("sandbox/wsl: start vpn: %w", err)
	}
	return out, nil
}

// BackupWSLEngagement writes a compressed (.tar.gz) backup of the engagement's
// workspace to destPath on the HOST. The oline program runs this (streaming the
// archive out of the sandbox as root); the AI agents never write to the host.
func BackupWSLEngagement(ctx context.Context, distro, engagement, destPath string) error {
	engagement = SafeEngagementName(engagement)
	if err := os.MkdirAll(filepath.Dir(destPath), 0o755); err != nil {
		return err
	}
	// Write to a temp file and rename over destPath, so there is never a partial
	// archive and only ever one backup per engagement.
	tmp := destPath + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}

	dir := WSLWorkspace + "/" + engagement
	// --warning flags quiet tar's benign notes (IPC sockets/fifos it skips, and files
	// the live agents change mid-read). Those still make tar exit 1, which is kept —
	// the precondition below uses exit 2 so a missing workspace stays a real failure.
	script := "[ -d " + shellQuote(dir) + " ] || { echo 'engagement workspace not found yet' >&2; exit 2; }; " +
		"tar --warning=no-file-ignored --warning=no-file-changed -czf - -C " + shellQuote(WSLWorkspace) + " " + shellQuote(engagement)
	c := exec.CommandContext(ctx, "wsl.exe", "-d", distro, "-u", "root", "--", "bash", "-lc", script)
	c.Env = wslEnv()
	c.Stdout = f
	var errb bytes.Buffer
	c.Stderr = &errb
	runErr := c.Run()
	f.Close()
	// tar exit 1 (sockets skipped, or a file changed while being read) still yields a
	// complete archive, so keep it; only a transport error or tar's fatal exit 2+ fails.
	if runErr != nil {
		var ee *exec.ExitError
		if !errors.As(runErr, &ee) || tarExitIsFatal(ee.ExitCode()) {
			os.Remove(tmp)
			return fmt.Errorf("sandbox/wsl: backup %q: %w: %s", engagement, runErr, strings.TrimSpace(errb.String()))
		}
	}
	if err := os.Rename(tmp, destPath); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("sandbox/wsl: finalize backup: %w", err)
	}
	return nil
}

// VPNStatus reports whether a tun/tap interface is currently up in the distro.
func VPNStatus(ctx context.Context, distro string) (bool, string) {
	out, _ := rootRun(ctx, distro, `ip -o -4 addr show 2>/dev/null | grep -E ' (tun|tap)' || echo none`)
	out = strings.TrimSpace(out)
	return out != "" && out != "none", out
}
