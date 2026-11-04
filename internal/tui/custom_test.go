package tui

import (
	"context"
	"testing"

	"github.com/nihaal-prasad/oline/internal/tools"
	"github.com/nihaal-prasad/oline/internal/tools/custom"
	"github.com/nihaal-prasad/oline/internal/wire"
)

type fakeCustomTool struct{}

func (fakeCustomTool) Name() string         { return "zz_fake_custom_tool" }
func (fakeCustomTool) Schema() tools.Schema { return tools.Schema{Name: "zz_fake_custom_tool"} }
func (fakeCustomTool) Execute(context.Context, tools.Call) (tools.Result, error) {
	return tools.Result{}, nil
}

func TestCustomGoToolRegistered(t *testing.T) {
	custom.Register(fakeCustomTool{})
	m := testModel(t)
	reg := wire.BuildRegistry(m.store)
	if _, ok := reg.Get("zz_fake_custom_tool"); !ok {
		t.Error("a custom.Register'd tool should be in the registry")
	}
	if !wire.CustomToolNames(m.store)["zz_fake_custom_tool"] {
		t.Error("custom tool should be listed as an exec tool (blocked in watcher mode)")
	}
}
