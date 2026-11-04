//go:build !linux

package sandbox

import "fmt"

// newHostNet is unsupported off Linux: the host tap + nftables boundary needs a
// Linux host. The TUI only requests it when qemu.hostFirewall is set and the host
// is Linux; this stub keeps the build correct on every OS.
func newHostNet() (hostNet, error) {
	return nil, fmt.Errorf("sandbox/qemu: qemu.hostFirewall requires a Linux host")
}
