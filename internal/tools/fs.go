package tools

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"path"
	"strings"

	"github.com/nihaal-prasad/oline/internal/sandbox"
)

// The file tools all operate INSIDE the sandbox via bash, so they honor the same
// isolation + egress boundary as every other command. None of them touch the
// host filesystem.

// q single-quotes a string for safe interpolation into a bash -lc script.
func q(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// offLimits rejects a path that reaches the Windows host through WSL's /mnt
// drive mounts. The file tools are meant to stay inside the sandbox; host access
// is a boundary violation. (The bash deny patterns and disabling drive automount
// are the other layers; this guards the structured file tools directly.)
//
// The input is lexically normalized first so `//mnt/c`, `mnt/c`, and `a/../mnt/c`
// all resolve to `/mnt/...`, while `/mnt/../home` resolves to `/home` and is
// allowed. (Symlink-based escapes can't be caught by a string check and rely on
// the other layers.)
func offLimits(p string) bool {
	p = strings.TrimSpace(p)
	if p == "" {
		return false
	}
	c := path.Clean(p)
	if !strings.HasPrefix(c, "/") {
		c = path.Clean("/" + c) // resolve a relative path against root
	}
	return c == "/mnt" || strings.HasPrefix(c, "/mnt/")
}

// runScript runs a bash script in the sandbox and returns its combined output.
func runScript(ctx context.Context, c Call, script string) (string, sandbox.ExitStatus, error) {
	if c.Sandbox == nil {
		return "", sandbox.ExitStatus{}, fmt.Errorf("no sandbox attached")
	}
	w := &emitWriter{emit: c.Emit}
	st, err := c.Sandbox.Exec(ctx, c.Agent, sandbox.Cmd{Argv: []string{"bash", "-lc", script}, Policy: c.Policy}, w)
	return w.buf.String(), st, err
}

// ---- read ----------------------------------------------------------------

type Read struct{}

func (Read) Name() string { return "read" }
func (Read) Schema() Schema {
	return Schema{
		Name:        "read",
		Description: "Read a file inside the sandbox. Optional offset/limit (line-based) for large files.",
		Parameters:  json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"},"offset":{"type":"integer","description":"0-based start line"},"limit":{"type":"integer","description":"max lines"}},"required":["path"]}`),
	}
}
func (Read) Execute(ctx context.Context, c Call) (Result, error) {
	var a struct {
		Path   string `json:"path"`
		Offset int    `json:"offset"`
		Limit  int    `json:"limit"`
	}
	if err := json.Unmarshal(c.Args, &a); err != nil || a.Path == "" {
		return Result{Content: "read: 'path' is required", IsError: true}, nil
	}
	if offLimits(a.Path) {
		return Result{Content: "read: path is outside the sandbox (host /mnt is off-limits)", IsError: true}, nil
	}
	var script string
	if a.Limit > 0 {
		start, end := a.Offset+1, a.Offset+a.Limit
		script = fmt.Sprintf("sed -n '%d,%dp' -- %s", start, end, q(a.Path))
	} else {
		script = "cat -- " + q(a.Path)
	}
	out, st, err := runScript(ctx, c, script)
	if err != nil {
		return Result{Content: "read: " + err.Error(), IsError: true}, nil
	}
	return Result{Content: out, IsError: st.Code != 0}, nil
}

// ---- write ---------------------------------------------------------------

type Write struct{}

func (Write) Name() string { return "write" }
func (Write) Schema() Schema {
	return Schema{
		Name:        "write",
		Description: "Create or overwrite a file inside the sandbox with the given content.",
		Parameters:  json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"},"content":{"type":"string"}},"required":["path","content"]}`),
	}
}
func (Write) Execute(ctx context.Context, c Call) (Result, error) {
	var a struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal(c.Args, &a); err != nil || a.Path == "" {
		return Result{Content: "write: 'path' and 'content' are required", IsError: true}, nil
	}
	if offLimits(a.Path) {
		return Result{Content: "write: path is outside the sandbox (host /mnt is off-limits)", IsError: true}, nil
	}
	if res, ok := lockFile(c, "write", a.Path); !ok {
		return res, nil
	}
	defer unlockFile(c, a.Path)
	return writeFile(ctx, c, a.Path, a.Content)
}

// lockFile claims a file for the calling agent so two subagents can't write it at
// once. On conflict it returns an error result naming the agent that holds it.
func lockFile(c Call, tool, p string) (Result, bool) {
	if c.Locks == nil {
		return Result{}, true
	}
	if holder, ok := c.Locks.Acquire(lockKey(p), c.Agent); !ok {
		return Result{Content: fmt.Sprintf("%s: %q is being written by another subagent (%s) right now — wait and retry", tool, p, holder), IsError: true}, false
	}
	return Result{}, true
}

func unlockFile(c Call, p string) {
	if c.Locks != nil {
		c.Locks.Release(lockKey(p), c.Agent)
	}
}

// lockKey canonicalizes a file path to a stable lock key so different spellings of
// the same file (x, ./x, /opt/oline/x) collide. The file tools run with the guest
// cwd at the workspace root (workspaceRoot), so a relative path is resolved against
// it. (Best-effort: it does not follow in-guest symlinks.)
func lockKey(p string) string {
	cp := path.Clean(p)
	if !strings.HasPrefix(cp, "/") {
		cp = path.Clean(workspaceRoot + "/" + cp)
	}
	return cp
}

// workspaceRoot is the guest working directory the sandboxed file tools run in
// (relative paths resolve against it).
const workspaceRoot = "/opt/oline"

// writeFile base64-encodes content and decodes it in the sandbox, which is safe
// for arbitrary bytes (no quoting/heredoc hazards).
func writeFile(ctx context.Context, c Call, path, content string) (Result, error) {
	b64 := base64.StdEncoding.EncodeToString([]byte(content))
	script := fmt.Sprintf("mkdir -p \"$(dirname -- %s)\" && printf %%s %s | base64 -d > %s && echo \"wrote %d bytes\"",
		q(path), q(b64), q(path), len(content))
	out, st, err := runScript(ctx, c, script)
	if err != nil {
		return Result{Content: "write: " + err.Error(), IsError: true}, nil
	}
	return Result{Content: strings.TrimSpace(out), IsError: st.Code != 0}, nil
}

// ---- edit ----------------------------------------------------------------

type Edit struct{}

func (Edit) Name() string { return "edit" }
func (Edit) Schema() Schema {
	return Schema{
		Name:        "edit",
		Description: "Replace an exact string in a file inside the sandbox. old_string must be unique unless replace_all is true.",
		Parameters:  json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"},"old_string":{"type":"string"},"new_string":{"type":"string"},"replace_all":{"type":"boolean"}},"required":["path","old_string","new_string"]}`),
	}
}
func (Edit) Execute(ctx context.Context, c Call) (Result, error) {
	var a struct {
		Path       string `json:"path"`
		OldString  string `json:"old_string"`
		NewString  string `json:"new_string"`
		ReplaceAll bool   `json:"replace_all"`
	}
	if err := json.Unmarshal(c.Args, &a); err != nil || a.Path == "" {
		return Result{Content: "edit: 'path', 'old_string', 'new_string' are required", IsError: true}, nil
	}
	if offLimits(a.Path) {
		return Result{Content: "edit: path is outside the sandbox (host /mnt is off-limits)", IsError: true}, nil
	}
	// Hold the file for the whole read-modify-write so a concurrent writer can't
	// slip in between (and so the slower of two editors gets the conflict error).
	if res, ok := lockFile(c, "edit", a.Path); !ok {
		return res, nil
	}
	defer unlockFile(c, a.Path)
	cur, st, err := runScript(ctx, c, "cat -- "+q(a.Path))
	if err != nil {
		return Result{Content: "edit: " + err.Error(), IsError: true}, nil
	}
	if st.Code != 0 {
		return Result{Content: "edit: cannot read " + a.Path, IsError: true}, nil
	}
	// The read above is capped at maxToolOutput; editing past that point would
	// write truncated content back and corrupt the file. Refuse instead.
	if len(cur) >= maxToolOutput {
		return Result{Content: "edit: file is too large to edit safely via this tool (>1 MiB); edit it in the sandbox with sed/awk/perl", IsError: true}, nil
	}
	n := strings.Count(cur, a.OldString)
	if n == 0 {
		return Result{Content: "edit: old_string not found", IsError: true}, nil
	}
	if n > 1 && !a.ReplaceAll {
		return Result{Content: fmt.Sprintf("edit: old_string found %d times; set replace_all or make it unique", n), IsError: true}, nil
	}
	var updated string
	if a.ReplaceAll {
		updated = strings.ReplaceAll(cur, a.OldString, a.NewString)
	} else {
		updated = strings.Replace(cur, a.OldString, a.NewString, 1)
	}
	res, err := writeFile(ctx, c, a.Path, updated)
	if err != nil || res.IsError {
		return res, err
	}
	return Result{Content: fmt.Sprintf("edited %s (%d replacement(s))", a.Path, max(1, boolToN(a.ReplaceAll, n)))}, nil
}

func boolToN(all bool, n int) int {
	if all {
		return n
	}
	return 1
}

// ---- find / grep / glob --------------------------------------------------

type Find struct{}

func (Find) Name() string { return "find" }
func (Find) Schema() Schema {
	return Schema{
		Name:        "find",
		Description: "Find files by name under a path inside the sandbox.",
		Parameters:  json.RawMessage(`{"type":"object","properties":{"name":{"type":"string","description":"filename glob, e.g. *.conf"},"path":{"type":"string","description":"search root (default .)"}},"required":["name"]}`),
	}
}
func (Find) Execute(ctx context.Context, c Call) (Result, error) {
	var a struct {
		Name string `json:"name"`
		Path string `json:"path"`
	}
	if err := json.Unmarshal(c.Args, &a); err != nil || a.Name == "" {
		return Result{Content: "find: 'name' is required", IsError: true}, nil
	}
	if offLimits(a.Path) {
		return Result{Content: "find: path is outside the sandbox (host /mnt is off-limits)", IsError: true}, nil
	}
	path := a.Path
	if path == "" {
		path = "."
	}
	out, _, err := runScript(ctx, c, fmt.Sprintf("find %s -name %s 2>/dev/null | head -n 500", q(path), q(a.Name)))
	if err != nil {
		return Result{Content: "find: " + err.Error(), IsError: true}, nil
	}
	return Result{Content: out}, nil
}

type Grep struct{}

func (Grep) Name() string { return "grep" }
func (Grep) Schema() Schema {
	return Schema{
		Name:        "grep",
		Description: "Search file contents for a regex inside the sandbox (recursive).",
		Parameters:  json.RawMessage(`{"type":"object","properties":{"pattern":{"type":"string"},"path":{"type":"string","description":"search root (default .)"}},"required":["pattern"]}`),
	}
}
func (Grep) Execute(ctx context.Context, c Call) (Result, error) {
	var a struct {
		Pattern string `json:"pattern"`
		Path    string `json:"path"`
	}
	if err := json.Unmarshal(c.Args, &a); err != nil || a.Pattern == "" {
		return Result{Content: "grep: 'pattern' is required", IsError: true}, nil
	}
	if offLimits(a.Path) {
		return Result{Content: "grep: path is outside the sandbox (host /mnt is off-limits)", IsError: true}, nil
	}
	path := a.Path
	if path == "" {
		path = "."
	}
	out, _, err := runScript(ctx, c, fmt.Sprintf("grep -rnI -e %s %s 2>/dev/null | head -n 500", q(a.Pattern), q(path)))
	if err != nil {
		return Result{Content: "grep: " + err.Error(), IsError: true}, nil
	}
	if strings.TrimSpace(out) == "" {
		out = "(no matches)"
	}
	return Result{Content: out}, nil
}

type Glob struct{}

func (Glob) Name() string { return "glob" }
func (Glob) Schema() Schema {
	return Schema{
		Name:        "glob",
		Description: "List paths matching a shell glob inside the sandbox (supports **).",
		Parameters:  json.RawMessage(`{"type":"object","properties":{"pattern":{"type":"string","description":"e.g. **/*.go"}},"required":["pattern"]}`),
	}
}
func (Glob) Execute(ctx context.Context, c Call) (Result, error) {
	var a struct {
		Pattern string `json:"pattern"`
	}
	if err := json.Unmarshal(c.Args, &a); err != nil || a.Pattern == "" {
		return Result{Content: "glob: 'pattern' is required", IsError: true}, nil
	}
	if offLimits(a.Pattern) {
		return Result{Content: "glob: pattern is outside the sandbox (host /mnt is off-limits)", IsError: true}, nil
	}
	out, _, err := runScript(ctx, c, fmt.Sprintf("shopt -s globstar nullglob 2>/dev/null; compgen -G %s 2>/dev/null | head -n 500", q(a.Pattern)))
	if err != nil {
		return Result{Content: "glob: " + err.Error(), IsError: true}, nil
	}
	if strings.TrimSpace(out) == "" {
		out = "(no matches)"
	}
	return Result{Content: out}, nil
}
