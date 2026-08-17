// Package calendar is a minimal client for the standard Google Calendar API
// v3 (https://developers.google.com/workspace/calendar/api/v3/reference) —
// used instead of Google's own hosted Calendar MCP server
// (calendarmcp.googleapis.com) because that product is gated behind
// enrollment in the Google Workspace Developer Preview Program, while this
// REST API is generally available and works with the exact same OAuth2
// access token internal/oauth2 already gets, stores, and refreshes per
// household member (see docs/adr/oauth2-layer.md). Confirmed working
// end-to-end against a real account before this package was written: the
// same access token that a Calendar MCP tool call rejected with "The caller
// does not have permission" returns 200 OK against this REST API.
//
// This package has no dependency on internal/httpapi, internal/oauth2, or
// the LLM types — internal/httpapi's executeTool is what resolves a userID
// to an access token (internal/oauth2.Service.AccessToken/RefreshNow) and
// adapts these methods to model-callable tools; this package only knows how
// to make the actual REST calls once it's handed one.
package calendar

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/archer-developer/miranda/internal/httpx"
)

// defaultAPIBase is the real Calendar API v3 origin. Overridable per Client
// (see NewWithAPIBase) only so tests can point at an httptest.Server
// instead — mirrors internal/tavily.Client's apiBase field.
const defaultAPIBase = "https://www.googleapis.com/calendar/v3"

// Client is a small HTTP client for the handful of Calendar API v3
// endpoints Miranda's calendar tools need. Stateless and holds no
// credentials of its own — every call takes the caller's already-resolved
// access token as an explicit argument, since a single process-wide Client
// is shared across every household member (see internal/httpapi's
// executeTool, which resolves the right token per userID before calling
// in).
type Client struct {
	apiBase string
	http    *http.Client
}

// New builds a Client against the real Calendar API.
func New() *Client {
	return NewWithAPIBase(defaultAPIBase)
}

// NewWithAPIBase is New, but against apiBase instead of the real Calendar
// API — for tests.
func NewWithAPIBase(apiBase string) *Client {
	return &Client{
		apiBase: apiBase,
		http:    &http.Client{Timeout: 30 * time.Second},
	}
}

// CalendarListEntry is one calendar a user has access to — the subset of
// Calendar API's CalendarListEntry resource (calendarList.list) Miranda's
// tools expose to the model, enough to resolve a human description ("my
// family calendar") to the calendarId every other method needs.
type CalendarListEntry struct {
	ID         string `json:"id"`
	Summary    string `json:"summary"`
	TimeZone   string `json:"timeZone,omitempty"`
	Primary    bool   `json:"primary,omitempty"`
	AccessRole string `json:"accessRole,omitempty"`
}

// EventDateTime is Calendar API's start/end representation: exactly one of
// DateTime (a timed event, RFC3339) or Date (an all-day event, YYYY-MM-DD)
// is set, matching the API's own documented shape — Miranda's tool schemas
// mirror this instead of collapsing it into a single field, since an
// all-day event has no meaningful time-of-day or timezone.
type EventDateTime struct {
	DateTime string `json:"dateTime,omitempty"`
	Date     string `json:"date,omitempty"`
	TimeZone string `json:"timeZone,omitempty"`
}

// EventAttendee is one entry of Event.Attendees.
type EventAttendee struct {
	Email          string `json:"email"`
	DisplayName    string `json:"displayName,omitempty"`
	ResponseStatus string `json:"responseStatus,omitempty"`
}

// Event is the subset of Calendar API's Event resource Miranda's tools
// read and write — deliberately not the full resource (which has ~40
// fields covering recurrence, conferencing, extended properties, etc.):
// only what create_event/update_event's tool schemas actually let the
// model set, and what list/get results are worth showing back to it.
type Event struct {
	ID          string          `json:"id,omitempty"`
	Summary     string          `json:"summary,omitempty"`
	Description string          `json:"description,omitempty"`
	Location    string          `json:"location,omitempty"`
	Start       *EventDateTime  `json:"start,omitempty"`
	End         *EventDateTime  `json:"end,omitempty"`
	Attendees   []EventAttendee `json:"attendees,omitempty"`
	HTMLLink    string          `json:"htmlLink,omitempty"`
	Status      string          `json:"status,omitempty"`
}

// eventsListResponse is Calendar API's events.list response envelope —
// unexported since callers only ever see the unwrapped []Event ListEvents
// returns.
type eventsListResponse struct {
	Items []Event `json:"items"`
}

// calendarListResponse is calendarList.list's response envelope.
type calendarListResponse struct {
	Items []CalendarListEntry `json:"items"`
}

// ListEventsOptions bounds and filters an events.list call — all optional,
// matching the underlying API's own optional query parameters.
type ListEventsOptions struct {
	// TimeMin/TimeMax are RFC3339 timestamps bounding the returned events'
	// start time — left empty, the API returns events regardless of time.
	TimeMin, TimeMax string
	// Query is a free-text search over summary/description/location/
	// attendees, Calendar API's own "q" parameter.
	Query string
	// MaxResults caps how many events come back; 0 means the API's own
	// default (250).
	MaxResults int
}

// ListCalendars returns every calendar accessToken's owner has access to
// (GET /users/me/calendarList).
func (c *Client) ListCalendars(ctx context.Context, accessToken string) ([]CalendarListEntry, error) {
	var resp calendarListResponse
	if err := c.call(ctx, http.MethodGet, accessToken, "/users/me/calendarList", nil, &resp); err != nil {
		return nil, err
	}
	return resp.Items, nil
}

// ListEvents returns events on calendarID matching opts (GET
// /calendars/{calendarID}/events). calendarID may be "primary" to mean the
// authorizing user's own default calendar, per the API's own convention.
func (c *Client) ListEvents(ctx context.Context, accessToken, calendarID string, opts ListEventsOptions) ([]Event, error) {
	q := url.Values{}
	if opts.TimeMin != "" {
		q.Set("timeMin", opts.TimeMin)
	}
	if opts.TimeMax != "" {
		q.Set("timeMax", opts.TimeMax)
	}
	if opts.Query != "" {
		q.Set("q", opts.Query)
	}
	if opts.MaxResults > 0 {
		q.Set("maxResults", fmt.Sprintf("%d", opts.MaxResults))
	}
	q.Set("singleEvents", "true") // expand recurring events into individual instances
	q.Set("orderBy", "startTime")

	path := "/calendars/" + url.PathEscape(calendarID) + "/events?" + q.Encode()
	var resp eventsListResponse
	if err := c.call(ctx, http.MethodGet, accessToken, path, nil, &resp); err != nil {
		return nil, err
	}
	return resp.Items, nil
}

// FreeBusyRequest is a freeBusy.query request body.
type FreeBusyRequest struct {
	TimeMin string             `json:"timeMin"`
	TimeMax string             `json:"timeMax"`
	Items   []FreeBusyCalendar `json:"items"`
}

// FreeBusyCalendar identifies one calendar to check in a FreeBusyRequest.
type FreeBusyCalendar struct {
	ID string `json:"id"`
}

// FreeBusyInterval is one busy time range in a FreeBusyResponse.
type FreeBusyInterval struct {
	Start string `json:"start"`
	End   string `json:"end"`
}

// FreeBusyCalendarStatus is one requested calendar's busy intervals in a
// FreeBusyResponse.
type FreeBusyCalendarStatus struct {
	Busy []FreeBusyInterval `json:"busy"`
}

// FreeBusyResponse is freeBusy.query's response body — keyed by calendar id,
// matching the API's own shape.
type FreeBusyResponse struct {
	Calendars map[string]FreeBusyCalendarStatus `json:"calendars"`
}

// FreeBusy queries busy intervals across one or more calendars (POST
// /freeBusy) — used to answer "when is everyone free" without the model
// having to fetch and reason over full event lists itself.
func (c *Client) FreeBusy(ctx context.Context, accessToken string, req FreeBusyRequest) (FreeBusyResponse, error) {
	var resp FreeBusyResponse
	if err := c.call(ctx, http.MethodPost, accessToken, "/freeBusy", req, &resp); err != nil {
		return FreeBusyResponse{}, err
	}
	return resp, nil
}

// CreateEvent creates an event on calendarID (POST
// /calendars/{calendarID}/events) and returns the created Event, including
// the id/htmlLink Calendar API assigns.
func (c *Client) CreateEvent(ctx context.Context, accessToken, calendarID string, event Event) (Event, error) {
	var resp Event
	path := "/calendars/" + url.PathEscape(calendarID) + "/events"
	if err := c.call(ctx, http.MethodPost, accessToken, path, event, &resp); err != nil {
		return Event{}, err
	}
	return resp, nil
}

// UpdateEvent patches an existing event (PATCH
// /calendars/{calendarID}/events/{eventID}) — only the fields set on event
// are changed, matching PATCH semantics (as opposed to PUT's full-resource
// replace), so callers only need to populate what's actually changing.
func (c *Client) UpdateEvent(ctx context.Context, accessToken, calendarID, eventID string, event Event) (Event, error) {
	var resp Event
	path := "/calendars/" + url.PathEscape(calendarID) + "/events/" + url.PathEscape(eventID)
	if err := c.call(ctx, "PATCH", accessToken, path, event, &resp); err != nil {
		return Event{}, err
	}
	return resp, nil
}

// DeleteEvent deletes an event (DELETE
// /calendars/{calendarID}/events/{eventID}). Calendar API returns 204 with
// an empty body on success.
func (c *Client) DeleteEvent(ctx context.Context, accessToken, calendarID, eventID string) error {
	path := "/calendars/" + url.PathEscape(calendarID) + "/events/" + url.PathEscape(eventID)
	return c.call(ctx, http.MethodDelete, accessToken, path, nil, nil)
}

// maxErrorBodyRunes bounds how much of a non-JSON error body gets embedded
// in an error return — mirrors internal/tavily and internal/oauth2's own
// caps, since a Calendar API failure surfaces to the model as a tool-call
// error persisted into conversation history.
const maxErrorBodyRunes = 2000

// call is the shared request/response plumbing every method above funnels
// through: builds the full URL, attaches the Bearer token, and decodes
// either a successful response into out (skipped entirely when out is nil,
// e.g. DeleteEvent's empty 204) or Calendar API's documented
// {"error":{"code","message","status"}} envelope into a descriptive error.
func (c *Client) call(ctx context.Context, method, accessToken, path string, payload, out any) error {
	headers := map[string]string{"Authorization": "Bearer " + accessToken}
	status, body, err := httpx.DoJSON(ctx, c.http, method, c.apiBase+path, headers, payload, httpx.DefaultMaxResponseBytes)
	if err != nil {
		return fmt.Errorf("calendar: %s %s: %w", method, path, err)
	}

	if status < 200 || status >= 300 {
		var apiErr struct {
			Error struct {
				Code    int    `json:"code"`
				Message string `json:"message"`
				Status  string `json:"status"`
			} `json:"error"`
		}
		_ = json.Unmarshal(body, &apiErr)
		if apiErr.Error.Message != "" {
			return fmt.Errorf("calendar: %s %s failed (status %d, %s): %s", method, path, status, apiErr.Error.Status, apiErr.Error.Message)
		}
		return fmt.Errorf("calendar: %s %s failed (status %d): %s", method, path, status, truncateRunes(string(body), maxErrorBodyRunes))
	}

	if out == nil || len(body) == 0 {
		return nil
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("calendar: decode %s %s response: %w", method, path, err)
	}
	return nil
}

// truncateRunes cuts s to at most n runes (not bytes, so a multi-byte
// character never gets split mid-encoding), appending a marker when it
// does — copied from internal/tavily rather than shared, since neither
// package depends on the other.
func truncateRunes(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n]) + "... (truncated)"
}
