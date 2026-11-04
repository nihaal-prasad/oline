//go:build linux

package sandbox

import (
	"bytes"
	"crypto/rand"
	"fmt"
	"os/exec"
	"os/user"
	"strings"
)

// linuxHostNet is the host-side tap + nftables boundary for a QEMU guest. The tap,
// its routing, and the nftables rules all live on the HOST (configured via sudo),
// so a root agent inside the guest cannot change them. QEMU attaches the guest's
// second NIC to the tap; the guest's default route goes out it (see
// configureGuestTap), so every off-box packet is filtered by the host rules.
type linuxHostNet struct {
	tapName string
	host    string // host-side tap IP (the guest's default gateway)
	guest   string // the IP the guest assigns to its tap NIC
	cidr    string // the /30 spanning host + guest
}

// newHostNet creates the tap and base host networking (idempotent teardown on
// failure). Requires nft + iproute2 and passwordless sudo on the host.
func newHostNet() (hostNet, error) {
	if _, err := exec.LookPath("nft"); err != nil {
		return nil, fmt.Errorf("sandbox/qemu: host firewall needs nftables (nft) on the host: %w", err)
	}
	if _, err := exec.LookPath("ip"); err != nil {
		return nil, fmt.Errorf("sandbox/qemu: host firewall needs iproute2 (ip) on the host: %w", err)
	}
	// Randomize the third octet so concurrent VMs get distinct taps/subnets:
	// 172.16.N.0/30 -> host .1, guest .2.
	var b [1]byte
	if _, err := rand.Read(b[:]); err != nil {
		return nil, err
	}
	octet := 1 + int(b[0])%250
	h := &linuxHostNet{
		tapName: fmt.Sprintf("oltap%d", octet),
		host:    fmt.Sprintf("172.16.%d.1", octet),
		guest:   fmt.Sprintf("172.16.%d.2", octet),
		cidr:    fmt.Sprintf("172.16.%d.0/30", octet),
	}
	owner := "root"
	if u, err := user.Current(); err == nil && u.Username != "" {
		owner = u.Username
	}
	setup := strings.Join([]string{
		"set -e",
		fmt.Sprintf("ip tuntap add dev %s mode tap user %s", h.tapName, owner),
		fmt.Sprintf("ip addr add %s/30 dev %s", h.host, h.tapName),
		fmt.Sprintf("ip link set %s up", h.tapName),
		"sysctl -w net.ipv4.ip_forward=1",
	}, "\n")
	if out, err := sudoBash(setup); err != nil {
		_, _ = sudoBash(fmt.Sprintf("ip link del %s 2>/dev/null || true", h.tapName))
		return nil, fmt.Errorf("sandbox/qemu: create host tap: %w: %s", err, strings.TrimSpace(out))
	}
	return h, nil
}

func (h *linuxHostNet) tap() string       { return h.tapName }
func (h *linuxHostNet) guestIP() string   { return h.guest }
func (h *linuxHostNet) hostIP() string    { return h.host }
func (h *linuxHostNet) guestCIDR() string { return h.cidr }

func (h *linuxHostNet) applyScope(scope Scope, allowInternet bool) error {
	script := strings.Join(BuildHostNftRules(h.tapName, h.cidr, scope, allowInternet), "\n") + "\n"
	if out, err := sudoNft(script); err != nil {
		return fmt.Errorf("apply nft rules: %w: %s", err, strings.TrimSpace(out))
	}
	return nil
}

func (h *linuxHostNet) teardown() error {
	_, _ = sudoNft(strings.Join(buildHostNftTeardown(), "\n") + "\n")
	_, err := sudoBash(fmt.Sprintf("ip link del %s 2>/dev/null || true", h.tapName))
	return err
}

// sudoBash runs a bash script as root on the HOST.
func sudoBash(script string) (string, error) {
	cmd := exec.Command("sudo", "-n", "bash", "-s")
	cmd.Stdin = strings.NewReader(script)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	return out.String(), cmd.Run()
}

// sudoNft feeds an nft script to `nft -f -` as root on the HOST.
func sudoNft(script string) (string, error) {
	cmd := exec.Command("sudo", "-n", "nft", "-f", "-")
	cmd.Stdin = strings.NewReader(script)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	return out.String(), cmd.Run()
}
