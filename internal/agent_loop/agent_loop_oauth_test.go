package agentloop

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	llm "github.com/archer-developer/miranda-llm"
	"github.com/archer-developer/miranda-llm/llmtest"
	"github.com/archer-developer/miranda-llm/router"
	"github.com/archer-developer/miranda/internal/config"
	"github.com/archer-developer/miranda/internal/history"
	"github.com/archer-developer/miranda/internal/hub"
	"github.com/archer-developer/miranda/internal/mcp"
	"github.com/archer-developer/miranda/internal/mcp/mcptest"
	"github.com/archer-developer/miranda/internal/memory"
	"github.com/archer-developer/miranda/internal/oauth2"
	"github.com/archer-developer/miranda/internal/telegram"
	"github.com/archer-developer/miranda/internal/users"
)

// newFakeOAuthTokenServer stands in for a provider's token endpoint,
// scripted well enough for oauth2.ExchangeCode's authorization_code grant.
func newFakeOAuthTokenServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(oauth2.TokenResponse{
			AccessToken: "access-token", RefreshToken: "refresh-token", ExpiresIn: 3600, Scope: "calendar",
		})
	}))
	t.Cleanup(srv.Close)
	return srv
}

// newTestOAuthService builds a real oauth2.Service (SQLite-backed, in a
// tempdir) configured with one provider, "google_calendar", pointed at a
// fake token endpoint — enough to exercise StartAuthorization/
// CompleteAuthorization/HasToken/AccessToken end to end without any real
// Google credentials.
func newTestOAuthService(t *testing.T) *oauth2.Service {
	t.Helper()
	store, err := oauth2.Open(filepath.Join(t.TempDir(), "oauth.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })

	tokenServer := newFakeOAuthTokenServer(t)
	provider := oauth2.Provider{
		Name: "google_calendar", Description: "Google Calendar",
		AuthorizeURL: "https://accounts.google.com/o/oauth2/v2/auth",
		TokenURL:     tokenServer.URL,
		ClientID:     "client-id", PKCE: true,
	}
	masterKey := make([]byte, 32)
	return oauth2.NewService(store, []oauth2.Provider{provider}, masterKey, "https://miranda.example.com", "/oauth/callback", time.Minute, nil)
}

// newTestOrchestratorWithOAuth builds an Orchestrator with the OAuth2 layer
// wired in (mirrors newTestOrchestratorWithTelegram's shape), returning the
// Orchestrator, its oauth2.Service, and the sent-Telegram-messages sink.
func newTestOrchestratorWithOAuth(t *testing.T, provider *llmtest.FakeProvider, configs []config.UserConfig) (*Orchestrator, *oauth2.Service, *[]sentTelegramMessage) {
	t.Helper()

	var sent []sentTelegramMessage
	fakeAPI := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		sent = append(sent, sentTelegramMessage{ChatID: body["chat_id"].(float64), Text: body["text"].(string)})
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	}))
	t.Cleanup(fakeAPI.Close)

	registry, err := users.NewRegistry(configs)
	require.NoError(t, err)

	chats, err := telegram.OpenChatStore(filepath.Join(t.TempDir(), "chats.json"))
	require.NoError(t, err)
	sender := telegram.NewSender(telegram.NewWithAPIBase("test-token", fakeAPI.URL), chats)
	for i, c := range configs {
		require.NoError(t, chats.Save(c.Username, int64(i+1)))
	}

	r, err := router.New([]llm.Provider{provider}, selfEscalation(provider.Name()), "")
	require.NoError(t, err)

	h, err := history.Open(filepath.Join(t.TempDir(), "miranda.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = h.Close() })

	mem, err := memory.New(t.TempDir())
	require.NoError(t, err)

	o := NewOrchestrator(
		r, mcp.NewManager(nil), h, mem, nil, hub.New(100, nil), registry,
		config.AgentConfig{},
		config.MemoryConfig{},
		config.TTSConfig{},
		100, "debug",
	)
	o.SetTelegram(sender, config.TelegramConfig{SendMessageTool: true})

	oauthSvc := newTestOAuthService(t)
	o.SetOAuth(oauthSvc, time.Millisecond, time.Millisecond, time.Second)

	return o, oauthSvc, &sent
}

func TestOrchestrator_OAuthAuthorizeTool_ReturnsLinkAndSendsToTelegram(t *testing.T) {
	fakeProvider := llmtest.New("local",
		llmtest.Response{ToolCall: &llm.ToolCall{ID: "call-1", Name: "oauth_authorize", Arguments: `{"provider":"google_calendar"}`}},
		llmtest.Response{Text: "Готово, перейди по ссылке."},
	)
	o, _, sent := newTestOrchestratorWithOAuth(t, fakeProvider, []config.UserConfig{
		{Username: "alex", PasswordHash: "x", FullName: "Alex"},
	})

	resp, err := o.Handle(context.Background(), InputRequest{Source: "cli", UserID: "alex", Text: "подключи google calendar"})
	require.NoError(t, err)
	require.Equal(t, "Готово, перейди по ссылке.", resp.Reply)

	// The tool result fed back to the model on the second Chat call must
	// carry the authorization URL.
	require.Len(t, fakeProvider.Requests, 2)
	toolResultMsg := fakeProvider.Requests[1].Messages[len(fakeProvider.Requests[1].Messages)-1]
	require.Equal(t, llm.RoleTool, toolResultMsg.Role)
	require.Contains(t, toolResultMsg.Content, "https://accounts.google.com/o/oauth2/v2/auth")

	// Best-effort proactive push to Telegram, since alex has a known chat id.
	require.Len(t, *sent, 1)
	require.Contains(t, (*sent)[0].Text, "https://accounts.google.com/o/oauth2/v2/auth")
}

func TestOrchestrator_OAuthAuthorizeTool_NotOfferedWhenOAuthNotConfigured(t *testing.T) {
	fakeProvider := llmtest.New("local", llmtest.Response{Text: "Привет!"})
	o, _, _ := newTestOrchestrator(t, fakeProvider) // SetOAuth never called

	_, err := o.Handle(context.Background(), InputRequest{Source: "cli", UserID: "alex", Text: "привет"})
	require.NoError(t, err)

	require.Len(t, fakeProvider.Requests, 1)
	for _, tool := range fakeProvider.Requests[0].Tools {
		require.NotEqual(t, oauthAuthorizeToolName, tool.Name)
	}
}

// TestOrchestrator_LoadToolGroup_OAuthGatedServer_RequiresAuthorizationFirst
// covers the load_tool_group pre-check (§4.4 of docs/adr/oauth2-layer.md):
// a lazy, OAuth-gated MCP server the user hasn't authorized yet must fail
// with a clear, model-relayable error instead of silently reporting success
// on a group with no real tools behind it.
func TestOrchestrator_LoadToolGroup_OAuthGatedServer_RequiresAuthorizationFirst(t *testing.T) {
	fakeProvider := llmtest.New("local",
		llmtest.Response{ToolCall: &llm.ToolCall{ID: "call-1", Name: loadToolGroupToolName, Arguments: `{"group":"google_calendar"}`}},
		llmtest.Response{Text: "Сначала нужно подключить календарь."},
	)
	o, _, _ := newTestOrchestratorWithOAuth(t, fakeProvider, nil)
	o.SetLazyMCPServers(map[string]string{"google_calendar": "Google Calendar"})
	o.SetMCPServerExtensions(map[string]MCPServerExtension{
		"google_calendar": {OAuthProvider: "google_calendar", MCPServerURL: "https://calendar.example.com/mcp"},
	}, 0)
	o.tools.SetOAuthServers(map[string]bool{"google_calendar": true})

	resp, err := o.Handle(context.Background(), InputRequest{Source: "cli", UserID: "alex", Text: "что у меня в календаре?"})
	require.NoError(t, err)
	require.Equal(t, "Сначала нужно подключить календарь.", resp.Reply)

	toolResultMsg := fakeProvider.Requests[1].Messages[len(fakeProvider.Requests[1].Messages)-1]
	require.Contains(t, toolResultMsg.Content, "hasn't connected")
	require.Contains(t, toolResultMsg.Content, oauthAuthorizeToolName)
	require.False(t, o.tools.HasUserClient("google_calendar", "alex"), "no session should ever be started before authorization")
}

// TestOrchestrator_OAuthGatedServer_ToolCallSucceedsAfterAuthorization
// drives a full authorization (StartAuthorization + CompleteAuthorization,
// exactly as the real oauth_authorize tool + HTTP callback would), pre-seeds
// the per-user MCP session with a fake client (standing in for what a real
// mcp.Connect against a real Google Calendar MCP server would produce), and
// confirms a subsequent load_tool_group + real tool call both succeed and
// route to that user's own session.
func TestOrchestrator_OAuthGatedServer_ToolCallSucceedsAfterAuthorization(t *testing.T) {
	fakeProvider := llmtest.New("local",
		llmtest.Response{ToolCall: &llm.ToolCall{ID: "call-1", Name: loadToolGroupToolName, Arguments: `{"group":"google_calendar"}`}},
		llmtest.Response{ToolCall: &llm.ToolCall{ID: "call-2", Name: "google_calendar_list_events", Arguments: `{}`}},
		llmtest.Response{Text: "У тебя встреча в 15:00."},
	)
	o, oauthSvc, _ := newTestOrchestratorWithOAuth(t, fakeProvider, nil)
	o.SetLazyMCPServers(map[string]string{"google_calendar": "Google Calendar"})
	o.SetMCPServerExtensions(map[string]MCPServerExtension{
		"google_calendar": {OAuthProvider: "google_calendar", MCPServerURL: "https://calendar.example.com/mcp"},
	}, 0)
	o.tools.SetOAuthServers(map[string]bool{"google_calendar": true})
	o.tools.SetBackgroundContext(context.Background())

	ctx := context.Background()
	authorizeURL, err := oauthSvc.StartAuthorization(ctx, "alex", "google_calendar")
	require.NoError(t, err)
	state := mustExtractState(t, authorizeURL)
	_, _, err = oauthSvc.CompleteAuthorization(ctx, state, "auth-code")
	require.NoError(t, err)

	// Pre-seed alex's per-user session with a fake client — this stands in
	// for what executeTool's own oauthConnectFunc would produce from a real
	// mcp.Connect call; EnsureUserSession is idempotent, so executeTool's
	// own (identically-keyed) call later is a no-op that leaves this fake
	// client in place instead of trying a real network connection.
	fakeCalendar := mcptest.New("google_calendar", llm.ToolDef{Name: "list_events"}).
		WithResult("list_events", "встреча в 15:00")
	o.tools.EnsureUserSession("google_calendar", "alex", time.Millisecond, time.Millisecond, time.Second,
		func(ctx context.Context) (mcp.Client, error) { return fakeCalendar, nil })
	require.Eventually(t, func() bool { return o.tools.HasUserClient("google_calendar", "alex") }, time.Second, time.Millisecond)

	resp, err := o.Handle(ctx, InputRequest{Source: "cli", UserID: "alex", Text: "что у меня в календаре?"})
	require.NoError(t, err)
	require.Equal(t, "У тебя встреча в 15:00.", resp.Reply)

	require.Len(t, fakeCalendar.Calls, 1)
	require.Equal(t, "list_events", fakeCalendar.Calls[0].Tool)
}

func mustExtractState(t *testing.T, authorizeURL string) string {
	t.Helper()
	u, err := url.Parse(authorizeURL)
	require.NoError(t, err)
	return u.Query().Get("state")
}
