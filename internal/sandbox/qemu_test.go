package sandbox_test

import (
	"context"
	"strings"
	"testing"

	"github.com/nihaal-prasad/oline/internal/sandbox"
)

func TestBuildQemuArgs(t *testing.T) {
	// Disposable boot: SLIRP NIC + seed CD-ROM, no tap.
	a := strings.Join(sandbox.BuildQemuArgs("/img/overlay.qcow2", "/img/seed.iso", 2048, 2, 2222, ""), " ")
	for _, want := range []string{"hostfwd=tcp:127.0.0.1:2222-:22", "file=/img/overlay.qcow2", "file=/img/seed.iso,media=cdrom", "-display none"} {
		if !strings.Contains(a, want) {
			t.Errorf("qemu args missing %q: %s", want, a)
		}
	}
	if strings.Contains(a, "netdev=n1") {
		t.Errorf("no tap requested but a second NIC appeared: %s", a)
	}
	// Resume boot with a host tap: no seed drive, a tap NIC bound to oltap7.
	a2 := strings.Join(sandbox.BuildQemuArgs("/img/disk.qcow2", "", 2048, 2, 2200, "oltap7"), " ")
	for _, want := range []string{"ifname=oltap7", "netdev=n1"} {
		if !strings.Contains(a2, want) {
			t.Errorf("qemu args missing %q: %s", want, a2)
		}
	}
	if strings.Contains(a2, "media=cdrom") {
		t.Errorf("resume boot should have no seed CD-ROM: %s", a2)
	}
}

func TestBuildHostNftRules(t *testing.T) {
	scope := sandbox.Scope{Targets: []string{"10.0.0.0/24", "1.2.3.4", "bad;rm -rf"}}

	// No internet agents: a hard scope-only cap (default drop; targets accepted).
	strict := strings.Join(sandbox.BuildHostNftRules("oltap7", "172.16.7.0/30", scope, false), "\n")
	for _, want := range []string{"policy drop", "ip daddr 10.0.0.0/24 accept", "ip daddr 1.2.3.4 accept", "oifname != \"oltap7\" masquerade"} {
		if !strings.Contains(strict, want) {
			t.Errorf("nft rules missing %q:\n%s", want, strict)
		}
	}
	if strings.Contains(strict, "rm -rf") {
		t.Errorf("unsafe token leaked into nft rules:\n%s", strict)
	}
	if strings.Contains(strict, `iifname "oltap7" accept`) {
		t.Errorf("scope-only cap must not blanket-accept egress:\n%s", strict)
	}
	// Internet agents present: the targets are still accepted and general egress is
	// allowed (the per-agent split is left to the in-guest firewall).
	open := strings.Join(sandbox.BuildHostNftRules("oltap7", "172.16.7.0/30", scope, true), "\n")
	if !strings.Contains(open, `iifname "oltap7" accept`) {
		t.Errorf("internet-allowed cap should accept general egress:\n%s", open)
	}
}

func TestQemuScanGraceful(t *testing.T) {
	// With no qemu / no images, Scan returns no candidates and no error.
	cs, err := sandbox.NewQEMU().Scan(context.Background())
	if err != nil {
		t.Fatalf("Scan err: %v", err)
	}
	_ = cs
}
