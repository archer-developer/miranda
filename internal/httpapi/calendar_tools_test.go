package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	llm "github.com/archer-developer/miranda-llm"
	"github.com/archer-developer/miranda-llm/llmtest"
	"github.com/archer-developer/miranda/internal/calendar"
	"github.com/archer-developer/miranda/internal/config"
	"github.com/archer-developer/miranda/internal/oauth2"
)

func TestOrchestrator_CalendarTools_NotOfferedWhenOAuthNotConfigured(t *testing.T) {
	fakeProvider := llmtest.New("local", llmtest.Response{Text: "Привет!"})
	o, _, _ := newTestOrchestrator(t, fakeProvider) // SetOAuth never called

	_, err := o.Handle(context.Background(), InputRequest{Source: "cli", UserID: "alex", Text: "привет"})
	require.NoError(t, err)

	require.Len(t, fakeProvider.Requests, 1)
	for _, tool := range fakeProvider.Requests[0].Tools {
		require.NotEqual(t, calendarListEventsToolName, tool.Name)
	}
}

// TestOrchestrator_CalendarTools_NotOfferedWhenProviderIsSomethingElse guards
// calendarEnabled's provider-name check specifically: oauth.enabled being
// true isn't enough on its own — offering calendar tools without a
// "google_calendar" provider configured would let the model call
// calendarAccessToken for a provider oauth_authorize can never actually
// connect.
func TestOrchestrator_CalendarTools_NotOfferedWhenProviderIsSomethingElse(t *testing.T) {
	fakeProvider := llmtest.New("local", llmtest.Response{Text: "Привет!"})
	o, _, _ := newTestOrchestrator(t, fakeProvider)

	store, err := oauth2.Open(filepath.Join(t.TempDir(), "oauth.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	otherProvider := oauth2.Provider{Name: "some_other_service", Description: "Something Else", AuthorizeURL: "https://example.com/auth", TokenURL: "https://example.com/token", ClientID: "client-id"}
	oauthSvc := oauth2.NewService(store, []oauth2.Provider{otherProvider}, make([]byte, 32), "https://miranda.example.com", "/oauth/callback", time.Minute, nil)
	o.SetOAuth(oauthSvc, time.Millisecond, time.Millisecond, time.Second)

	_, err = o.Handle(context.Background(), InputRequest{Source: "cli", UserID: "alex", Text: "привет"})
	require.NoError(t, err)

	require.Len(t, fakeProvider.Requests, 1)
	for _, tool := range fakeProvider.Requests[0].Tools {
		require.NotEqual(t, calendarListEventsToolName, tool.Name)
	}
	// oauth_authorize itself should still be offered — oauth is enabled,
	// just for a provider calendar tools don't know about.
	var oauthOffered bool
	for _, tool := range fakeProvider.Requests[0].Tools {
		if tool.Name == oauthAuthorizeToolName {
			oauthOffered = true
		}
	}
	require.True(t, oauthOffered)
}

func TestOrchestrator_CalendarListCalendars_NotConnectedYet(t *testing.T) {
	fakeProvider := llmtest.New("local",
		llmtest.Response{ToolCall: &llm.ToolCall{ID: "call-1", Name: calendarListCalendarsToolName, Arguments: `{}`}},
		llmtest.Response{Text: "Сначала нужно подключить календарь."},
	)
	o, _, _ := newTestOrchestratorWithOAuth(t, fakeProvider, []config.UserConfig{
		{Username: "alex", PasswordHash: "x", FullName: "Alex"},
	})

	resp, err := o.Handle(context.Background(), InputRequest{Source: "cli", UserID: "alex", Text: "покажи календари"})
	require.NoError(t, err)
	require.Equal(t, "Сначала нужно подключить календарь.", resp.Reply)

	toolResultMsg := fakeProvider.Requests[1].Messages[len(fakeProvider.Requests[1].Messages)-1]
	require.Contains(t, toolResultMsg.Content, "not connected")
	require.Contains(t, toolResultMsg.Content, oauthAuthorizeToolName)
}

// newFakeCalendarAPIServer stands in for www.googleapis.com/calendar/v3,
// scripted with the handful of endpoints the tests below exercise.
func newFakeCalendarAPIServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/calendars/primary/events":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"items": []map[string]any{
					{"id": "evt1", "summary": "Встреча", "start": map[string]any{"dateTime": "2026-08-20T15:00:00+03:00"}, "end": map[string]any{"dateTime": "2026-08-20T16:00:00+03:00"}},
				},
			})
		case r.Method == http.MethodPost && r.URL.Path == "/calendars/primary/events":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "evt-new", "summary": body["summary"]})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// authorizeTestUser drives oauthSvc's authorization_code grant for username
// exactly as the real oauth_authorize tool + HTTP callback would — shared
// setup for every test below that needs a connected calendar.
func authorizeTestUser(t *testing.T, o *Orchestrator, username string) {
	t.Helper()
	ctx := context.Background()
	authorizeURL, err := o.oauth.StartAuthorization(ctx, username, "google_calendar")
	require.NoError(t, err)
	state := mustExtractState(t, authorizeURL)
	_, _, err = o.oauth.CompleteAuthorization(ctx, state, "auth-code")
	require.NoError(t, err)
}

func TestOrchestrator_CalendarListEvents_SucceedsAfterAuthorization(t *testing.T) {
	fakeProvider := llmtest.New("local",
		llmtest.Response{ToolCall: &llm.ToolCall{ID: "call-1", Name: calendarListEventsToolName, Arguments: `{"calendar_id":"primary"}`}},
		llmtest.Response{Text: "У тебя встреча в 15:00."},
	)
	o, _, _ := newTestOrchestratorWithOAuth(t, fakeProvider, []config.UserConfig{
		{Username: "alex", PasswordHash: "x", FullName: "Alex"},
	})
	o.calendar = calendar.NewWithAPIBase(newFakeCalendarAPIServer(t).URL)
	authorizeTestUser(t, o, "alex")

	resp, err := o.Handle(context.Background(), InputRequest{Source: "cli", UserID: "alex", Text: "что у меня в календаре?"})
	require.NoError(t, err)
	require.Equal(t, "У тебя встреча в 15:00.", resp.Reply)

	toolResultMsg := fakeProvider.Requests[1].Messages[len(fakeProvider.Requests[1].Messages)-1]
	require.Contains(t, toolResultMsg.Content, "Встреча")
}

func TestOrchestrator_CalendarCreateEvent_Succeeds(t *testing.T) {
	fakeProvider := llmtest.New("local",
		llmtest.Response{ToolCall: &llm.ToolCall{ID: "call-1", Name: calendarCreateEventToolName, Arguments: `{
			"calendar_id":"primary","summary":"Стоматолог",
			"start":{"date_time":"2026-08-20T10:00:00+03:00","time_zone":"Europe/Minsk"},
			"end":{"date_time":"2026-08-20T11:00:00+03:00","time_zone":"Europe/Minsk"}
		}`}},
		llmtest.Response{Text: "Записал к стоматологу."},
	)
	o, _, _ := newTestOrchestratorWithOAuth(t, fakeProvider, []config.UserConfig{
		{Username: "alex", PasswordHash: "x", FullName: "Alex"},
	})
	o.calendar = calendar.NewWithAPIBase(newFakeCalendarAPIServer(t).URL)
	authorizeTestUser(t, o, "alex")

	resp, err := o.Handle(context.Background(), InputRequest{Source: "cli", UserID: "alex", Text: "запиши меня к стоматологу"})
	require.NoError(t, err)
	require.Equal(t, "Записал к стоматологу.", resp.Reply)

	toolResultMsg := fakeProvider.Requests[1].Messages[len(fakeProvider.Requests[1].Messages)-1]
	require.Contains(t, toolResultMsg.Content, "Стоматолог")
	require.Contains(t, toolResultMsg.Content, "evt-new")
}

// TestOrchestrator_CalendarAccessToken_CacheHit covers calendarAccessToken's
// cache-hit branch (CompleteAuthorization already warms oauth2.Service's
// in-memory cache, so this is what a normal calendar tool call resolves
// through). The cache-miss/synchronous-RefreshNow branch is exercised by
// internal/oauth2's own tests (RefreshNow is oauth2.Service's method, not
// reimplemented here) rather than duplicated in this package.
func TestOrchestrator_CalendarAccessToken_CacheHit(t *testing.T) {
	fakeProvider := llmtest.New("local", llmtest.Response{Text: "ok"})
	o, _, _ := newTestOrchestratorWithOAuth(t, fakeProvider, []config.UserConfig{
		{Username: "alex", PasswordHash: "x", FullName: "Alex"},
	})
	authorizeTestUser(t, o, "alex")

	token, ok, err := o.calendarAccessToken(context.Background(), "alex")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, "access-token", token)
}
