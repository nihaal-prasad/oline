package sandbox_test

import (
	"context"
	"strings"
	"testing"

	"github.com/nihaal-prasad/oline/internal/sandbox"
)

func TestBuildQemuArgs(t *testing.T) {
	a := strings.Join(sandbox.BuildQemuArgs("/img/overlay.qcow2", "/img/seed.iso", 2048, 2, 2222), " ")
	for _, want := range []string{"hostfwd=tcp:127.0.0.1:2222-:22", "file=/img/overlay.qcow2", "file=/img/seed.iso,media=cdrom", "-display none"} {
		if !strings.Contains(a, want) {
			t.Errorf("qemu args missing %q: %s", want, a)
		}
	}
}

func TestBuildNftRules(t *testing.T) {
	r := strings.Join(sandbox.BuildNftRules("tap0", sandbox.Scope{Targets: []string{"10.0.0.0/24", "1.2.3.4", "bad;rm -rf"}}), "\n")
	for _, want := range []string{"policy drop", "ip daddr 10.0.0.0/24 accept", "ip daddr 1.2.3.4 accept", `iifname "tap0" drop`} {
		if !strings.Contains(r, want) {
			t.Errorf("nft rules missing %q:\n%s", want, r)
		}
	}
	if strings.Contains(r, "rm -rf") {
		t.Errorf("unsafe token leaked into nft rules:\n%s", r)
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
