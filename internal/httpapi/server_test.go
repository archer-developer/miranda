package httpapi

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	llm "github.com/archer-developer/miranda-llm"
	"github.com/archer-developer/miranda-llm/llmtest"
	"github.com/archer-developer/miranda/internal/config"
	"github.com/archer-developer/miranda/internal/session"
	"github.com/archer-developer/miranda/internal/users"
)

func TestServer_Healthz_IsUnauthenticated(t *testing.T) {
	provider := llmtest.New("local")
	o, _, _ := newTestOrchestrator(t, provider)
	server := NewServer(o, o.hub, "secret", nil, nil, nil, nil)

	ts := httptest.NewServer(server)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/healthz")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
}

func TestServer_HandleInput_ReturnsJSONReply(t *testing.T) {
	provider := llmtest.New("local", llmtest.Response{Text: "hello"})
	o, _, _ := newTestOrchestrator(t, provider)
	server := NewServer(o, o.hub, "", nil, nil, nil, nil)

	ts := httptest.NewServer(server)
	defer ts.Close()

	body, _ := json.Marshal(InputRequest{Source: "cli", UserID: "alex", Text: "hi"})
	resp, err := http.Post(ts.URL+"/api/v1/input", "application/json", bytes.NewReader(body))
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var out InputResponse
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&out))
	require.Equal(t, "hello", out.Reply)
}

func TestServer_HandleInput_LogsRawRequestBody(t *testing.T) {
	provider := llmtest.New("local", llmtest.Response{Text: "hello"})
	o, _, _ := newTestOrchestrator(t, provider)

	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, nil))
	server := NewServer(o, o.hub, "", nil, logger, nil, nil)

	ts := httptest.NewServer(server)
	defer ts.Close()

	raw := []byte(`{"source":"ha_assist","user_id":"unknown","text":"привет","conversation_id":"abc-123"}`)
	resp, err := http.Post(ts.URL+"/api/v1/input", "application/json", bytes.NewReader(raw))
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	logged := logBuf.String()
	require.Contains(t, logged, "received input request")
	require.Contains(t, logged, `\"source\":\"ha_assist\"`)
	require.Contains(t, logged, `\"conversation_id\":\"abc-123\"`)
}

func TestServer_HandleInput_LogsRawRequestBodyEvenWhenUnauthorized(t *testing.T) {
	provider := llmtest.New("local")
	o, _, _ := newTestOrchestrator(t, provider)

	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, nil))
	server := NewServer(o, o.hub, "secret-token", nil, logger, nil, nil)

	ts := httptest.NewServer(server)
	defer ts.Close()

	raw := []byte(`{"source":"ha_assist","user_id":"unknown","text":"это не пройдёт"}`)
	resp, err := http.Post(ts.URL+"/api/v1/input", "application/json", bytes.NewReader(raw))
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusUnauthorized, resp.StatusCode)

	// Even a request that fails auth must still show up raw in the log —
	// that's exactly the case someone debugging a bad HA token needs to see.
	require.Contains(t, logBuf.String(), "это не пройдёт")
}

func TestServer_HandleInput_RejectsMissingText(t *testing.T) {
	provider := llmtest.New("local")
	o, _, _ := newTestOrchestrator(t, provider)
	server := NewServer(o, o.hub, "", nil, nil, nil, nil)

	ts := httptest.NewServer(server)
	defer ts.Close()

	body, _ := json.Marshal(InputRequest{Source: "cli", UserID: "alex"})
	resp, err := http.Post(ts.URL+"/api/v1/input", "application/json", bytes.NewReader(body))
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestServer_HandleInput_RequiresBearerTokenWhenConfigured(t *testing.T) {
	provider := llmtest.New("local", llmtest.Response{Text: "hello"})
	o, _, _ := newTestOrchestrator(t, provider)
	server := NewServer(o, o.hub, "secret", nil, nil, nil, nil)

	ts := httptest.NewServer(server)
	defer ts.Close()

	body, _ := json.Marshal(InputRequest{Source: "cli", UserID: "alex", Text: "hi"})

	resp, err := http.Post(ts.URL+"/api/v1/input", "application/json", bytes.NewReader(body))
	require.NoError(t, err)
	require.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	resp.Body.Close()

	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/v1/input", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set("Content-Type", "application/json")
	resp2, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp2.Body.Close()
	require.Equal(t, http.StatusOK, resp2.StatusCode)
}

func TestServer_HandleInput_ResolvesHAUserIDToCanonicalUsername(t *testing.T) {
	provider := llmtest.New("local", llmtest.Response{Text: "hello"})
	o, historyStore, _ := newTestOrchestrator(t, provider)

	registry, err := users.NewRegistry([]config.UserConfig{
		{Username: "alex", PasswordHash: "x", HAUserID: "ha-uuid-alex"},
	})
	require.NoError(t, err)

	server := NewServer(o, o.hub, "", nil, nil, registry, nil)
	ts := httptest.NewServer(server)
	defer ts.Close()

	body, _ := json.Marshal(InputRequest{Source: "ha_assist", UserID: "ha-uuid-alex", Text: "привет"})
	resp, err := http.Post(ts.URL+"/api/v1/input", "application/json", bytes.NewReader(body))
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var out InputResponse
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&out))

	// The whole point of resolution: history/memory must be keyed by the
	// canonical username, never by HA's raw speaker-recognition id.
	convos, err := historyStore.RecentConversations(t.Context(), "alex", 10)
	require.NoError(t, err)
	require.Len(t, convos, 1)
	require.Equal(t, out.ConversationID, convos[0].ID)

	convosByRawID, err := historyStore.RecentConversations(t.Context(), "ha-uuid-alex", 10)
	require.NoError(t, err)
	require.Empty(t, convosByRawID)
}

func TestServer_HandleInput_SessionCookieGrantsAccessAndSetsIdentity(t *testing.T) {
	provider := llmtest.New("local", llmtest.Response{Text: "hello"})
	o, historyStore, _ := newTestOrchestrator(t, provider)

	registry, err := users.NewRegistry([]config.UserConfig{{Username: "anna", PasswordHash: "x"}})
	require.NoError(t, err)
	sessions := session.NewStore(time.Hour)
	token, err := sessions.Create("anna")
	require.NoError(t, err)

	server := NewServer(o, o.hub, "secret", nil, nil, registry, sessions)
	ts := httptest.NewServer(server)
	defer ts.Close()

	// No bearer token, but a valid session cookie — and the client tries to
	// claim a different user_id/source, which must be ignored.
	body, _ := json.Marshal(InputRequest{Source: "ha_assist", UserID: "someone-else", Text: "привет"})
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/v1/input", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(&http.Cookie{Name: session.CookieName, Value: token})

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var out InputResponse
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&out))

	msgs, err := historyStore.ConversationMessages(t.Context(), out.ConversationID)
	require.NoError(t, err)
	require.NotEmpty(t, msgs)

	convos, err := historyStore.RecentConversations(t.Context(), "anna", 10)
	require.NoError(t, err)
	require.Len(t, convos, 1, "the turn must be recorded under the session's username, not the client-supplied user_id")
}

func TestServer_HandleInput_SessionCookieIdentityWinsEvenWithOpenBearerAuth(t *testing.T) {
	provider := llmtest.New("local", llmtest.Response{Text: "hello"})
	o, historyStore, _ := newTestOrchestrator(t, provider)

	registry, err := users.NewRegistry([]config.UserConfig{{Username: "archer", PasswordHash: "x"}})
	require.NoError(t, err)
	sessions := session.NewStore(time.Hour)
	token, err := sessions.Create("archer")
	require.NoError(t, err)

	// auth_token empty ("LAN-only dev mode" — bearerAuthorized always passes)
	// must not cause a logged-in dashboard session's identity to be
	// discarded in favor of the anonymous/default user.
	server := NewServer(o, o.hub, "", nil, nil, registry, sessions)
	ts := httptest.NewServer(server)
	defer ts.Close()

	body, _ := json.Marshal(InputRequest{Source: "web_ui", Text: "Привет! как меня зовут?"})
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/v1/input", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(&http.Cookie{Name: session.CookieName, Value: token})

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	convos, err := historyStore.RecentConversations(t.Context(), "archer", 10)
	require.NoError(t, err)
	require.Len(t, convos, 1)

	debugConvos, err := historyStore.RecentConversations(t.Context(), "debug", 10)
	require.NoError(t, err)
	require.Empty(t, debugConvos, "must never fall back to the anonymous default identity when a valid session cookie is present")
}

func TestServer_HandleInput_MemoryAndConversationShareSameKeyAcrossChannels(t *testing.T) {
	provider := llmtest.New("local",
		llmtest.Response{ToolCall: &llm.ToolCall{ID: "call-1", Name: "remember_this", Arguments: `{"fact":"любит чай"}`}},
		llmtest.Response{Text: "Запомнил."},
		llmtest.Response{Text: "Да, ты говорил про чай."},
	)
	o, historyStore, memoryStore := newTestOrchestrator(t, provider)

	registry, err := users.NewRegistry([]config.UserConfig{
		{Username: "archer", PasswordHash: "x", HAUserID: "ha-raw-archer"},
	})
	require.NoError(t, err)
	sessions := session.NewStore(time.Hour)
	token, err := sessions.Create("archer")
	require.NoError(t, err)

	server := NewServer(o, o.hub, "", nil, nil, registry, sessions)
	ts := httptest.NewServer(server)
	defer ts.Close()

	// Turn 1 arrives via HA voice, identified only by its raw speaker id.
	body1, _ := json.Marshal(InputRequest{Source: "ha_assist", UserID: "ha-raw-archer", Text: "я люблю чай, запомни"})
	resp1, err := http.Post(ts.URL+"/api/v1/input", "application/json", bytes.NewReader(body1))
	require.NoError(t, err)
	defer resp1.Body.Close()
	require.Equal(t, http.StatusOK, resp1.StatusCode)

	// The fact must be filed under the canonical username, never the raw HA id.
	content, err := memoryStore.Read("archer")
	require.NoError(t, err)
	require.Contains(t, content, "любит чай")
	rawContent, err := memoryStore.Read("ha-raw-archer")
	require.NoError(t, err)
	require.Empty(t, rawContent)

	// Turn 2 arrives via an authenticated web UI session for the same person.
	body2, _ := json.Marshal(InputRequest{Source: "web_ui", Text: "помнишь что я люблю?"})
	req2, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/v1/input", bytes.NewReader(body2))
	req2.Header.Set("Content-Type", "application/json")
	req2.AddCookie(&http.Cookie{Name: session.CookieName, Value: token})
	resp2, err := http.DefaultClient.Do(req2)
	require.NoError(t, err)
	defer resp2.Body.Close()
	require.Equal(t, http.StatusOK, resp2.StatusCode)

	var out2 InputResponse
	require.NoError(t, json.NewDecoder(resp2.Body).Decode(&out2))

	// Both channels must land in the very same open conversation — session
	// continuity, like memory, is keyed only on the resolved canonical user.
	convos, err := historyStore.RecentConversations(t.Context(), "archer", 10)
	require.NoError(t, err)
	require.Len(t, convos, 1)
	require.Equal(t, out2.ConversationID, convos[0].ID)
}

func TestServer_HandleInput_InvalidSessionCookieIsUnauthorized(t *testing.T) {
	provider := llmtest.New("local")
	o, _, _ := newTestOrchestrator(t, provider)
	sessions := session.NewStore(time.Hour)

	server := NewServer(o, o.hub, "secret", nil, nil, nil, sessions)
	ts := httptest.NewServer(server)
	defer ts.Close()

	body, _ := json.Marshal(InputRequest{Source: "web_ui", Text: "hi"})
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/v1/input", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(&http.Cookie{Name: session.CookieName, Value: "not-a-real-token"})

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusUnauthorized, resp.StatusCode)
}
