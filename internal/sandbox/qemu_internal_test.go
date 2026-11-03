package sandbox

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

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
	_, authLine, err := genSSHKey()
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
	signer, authLine, err := genSSHKey()
	if err != nil {
		t.Fatal(err)
	}
	if signer == nil {
		t.Fatal("nil signer")
	}
	if !strings.HasPrefix(authLine, "ssh-ed25519 ") {
		t.Errorf("authorized_keys line = %q, want ssh-ed25519 prefix", authLine)
	}
}
