package agentloop

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/robfig/cron/v3"

	llm "github.com/archer-developer/miranda-llm"
	"github.com/archer-developer/miranda/internal/replyformat"
	"github.com/archer-developer/miranda/internal/schedule"
)

// searchHistoryLimit bounds how many past conversations the search_history
// tool feeds back to the model in one call, so a broad query doesn't blow up
// the prompt.
const searchHistoryLimit = 8

// executeTool runs one tool call: locally (remember_this, search_history,
// end_conversation, forget_conversation), via an internal/tools.Tool
// (tavily_web_search, tavily_web_fetch — see o.webTools), or via the MCP tool manager.
// Errors are turned into a result string rather than aborting the turn, so
// the model can see what went wrong and react (apologize, retry
// differently) instead of the whole request failing.
func (o *Orchestrator) executeTool(ctx context.Context, userID, conversationID string, tc llm.ToolCall, control *turnControl) string {
	if tc.Name == rememberToolName {
		var args struct {
			Fact  string `json:"fact"`
			Scope string `json:"scope"`
		}
		if err := json.Unmarshal([]byte(tc.Arguments), &args); err != nil {
			return fmt.Sprintf("error: invalid arguments: %v", err)
		}
		if args.Scope == "shared" {
			if err := o.memory.RememberShared(args.Fact); err != nil {
				return fmt.Sprintf("error: %v", err)
			}
			return "remembered in shared household memory"
		}
		if err := o.memory.Remember(userID, args.Fact); err != nil {
			return fmt.Sprintf("error: %v", err)
		}
		return "remembered"
	}

	if tc.Name == searchHistoryToolName {
		var args struct {
			Query string `json:"query"`
		}
		if err := json.Unmarshal([]byte(tc.Arguments), &args); err != nil {
			return fmt.Sprintf("error: invalid arguments: %v", err)
		}
		results, err := o.history.SearchConversations(ctx, userID, args.Query, searchHistoryLimit)
		if err != nil {
			return fmt.Sprintf("error: %v", err)
		}
		if len(results) == 0 {
			return "no matching past conversations found"
		}
		var b strings.Builder
		for _, c := range results {
			fmt.Fprintf(&b, "[%s] %s\n", c.StartedAt.Format("2006-01-02"), c.Summary)
		}
		return b.String()
	}

	if tc.Name == loadToolGroupToolName {
		var args struct {
			Group string `json:"group"`
		}
		if err := json.Unmarshal([]byte(tc.Arguments), &args); err != nil {
			return fmt.Sprintf("error: invalid arguments: %v", err)
		}
		_, isLazyMCPGroup := o.lazyServerDescriptions[args.Group]
		isCalendarGroup := args.Group == googleCalendarProvider && o.calendarEnabled()
		if !isLazyMCPGroup && !isCalendarGroup {
			return fmt.Sprintf("error: unknown tool group %q", args.Group)
		}
		// An OAuth-gated group (config.MCPServer.OAuthProvider != "") has no
		// live per-user MCP session until EnsureUserSession brings one up —
		// unlike every other lazy group, whose real tools come from the one
		// already-connected global client. Without this, the model's first
		// load_tool_group call for such a group would report success while
		// the very next availableTools() call still saw zero real tools for
		// it — see docs/adr/oauth2-layer.md.
		if ext, ok := o.mcpExtensions[args.Group]; ok && ext.OAuthProvider != "" && o.oauth != nil {
			authorized, err := o.oauth.HasToken(ctx, userID, ext.OAuthProvider)
			if err != nil {
				return fmt.Sprintf("error: %v", err)
			}
			if !authorized {
				return fmt.Sprintf("the user hasn't connected %q yet — call %s with provider=%q first", args.Group, oauthAuthorizeToolName, ext.OAuthProvider)
			}
			o.tools.EnsureUserSession(args.Group, userID, o.oauthReconnect, o.oauthMaxReconnect, o.oauthConnectTimeout, o.oauthConnectFunc(ext, args.Group, userID))
			// Bounded wait (not unbounded — a scoped exception to "no
			// blocking I/O in executeTool", like Service.RefreshNow: at most
			// once per user per process restart, not a per-call cost) so the
			// very next availableTools() call in this same turn has a real
			// chance of seeing a live client instead of falsely reporting
			// success on a session that isn't up yet.
			if !waitForUserSession(ctx, o.tools, args.Group, userID, 5*time.Second) {
				return fmt.Sprintf("still connecting to %q — try again in a moment", args.Group)
			}
		}
		// google_calendar is native, not MCP-routed — no session to bring
		// up (calendar_* tools fetch their access token per-call, see
		// calendarAccessToken), just the same "authorized yet?" pre-check
		// as the OAuth-gated MCP branch above, so a missing connection
		// fails at load_tool_group time with a clear message instead of
		// loading six real schemas that would all fail identically on
		// their first actual call.
		if isCalendarGroup {
			authorized, err := o.oauth.HasToken(ctx, userID, googleCalendarProvider)
			if err != nil {
				return fmt.Sprintf("error: %v", err)
			}
			if !authorized {
				return fmt.Sprintf("the user hasn't connected %q yet — call %s with provider=%q first", args.Group, oauthAuthorizeToolName, googleCalendarProvider)
			}
		}
		if control.loadedGroups == nil {
			control.loadedGroups = map[string]bool{}
		}
		control.loadedGroups[args.Group] = true
		control.groupsChanged = true
		return fmt.Sprintf("tools for %q are now available — call the specific tool you need now", args.Group)
	}

	if tc.Name == endConversationToolName {
		control.endRequested = true
		return "conversation ended"
	}

	if tc.Name == forgetConversationToolName {
		control.forgetRequested = true
		return "conversation forgotten"
	}

	if tc.Name == speakReplyToolName {
		var args struct {
			Text   string `json:"text"`
			Device string `json:"device"`
		}
		if err := json.Unmarshal([]byte(tc.Arguments), &args); err != nil {
			return fmt.Sprintf("error: invalid arguments: %v", err)
		}
		if args.Text == "" {
			return "error: text is required"
		}
		spoken := voiceText(args.Text)
		if args.Device != "" && o.tts != nil {
			o.tts.SpeakTo(ctx, spoken, args.Device)
		} else {
			o.speakText(ctx, spoken)
		}
		return "spoken"
	}

	if tc.Name == stopSpeechToolName {
		if o.tts != nil {
			o.tts.Stop()
		}
		return "stopped"
	}

	if tc.Name == sendTelegramToolName {
		var args struct {
			Text      string `json:"text"`
			Recipient string `json:"recipient"`
		}
		if err := json.Unmarshal([]byte(tc.Arguments), &args); err != nil {
			return fmt.Sprintf("error: invalid arguments: %v", err)
		}

		targetUsername := userID
		if args.Recipient != "" {
			if o.users == nil {
				return fmt.Sprintf("error: no household member matches %q", args.Recipient)
			}
			target, ok := o.users.ResolveByDisplayName(args.Recipient)
			if !ok {
				return fmt.Sprintf("error: no household member matches %q", args.Recipient)
			}
			targetUsername = target.Username
		}

		if err := o.telegram.SendHTMLToUser(ctx, targetUsername, replyformat.Parse(args.Text)); err != nil {
			return fmt.Sprintf("error: %v", err)
		}
		return "sent"
	}

	if tc.Name == oauthAuthorizeToolName {
		var args struct {
			Provider string `json:"provider"`
		}
		if err := json.Unmarshal([]byte(tc.Arguments), &args); err != nil {
			return fmt.Sprintf("error: invalid arguments: %v", err)
		}
		authorizeURL, err := o.oauth.StartAuthorization(ctx, userID, args.Provider)
		if err != nil {
			return fmt.Sprintf("error: %v", err)
		}
		// Best-effort, mirrors send_telegram's own "silently skip if no chat
		// id known" shape — a raw URL is useless spoken aloud (ha_assist), so
		// push it out-of-band regardless of which channel actually asked, in
		// addition to the reply text below (which works fine for web
		// UI/typed Telegram).
		if o.telegram != nil {
			_ = o.telegram.SendToUser(ctx, userID, "Follow this link to connect "+args.Provider+": "+authorizeURL)
		}
		return "Send the user this link to complete authorization: " + authorizeURL
	}

	if result, handled := o.executeCalendarTool(ctx, userID, tc); handled {
		return result
	}

	if tc.Name == createScheduledTaskToolName {
		var args struct {
			Task     string `json:"task"`
			RunAt    string `json:"run_at"`
			Schedule string `json:"schedule"`
		}
		if err := json.Unmarshal([]byte(tc.Arguments), &args); err != nil {
			return fmt.Sprintf("error: invalid arguments: %v", err)
		}
		if args.Task == "" {
			return "error: task is required"
		}
		if (args.RunAt == "") == (args.Schedule == "") {
			return "error: provide exactly one of run_at or schedule"
		}

		task := schedule.Task{UserID: userID, Prompt: args.Task}
		if args.Schedule != "" {
			sched, err := cron.ParseStandard(args.Schedule)
			if err != nil {
				return fmt.Sprintf("error: invalid schedule expression: %v", err)
			}
			// Interpret the cron expression in the user's local timezone so
			// "1 9 * * *" means 09:01 in the user's time, not the server's.
			if specSched, ok := sched.(*cron.SpecSchedule); ok {
				specSched.Location = o.userLocation(userID)
			}
			task.CronExpr = args.Schedule
			task.NextRunAt = sched.Next(time.Now())
		} else {
			runAt, err := time.Parse(time.RFC3339, args.RunAt)
			if err != nil {
				return fmt.Sprintf("error: invalid run_at (expected RFC3339): %v", err)
			}
			if !runAt.After(time.Now()) {
				return "error: run_at is in the past"
			}
			task.RunAt = &runAt
			task.NextRunAt = runAt
		}

		id, err := o.schedule.Create(ctx, task)
		if err != nil {
			return fmt.Sprintf("error: %v", err)
		}
		return "scheduled: " + id
	}

	if tc.Name == listScheduledTasksToolName {
		tasksList, err := o.schedule.ListForUser(ctx, userID)
		if err != nil {
			return fmt.Sprintf("error: %v", err)
		}
		if len(tasksList) == 0 {
			return "no scheduled tasks"
		}
		userLoc := o.userLocation(userID)
		var b strings.Builder
		for _, t := range tasksList {
			fmt.Fprintf(&b, "[%s] next: %s — %s\n", t.ID, t.NextRunAt.In(userLoc).Format(time.RFC3339), t.Prompt)
		}
		return b.String()
	}

	if tc.Name == deleteScheduledTaskToolName {
		var args struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal([]byte(tc.Arguments), &args); err != nil {
			return fmt.Sprintf("error: invalid arguments: %v", err)
		}
		if err := o.schedule.Delete(ctx, args.ID, userID); err != nil {
			if err == schedule.ErrNotFound {
				return "error: no such scheduled task"
			}
			return fmt.Sprintf("error: %v", err)
		}
		return "deleted"
	}

	for _, t := range o.webTools {
		if t.Def().Name != tc.Name {
			continue
		}
		result, err := t.Call(ctx, tc.Arguments)
		if err != nil {
			return fmt.Sprintf("error: %v", err)
		}
		return result
	}

	return o.executeMCPTool(ctx, userID, conversationID, tc, control)
}
