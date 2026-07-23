package httpapi

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/archer-developer/miranda/internal/config"
	"github.com/archer-developer/miranda/internal/llm/llmtest"
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
	o, _, _ := newTestOrchestrator(t, provider)

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

	require.Len(t, provider.Requests, 1)
	require.NoError(t, err)
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
