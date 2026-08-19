package mcp

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	llm "github.com/archer-developer/miranda-llm"
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
	require.False(t, flaky.IsClosed(), "a retained client must not be closed")
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
	require.True(t, broken.IsClosed(), "an evicted client must be closed")

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

	require.True(t, first.IsClosed())
	require.False(t, second.IsClosed())
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
	require.True(t, broken.IsClosed())
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

// TestManager_KeepConnectedReconnectsAfterSilentDisconnect guards the fix for
// a real gap: eviction used to only happen inside Tools/Call, i.e. only when
// a live agent turn happened to hit the dead session. A server that restarts
// with nobody talking to Miranda at that moment would sit marked "connected"
// forever. KeepConnected must notice on its own (via checkHealth) and
// reconnect, with no Tools/Call in between.
func TestManager_KeepConnectedReconnectsAfterSilentDisconnect(t *testing.T) {
	m := NewManager(nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	dead := mcptest.New("ha").WithListToolsError(fmt.Errorf("connection reset: %w", ErrDisconnected))
	m.SetClient(dead)

	var reconnected atomic.Bool
	connect := func(ctx context.Context) (Client, error) {
		reconnected.Store(true)
		return mcptest.New("ha", llm.ToolDef{Name: "get_state"}), nil
	}

	done := make(chan struct{})
	go func() {
		m.KeepConnected(ctx, "ha", time.Millisecond, 10*time.Millisecond, 50*time.Millisecond, connect)
		close(done)
	}()

	require.Eventually(t, reconnected.Load, time.Second, time.Millisecond,
		"a dead session must be reconnected without any Tools()/Call() ever being made")
	require.True(t, dead.IsClosed(), "the health check must have evicted (and closed) the dead client")

	cancel()
	<-done
}

// TestManager_RemoveClientOnlyEvictsMatchingInstance guards the fix for a
// real race: a caller that observed some client fail must not be able to
// evict a *different* client a concurrent reconnect has since installed
// under the same server name. Before this fix, removeClient deleted
// whatever was currently registered under name, with no check that it was
// still the same instance the caller saw die — see mcp.go's own doc comment
// on removeClient for the full failure scenario (a slow Tools()/Call() on a
// dying client racing against KeepConnected's own health-check-then-reconnect
// cycle for the same server).
func TestManager_RemoveClientOnlyEvictsMatchingInstance(t *testing.T) {
	m := NewManager(nil)
	original := mcptest.New("ha", llm.ToolDef{Name: "get_state"})
	m.SetClient(original)

	// A reconnect races in and replaces "ha" with a fresh, healthy client —
	// SetClient's own old != c guard closes original here.
	replacement := mcptest.New("ha", llm.ToolDef{Name: "get_state"})
	m.SetClient(replacement)
	require.True(t, original.IsClosed())

	// A caller that only ever observed `original` — not the replacement —
	// finally notices it failed and tries to evict it by name.
	m.removeClientKeyed(clientKey{server: "ha"}, original)

	require.True(t, m.HasClient("ha"), "removeClient must not evict a different client instance registered under the same name")
	require.False(t, replacement.IsClosed(), "the live replacement must survive a stale caller's eviction of a different instance")
}

// TestManager_CheckHealthDoesNotEvictOnBareTimeout guards the fix for
// checkHealth conflating "the health check ran out of time" with "the
// server reported it's disconnected" — a client that's merely slow to
// answer ListTools (not dead) must not be evicted, since SDKClient's own
// classifyErr wraps a bare context.DeadlineExceeded as ErrDisconnected the
// same as a real transport failure, and checkHealth has to tell the two
// apart itself via checkCtx.Err() rather than trusting the error alone.
func TestManager_CheckHealthDoesNotEvictOnBareTimeout(t *testing.T) {
	m := NewManager(nil)

	block := make(chan struct{})
	defer close(block) // never closes during the health check itself
	// listErr is wrapped with ErrDisconnected — the same shape a real
	// classifyErr produces from a bare timeout — so this test actually
	// exercises checkHealth telling "my own deadline fired" apart from "the
	// server said it's gone" by timing, not by the error's own type.
	slow := mcptest.New("ha", llm.ToolDef{Name: "get_state"}).
		WithListToolsError(fmt.Errorf("checkHealth's own timeout: %w", ErrDisconnected)).
		WithListToolsBlockingOn(block)
	m.SetClient(slow)

	m.checkHealth(context.Background(), clientKey{server: "ha"}, 20*time.Millisecond)

	require.True(t, m.HasClient("ha"), "a health check that merely timed out must not evict an otherwise-healthy client")
	require.False(t, slow.IsClosed())
}

// TestManager_ToolsForServer covers the input composeTools uses to splice a
// lazy MCP server's real tools back in once the model has called
// load_tool_group for it — see docs/adr/lazy-mcp-tool-loading.md.
func TestManager_ToolsForServer(t *testing.T) {
	ha := mcptest.New("ha", llm.ToolDef{Name: "get_state"})
	diary := mcptest.New("diary", llm.ToolDef{Name: "add_entry"})
	m := NewManager(nil, ha, diary)

	tools := m.ToolsForServer(context.Background(), "diary")

	names := make([]string, len(tools))
	for i, tool := range tools {
		names[i] = tool.Name
	}
	require.Equal(t, []string{"diary_add_entry"}, names)
}

func TestManager_ToolsForServerUnknownServerReturnsNil(t *testing.T) {
	m := NewManager(nil, mcptest.New("ha", llm.ToolDef{Name: "get_state"}))
	require.Nil(t, m.ToolsForServer(context.Background(), "nonexistent"))
}

// TestManager_ToolsExcludingSkipsGivenServers covers composeTools' other
// half: every non-lazy (or not-yet-loaded) server's tools, minus whichever
// names are still pending behind the load_tool_group stub.
func TestManager_ToolsExcludingSkipsGivenServers(t *testing.T) {
	ha := mcptest.New("ha", llm.ToolDef{Name: "get_state"})
	diary := mcptest.New("diary", llm.ToolDef{Name: "add_entry"})
	m := NewManager(nil, ha, diary)

	tools := m.ToolsExcluding(context.Background(), map[string]bool{"diary": true})

	names := make([]string, len(tools))
	for i, tool := range tools {
		names[i] = tool.Name
	}
	require.Equal(t, []string{"ha_get_state"}, names)
}

// TestManager_ToolsExcludingNeverCallsListToolsOnSkippedServer guards the
// point of ToolsExcluding existing at all rather than just filtering
// Tools()'s own output: a lazy server the model hasn't asked to load yet
// must not cost a ListTools RPC on every single turn. diary is rigged to
// block forever on ListTools; if ToolsExcluding ever called it despite the
// skip, this test would run until its own context timeout instead of
// returning immediately.
func TestManager_ToolsExcludingNeverCallsListToolsOnSkippedServer(t *testing.T) {
	ha := mcptest.New("ha", llm.ToolDef{Name: "get_state"})
	block := make(chan struct{}) // deliberately never closed
	diary := mcptest.New("diary", llm.ToolDef{Name: "add_entry"}).WithListToolsBlockingOn(block)
	m := NewManager(nil, ha, diary)

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	start := time.Now()
	tools := m.ToolsExcluding(ctx, map[string]bool{"diary": true})
	require.Less(t, time.Since(start), 100*time.Millisecond, "must not wait on a skipped server's ListTools call")

	names := make([]string, len(tools))
	for i, tool := range tools {
		names[i] = tool.Name
	}
	require.Equal(t, []string{"ha_get_state"}, names)
}

func TestManager_ServerForTool(t *testing.T) {
	m := NewManager(nil, mcptest.New("ha", llm.ToolDef{Name: "get_state"}), mcptest.New("diary", llm.ToolDef{Name: "add_entry"}))

	name, ok := m.ServerForTool("diary_add_entry")
	require.True(t, ok)
	require.Equal(t, "diary", name)

	_, ok = m.ServerForTool("unknown_tool")
	require.False(t, ok)
}

func TestManager_ServerAndTool(t *testing.T) {
	m := NewManager(nil, mcptest.New("ha", llm.ToolDef{Name: "get_state"}), mcptest.New("medical_card", llm.ToolDef{Name: "medical.ask"}))

	server, tool, ok := m.ServerAndTool("medical_card_medical.ask")
	require.True(t, ok)
	require.Equal(t, "medical_card", server)
	require.Equal(t, "medical.ask", tool)

	_, _, ok = m.ServerAndTool("unknown_tool")
	require.False(t, ok)
}
