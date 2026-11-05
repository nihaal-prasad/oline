//go:build !windows

package sandbox

import (
	"context"
	"errors"
)

// errNoWSL is returned by the WSL backend on non-Windows hosts.
var errNoWSL = errors.New("sandbox/wsl: WSL is only available on Windows; use the QEMU backend")

type wslUnsupported struct{}

// NewWSL returns a Sandbox whose operations fail: WSL exists only on Windows.
func NewWSL() Sandbox { return wslUnsupported{} }

// WSLAvailable is always false off Windows (no wsl.exe).
func WSLAvailable() bool { return false }

func (wslUnsupported) Scan(context.Context) ([]Candidate, error)    { return nil, errNoWSL }
func (wslUnsupported) Create(context.Context, Spec) (Handle, error) { return nil, errNoWSL }
func (wslUnsupported) Open(context.Context, string) (Handle, error) { return nil, errNoWSL }
func (wslUnsupported) Destroy(context.Context, string) error        { return errNoWSL }

func (wslUnsupported) CreateDistro(context.Context, string, string) (Handle, error) {
	return nil, errNoWSL
}
func (wslUnsupported) InstallSupportsName(context.Context) bool { return false }
