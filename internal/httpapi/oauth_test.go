package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/archer-developer/miranda-llm/llmtest"
	"github.com/archer-developer/miranda/internal/oauth2"
)

// newFakeOAuthTokenServer stands in for a provider's real token endpoint,
// always returning a fixed access/refresh token pair — enough to exercise
// the authorization-code exchange without any real OAuth2 provider.
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

// newTestOAuthCallbackServer wires a minimal agentloop.Orchestrator behind a
// Server with only the OAuth2 callback route registered — everything this
// file's tests need to drive Server.SetOAuthCallback end to end.
func newTestOAuthCallbackServer(t *testing.T) (*httptest.Server, *oauth2.Service) {
	t.Helper()
	provider := llmtest.New("local")
	o, _, _ := newTestOrchestrator(t, provider)

	oauthSvc := newTestOAuthService(t)
	o.SetOAuth(oauthSvc, time.Millisecond, time.Millisecond, time.Second)

	server := NewServer(o, o.Hub(), "", nil, nil, nil, nil)
	server.SetOAuthCallback(&OAuthCallback{PathPrefix: "/oauth/callback", Service: oauthSvc})

	ts := httptest.NewServer(server)
	t.Cleanup(ts.Close)
	return ts, oauthSvc
}

func TestOAuthCallback_MissingStateOrCode(t *testing.T) {
	ts, _ := newTestOAuthCallbackServer(t)

	resp, err := http.Get(ts.URL + "/oauth/callback/google_calendar")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)

	resp2, err := http.Get(ts.URL + "/oauth/callback/google_calendar?state=abc")
	require.NoError(t, err)
	defer resp2.Body.Close()
	require.Equal(t, http.StatusBadRequest, resp2.StatusCode)
}

func TestOAuthCallback_ProviderErrorParam(t *testing.T) {
	ts, _ := newTestOAuthCallbackServer(t)

	resp, err := http.Get(ts.URL + "/oauth/callback/google_calendar?error=access_denied")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
	require.Contains(t, bodyString(t, resp), "not completed")
}

func TestOAuthCallback_UnknownOrExpiredState(t *testing.T) {
	ts, _ := newTestOAuthCallbackServer(t)

	resp, err := http.Get(ts.URL + "/oauth/callback/google_calendar?state=bogus&code=whatever")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
	require.Contains(t, bodyString(t, resp), "Authorization failed")
}

func TestOAuthCallback_SuccessPersistsToken(t *testing.T) {
	ts, oauthSvc := newTestOAuthCallbackServer(t)

	authorizeURL, err := oauthSvc.StartAuthorization(t.Context(), "alex", "google_calendar")
	require.NoError(t, err)
	u, err := url.Parse(authorizeURL)
	require.NoError(t, err)
	state := u.Query().Get("state")

	resp, err := http.Get(ts.URL + "/oauth/callback/google_calendar?state=" + state + "&code=auth-code")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Contains(t, bodyString(t, resp), "Connected")

	has, err := oauthSvc.HasToken(t.Context(), "alex", "google_calendar")
	require.NoError(t, err)
	require.True(t, has)

	token, ok := oauthSvc.AccessToken("alex", "google_calendar")
	require.True(t, ok)
	require.Equal(t, "access-token", token)
}

func TestOAuthCallback_StateIsSingleUse(t *testing.T) {
	ts, oauthSvc := newTestOAuthCallbackServer(t)

	authorizeURL, err := oauthSvc.StartAuthorization(t.Context(), "alex", "google_calendar")
	require.NoError(t, err)
	u, err := url.Parse(authorizeURL)
	require.NoError(t, err)
	state := u.Query().Get("state")

	resp1, err := http.Get(ts.URL + "/oauth/callback/google_calendar?state=" + state + "&code=auth-code")
	require.NoError(t, err)
	resp1.Body.Close()
	require.Equal(t, http.StatusOK, resp1.StatusCode)

	resp2, err := http.Get(ts.URL + "/oauth/callback/google_calendar?state=" + state + "&code=auth-code")
	require.NoError(t, err)
	defer resp2.Body.Close()
	require.Equal(t, http.StatusBadRequest, resp2.StatusCode, "a replayed callback must not re-exchange an already-used state")
}

func bodyString(t *testing.T, resp *http.Response) string {
	t.Helper()
	buf := make([]byte, 4096)
	n, _ := resp.Body.Read(buf)
	return string(buf[:n])
}
