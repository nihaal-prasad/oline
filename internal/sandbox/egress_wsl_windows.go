//go:build windows

package sandbox

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"strings"
)

// wslEgress enforces per-agent egress inside a WSL distro. The policy, users, and
// firewall rules live in egressCore; this type only supplies the transport —
// running the root scripts via `wsl -u root`.
type wslEgress struct {
	*egressCore
	distro string
}

func newWSLEgress(distro string) *wslEgress {
	e := &wslEgress{distro: distro}
	e.egressCore = newEgressCore(e.rootExec)
	return e
}

// rootExec runs a script as root in the distro and returns combined output.
func (e *wslEgress) rootExec(ctx context.Context, script string) (string, error) {
	c := exec.CommandContext(ctx, "wsl.exe", "-d", e.distro, "-u", "root", "--", "bash", "-s")
	c.Env = append(os.Environ(), "WSL_UTF8=1")
	c.Stdin = strings.NewReader(script)
	var buf bytes.Buffer
	c.Stdout = &buf
	c.Stderr = &buf
	err := c.Run()
	return buf.String(), err
}

// TeardownWSLEgress restores the firewall snapshot and removes the oline users
// and shared home from a distro. Used at full cleanup / by tests.
func TeardownWSLEgress(distro string) error {
	e := newWSLEgress(distro)
	e.applied = true // force the restore attempt even on a fresh controller
	_ = e.Teardown()
	return e.removeUsers()
}
