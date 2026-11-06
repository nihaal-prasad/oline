package engine

import (
	"encoding/json"
	"testing"
)

// TestBashHostEscapeDenyList pins the config-independent host-escape backstop
// (hostEscapeRe, via bashDenied): any WSL /mnt drive access and the high-value
// Windows-interop binaries are blocked, while ordinary in-sandbox pentest work —
// including commands that merely NAME a .exe file (generating or inspecting a Windows
// payload) — is allowed. Locking this guards against a regression that weakens the
// backstop AND against an over-broadening that would break legitimate payload work.
func TestBashHostEscapeDenyList(t *testing.T) {
	s := &Session{} // no config deny patterns: exercise hostEscapeRe alone
	denied := func(cmd string) bool {
		b, _ := json.Marshal(struct {
			Cmd string `json:"cmd"`
		}{cmd})
		return s.bashDenied(string(b))
	}

	blocked := []string{
		"cat /mnt/c/Windows/win.ini",     // host drive read
		"cd /mnt/c && ls",                // host drive, mid-command
		"ls /mnt/d",                      // any drive letter, not just c
		"cat /mnt/c",                     // drive at end of line
		"echo hi > /mnt/e/loot",          // host drive write
		"/MNT/C/tool",                    // case-insensitive
		"cmd.exe /c dir",                 // interop
		"powershell.exe -c whoami",       // interop
		"pwsh.exe -File x",               // interop
		"wsl.exe --list",                 // re-entering WSL
		"explorer.exe .",                 // interop
		"foo; POWERSHELL.EXE -enc ZZ",    // interop, uppercase, mid-command
		"timeout 5 && cmd.exe /c whoami", // interop after a benign prefix
	}
	for _, cmd := range blocked {
		if !denied(cmd) {
			t.Errorf("command must be blocked by the host-escape backstop: %q", cmd)
		}
	}

	allowed := []string{
		"ls -la",
		"nmap -sV -p- 10.0.0.5",
		"curl -s http://10.0.0.5/",
		"grep -r TODO .",
		// Payload work: a .exe as a FILENAME argument to a Linux tool must stay allowed.
		"msfvenom -p windows/meterpreter/reverse_tcp -f exe -o /opt/oline/engagements/e1/shell.exe",
		"file /opt/oline/engagements/e1/sample.exe",
		"strings payload.exe | head",
		"mv a.exe b.exe",
		// Lookalikes that are not the host mount.
		"ls /mnt-backup",
		"cat /opt/oline/mnt/notes",
	}
	for _, cmd := range allowed {
		if denied(cmd) {
			t.Errorf("legitimate in-sandbox command must NOT be blocked: %q", cmd)
		}
	}

	// A malformed / empty arguments object is not treated as a denied command (the
	// dispatcher handles the real parse error separately).
	if s.bashDenied(`{bad json`) || s.bashDenied(`{"cmd":""}`) {
		t.Error("malformed or empty bash args must not count as a denied command")
	}
}
