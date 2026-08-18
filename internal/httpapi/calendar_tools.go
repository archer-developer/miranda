package httpapi

// Tool defs and dispatch for internal/calendar — Google Calendar access via
// the plain REST API instead of Google's own hosted Calendar MCP server
// (see internal/calendar's package doc comment for why: that product is
// gated behind Developer Preview Program enrollment, confirmed on a real
// account, while this REST API works today with the exact same OAuth2
// token internal/oauth2 already manages). Kept in its own file, same
// reasoning as oauth.go/schedule.go/telegram.go, rather than growing
// agent_loop.go's executeTool further.
//
// Unlike an MCP-routed OAuth-gated tool (see MCPServerExtension.OAuthProvider
// in agent_loop.go's executeTool), there's no per-user session to keep warm
// in the background here — a calendar tool call resolves straight to
// internal/oauth2.Service.AccessToken (cache-only) and falls back to a
// synchronous RefreshNow on a cache miss. That's a deliberate, narrow
// exception to "executeTool never blocks on OAuth network I/O": the
// MCP-session design carved out that exception by doing the blocking
// refresh in EnsureUserSession's background goroutine specifically because
// executeTool itself must not block; a native calendar tool has no such
// background goroutine to hand it off to, and the tool call is already
// about to make its own (larger) network round trip to the Calendar API
// regardless, so one more short round trip ahead of it isn't a new category
// of latency — see RefreshNow's own doc comment for the MCP-session
// exception this mirrors.

import (
	"context"
	"encoding/json"
	"fmt"

	llm "github.com/archer-developer/miranda-llm"

	"github.com/archer-developer/miranda/internal/calendar"
)

const calendarListCalendarsToolName = "calendar_list_calendars"
const calendarListEventsToolName = "calendar_list_events"
const calendarFreeBusyToolName = "calendar_freebusy"
const calendarCreateEventToolName = "calendar_create_event"
const calendarUpdateEventToolName = "calendar_update_event"
const calendarDeleteEventToolName = "calendar_delete_event"

// calendarToolGroupDescription is google_calendar's one-line entry inside
// load_tool_group's stub (see availableTools) — shown to the model instead
// of the six calendar_* tools' real schemas until it decides the domain is
// actually relevant to the current turn.
const calendarToolGroupDescription = "Google Calendar: list calendars, list/search events, check free/busy, create/update/delete events."

// calendarToolNames is every tool name calendarToolDefs can advertise — its
// own copy (not shared with ReservedToolNames' return value) since the two
// lists are built for different purposes and there's no reason to couple
// them structurally.
func calendarToolNames() []string {
	return []string{
		calendarListCalendarsToolName,
		calendarListEventsToolName,
		calendarFreeBusyToolName,
		calendarCreateEventToolName,
		calendarUpdateEventToolName,
		calendarDeleteEventToolName,
	}
}

// eventDateTimeSchema is the JSON schema fragment shared by create/update's
// "start"/"end" parameters — Calendar API's own EventDateTime shape (see
// internal/calendar.EventDateTime's doc comment on why exactly one of
// date_time/date is set, never both).
var eventDateTimeSchema = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"date_time": map[string]any{
			"type":        "string",
			"description": "RFC3339 timestamp for a timed event, e.g. \"2026-08-20T10:00:00+03:00\" — omit for an all-day event",
		},
		"date": map[string]any{
			"type":        "string",
			"description": "YYYY-MM-DD for an all-day event — omit for a timed event",
		},
		"time_zone": map[string]any{
			"type":        "string",
			"description": "IANA timezone, e.g. \"Europe/Minsk\" — only meaningful alongside date_time",
		},
	},
}

// calendarToolDefs returns the six calendar_* tools' llm.ToolDefs —
// advertised in availableTools only when Orchestrator.calendarEnabled().
func calendarToolDefs() []llm.ToolDef {
	return []llm.ToolDef{
		{
			Name: calendarListCalendarsToolName,
			Description: "List the current user's Google calendars — use this first to resolve a description " +
				"like \"my family calendar\" to the calendar_id every other calendar_* tool needs.",
			Parameters: map[string]any{"type": "object", "properties": map[string]any{}},
		},
		{
			Name: calendarListEventsToolName,
			Description: "List events on one of the current user's Google calendars, optionally filtered by a time " +
				"range or free-text search.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"calendar_id": map[string]any{
						"type":        "string",
						"description": "calendar_id from calendar_list_calendars, or \"primary\" for the user's default calendar",
					},
					"time_min": map[string]any{"type": "string", "description": "RFC3339 lower bound on event start time — omit for no lower bound"},
					"time_max": map[string]any{"type": "string", "description": "RFC3339 upper bound on event start time — omit for no upper bound"},
					"query":    map[string]any{"type": "string", "description": "free-text search over summary/description/location/attendees"},
				},
				"required": []string{"calendar_id"},
			},
		},
		{
			Name: calendarFreeBusyToolName,
			Description: "Check busy time intervals across one or more of the current user's calendars in a time " +
				"range — use this to answer \"when am I free\" without listing and reasoning over full events yourself.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"calendar_ids": map[string]any{
						"type":        "array",
						"items":       map[string]any{"type": "string"},
						"description": "calendar_ids to check, from calendar_list_calendars — usually just [\"primary\"]",
					},
					"time_min": map[string]any{"type": "string", "description": "RFC3339 lower bound"},
					"time_max": map[string]any{"type": "string", "description": "RFC3339 upper bound"},
				},
				"required": []string{"calendar_ids", "time_min", "time_max"},
			},
		},
		{
			Name:        calendarCreateEventToolName,
			Description: "Create an event on one of the current user's Google calendars.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"calendar_id": map[string]any{"type": "string", "description": "calendar_id from calendar_list_calendars, or \"primary\""},
					"summary":     map[string]any{"type": "string", "description": "event title"},
					"description": map[string]any{"type": "string"},
					"location":    map[string]any{"type": "string"},
					"start":       eventDateTimeSchema,
					"end":         eventDateTimeSchema,
				},
				"required": []string{"calendar_id", "summary", "start", "end"},
			},
		},
		{
			Name: calendarUpdateEventToolName,
			Description: "Update an existing event on one of the current user's Google calendars — only the fields " +
				"you provide are changed, everything else stays as it was.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"calendar_id": map[string]any{"type": "string", "description": "calendar_id from calendar_list_calendars, or \"primary\""},
					"event_id":    map[string]any{"type": "string", "description": "from calendar_list_events"},
					"summary":     map[string]any{"type": "string"},
					"description": map[string]any{"type": "string"},
					"location":    map[string]any{"type": "string"},
					"start":       eventDateTimeSchema,
					"end":         eventDateTimeSchema,
				},
				"required": []string{"calendar_id", "event_id"},
			},
		},
		{
			Name:        calendarDeleteEventToolName,
			Description: "Delete an event from one of the current user's Google calendars.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"calendar_id": map[string]any{"type": "string", "description": "calendar_id from calendar_list_calendars, or \"primary\""},
					"event_id":    map[string]any{"type": "string", "description": "from calendar_list_events"},
				},
				"required": []string{"calendar_id", "event_id"},
			},
		},
	}
}

// calendarAccessToken resolves userID's current Google Calendar access
// token: cache-only first (the common case — see this file's own doc
// comment on why a synchronous RefreshNow here is an accepted exception),
// falling back to a synchronous refresh on a cache miss. ok=false with a
// nil error means "never authorized" — the caller should point the model at
// oauth_authorize, same UX as the MCP-routed OAuth path.
func (o *Orchestrator) calendarAccessToken(ctx context.Context, userID string) (token string, ok bool, err error) {
	if token, ok := o.oauth.AccessToken(userID, googleCalendarProvider); ok {
		return token, true, nil
	}
	token, ok, err = o.oauth.RefreshNow(ctx, userID, googleCalendarProvider)
	if err != nil {
		return "", false, err
	}
	return token, ok, nil
}

// toEventDateTime converts a tool argument's "start"/"end" object
// (snake_case date_time/date/time_zone) into internal/calendar.EventDateTime.
func toEventDateTime(m map[string]any) *calendar.EventDateTime {
	if m == nil {
		return nil
	}
	dt := &calendar.EventDateTime{}
	if v, ok := m["date_time"].(string); ok {
		dt.DateTime = v
	}
	if v, ok := m["date"].(string); ok {
		dt.Date = v
	}
	if v, ok := m["time_zone"].(string); ok {
		dt.TimeZone = v
	}
	return dt
}

// formatEvent renders one Event as a compact, model-readable line — used
// for both list_events and create/update's own result, so the model sees
// the same shape either way.
func formatEvent(e calendar.Event) string {
	start := ""
	if e.Start != nil {
		start = e.Start.DateTime
		if start == "" {
			start = e.Start.Date
		}
	}
	end := ""
	if e.End != nil {
		end = e.End.DateTime
		if end == "" {
			end = e.End.Date
		}
	}
	return fmt.Sprintf("- %s | %s -> %s | id=%s%s", e.Summary, start, end, e.ID, func() string {
		if e.Location != "" {
			return " | location=" + e.Location
		}
		return ""
	}())
}

// executeCalendarTool handles every calendar_* tool call, returning
// handled=false for any other tc.Name so executeTool's caller can fall
// through to its next branch unchanged.
func (o *Orchestrator) executeCalendarTool(ctx context.Context, userID string, tc llm.ToolCall) (result string, handled bool) {
	switch tc.Name {
	case calendarListCalendarsToolName, calendarListEventsToolName, calendarFreeBusyToolName,
		calendarCreateEventToolName, calendarUpdateEventToolName, calendarDeleteEventToolName:
	default:
		return "", false
	}

	token, ok, err := o.calendarAccessToken(ctx, userID)
	if err != nil {
		return fmt.Sprintf("error: %v", err), true
	}
	if !ok {
		return fmt.Sprintf("error: not connected to Google Calendar yet — call %s with provider=%q first",
			oauthAuthorizeToolName, googleCalendarProvider), true
	}

	switch tc.Name {
	case calendarListCalendarsToolName:
		cals, err := o.calendar.ListCalendars(ctx, token)
		if err != nil {
			return fmt.Sprintf("error: %v", err), true
		}
		if len(cals) == 0 {
			return "no calendars found", true
		}
		out := ""
		for _, c := range cals {
			out += fmt.Sprintf("- %s | id=%s%s\n", c.Summary, c.ID, func() string {
				if c.Primary {
					return " | primary"
				}
				return ""
			}())
		}
		return out, true

	case calendarListEventsToolName:
		var args struct {
			CalendarID string `json:"calendar_id"`
			TimeMin    string `json:"time_min"`
			TimeMax    string `json:"time_max"`
			Query      string `json:"query"`
		}
		if err := json.Unmarshal([]byte(tc.Arguments), &args); err != nil {
			return fmt.Sprintf("error: invalid arguments: %v", err), true
		}
		events, err := o.calendar.ListEvents(ctx, token, args.CalendarID, calendar.ListEventsOptions{
			TimeMin: args.TimeMin, TimeMax: args.TimeMax, Query: args.Query,
		})
		if err != nil {
			return fmt.Sprintf("error: %v", err), true
		}
		if len(events) == 0 {
			return "no events found", true
		}
		out := ""
		for _, e := range events {
			out += formatEvent(e) + "\n"
		}
		return out, true

	case calendarFreeBusyToolName:
		var args struct {
			CalendarIDs []string `json:"calendar_ids"`
			TimeMin     string   `json:"time_min"`
			TimeMax     string   `json:"time_max"`
		}
		if err := json.Unmarshal([]byte(tc.Arguments), &args); err != nil {
			return fmt.Sprintf("error: invalid arguments: %v", err), true
		}
		items := make([]calendar.FreeBusyCalendar, len(args.CalendarIDs))
		for i, id := range args.CalendarIDs {
			items[i] = calendar.FreeBusyCalendar{ID: id}
		}
		resp, err := o.calendar.FreeBusy(ctx, token, calendar.FreeBusyRequest{
			TimeMin: args.TimeMin, TimeMax: args.TimeMax, Items: items,
		})
		if err != nil {
			return fmt.Sprintf("error: %v", err), true
		}
		out := ""
		for id, status := range resp.Calendars {
			if len(status.Busy) == 0 {
				out += fmt.Sprintf("%s: free the whole time\n", id)
				continue
			}
			out += id + " busy:\n"
			for _, b := range status.Busy {
				out += fmt.Sprintf("  - %s -> %s\n", b.Start, b.End)
			}
		}
		return out, true

	case calendarCreateEventToolName:
		var args struct {
			CalendarID  string         `json:"calendar_id"`
			Summary     string         `json:"summary"`
			Description string         `json:"description"`
			Location    string         `json:"location"`
			Start       map[string]any `json:"start"`
			End         map[string]any `json:"end"`
		}
		if err := json.Unmarshal([]byte(tc.Arguments), &args); err != nil {
			return fmt.Sprintf("error: invalid arguments: %v", err), true
		}
		created, err := o.calendar.CreateEvent(ctx, token, args.CalendarID, calendar.Event{
			Summary:     args.Summary,
			Description: args.Description,
			Location:    args.Location,
			Start:       toEventDateTime(args.Start),
			End:         toEventDateTime(args.End),
		})
		if err != nil {
			return fmt.Sprintf("error: %v", err), true
		}
		return "created: " + formatEvent(created), true

	case calendarUpdateEventToolName:
		var args struct {
			CalendarID  string         `json:"calendar_id"`
			EventID     string         `json:"event_id"`
			Summary     string         `json:"summary"`
			Description string         `json:"description"`
			Location    string         `json:"location"`
			Start       map[string]any `json:"start"`
			End         map[string]any `json:"end"`
		}
		if err := json.Unmarshal([]byte(tc.Arguments), &args); err != nil {
			return fmt.Sprintf("error: invalid arguments: %v", err), true
		}
		updated, err := o.calendar.UpdateEvent(ctx, token, args.CalendarID, args.EventID, calendar.Event{
			Summary:     args.Summary,
			Description: args.Description,
			Location:    args.Location,
			Start:       toEventDateTime(args.Start),
			End:         toEventDateTime(args.End),
		})
		if err != nil {
			return fmt.Sprintf("error: %v", err), true
		}
		return "updated: " + formatEvent(updated), true

	case calendarDeleteEventToolName:
		var args struct {
			CalendarID string `json:"calendar_id"`
			EventID    string `json:"event_id"`
		}
		if err := json.Unmarshal([]byte(tc.Arguments), &args); err != nil {
			return fmt.Sprintf("error: invalid arguments: %v", err), true
		}
		if err := o.calendar.DeleteEvent(ctx, token, args.CalendarID, args.EventID); err != nil {
			return fmt.Sprintf("error: %v", err), true
		}
		return "deleted", true
	}

	// Unreachable: the switch above already matched tc.Name against the
	// same set checked at the top of this function.
	return "", false
}
