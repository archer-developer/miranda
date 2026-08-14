package oauth2

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// fakeTokenServer is a minimal stand-in for a provider's token endpoint,
// tracking every grant it served so tests can assert on call counts/args.
type fakeTokenServer struct {
	srv        *httptest.Server
	accessSeq  int
	expiresIn  int
	refreshErr bool
}

func newFakeTokenServer(t *testing.T) *fakeTokenServer {
	t.Helper()
	f := &fakeTokenServer{expiresIn: 3600}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, r.ParseForm())
		if f.refreshErr && r.Form.Get("grant_type") == "refresh_token" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
			return
		}
		f.accessSeq++
		resp := TokenResponse{
			AccessToken: "access-" + strconv.Itoa(f.accessSeq),
			ExpiresIn:   f.expiresIn,
			Scope:       "calendar",
		}
		if r.Form.Get("grant_type") == "authorization_code" {
			resp.RefreshToken = "refresh-1"
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func newTestService(t *testing.T, tokenURL string) *Service {
	t.Helper()
	store := openTestStore(t)
	provider := Provider{
		Name: "google_calendar", Description: "Google Calendar",
		AuthorizeURL: "https://accounts.google.com/o/oauth2/v2/auth",
		TokenURL:     tokenURL,
		ClientID:     "client-id", ClientSecret: "client-secret",
		Scopes: []string{"calendar"}, PKCE: true,
	}
	return NewService(store, []Provider{provider}, testMasterKey(t), "https://miranda.example.com", "/oauth/callback", time.Minute, nil)
}

func TestService_StartAndCompleteAuthorization(t *testing.T) {
	fake := newFakeTokenServer(t)
	svc := newTestService(t, fake.srv.URL)
	ctx := context.Background()

	authorizeURL, err := svc.StartAuthorization(ctx, "alice", "google_calendar")
	require.NoError(t, err)

	parsed, err := url.Parse(authorizeURL)
	require.NoError(t, err)
	state := parsed.Query().Get("state")
	require.NotEmpty(t, state)
	require.Equal(t, "S256", parsed.Query().Get("code_challenge_method"))
	require.Equal(t, "https://miranda.example.com/oauth/callback/google_calendar", parsed.Query().Get("redirect_uri"))

	username, provider, err := svc.CompleteAuthorization(ctx, state, "auth-code")
	require.NoError(t, err)
	require.Equal(t, "alice", username)
	require.Equal(t, "google_calendar", provider)

	token, ok := svc.AccessToken("alice", "google_calendar")
	require.True(t, ok)
	require.Equal(t, "access-1", token)

	has, err := svc.HasToken(ctx, "alice", "google_calendar")
	require.NoError(t, err)
	require.True(t, has)
}

func TestService_CompleteAuthorization_UnknownState(t *testing.T) {
	fake := newFakeTokenServer(t)
	svc := newTestService(t, fake.srv.URL)

	_, _, err := svc.CompleteAuthorization(context.Background(), "bogus-state", "auth-code")
	require.Error(t, err)
}

func TestService_AccessToken_NeverAuthorized(t *testing.T) {
	fake := newFakeTokenServer(t)
	svc := newTestService(t, fake.srv.URL)

	_, ok := svc.AccessToken("nobody", "google_calendar")
	require.False(t, ok)
}

func TestService_RefreshNow(t *testing.T) {
	fake := newFakeTokenServer(t)
	svc := newTestService(t, fake.srv.URL)
	ctx := context.Background()

	authorizeURL, err := svc.StartAuthorization(ctx, "alice", "google_calendar")
	require.NoError(t, err)
	state := mustParseState(t, authorizeURL)
	_, _, err = svc.CompleteAuthorization(ctx, state, "auth-code")
	require.NoError(t, err)

	token, ok, err := svc.RefreshNow(ctx, "alice", "google_calendar")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, "access-2", token)

	// The cache must reflect the refreshed token immediately.
	cached, ok := svc.AccessToken("alice", "google_calendar")
	require.True(t, ok)
	require.Equal(t, "access-2", cached)
}

func TestService_RefreshNow_NoStoredToken(t *testing.T) {
	fake := newFakeTokenServer(t)
	svc := newTestService(t, fake.srv.URL)

	_, ok, err := svc.RefreshNow(context.Background(), "nobody", "google_calendar")
	require.NoError(t, err)
	require.False(t, ok)
}

func TestService_StartRefresher_RefreshesDueTokens(t *testing.T) {
	fake := newFakeTokenServer(t)
	fake.expiresIn = 1 // expires almost immediately, well within any refresh margin
	svc := newTestService(t, fake.srv.URL)
	ctx := context.Background()

	authorizeURL, err := svc.StartAuthorization(ctx, "alice", "google_calendar")
	require.NoError(t, err)
	state := mustParseState(t, authorizeURL)
	_, _, err = svc.CompleteAuthorization(ctx, state, "auth-code")
	require.NoError(t, err)

	refresherCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go svc.StartRefresher(refresherCtx, 5*time.Millisecond)

	require.Eventually(t, func() bool {
		token, ok := svc.AccessToken("alice", "google_calendar")
		return ok && token == "access-2"
	}, time.Second, 5*time.Millisecond, "background refresher should have refreshed the near-expiry token")
}

func TestService_RevokeToken(t *testing.T) {
	fake := newFakeTokenServer(t)
	svc := newTestService(t, fake.srv.URL)
	ctx := context.Background()

	authorizeURL, err := svc.StartAuthorization(ctx, "alice", "google_calendar")
	require.NoError(t, err)
	state := mustParseState(t, authorizeURL)
	_, _, err = svc.CompleteAuthorization(ctx, state, "auth-code")
	require.NoError(t, err)

	require.NoError(t, svc.RevokeToken(ctx, "alice", "google_calendar"))

	_, ok := svc.AccessToken("alice", "google_calendar")
	require.False(t, ok)
	has, err := svc.HasToken(ctx, "alice", "google_calendar")
	require.NoError(t, err)
	require.False(t, has)
}

func mustParseState(t *testing.T, authorizeURL string) string {
	t.Helper()
	parsed, err := url.Parse(authorizeURL)
	require.NoError(t, err)
	return parsed.Query().Get("state")
}
