//go:build calendar_e2e

// This file is excluded from the normal `go test ./...` sweep (build tag
// calendar_e2e) — it makes real network calls against the live Google
// Calendar API using a household member's already-completed OAuth2
// authorization, and (transiently) creates/updates/deletes a real event on
// a real calendar. Run on demand:
//
//	go test -tags calendar_e2e ./test/integration/... -run TestCalendarE2E -v
//
// Requires, all satisfied automatically on a checkout that already has a
// connected Google Calendar (e.g. this repo's own dev setup after running
// oauth_authorize once for archer):
//   - .env (or MIRANDA_ENV_FILE) with OAUTH_MASTER_KEY, GOOGLE_OAUTH_CLIENT_ID,
//     GOOGLE_OAUTH_CLIENT_SECRET set — the same secrets the running service uses.
//   - data/oauth.db (or MIRANDA_OAUTH_DB) containing a completed
//     google_calendar authorization for the user named by MIRANDA_OAUTH_USER
//     (default "archer") — i.e. someone has already run oauth_authorize once;
//     this test reuses that stored refresh token and never drives a browser
//     consent flow itself.
//   - A calendar named "TEST" (or MIRANDA_E2E_CALENDAR_SUMMARY) in that
//     account's calendar list — kept separate from any real calendar
//     (primary included) so a test bug can't corrupt real data.
//
// Missing secrets/db/token cause a clean t.Skip, not a failure — this test
// is meant to be runnable on demand wherever those happen to be available,
// not to gate CI.
package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	llm "github.com/archer-developer/miranda-llm"
	"github.com/archer-developer/miranda-llm/llmtest"
	"github.com/archer-developer/miranda-llm/router"
	agentloop "github.com/archer-developer/miranda/internal/agent_loop"
	"github.com/archer-developer/miranda/internal/config"
	"github.com/archer-developer/miranda/internal/envfile"
	"github.com/archer-developer/miranda/internal/history"
	"github.com/archer-developer/miranda/internal/httpapi"
	"github.com/archer-developer/miranda/internal/hub"
	"github.com/archer-developer/miranda/internal/mcp"
	"github.com/archer-developer/miranda/internal/memory"
	"github.com/archer-developer/miranda/internal/oauth2"
)

const calendarE2EProvider = "google_calendar"

// envOrDefault returns the environment variable named key, or fallback if
// it's unset or empty.
func envOrDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// realGoogleCalendarService loads this checkout's real, already-completed
// google_calendar OAuth2 authorization (see this file's own doc comment for
// exactly what it needs) and forces one real refresh, so the rest of the
// test always starts from a known-fresh access token rather than depending
// on however long ago the stored one was minted. Skips the test (not a
// failure) when any prerequisite is missing.
func realGoogleCalendarService(t *testing.T) (*oauth2.Service, string) {
	t.Helper()

	_ = envfile.Load(envOrDefault("MIRANDA_ENV_FILE", "../../.env"))

	dbPath := envOrDefault("MIRANDA_OAUTH_DB", "../../data/oauth.db")
	if _, err := os.Stat(dbPath); err != nil {
		t.Skipf("calendar e2e: %s not found — copy the real oauth.db locally to run this test (%v)", dbPath, err)
	}

	masterKey, err := oauth2.LoadMasterKey("OAUTH_MASTER_KEY")
	if err != nil {
		t.Skipf("calendar e2e: %v", err)
	}

	clientID := os.Getenv("GOOGLE_OAUTH_CLIENT_ID")
	clientSecret := os.Getenv("GOOGLE_OAUTH_CLIENT_SECRET")
	if clientID == "" || clientSecret == "" {
		t.Skip("calendar e2e: GOOGLE_OAUTH_CLIENT_ID/GOOGLE_OAUTH_CLIENT_SECRET not set")
	}

	store, err := oauth2.Open(dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })

	username := envOrDefault("MIRANDA_OAUTH_USER", "archer")
	authorized, err := store.HasToken(context.Background(), username, calendarE2EProvider)
	require.NoError(t, err)
	if !authorized {
		t.Skipf("calendar e2e: no stored %s token for user %q — run oauth_authorize once first", calendarE2EProvider, username)
	}

	provider := oauth2.Provider{
		Name:         calendarE2EProvider,
		AuthorizeURL: "https://accounts.google.com/o/oauth2/v2/auth",
		TokenURL:     "https://oauth2.googleapis.com/token",
		ClientID:     clientID,
		ClientSecret: clientSecret,
		PKCE:         true,
	}
	svc := oauth2.NewService(store, []oauth2.Provider{provider}, masterKey, "https://miranda.example.com", "/oauth/callback", time.Minute, nil)

	_, ok, err := svc.RefreshNow(context.Background(), username, calendarE2EProvider)
	require.NoError(t, err)
	require.True(t, ok, "expected a usable stored refresh token for %s/%s", username, calendarE2EProvider)

	return svc, username
}

// calendarE2EHarness is this file's own minimal httptest harness — kept
// separate from agent_loop_test.go's harness/newHarness/post (same
// package, always compiled) since this one needs oauthSvc wired in via
// SetOAuth before serving, which that one has no hook for, and duplicate
// type/function names in the same package would collide.
type calendarE2EHarness struct {
	server *httptest.Server
}

// newCalendarE2EHarness builds a real Orchestrator wired to a real,
// production-pointed internal/calendar.Client (Orchestrator's default —
// nothing to override for this test) and the real oauthSvc from
// realGoogleCalendarService, so calendarAccessToken resolves a genuine
// Google access token. The only fake is the LLM provider itself — see
// callCalendarTool, which scripts exactly one tool call per harness.
func newCalendarE2EHarness(t *testing.T, provider *llmtest.FakeProvider, oauthSvc *oauth2.Service) *calendarE2EHarness {
	t.Helper()

	historyStore, err := history.Open(filepath.Join(t.TempDir(), "miranda.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = historyStore.Close() })

	memoryStore, err := memory.New(t.TempDir())
	require.NoError(t, err)

	r, err := router.New([]llm.Provider{provider}, nil, "")
	require.NoError(t, err)

	eventHub := hub.New(100)
	orchestrator := agentloop.NewOrchestrator(
		r, mcp.NewManager(nil), historyStore, memoryStore, nil, eventHub, nil,
		config.AgentConfig{}, config.MemoryConfig{}, config.TTSConfig{}, 100, "debug",
	)
	orchestrator.SetOAuth(oauthSvc, time.Minute, time.Minute, 10*time.Second)

	server := httpapi.NewServer(orchestrator, eventHub, "", nil, nil, nil, nil)
	ts := httptest.NewServer(server)
	t.Cleanup(ts.Close)

	return &calendarE2EHarness{server: ts}
}

func (h *calendarE2EHarness) post(t *testing.T, req agentloop.InputRequest) agentloop.InputResponse {
	t.Helper()
	body, err := json.Marshal(req)
	require.NoError(t, err)

	resp, err := http.Post(h.server.URL+"/api/v1/input", "application/json", bytes.NewReader(body))
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var out agentloop.InputResponse
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&out))
	return out
}

// callCalendarTool drives exactly one calendar_* tool call through the real
// HTTP endpoint (POST /api/v1/input -> Orchestrator.Handle -> executeTool
// -> executeCalendarTool -> internal/calendar.Client -> the real Google
// Calendar API) and returns the raw tool-result text the model would have
// seen — the same string a real LLM's next Chat call is fed.
func callCalendarTool(t *testing.T, oauthSvc *oauth2.Service, username, toolName string, args map[string]any) string {
	t.Helper()

	argsJSON, err := json.Marshal(args)
	require.NoError(t, err)

	provider := llmtest.New("e2e",
		llmtest.Response{ToolCall: &llm.ToolCall{ID: "call-1", Name: toolName, Arguments: string(argsJSON)}},
		llmtest.Response{Text: "ok"},
	)
	h := newCalendarE2EHarness(t, provider, oauthSvc)

	resp := h.post(t, agentloop.InputRequest{Source: "cli", UserID: username, Text: "e2e: " + toolName})
	require.Equal(t, "ok", resp.Reply)
	require.Len(t, provider.Requests, 2)

	msgs := provider.Requests[1].Messages
	result := msgs[len(msgs)-1].Content
	t.Logf("%s(%s) -> %s", toolName, argsJSON, result)
	require.NotContains(t, result, "error:", "tool call should not have failed")
	return result
}

// callCalendarToolIgnoringResult is callCalendarTool minus the *testing.T
// dependency (so it's safe to call from a t.Cleanup closure that must run
// even after the test itself failed) and minus any assertion on the
// outcome — used only for the best-effort delete safety net in
// TestCalendarE2E_FullLifecycle.
func callCalendarToolIgnoringResult(oauthSvc *oauth2.Service, username, toolName string, args map[string]any) error {
	argsJSON, err := json.Marshal(args)
	if err != nil {
		return err
	}

	tmpDir, err := os.MkdirTemp("", "miranda-e2e-cleanup-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmpDir)

	historyStore, err := history.Open(filepath.Join(tmpDir, "miranda.db"))
	if err != nil {
		return err
	}
	defer historyStore.Close()

	memoryStore, err := memory.New(tmpDir)
	if err != nil {
		return err
	}

	provider := llmtest.New("e2e-cleanup",
		llmtest.Response{ToolCall: &llm.ToolCall{ID: "cleanup", Name: toolName, Arguments: string(argsJSON)}},
		llmtest.Response{Text: "ok"},
	)
	r, err := router.New([]llm.Provider{provider}, nil, "")
	if err != nil {
		return err
	}

	orchestrator := agentloop.NewOrchestrator(
		r, mcp.NewManager(nil), historyStore, memoryStore, nil, hub.New(10), nil,
		config.AgentConfig{}, config.MemoryConfig{}, config.TTSConfig{}, 100, "debug",
	)
	orchestrator.SetOAuth(oauthSvc, time.Minute, time.Minute, 10*time.Second)

	_, err = orchestrator.Handle(context.Background(), agentloop.InputRequest{Source: "cli", UserID: username, Text: "cleanup"})
	return err
}

var calendarLineRe = regexp.MustCompile(`^- (.+) \| id=(\S+)`)

// findCalendarID scans calendar_list_calendars' output (one "- Summary |
// id=ID[ | primary]" line per calendar — see calendar_tools.go's own
// rendering) for the calendar named wantSummary.
func findCalendarID(t *testing.T, listing, wantSummary string) string {
	t.Helper()
	for _, line := range strings.Split(listing, "\n") {
		m := calendarLineRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		summary := strings.TrimSuffix(m[1], " | primary")
		if summary == wantSummary {
			return m[2]
		}
	}
	t.Fatalf("calendar e2e: no calendar named %q found in:\n%s", wantSummary, listing)
	return ""
}

var eventIDRe = regexp.MustCompile(`id=(\S+)`)

// findEventID extracts the id=... field formatEvent renders into
// create_event/update_event's result text.
func findEventID(t *testing.T, result string) string {
	t.Helper()
	m := eventIDRe.FindStringSubmatch(result)
	require.NotNil(t, m, "expected an id=... field in: %s", result)
	return m[1]
}

// TestCalendarE2E_FullLifecycle drives all six calendar_* tools in sequence
// against the real Google Calendar API on a dedicated, disposable test
// calendar: list_calendars (also used to resolve the test calendar's id),
// create_event, list_events (confirms the created event appears),
// update_event, freebusy (confirms the updated event's slot is reported
// busy), and finally delete_event (confirms it's gone from a follow-up
// list_events). See this file's own doc comment for how to run it and what
// it needs.
func TestCalendarE2E_FullLifecycle(t *testing.T) {
	oauthSvc, username := realGoogleCalendarService(t)
	testCalendarSummary := envOrDefault("MIRANDA_E2E_CALENDAR_SUMMARY", "TEST")

	calendarsResult := callCalendarTool(t, oauthSvc, username, "calendar_list_calendars", map[string]any{})
	calendarID := findCalendarID(t, calendarsResult, testCalendarSummary)

	start := time.Now().Add(24 * time.Hour).Truncate(time.Hour).UTC()
	end := start.Add(30 * time.Minute)
	summary := fmt.Sprintf("Miranda E2E test %s", start.Format("20060102T150405Z"))

	createResult := callCalendarTool(t, oauthSvc, username, "calendar_create_event", map[string]any{
		"calendar_id": calendarID,
		"summary":     summary,
		"location":    "Miranda E2E",
		"start":       map[string]any{"date_time": start.Format(time.RFC3339)},
		"end":         map[string]any{"date_time": end.Format(time.RFC3339)},
	})
	require.Contains(t, createResult, summary)
	eventID := findEventID(t, createResult)

	// Best-effort safety net: if a later assertion aborts the test before
	// the explicit delete_event call below runs, this still cleans the
	// real calendar up. An "already deleted" error from Google is exactly
	// this cleanup's success case too, so its own error is ignored.
	t.Cleanup(func() {
		_ = callCalendarToolIgnoringResult(oauthSvc, username, "calendar_delete_event", map[string]any{
			"calendar_id": calendarID, "event_id": eventID,
		})
	})

	listResult := callCalendarTool(t, oauthSvc, username, "calendar_list_events", map[string]any{
		"calendar_id": calendarID,
		"time_min":    start.Add(-time.Hour).Format(time.RFC3339),
		"time_max":    end.Add(time.Hour).Format(time.RFC3339),
	})
	require.Contains(t, listResult, summary, "the just-created event should appear in list_events")

	updatedSummary := summary + " (updated)"
	updateResult := callCalendarTool(t, oauthSvc, username, "calendar_update_event", map[string]any{
		"calendar_id": calendarID,
		"event_id":    eventID,
		"summary":     updatedSummary,
	})
	require.Contains(t, updateResult, updatedSummary)

	freebusyResult := callCalendarTool(t, oauthSvc, username, "calendar_freebusy", map[string]any{
		"calendar_ids": []string{calendarID},
		"time_min":     start.Add(-time.Hour).Format(time.RFC3339),
		"time_max":     end.Add(time.Hour).Format(time.RFC3339),
	})
	require.Contains(t, freebusyResult, "busy:", "the calendar should show busy time covering the created event")
	require.NotContains(t, freebusyResult, "free the whole time")

	deleteResult := callCalendarTool(t, oauthSvc, username, "calendar_delete_event", map[string]any{
		"calendar_id": calendarID, "event_id": eventID,
	})
	require.Equal(t, "deleted", strings.TrimSpace(deleteResult))

	verifyResult := callCalendarTool(t, oauthSvc, username, "calendar_list_events", map[string]any{
		"calendar_id": calendarID,
		"time_min":    start.Add(-time.Hour).Format(time.RFC3339),
		"time_max":    end.Add(time.Hour).Format(time.RFC3339),
	})
	require.NotContains(t, verifyResult, eventID, "the deleted event must no longer appear in list_events")
}
