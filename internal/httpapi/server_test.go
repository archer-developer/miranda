package httpapi

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/archer-developer/miranda/internal/llm/llmtest"
)

func TestServer_Healthz_IsUnauthenticated(t *testing.T) {
	provider := llmtest.New("local")
	o, _, _ := newTestOrchestrator(t, provider)
	server := NewServer(o, o.hub, "secret", nil, nil)

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
	server := NewServer(o, o.hub, "", nil, nil)

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
	server := NewServer(o, o.hub, "", nil, nil)

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
	server := NewServer(o, o.hub, "secret", nil, nil)

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
