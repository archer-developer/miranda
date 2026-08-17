package calendar

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/stretchr/testify/require"
)

func newTestClient(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return NewWithAPIBase(server.URL)
}

func TestListCalendars_GetsWithAuthHeader(t *testing.T) {
	var gotPath, gotMethod, gotAuth string
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotMethod, gotAuth = r.URL.Path, r.Method, r.Header.Get("Authorization")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"items": []map[string]any{{"id": "primary", "summary": "Main", "primary": true}},
		})
	})

	cals, err := c.ListCalendars(context.Background(), "test-token")
	require.NoError(t, err)
	require.Equal(t, "/users/me/calendarList", gotPath)
	require.Equal(t, http.MethodGet, gotMethod)
	require.Equal(t, "Bearer test-token", gotAuth)
	require.Len(t, cals, 1)
	require.Equal(t, "primary", cals[0].ID)
	require.True(t, cals[0].Primary)
}

func TestListEvents_SetsQueryParams(t *testing.T) {
	var gotPath, gotQuery string
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotQuery = r.URL.Path, r.URL.RawQuery
		_ = json.NewEncoder(w).Encode(map[string]any{
			"items": []map[string]any{{"id": "evt1", "summary": "Standup"}},
		})
	})

	events, err := c.ListEvents(context.Background(), "test-token", "primary", ListEventsOptions{
		TimeMin: "2026-08-18T00:00:00Z", Query: "standup", MaxResults: 10,
	})
	require.NoError(t, err)
	require.Equal(t, "/calendars/primary/events", gotPath)

	q, err := url.ParseQuery(gotQuery)
	require.NoError(t, err)
	require.Equal(t, "2026-08-18T00:00:00Z", q.Get("timeMin"))
	require.Equal(t, "standup", q.Get("q"))
	require.Equal(t, "10", q.Get("maxResults"))
	require.Equal(t, "true", q.Get("singleEvents"))
	require.Len(t, events, 1)
	require.Equal(t, "Standup", events[0].Summary)
}

func TestListEvents_EscapesCalendarIDInPath(t *testing.T) {
	var gotPath string
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_ = json.NewEncoder(w).Encode(map[string]any{"items": []map[string]any{}})
	})

	_, err := c.ListEvents(context.Background(), "test-token", "family@group.calendar.google.com", ListEventsOptions{})
	require.NoError(t, err)
	require.Equal(t, "/calendars/family@group.calendar.google.com/events", gotPath)
}

func TestCreateEvent_PostsEventBody(t *testing.T) {
	var gotMethod, gotPath string
	var gotBody map[string]any
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		require.NoError(t, json.NewDecoder(r.Body).Decode(&gotBody))
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "new-evt", "summary": gotBody["summary"]})
	})

	created, err := c.CreateEvent(context.Background(), "test-token", "primary", Event{
		Summary: "Dentist",
		Start:   &EventDateTime{DateTime: "2026-08-20T10:00:00+03:00", TimeZone: "Europe/Minsk"},
		End:     &EventDateTime{DateTime: "2026-08-20T11:00:00+03:00", TimeZone: "Europe/Minsk"},
	})
	require.NoError(t, err)
	require.Equal(t, http.MethodPost, gotMethod)
	require.Equal(t, "/calendars/primary/events", gotPath)
	require.Equal(t, "Dentist", gotBody["summary"])
	require.Equal(t, "new-evt", created.ID)
}

func TestUpdateEvent_PatchesEvent(t *testing.T) {
	var gotMethod, gotPath string
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "evt1", "summary": "Updated"})
	})

	updated, err := c.UpdateEvent(context.Background(), "test-token", "primary", "evt1", Event{Summary: "Updated"})
	require.NoError(t, err)
	require.Equal(t, "PATCH", gotMethod)
	require.Equal(t, "/calendars/primary/events/evt1", gotPath)
	require.Equal(t, "Updated", updated.Summary)
}

func TestDeleteEvent_SendsDeleteAndIgnoresEmptyBody(t *testing.T) {
	var gotMethod, gotPath string
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		w.WriteHeader(http.StatusNoContent)
	})

	err := c.DeleteEvent(context.Background(), "test-token", "primary", "evt1")
	require.NoError(t, err)
	require.Equal(t, http.MethodDelete, gotMethod)
	require.Equal(t, "/calendars/primary/events/evt1", gotPath)
}

func TestFreeBusy_PostsRequestBody(t *testing.T) {
	var gotBody map[string]any
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, json.NewDecoder(r.Body).Decode(&gotBody))
		_ = json.NewEncoder(w).Encode(map[string]any{
			"calendars": map[string]any{
				"primary": map[string]any{"busy": []map[string]any{{"start": "2026-08-20T10:00:00Z", "end": "2026-08-20T11:00:00Z"}}},
			},
		})
	})

	resp, err := c.FreeBusy(context.Background(), "test-token", FreeBusyRequest{
		TimeMin: "2026-08-20T00:00:00Z",
		TimeMax: "2026-08-21T00:00:00Z",
		Items:   []FreeBusyCalendar{{ID: "primary"}},
	})
	require.NoError(t, err)
	require.Equal(t, "2026-08-20T00:00:00Z", gotBody["timeMin"])
	require.Len(t, resp.Calendars["primary"].Busy, 1)
}

func TestCall_NonOKStatusReturnsGoogleErrorMessage(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]any{"code": 403, "message": "The caller does not have permission", "status": "PERMISSION_DENIED"},
		})
	})

	_, err := c.ListCalendars(context.Background(), "test-token")
	require.Error(t, err)
	require.Contains(t, err.Error(), "The caller does not have permission")
	require.Contains(t, err.Error(), "PERMISSION_DENIED")
}

func TestCall_NonJSONErrorBodyIsTruncatedNotDecoded(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("<html>bad gateway</html>"))
	})

	_, err := c.ListCalendars(context.Background(), "test-token")
	require.Error(t, err)
	require.Contains(t, err.Error(), "bad gateway")
}
