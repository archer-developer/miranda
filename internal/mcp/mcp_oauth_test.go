package mcp

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	llm "github.com/archer-developer/miranda-llm"
	"github.com/archer-developer/miranda/internal/mcp/mcptest"
)

// TestManager_EnsureUserSession_IndependentClientsPerUser is the core
// correctness property the whole OAuth-gated per-user design exists for:
// two different users hitting the same OAuth-gated server name must end up
// with two entirely independent MCP sessions, never sharing one connection.
func TestManager_EnsureUserSession_IndependentClientsPerUser(t *testing.T) {
	m := NewManager(nil)
	m.SetOAuthServers(map[string]bool{"google_calendar": true})
	m.SetBackgroundContext(context.Background())

	alice := mcptest.New("google_calendar", llm.ToolDef{Name: "list_events"}).WithResult("list_events", "alice's events")
	bob := mcptest.New("google_calendar", llm.ToolDef{Name: "list_events"}).WithResult("list_events", "bob's events")

	m.EnsureUserSession("google_calendar", "alice", time.Millisecond, time.Millisecond, time.Second, func(ctx context.Context) (Client, error) {
		return alice, nil
	})
	m.EnsureUserSession("google_calendar", "bob", time.Millisecond, time.Millisecond, time.Second, func(ctx context.Context) (Client, error) {
		return bob, nil
	})

	require.Eventually(t, func() bool { return m.HasUserClient("google_calendar", "alice") }, time.Second, time.Millisecond)
	require.Eventually(t, func() bool { return m.HasUserClient("google_calendar", "bob") }, time.Second, time.Millisecond)

	aliceResult, err := m.CallForUser(context.Background(), "google_calendar_list_events", "{}", "alice")
	require.NoError(t, err)
	require.Equal(t, "alice's events", aliceResult)

	bobResult, err := m.CallForUser(context.Background(), "google_calendar_list_events", "{}", "bob")
	require.NoError(t, err)
	require.Equal(t, "bob's events", bobResult)

	require.Len(t, alice.Calls, 1, "alice's call must never reach bob's session")
	require.Len(t, bob.Calls, 1, "bob's call must never reach alice's session")
}

// TestManager_EnsureUserSession_Idempotent guards EnsureUserSession's
// goroutine-spawn idempotency: calling it twice for the same (server, user)
// must not start a second background loop (which would race two connect
// attempts against each other).
func TestManager_EnsureUserSession_Idempotent(t *testing.T) {
	m := NewManager(nil)
	m.SetOAuthServers(map[string]bool{"google_calendar": true})
	m.SetBackgroundContext(context.Background())

	var connectCalls int
	connect := func(ctx context.Context) (Client, error) {
		connectCalls++
		return mcptest.New("google_calendar", llm.ToolDef{Name: "list_events"}), nil
	}

	for i := 0; i < 5; i++ {
		m.EnsureUserSession("google_calendar", "alice", time.Millisecond, time.Millisecond, time.Second, connect)
	}

	require.Eventually(t, func() bool { return m.HasUserClient("google_calendar", "alice") }, time.Second, time.Millisecond)
	// Give any accidental second loop a chance to also connect before asserting.
	time.Sleep(20 * time.Millisecond)
	require.Equal(t, 1, connectCalls, "EnsureUserSession must only start one background loop per (server, user)")
}

// TestManager_ToolsForUser_NarrowsToOAuthGatedServer covers the listing
// side: a user with no live session for an OAuth-gated server sees no tools
// from it, while a globally-connected (non-OAuth) server is unaffected by
// userID.
func TestManager_ToolsForUser_NarrowsToOAuthGatedServer(t *testing.T) {
	m := NewManager(nil)
	m.SetOAuthServers(map[string]bool{"google_calendar": true})
	m.SetBackgroundContext(context.Background())

	ha := mcptest.New("ha", llm.ToolDef{Name: "get_state"})
	m.SetClient(ha) // global server, unaffected by per-user keying

	// No session yet for alice on google_calendar.
	tools := m.ToolsForUser(context.Background(), "alice")
	names := toolNames(tools)
	require.ElementsMatch(t, []string{"ha_get_state"}, names)

	calendar := mcptest.New("google_calendar", llm.ToolDef{Name: "list_events"})
	m.EnsureUserSession("google_calendar", "alice", time.Millisecond, time.Millisecond, time.Second, func(ctx context.Context) (Client, error) {
		return calendar, nil
	})
	require.Eventually(t, func() bool { return m.HasUserClient("google_calendar", "alice") }, time.Second, time.Millisecond)

	tools = m.ToolsForUser(context.Background(), "alice")
	require.ElementsMatch(t, []string{"ha_get_state", "google_calendar_list_events"}, toolNames(tools))

	// bob still has no session at all for google_calendar.
	tools = m.ToolsForUser(context.Background(), "bob")
	require.ElementsMatch(t, []string{"ha_get_state"}, toolNames(tools))
}

// TestManager_ExistingGlobalPathUnaffectedByOAuthServers is a regression
// guard: a server never marked via SetOAuthServers must behave exactly as
// before regardless of what userID is passed in.
func TestManager_ExistingGlobalPathUnaffectedByOAuthServers(t *testing.T) {
	m := NewManager(nil)
	ha := mcptest.New("ha", llm.ToolDef{Name: "get_state"}).WithResult("get_state", "kitchen: off")
	m.SetClient(ha)

	for _, userID := range []string{"", "alice", "bob"} {
		result, err := m.CallForUser(context.Background(), "ha_get_state", "{}", userID)
		require.NoError(t, err)
		require.Equal(t, "kitchen: off", result)
	}
	require.Len(t, ha.Calls, 3)
}

func toolNames(tools []llm.ToolDef) []string {
	names := make([]string, len(tools))
	for i, t := range tools {
		names[i] = t.Name
	}
	return names
}
