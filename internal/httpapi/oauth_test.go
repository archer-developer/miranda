package httpapi

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/archer-developer/miranda-llm/llmtest"
)

func newTestOAuthCallbackServer(t *testing.T) (*httptest.Server, *Orchestrator) {
	t.Helper()
	provider := llmtest.New("local")
	o, _, _ := newTestOrchestrator(t, provider)

	oauthSvc := newTestOAuthService(t)
	o.SetOAuth(oauthSvc, time.Millisecond, time.Millisecond, time.Second)

	server := NewServer(o, o.hub, "", nil, nil, nil, nil)
	server.SetOAuthCallback(&OAuthCallback{PathPrefix: "/oauth/callback", Service: oauthSvc})

	ts := httptest.NewServer(server)
	t.Cleanup(ts.Close)
	return ts, o
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
	ts, o := newTestOAuthCallbackServer(t)

	authorizeURL, err := o.oauth.StartAuthorization(t.Context(), "alex", "google_calendar")
	require.NoError(t, err)
	u, err := url.Parse(authorizeURL)
	require.NoError(t, err)
	state := u.Query().Get("state")

	resp, err := http.Get(ts.URL + "/oauth/callback/google_calendar?state=" + state + "&code=auth-code")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Contains(t, bodyString(t, resp), "Connected")

	has, err := o.oauth.HasToken(t.Context(), "alex", "google_calendar")
	require.NoError(t, err)
	require.True(t, has)

	token, ok := o.oauth.AccessToken("alex", "google_calendar")
	require.True(t, ok)
	require.Equal(t, "access-token", token)
}

func TestOAuthCallback_StateIsSingleUse(t *testing.T) {
	ts, o := newTestOAuthCallbackServer(t)

	authorizeURL, err := o.oauth.StartAuthorization(t.Context(), "alex", "google_calendar")
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
