package oauth2

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestExchangeCode_FormEncodedRequest(t *testing.T) {
	var gotContentType string
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotContentType = r.Header.Get("Content-Type")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"at","refresh_token":"rt","expires_in":3600,"scope":"calendar","token_type":"Bearer"}`))
	}))
	defer srv.Close()

	p := Provider{
		Name: "google_calendar", TokenURL: srv.URL,
		ClientID: "client-id", ClientSecret: "client-secret", PKCE: true,
	}

	resp, err := ExchangeCode(context.Background(), srv.Client(), p, "auth-code", "https://miranda.example.com/oauth/callback/google_calendar", "verifier-value")
	require.NoError(t, err)
	require.Equal(t, "at", resp.AccessToken)
	require.Equal(t, "rt", resp.RefreshToken)
	require.Equal(t, 3600, resp.ExpiresIn)

	require.Equal(t, "application/x-www-form-urlencoded", gotContentType)
	require.Contains(t, gotBody, "grant_type=authorization_code")
	require.Contains(t, gotBody, "code=auth-code")
	require.Contains(t, gotBody, "client_id=client-id")
	require.Contains(t, gotBody, "client_secret=client-secret")
	require.Contains(t, gotBody, "code_verifier=verifier-value")
	require.Contains(t, gotBody, "redirect_uri=")
}

func TestExchangeCode_OmitsEmptyClientSecret(t *testing.T) {
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"at","expires_in":3600}`))
	}))
	defer srv.Close()

	p := Provider{Name: "google_calendar", TokenURL: srv.URL, ClientID: "client-id", PKCE: true}
	_, err := ExchangeCode(context.Background(), srv.Client(), p, "auth-code", "https://x/cb", "verifier")
	require.NoError(t, err)
	require.NotContains(t, gotBody, "client_secret")
}

func TestRefreshAccessToken_FormEncodedRequest(t *testing.T) {
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"new-at","expires_in":3600}`))
	}))
	defer srv.Close()

	p := Provider{Name: "google_calendar", TokenURL: srv.URL, ClientID: "client-id", ClientSecret: "secret"}
	resp, err := RefreshAccessToken(context.Background(), srv.Client(), p, "refresh-token-value")
	require.NoError(t, err)
	require.Equal(t, "new-at", resp.AccessToken)
	require.Empty(t, resp.RefreshToken, "a refresh response may legitimately omit a new refresh token")

	require.Contains(t, gotBody, "grant_type=refresh_token")
	require.Contains(t, gotBody, "refresh_token=refresh-token-value")
}

func TestDoTokenRequest_ErrorStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"invalid_grant","error_description":"code expired"}`))
	}))
	defer srv.Close()

	p := Provider{Name: "google_calendar", TokenURL: srv.URL, ClientID: "client-id"}
	_, err := ExchangeCode(context.Background(), srv.Client(), p, "auth-code", "https://x/cb", "verifier")
	require.Error(t, err)
	require.Contains(t, err.Error(), "invalid_grant")
}
