package oauth2

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"

	"github.com/archer-developer/miranda/internal/httpx"
)

// TokenResponse is a provider's token-endpoint response (RFC 6749 §5.1),
// the subset Miranda cares about.
type TokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"` // may be "" — see RefreshAccessToken
	ExpiresIn    int    `json:"expires_in"`    // seconds
	Scope        string `json:"scope"`
	TokenType    string `json:"token_type"`
}

// maxErrorBodyRunes bounds how much of a non-JSON error body gets embedded
// in an error return — mirrors internal/tavily's own cap, since a token
// exchange failure surfaces to the model as a tool-call error persisted
// into conversation history.
const maxErrorBodyRunes = 2000

// ExchangeCode performs RFC 6749 §4.1.3's authorization_code grant against
// p.TokenURL, form-encoded (OAuth2 token endpoints, including Google's,
// reject a JSON body here, unlike every other outbound call this codebase
// otherwise makes — see httpx.PostForm). clientSecret is omitted from the
// form entirely when p.ClientSecret == "" (a public/PKCE-only client)
// rather than sent as an empty string.
func ExchangeCode(ctx context.Context, httpClient *http.Client, p Provider, code, redirectURI, codeVerifier string) (TokenResponse, error) {
	values := url.Values{
		"grant_type":   {"authorization_code"},
		"code":         {code},
		"redirect_uri": {redirectURI},
		"client_id":    {p.ClientID},
	}
	if p.ClientSecret != "" {
		values.Set("client_secret", p.ClientSecret)
	}
	if p.PKCE && codeVerifier != "" {
		values.Set("code_verifier", codeVerifier)
	}
	return doTokenRequest(ctx, httpClient, p.TokenURL, values)
}

// RefreshAccessToken performs RFC 6749 §6's refresh_token grant. The
// response's RefreshToken may be "" — most providers (including Google)
// only issue a new refresh token on first-ever consent or when explicitly
// forced via extra authorize params; callers must keep the previously
// stored refresh token rather than overwrite it with an empty value (see
// Store.PutToken's doc comment).
func RefreshAccessToken(ctx context.Context, httpClient *http.Client, p Provider, refreshToken string) (TokenResponse, error) {
	values := url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refreshToken},
		"client_id":     {p.ClientID},
	}
	if p.ClientSecret != "" {
		values.Set("client_secret", p.ClientSecret)
	}
	return doTokenRequest(ctx, httpClient, p.TokenURL, values)
}

func doTokenRequest(ctx context.Context, httpClient *http.Client, tokenURL string, values url.Values) (TokenResponse, error) {
	status, body, err := httpx.PostForm(ctx, httpClient, tokenURL, nil, values, httpx.DefaultMaxResponseBytes)
	if err != nil {
		return TokenResponse{}, fmt.Errorf("oauth2: token request: %w", err)
	}

	if status < 200 || status >= 300 {
		var apiErr struct {
			Error            string `json:"error"`
			ErrorDescription string `json:"error_description"`
		}
		_ = json.Unmarshal(body, &apiErr)
		if apiErr.Error != "" {
			return TokenResponse{}, fmt.Errorf("oauth2: token request failed (status %d): %s: %s", status, apiErr.Error, apiErr.ErrorDescription)
		}
		return TokenResponse{}, fmt.Errorf("oauth2: token request failed (status %d): %s", status, truncateRunes(string(body), maxErrorBodyRunes))
	}

	var resp TokenResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return TokenResponse{}, fmt.Errorf("oauth2: decode token response: %w", err)
	}
	return resp, nil
}

// truncateRunes cuts s to at most n runes (not bytes), appending a marker
// when it does — mirrors internal/tavily's own helper.
func truncateRunes(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n]) + "... (truncated)"
}
