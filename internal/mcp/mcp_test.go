package mcp

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/archer-developer/miranda/internal/llm"
	"github.com/archer-developer/miranda/internal/mcp/mcptest"
)

func TestManager_PrefixesToolsPerServer(t *testing.T) {
	ha := mcptest.New("ha", llm.ToolDef{Name: "get_state"})
	noolite := mcptest.New("noolite", llm.ToolDef{Name: "get_state"}) // deliberately colliding name

	m := NewManager(ha, noolite)
	tools, err := m.Tools(context.Background())
	require.NoError(t, err)

	names := make([]string, len(tools))
	for i, tool := range tools {
		names[i] = tool.Name
	}
	require.ElementsMatch(t, []string{"ha_get_state", "noolite_get_state"}, names)
}

func TestManager_CallRoutesToOwningServer(t *testing.T) {
	ha := mcptest.New("ha", llm.ToolDef{Name: "get_state"}).WithResult("get_state", "living_room: on")
	noolite := mcptest.New("noolite", llm.ToolDef{Name: "get_state"}).WithResult("get_state", "kitchen: off")

	m := NewManager(ha, noolite)

	result, err := m.Call(context.Background(), "noolite_get_state", `{}`)
	require.NoError(t, err)
	require.Equal(t, "kitchen: off", result)
	require.Len(t, ha.Calls, 0)
	require.Len(t, noolite.Calls, 1)
}

func TestManager_CallUnknownToolReturnsError(t *testing.T) {
	m := NewManager(mcptest.New("ha"))
	_, err := m.Call(context.Background(), "unknown_tool", `{}`)
	require.Error(t, err)
}
