package sandbox

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// TestPersistDisk: a persisted handle moves its overlay and SSH key under the
// engagement's state dir (derived from the workspace path) so bootResume can find
// them, and updates its overlay pointer to the new location.
func TestPersistDisk(t *testing.T) {
	img := t.TempDir()
	run := filepath.Join(img, ".run")
	if err := os.MkdirAll(run, 0o755); err != nil {
		t.Fatal(err)
	}
	overlay := filepath.Join(run, "oline-debian13-123.qcow2")
	if err := os.WriteFile(overlay, []byte("disk-bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	h := &qemuHandle{
		overlay: overlay, imagesDir: img, persist: true,
		keyPEM:  []byte("KEYPEM"),
		workdir: WSLWorkspace + "/engagement-20250102-030405",
	}
	if err := h.persistDisk(); err != nil {
		t.Fatalf("persistDisk: %v", err)
	}
	stateDir := filepath.Join(img, ".state", "engagement-20250102-030405")
	disk := filepath.Join(stateDir, stateDiskName)
	if _, err := os.Stat(disk); err != nil {
		t.Fatalf("disk not persisted: %v", err)
	}
	if h.overlay != disk {
		t.Errorf("overlay pointer = %q, want %q", h.overlay, disk)
	}
	if b, _ := os.ReadFile(filepath.Join(stateDir, stateKeyName)); string(b) != "KEYPEM" {
		t.Errorf("key not persisted, got %q", b)
	}
	if _, err := os.Stat(overlay); !os.IsNotExist(err) {
		t.Error("original overlay should be gone after the move")
	}
	// Idempotent when the overlay is already in place (resume path).
	if err := h.persistDisk(); err != nil {
		t.Errorf("persistDisk (in place) should be a no-op, got %v", err)
	}
}

// TestResumableEngagements lists only state dirs that actually hold a disk, sorted.
func TestResumableEngagements(t *testing.T) {
	img := t.TempDir()
	q := &qemuSandbox{imagesDir: img}
	if got := q.ResumableEngagements(); len(got) != 0 {
		t.Fatalf("empty images dir should list nothing, got %v", got)
	}
	mk := func(name string, withDisk bool) {
		d := filepath.Join(img, ".state", name)
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		if withDisk {
			if err := os.WriteFile(filepath.Join(d, stateDiskName), []byte("x"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	mk("eng-b", true)
	mk("eng-a", true)
	mk("eng-no-disk", false)
	if got, want := q.ResumableEngagements(), []string{"eng-a", "eng-b"}; !reflect.DeepEqual(got, want) {
		t.Errorf("ResumableEngagements = %v, want %v", got, want)
	}
}

// TestGuestTapRouteScript: the guest route setup uses the right IPs and leaves the
// SLIRP NIC (10.0.2.0/24) for SSH.
func TestGuestTapRouteScript(t *testing.T) {
	s := guestTapRouteScript("172.16.9.2", "172.16.9.1")
	for _, w := range []string{"172.16.9.2/30", "default via 172.16.9.1", `10\.0\.2\.`, "ip link set"} {
		if !strings.Contains(s, w) {
			t.Errorf("guest tap script missing %q:\n%s", w, s)
		}
	}
}

func TestSSHQuoteJoin(t *testing.T) {
	if got := sshQuote("a'b"); got != `'a'\''b'` {
		t.Errorf("sshQuote = %q", got)
	}
	if got := sshJoin([]string{"bash", "-lc", "echo hi"}); got != `'bash' '-lc' 'echo hi'` {
		t.Errorf("sshJoin = %q", got)
	}
}

func TestSanitizeHostname(t *testing.T) {
	cases := map[string]string{
		"oline-debian13": "oline-debian13",
		"my engagement!": "my-engagement",
		"":               "oline",
		"---":            "oline",
	}
	for in, want := range cases {
		if got := sanitizeHostname(in); got != want {
			t.Errorf("sanitizeHostname(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestBuildEgressRules(t *testing.T) {
	s := buildEgressRules(1001, 1002, 1003, []string{"10.0.0.0/24", "1.2.3.4"}, nil, false)
	for _, want := range []string{
		"--uid-owner 1001 -d 10.0.0.0/24 -j ACCEPT", // scope allows target
		"--uid-owner 1001 -j DROP",                  // scope drops the rest
		"--uid-owner 1002 -d 1.2.3.4 -j DROP",       // net drops the target
		"--uid-owner 1002 -j ACCEPT",                // net allows the internet
		"--uid-owner 1003 -j DROP",                  // iso: nothing
		"ip6tables",                                 // IPv6 egress blocked
		"APPLIED",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("egress rules missing %q:\n%s", want, s)
		}
	}
}

func TestSanitizeCIDRs(t *testing.T) {
	got := sanitizeCIDRs([]string{"10.0.0.0/24", "1.2.3.4", "1.2.3.4/33", "bad;rm", "2001:db8::/32", ""})
	want := []string{"10.0.0.0/24", "1.2.3.4"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("sanitizeCIDRs = %v, want %v", got, want)
	}
}

func TestCloudInitUserData(t *testing.T) {
	ud := cloudInitUserData("ssh-ed25519 AAAAC3Nz... oline")
	for _, want := range []string{"#cloud-config", "name: " + sshProvisionUser, "NOPASSWD:ALL", "ssh-ed25519 AAAAC3Nz", "ssh_pwauth: false"} {
		if !strings.Contains(ud, want) {
			t.Errorf("user-data missing %q:\n%s", want, ud)
		}
	}
}

func TestMakeSeedISO(t *testing.T) {
	q := &qemuSandbox{imagesDir: t.TempDir()}
	_, authLine, _, err := genSSHKey()
	if err != nil {
		t.Fatal(err)
	}
	seed, err := q.makeSeedISO("oline-test", authLine)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(seed)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) == 0 {
		t.Fatal("seed ISO is empty")
	}
	// The NoCloud volume identifier must be CIDATA for cloud-init to pick it up.
	if !strings.Contains(string(data), "CIDATA") {
		t.Error("seed ISO missing CIDATA volume identifier")
	}
	if filepath.Dir(seed) != q.runDir() {
		t.Errorf("seed written to %q, want under %q", seed, q.runDir())
	}
}

func TestGenSSHKey(t *testing.T) {
	signer, authLine, keyPEM, err := genSSHKey()
	if err != nil {
		t.Fatal(err)
	}
	if signer == nil {
		t.Fatal("nil signer")
	}
	if !strings.HasPrefix(authLine, "ssh-ed25519 ") {
		t.Errorf("authorized_keys line = %q, want ssh-ed25519 prefix", authLine)
	}
	// The private key must round-trip from its persisted PEM, so a resumed VM can
	// reuse the key cloud-init injected on first boot.
	if len(keyPEM) == 0 {
		t.Fatal("no key PEM returned")
	}
	loaded, err := loadSSHKey(keyPEM)
	if err != nil {
		t.Fatalf("loadSSHKey: %v", err)
	}
	if string(loaded.PublicKey().Marshal()) != string(signer.PublicKey().Marshal()) {
		t.Error("reloaded key does not match the original")
	}
}
