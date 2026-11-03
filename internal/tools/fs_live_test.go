package tools_test

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/nihaal-prasad/oline/internal/sandbox"
	"github.com/nihaal-prasad/oline/internal/tools"
)

// TestLiveFileTools exercises the sandbox-backed file tools end-to-end against a
// real sandbox (WSL). Gated on OLINE_TEST_WSL; benign (operates in a scratch dir
// under the sandbox user's home and cleans up).
func TestLiveFileTools(t *testing.T) {
	distro := os.Getenv("OLINE_TEST_WSL")
	if distro == "" {
		t.Skip("OLINE_TEST_WSL not set; skipping live file-tools test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	h, err := sandbox.NewWSL().Open(ctx, distro)
	if err != nil {
		t.Fatal(err)
	}
	dir := "oline-selftest"
	call := func(args string, h sandbox.Handle) tools.Call {
		return tools.Call{Agent: "t", Args: json.RawMessage(args), Sandbox: h}
	}
	defer func() {
		_, _ = tools.Bash{}.Execute(context.Background(), call(`{"cmd":"rm -rf `+dir+`"}`, h))
	}()

	run := func(tool tools.Tool, args string) tools.Result {
		r, err := tool.Execute(ctx, call(args, h))
		if err != nil {
			t.Fatalf("%s: %v", tool.Name(), err)
		}
		return r
	}

	if r := run(tools.Write{}, `{"path":"`+dir+`/a.txt","content":"hello\nworld\n"}`); r.IsError {
		t.Fatalf("write failed: %s", r.Content)
	}
	if r := run(tools.Read{}, `{"path":"`+dir+`/a.txt"}`); !strings.Contains(r.Content, "hello") || !strings.Contains(r.Content, "world") {
		t.Fatalf("read back wrong: %q", r.Content)
	}
	if r := run(tools.Edit{}, `{"path":"`+dir+`/a.txt","old_string":"world","new_string":"kali"}`); r.IsError {
		t.Fatalf("edit failed: %s", r.Content)
	}
	if r := run(tools.Read{}, `{"path":"`+dir+`/a.txt"}`); !strings.Contains(r.Content, "kali") || strings.Contains(r.Content, "world") {
		t.Fatalf("edit not applied: %q", r.Content)
	}
	if r := run(tools.Grep{}, `{"pattern":"kali","path":"`+dir+`"}`); !strings.Contains(r.Content, "a.txt") {
		t.Fatalf("grep missed the match: %q", r.Content)
	}
	if r := run(tools.Glob{}, `{"pattern":"`+dir+`/*.txt"}`); !strings.Contains(r.Content, "a.txt") {
		t.Fatalf("glob missed the file: %q", r.Content)
	}
	if r := run(tools.Find{}, `{"name":"a.txt","path":"`+dir+`"}`); !strings.Contains(r.Content, "a.txt") {
		t.Fatalf("find missed the file: %q", r.Content)
	}
	t.Log("file tools round-trip OK")
}
