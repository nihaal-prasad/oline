// Package audit implements oline's tamper-evident, per-engagement audit log.
//
// Every command executed during an engagement — by an agent (bash, bash_bg,
// revshell) or by the operator (their own shell/terminal panes) — is appended as
// one JSON record to a host-side log, one file per engagement. Each record carries
// a SHA-256 hash that covers the record's own fields AND the previous record's
// hash, forming a hash chain: altering, inserting, removing, or reordering any
// record breaks the chain from that point on, which Verify detects. The log lives
// on the HOST (never inside the sandbox), so a compromised target cannot reach it.
//
// Tamper-evidence scope: the chain makes any edit to existing content evident. As
// with any purely local append-only log, someone with write access to the host
// file could still rewrite the whole chain or lop off the newest entries; defending
// against that needs an off-host anchor (e.g. shipping the head hash elsewhere) or
// an HMAC keyed by a secret kept off the box — both natural extensions of this
// format, which already hashes a `prev` field into every record.
package audit

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Entry is one audit record. Field order is fixed (it is part of the hashed bytes),
// and Hash is always the last field: it is zeroed when computing the hash and then
// filled in, so the stored Hash never feeds into its own computation.
type Entry struct {
	Seq        int       `json:"seq"`
	Time       time.Time `json:"time"`
	Engagement string    `json:"engagement"`
	Actor      string    `json:"actor"`  // the agent id that ran it, or "User" for the operator
	Action     string    `json:"action"` // bash | bash_bg | revshell.send | user.shell | …
	Target     string    `json:"target,omitempty"`
	Detail     string    `json:"detail"` // the command line / payload / summary
	Prev       string    `json:"prev"`   // hex SHA-256 of the previous record (empty for the first)
	Hash       string    `json:"hash"`   // hex SHA-256 over this record with Hash zeroed
}

// hashEntry returns the hex SHA-256 over the record with its Hash field zeroed, so
// the hash covers every field including Prev (the chain link) but not itself.
func hashEntry(e Entry) string {
	e.Hash = ""
	b, _ := json.Marshal(e)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// Log is an append-only, hash-chained audit log for one engagement. A nil *Log is
// fully usable — every method no-ops — so a logging failure (e.g. the host dir is
// not writable) never disrupts the engagement. Safe for concurrent use.
type Log struct {
	mu         sync.Mutex
	f          *os.File
	path       string
	engagement string
	seq        int
	prevHash   string
}

// Open opens (creating as needed) the per-engagement audit log at path in append
// mode, recovering the running sequence and chain head from any existing content so
// a resumed engagement extends the same chain. Returns nil on any failure.
func Open(path, engagement string) (*Log, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	seq, prev := 0, ""
	if data, err := os.ReadFile(path); err == nil {
		for _, ln := range bytes.Split(data, []byte("\n")) {
			if len(bytes.TrimSpace(ln)) == 0 {
				continue
			}
			var e Entry
			if json.Unmarshal(ln, &e) == nil {
				seq, prev = e.Seq, e.Hash
			}
		}
	}
	// 0600: audit records can contain whatever was typed/run, so keep the file as
	// private as the secrets and activity-log files.
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	return &Log{f: f, path: path, engagement: engagement, seq: seq, prevHash: prev}, nil
}

// Append writes one command record, extending the hash chain. Thread-safe; a nil
// *Log no-ops.
func (l *Log) Append(actor, action, target, detail string) error {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	e := Entry{
		Seq:        l.seq + 1,
		Time:       time.Now().UTC(),
		Engagement: l.engagement,
		Actor:      actor,
		Action:     action,
		Target:     target,
		Detail:     detail,
		Prev:       l.prevHash,
	}
	e.Hash = hashEntry(e)
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	if _, err := l.f.Write(append(b, '\n')); err != nil {
		return err
	}
	l.seq = e.Seq
	l.prevHash = e.Hash
	return nil
}

// Command appends a command record and swallows any write error, so logging can be
// wired into hot paths without complicating their error handling. Nil-safe.
func (l *Log) Command(actor, action, target, detail string) {
	if l == nil {
		return
	}
	_ = l.Append(actor, action, target, detail)
}

// Path returns the log file path (empty for a nil log).
func (l *Log) Path() string {
	if l == nil {
		return ""
	}
	return l.path
}

// Close closes the underlying file. Nil-safe.
func (l *Log) Close() error {
	if l == nil || l.f == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.f.Close()
}

// Result is the outcome of verifying a log's hash chain.
type Result struct {
	OK       bool   // the chain is intact
	Entries  int    // number of records read
	BrokenAt int    // 1-based record where verification failed (0 if OK)
	Reason   string // why it failed (empty if OK)
}

// Verify re-reads an audit log and checks its hash chain end to end: every record's
// stored hash must match a recomputation, each record's Prev must equal the prior
// record's hash, and the sequence numbers must increase by one. The first failure
// is reported. An empty or absent-but-readable log verifies as OK with zero entries.
func Verify(path string) (Result, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Result{}, err
	}
	prev := ""
	n := 0
	for _, ln := range bytes.Split(data, []byte("\n")) {
		if len(bytes.TrimSpace(ln)) == 0 {
			continue
		}
		n++
		var e Entry
		if err := json.Unmarshal(ln, &e); err != nil {
			return Result{Entries: n - 1, BrokenAt: n, Reason: "record is not valid JSON"}, nil
		}
		if hashEntry(e) != e.Hash {
			return Result{Entries: n - 1, BrokenAt: n, Reason: "hash mismatch — this record was altered"}, nil
		}
		if e.Prev != prev {
			return Result{Entries: n - 1, BrokenAt: n, Reason: "chain broken — a record was inserted, removed, or reordered"}, nil
		}
		if e.Seq != n {
			return Result{Entries: n - 1, BrokenAt: n, Reason: fmt.Sprintf("sequence gap — record says seq %d, expected %d", e.Seq, n)}, nil
		}
		prev = e.Hash
	}
	return Result{OK: true, Entries: n}, nil
}
