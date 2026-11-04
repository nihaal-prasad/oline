package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/nihaal-prasad/oline/internal/sandbox"
)

// CommandTool is a custom tool defined entirely in config (no recompile): a named
// tool whose call runs a shell command inside the sandbox. The operator declares its
// name, description, a JSON-schema for its arguments, and a command template; when an
// agent calls it, each {{arg}} placeholder is replaced with the shell-quoted value of
// that argument and the command runs as the calling agent's user/egress policy, with
// output streamed back like bash. Only agents that list its name in their `tools:`
// frontmatter can call it. See docs/extending.md.
type CommandTool struct {
	ToolName    string          // the tool's name (what agents list and call)
	Desc        string          // one-line description shown to the model
	Params      json.RawMessage // JSON Schema object for the arguments ("" => no args)
	CommandTmpl string          // shell command template with {{arg}} placeholders
	Dir         string          // guest working directory ("" => the engagement workspace)
}

func (t CommandTool) Name() string { return t.ToolName }

func (t CommandTool) Schema() Schema {
	params := t.Params
	if len(params) == 0 {
		params = json.RawMessage(`{"type":"object","properties":{}}`)
	}
	return Schema{Name: t.ToolName, Description: t.Desc, Parameters: params}
}

func (t CommandTool) Execute(ctx context.Context, c Call) (Result, error) {
	if c.Sandbox == nil {
		return Result{Content: t.ToolName + ": no sandbox attached", IsError: true}, nil
	}
	args := map[string]any{}
	if len(c.Args) > 0 {
		if err := json.Unmarshal(c.Args, &args); err != nil {
			return Result{Content: t.ToolName + ": invalid arguments: " + err.Error(), IsError: true}, nil
		}
	}
	cmd := renderCommand(t.CommandTmpl, args)
	w := &emitWriter{emit: c.Emit}
	st, err := c.Sandbox.Exec(ctx, c.Agent, sandbox.Cmd{Argv: []string{"bash", "-lc", cmd}, Dir: t.Dir, Policy: c.Policy}, w)
	if err != nil {
		return Result{Content: w.buf.String() + "\n" + err.Error(), IsError: true}, nil
	}
	return Result{Content: fmt.Sprintf("%s\n[exit %d]", w.buf.String(), st.Code), IsError: st.Code != 0}, nil
}

var cmdPlaceholder = regexp.MustCompile(`\{\{\s*([A-Za-z0-9_]+)\s*\}\}`)

// renderCommand substitutes {{name}} placeholders in a command template with the
// SHELL-QUOTED value of the matching argument, so an argument value can never break
// out of its quoting into the command. A missing argument becomes an empty string.
func renderCommand(tmpl string, args map[string]any) string {
	return cmdPlaceholder.ReplaceAllStringFunc(tmpl, func(m string) string {
		name := cmdPlaceholder.FindStringSubmatch(m)[1]
		v, ok := args[name]
		s := ""
		if ok && v != nil {
			s = fmt.Sprint(v)
		}
		return shQuoteArg(s)
	})
}

// shQuoteArg single-quotes a value for safe interpolation into a bash command.
func shQuoteArg(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
