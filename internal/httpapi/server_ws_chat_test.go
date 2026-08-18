package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/stretchr/testify/require"

	"github.com/archer-developer/miranda-llm/llmtest"
	agentloop "github.com/archer-developer/miranda/internal/agent_loop"
	"github.com/archer-developer/miranda/internal/session"
)

// dialChatWS opens /ws/chat/{username} authenticated as sessionCookie's
// owner, failing the test immediately if the handshake is rejected —
// mirrors handleWSChat's own auth flow (see server.go), just from the
// client side.
func dialChatWS(t *testing.T, wsURL, sessionToken string) *websocket.Conn {
	t.Helper()
	header := http.Header{}
	if sessionToken != "" {
		header.Set("Cookie", session.CookieName+"="+sessionToken)
	}
	conn, _, err := websocket.Dial(context.Background(), wsURL, &websocket.DialOptions{HTTPHeader: header})
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.CloseNow() })
	return conn
}

func TestServer_HandleWSChat_RejectsUnauthenticated(t *testing.T) {
	provider := llmtest.New("local")
	o, _, _ := newTestOrchestrator(t, provider)
	server := NewServer(o, o.Hub(), "secret", nil, nil, nil, nil)

	ts := httptest.NewServer(server)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/ws/chat/alice")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusUnauthorized, resp.StatusCode)
}

func TestServer_HandleWSChat_RejectsCrossUserAccess(t *testing.T) {
	provider := llmtest.New("local")
	o, _, _ := newTestOrchestrator(t, provider)
	sessions := session.NewStore(time.Hour)
	token, err := sessions.Create("alice")
	require.NoError(t, err)

	server := NewServer(o, o.Hub(), "", nil, nil, nil, sessions)
	ts := httptest.NewServer(server)
	defer ts.Close()

	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/ws/chat/bob", nil)
	req.AddCookie(&http.Cookie{Name: session.CookieName, Value: token})
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusForbidden, resp.StatusCode, "alice's session must not read bob's chat channel just by changing the URL")
}

func TestServer_HandleWSChat_RejectsBearerOnlyAuth(t *testing.T) {
	provider := llmtest.New("local")
	o, _, _ := newTestOrchestrator(t, provider)
	server := NewServer(o, o.Hub(), "", nil, nil, nil, nil) // no session store: only the "LAN dev mode" bearer path is available

	ts := httptest.NewServer(server)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/ws/chat/alice")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusForbidden, resp.StatusCode, "bearer/no-auth mode carries no per-user identity to scope a chat channel to")
}

func TestServer_HandleWSChat_ReceivesOwnConversationEvents(t *testing.T) {
	provider := llmtest.New("local", llmtest.Response{Text: "Привет!"})
	o, _, _ := newTestOrchestrator(t, provider)
	sessions := session.NewStore(time.Hour)
	token, err := sessions.Create("alice")
	require.NoError(t, err)

	server := NewServer(o, o.Hub(), "", nil, nil, nil, sessions)
	ts := httptest.NewServer(server)
	defer ts.Close()

	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws/chat/alice"
	conn := dialChatWS(t, wsURL, token)

	resp, err := o.Handle(context.Background(), agentloop.InputRequest{Source: "ha_assist", UserID: "alice", Text: "привет"})
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	var userEvent, assistantEvent chatEventEnvelope
	require.NoError(t, wsjson.Read(ctx, conn, &userEvent))
	require.NoError(t, wsjson.Read(ctx, conn, &assistantEvent))

	require.Equal(t, "chat", userEvent.Source)
	require.Equal(t, "alice", userEvent.UserID)
	require.Equal(t, "message", userEvent.Data.Type)
	require.Equal(t, "user", userEvent.Data.Message.Role)
	require.Equal(t, resp.UserMessageID, userEvent.Data.Message.ID)

	require.Equal(t, "message", assistantEvent.Data.Type)
	require.Equal(t, "assistant", assistantEvent.Data.Message.Role)
	require.Equal(t, "Привет!", assistantEvent.Data.Message.Content)
	require.Equal(t, resp.AssistantMessageID, assistantEvent.Data.Message.ID)
}

func TestServer_HandleWSChat_DoesNotLeakOtherUsersEvents(t *testing.T) {
	provider := llmtest.New("local", llmtest.Response{Text: "hi alice"})
	o, _, _ := newTestOrchestrator(t, provider)
	sessions := session.NewStore(time.Hour)
	bobToken, err := sessions.Create("bob")
	require.NoError(t, err)

	server := NewServer(o, o.Hub(), "", nil, nil, nil, sessions)
	ts := httptest.NewServer(server)
	defer ts.Close()

	bobURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws/chat/bob"
	bobConn := dialChatWS(t, bobURL, bobToken)

	_, err = o.Handle(context.Background(), agentloop.InputRequest{Source: "ha_assist", UserID: "alice", Text: "привет"})
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	var ev chatEventEnvelope
	err = wsjson.Read(ctx, bobConn, &ev)
	require.Error(t, err, "bob's socket must never see alice's chat events")
}

// chatEventEnvelope mirrors hub.Event/ChatEvent for test-side decoding —
// hub.Event.Data is `any`, so the test needs its own concrete shape rather
// than reusing httpapi.ChatEvent directly through json.RawMessage.
type chatEventEnvelope struct {
	Source string `json:"source"`
	UserID string `json:"user_id"`
	Data   struct {
		Type    string `json:"type"`
		Message struct {
			ID      int64  `json:"id"`
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"message"`
	} `json:"data"`
}
