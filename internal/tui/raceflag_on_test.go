//go:build race

package tui

// raceDetectorOn is true when the test binary is built with -race. A couple of
// terminal-teardown tests skip under it to avoid a KNOWN, benign data race inside
// github.com/charmbracelet/x/vt@v0.1.0: (*Emulator).Read checks an unguarded `closed`
// bool that (*Emulator).Close writes, and Read can only be unblocked by that same
// Close, so the reply-pump cannot be stopped without the two touching `closed`
// concurrently. The underlying io.Pipe is itself concurrency-safe, so the race is
// harmless in practice, but there is no in-tree fix short of vendoring a patched vt.
const raceDetectorOn = true
