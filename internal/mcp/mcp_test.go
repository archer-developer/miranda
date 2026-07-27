package mcp

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/archer-developer/miranda/internal/llm"
	"github.com/archer-developer/miranda/internal/mcp/mcptest"
)

func TestManager_PrefixesToolsPerServer(t *testing.T) {
	ha := mcptest.New("ha", llm.ToolDef{Name: "get_state"})
	noolite := mcptest.New("noolite", llm.ToolDef{Name: "get_state"}) // deliberately colliding name

	m := NewManager(nil, ha, noolite)
	tools := m.Tools(context.Background())

	names := make([]string, len(tools))
	for i, tool := range tools {
		names[i] = tool.Name
	}
	require.ElementsMatch(t, []string{"ha_get_state", "noolite_get_state"}, names)
}

func TestManager_CallRoutesToOwningServer(t *testing.T) {
	ha := mcptest.New("ha", llm.ToolDef{Name: "get_state"}).WithResult("get_state", "living_room: on")
	noolite := mcptest.New("noolite", llm.ToolDef{Name: "get_state"}).WithResult("get_state", "kitchen: off")

	m := NewManager(nil, ha, noolite)

	result, err := m.Call(context.Background(), "noolite_get_state", `{}`)
	require.NoError(t, err)
	require.Equal(t, "kitchen: off", result)
	require.Len(t, ha.Calls, 0)
	require.Len(t, noolite.Calls, 1)
}

func TestManager_CallUnknownToolReturnsError(t *testing.T) {
	m := NewManager(nil, mcptest.New("ha"))
	_, err := m.Call(context.Background(), "unknown_tool", `{}`)
	require.Error(t, err)
}

// TestManager_ToolsSkipsServerThatFailsToList guards the fix for a real
// incident: an HA MCP server that's temporarily unreachable used to fail
// Manager.Tools entirely, which took down every other server's tools (and
// the whole turn) with it. One bad server must only cost its own tools.
func TestManager_ToolsSkipsServerThatFailsToList(t *testing.T) {
	ha := mcptest.New("ha", llm.ToolDef{Name: "get_state"})
	broken := mcptest.New("noolite", llm.ToolDef{Name: "get_state"}).WithListToolsError(errors.New("connection refused"))

	m := NewManager(nil, ha, broken)
	tools := m.Tools(context.Background())

	names := make([]string, len(tools))
	for i, tool := range tools {
		names[i] = tool.Name
	}
	require.ElementsMatch(t, []string{"ha_get_state"}, names)
}

// TestManager_ToolsRetainsClientOnTransientError guards the other side of
// that same fix: a plain application-level error (not wrapped with
// ErrDisconnected) means the session itself is still alive, so it must NOT
// evict the client — just skip its tools for this one call and retry next
// turn on the same session, instead of paying for a full reconnect cycle
// over a one-off error.
func TestManager_ToolsRetainsClientOnTransientError(t *testing.T) {
	flaky := mcptest.New("ha", llm.ToolDef{Name: "get_state"}).WithListToolsError(errors.New("rate limited"))

	m := NewManager(nil, flaky)
	tools := m.Tools(context.Background())

	require.Empty(t, tools)
	require.True(t, m.HasClient("ha"), "a transient error must not evict a still-connected client")
	require.False(t, flaky.Closed, "a retained client must not be closed")
}

// TestManager_ToolsEvictsFailedServerUntilReconnected verifies the
// disconnection half of the fix: a server whose ListTools error is wrapped
// with ErrDisconnected is closed and dropped from the Manager so the
// background reconnect loop (HasClient) notices and retries it, and
// SetClient brings its tools back with no restart needed.
func TestManager_ToolsEvictsFailedServerUntilReconnected(t *testing.T) {
	broken := mcptest.New("noolite").WithListToolsError(fmt.Errorf("session dead: %w", ErrDisconnected))
	m := NewManager(nil, broken)

	tools := m.Tools(context.Background())
	require.Empty(t, tools)
	require.False(t, m.HasClient("noolite"))
	require.True(t, broken.Closed, "an evicted client must be closed")

	m.SetClient(mcptest.New("noolite", llm.ToolDef{Name: "get_state"}))
	require.True(t, m.HasClient("noolite"))

	tools = m.Tools(context.Background())
	require.Len(t, tools, 1)
	require.Equal(t, "noolite_get_state", tools[0].Name)
}

// TestManager_SetClientClosesReplacedLiveClient guards against the leak a
// misconfigured duplicate server name would otherwise cause: if two connect
// attempts for the same name race and both succeed, the one being replaced
// must be closed, not silently dropped.
func TestManager_SetClientClosesReplacedLiveClient(t *testing.T) {
	first := mcptest.New("ha", llm.ToolDef{Name: "get_state"})
	m := NewManager(nil, first)

	second := mcptest.New("ha", llm.ToolDef{Name: "get_state"})
	m.SetClient(second)

	require.True(t, first.Closed)
	require.False(t, second.Closed)
}

// TestManager_CallEvictsOnDisconnectedError verifies Call reacts to a
// disconnection discovered mid-call the same way Tools does, so the
// reconnect loop doesn't have to wait for the next Tools() call to notice.
func TestManager_CallEvictsOnDisconnectedError(t *testing.T) {
	broken := mcptest.New("ha").WithError("get_state", fmt.Errorf("session dead: %w", ErrDisconnected))
	m := NewManager(nil, broken)

	_, err := m.Call(context.Background(), "ha_get_state", `{}`)
	require.Error(t, err)
	require.False(t, m.HasClient("ha"))
	require.True(t, broken.Closed)
}

// TestManager_KeepConnectedRetriesUntilSuccess exercises the reconnect loop
// itself (previously untested — it lived as an entrypoint-only function with
// no test coverage): it should keep calling connect until one succeeds, then
// stop attempting new connections once HasClient is true.
func TestManager_KeepConnectedRetriesUntilSuccess(t *testing.T) {
	m := NewManager(nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var attempts atomic.Int32
	connect := func(ctx context.Context) (Client, error) {
		n := attempts.Add(1)
		if n < 3 {
			return nil, errors.New("not up yet")
		}
		return mcptest.New("ha", llm.ToolDef{Name: "get_state"}), nil
	}

	done := make(chan struct{})
	go func() {
		m.KeepConnected(ctx, "ha", time.Millisecond, 10*time.Millisecond, 50*time.Millisecond, connect)
		close(done)
	}()

	require.Eventually(t, func() bool { return m.HasClient("ha") }, time.Second, time.Millisecond)
	require.GreaterOrEqual(t, attempts.Load(), int32(3))

	settled := attempts.Load()
	time.Sleep(20 * time.Millisecond)
	require.Equal(t, settled, attempts.Load(), "must stop attempting new connections once connected")

	cancel()
	<-done
}
