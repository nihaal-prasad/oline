package sandbox

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
)

// genSSHKey makes an ed25519 keypair for talking to one VM. The signer drives the
// SSH client, authLine is the authorized_keys entry injected via cloud-init, and
// keyPEM is the PKCS#8 private key so a persisted (resumable) VM can reuse it on a
// later boot (cloud-init only injects the key on first boot).
func genSSHKey() (signer ssh.Signer, authLine string, keyPEM []byte, err error) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, "", nil, err
	}
	signer, err = ssh.NewSignerFromKey(priv)
	if err != nil {
		return nil, "", nil, err
	}
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return nil, "", nil, err
	}
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	authLine = strings.TrimSpace(string(ssh.MarshalAuthorizedKey(signer.PublicKey())))
	return signer, authLine, keyPEM, nil
}

// loadSSHKey parses a PKCS#8 private-key PEM (as written by genSSHKey) back into a
// signer, for resuming a persisted VM with the key it was first provisioned with.
func loadSSHKey(keyPEM []byte) (ssh.Signer, error) {
	return ssh.ParsePrivateKey(keyPEM)
}

// sshConfig builds a client config for the given user and key. The host key check
// is intentionally skipped: we connect only to our own just-booted VM over a
// loopback hostfwd, and the guest generates a fresh host key on first boot that
// we have no prior knowledge of.
func sshConfig(user string, signer ssh.Signer) *ssh.ClientConfig {
	return &ssh.ClientConfig{
		User:            user,
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         5 * time.Second,
	}
}

// dialSSH polls 127.0.0.1:port until SSH is up (the guest is booting + running
// cloud-init) or ctx is done.
func dialSSH(ctx context.Context, port int, cfg *ssh.ClientConfig) (*ssh.Client, error) {
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	var lastErr error
	for {
		if ctx.Err() != nil {
			if lastErr != nil {
				return nil, fmt.Errorf("sandbox/qemu: ssh never came up: %w (last: %v)", ctx.Err(), lastErr)
			}
			return nil, ctx.Err()
		}
		d := net.Dialer{Timeout: 3 * time.Second}
		conn, err := d.DialContext(ctx, "tcp", addr)
		if err == nil {
			c, chans, reqs, herr := ssh.NewClientConn(conn, addr, cfg)
			if herr == nil {
				return ssh.NewClient(c, chans, reqs), nil
			}
			_ = conn.Close()
			lastErr = herr
		} else {
			lastErr = err
		}
		select {
		case <-ctx.Done():
		case <-time.After(2 * time.Second):
		}
	}
}

// lockedWriter serializes concurrent writes. golang.org/x/crypto/ssh copies the
// session's stdout and stderr on separate goroutines, so when both are pointed at
// one sink that sink must be synchronized.
type lockedWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

// runSSHCombined merges stdout and stderr into one writer (safely synchronized)
// — the right choice for commands whose combined text output we want. For a
// command whose stdout is a binary stream (CopyOut/Backup), use runSSH with a
// SEPARATE stderr sink so stderr can't corrupt the stream.
func runSSHCombined(ctx context.Context, client *ssh.Client, cmd string, stdin io.Reader, out io.Writer) (int, error) {
	lw := &lockedWriter{w: out}
	return runSSH(ctx, client, cmd, stdin, lw, lw)
}

// runSSH runs one remote command, wiring stdin and separate stdout/stderr, and
// returns its exit code. Cancelling ctx kills the remote command and returns
// ctx.Err().
func runSSH(ctx context.Context, client *ssh.Client, cmd string, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	sess, err := client.NewSession()
	if err != nil {
		return -1, err
	}
	defer sess.Close()
	sess.Stdin = stdin
	sess.Stdout = stdout
	sess.Stderr = stderr

	done := make(chan error, 1)
	go func() { done <- sess.Run(cmd) }()
	select {
	case err := <-done:
		if ee, ok := err.(*ssh.ExitError); ok {
			return ee.ExitStatus(), nil // a non-zero exit is a result, not an infra error
		}
		if err != nil {
			return -1, err
		}
		return 0, nil
	case <-ctx.Done():
		_ = sess.Signal(ssh.SIGKILL)
		_ = sess.Close()
		<-done
		return -1, ctx.Err()
	}
}

// sshQuote single-quotes a string for safe use inside a remote shell command.
func sshQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// sshJoin quotes and space-joins an argv for remote execution.
func sshJoin(argv []string) string {
	parts := make([]string, len(argv))
	for i, a := range argv {
		parts[i] = sshQuote(a)
	}
	return strings.Join(parts, " ")
}

// freePort returns an unused loopback TCP port for an SSH hostfwd.
func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}
