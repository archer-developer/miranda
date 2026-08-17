package oauth2

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

// defaultRefreshMargin is how far ahead of an access token's actual expiry
// StartRefresher proactively refreshes it — generous enough that a normal
// tool call reading from Cache.Get almost never races a token that's about
// to expire mid-call.
const defaultRefreshMargin = 5 * time.Minute

// Service is the single entry point internal/httpapi and internal/mcp call
// for everything OAuth2-related — mirrors internal/keyring.Service's role
// of orchestrating a Store and a Cache behind one API.
type Service struct {
	store         *Store
	cache         *Cache
	pending       *PendingAuthStore
	providers     map[string]Provider
	masterKey     []byte
	httpClient    *http.Client
	publicBaseURL string
	callbackPath  string
	logger        *slog.Logger
}

// NewService builds a Service. publicBaseURL/callbackPath are combined with
// a provider's name to build the exact redirect_uri sent to that provider
// (config.OAuthConfig.PublicBaseURL/CallbackPath) — must match what's
// registered with each provider exactly.
func NewService(store *Store, providers []Provider, masterKey []byte, publicBaseURL, callbackPath string, pendingTTL time.Duration, logger *slog.Logger) *Service {
	if logger == nil {
		logger = slog.Default()
	}
	byName := make(map[string]Provider, len(providers))
	for _, p := range providers {
		byName[p.Name] = p
	}
	return &Service{
		store:         store,
		cache:         NewCache(),
		pending:       NewPendingAuthStore(pendingTTL),
		providers:     byName,
		masterKey:     masterKey,
		httpClient:    &http.Client{Timeout: 30 * time.Second},
		publicBaseURL: strings.TrimSuffix(publicBaseURL, "/"),
		callbackPath:  callbackPath,
		logger:        logger,
	}
}

// ProviderNames returns every configured provider's Name, sorted — used for
// the oauth_authorize tool's enum. Sorted (rather than raw map iteration
// order, which Go randomizes) so the tool schema is stable turn to turn,
// the same prompt-caching concern documented on users.Registry.All.
func (s *Service) ProviderNames() []string {
	names := make([]string, 0, len(s.providers))
	for name := range s.providers {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func (s *Service) redirectURI(providerName string) string {
	return s.publicBaseURL + s.callbackPath + "/" + providerName
}

// StartAuthorization begins an authorization attempt for username against
// providerName: generates a PKCE verifier + state, computes the exact
// redirect_uri, stores a PendingAuth, and returns the URL to send the user
// to.
func (s *Service) StartAuthorization(ctx context.Context, username, providerName string) (string, error) {
	p, ok := s.providers[providerName]
	if !ok {
		return "", fmt.Errorf("oauth2: unknown provider %q", providerName)
	}

	verifier, err := GenerateVerifier()
	if err != nil {
		return "", err
	}
	state, err := NewState()
	if err != nil {
		return "", err
	}
	redirectURI := s.redirectURI(providerName)

	s.pending.Put(state, PendingAuth{
		Username:     username,
		Provider:     providerName,
		CodeVerifier: verifier,
		RedirectURI:  redirectURI,
	})

	q := url.Values{
		"response_type": {"code"},
		"client_id":     {p.ClientID},
		"redirect_uri":  {redirectURI},
		"state":         {state},
	}
	if len(p.Scopes) > 0 {
		q.Set("scope", strings.Join(p.Scopes, " "))
	}
	if p.PKCE {
		q.Set("code_challenge", ChallengeS256(verifier))
		q.Set("code_challenge_method", "S256")
	}
	for k, v := range p.ExtraAuthorizeParams {
		q.Set(k, v)
	}

	return p.AuthorizeURL + "?" + q.Encode(), nil
}

// CompleteAuthorization validates+consumes state (single-use), exchanges
// code for tokens using the stored PKCE verifier + redirect_uri, persists
// the encrypted tokens (merging in the previous refresh token if this
// response omitted one), and warms the in-memory cache.
func (s *Service) CompleteAuthorization(ctx context.Context, state, code string) (username, provider string, err error) {
	pending, ok := s.pending.Consume(state)
	if !ok {
		return "", "", fmt.Errorf("oauth2: unknown or expired state")
	}
	p, ok := s.providers[pending.Provider]
	if !ok {
		return "", "", fmt.Errorf("oauth2: unknown provider %q", pending.Provider)
	}

	resp, err := ExchangeCode(ctx, s.httpClient, p, code, pending.RedirectURI, pending.CodeVerifier)
	if err != nil {
		return "", "", fmt.Errorf("oauth2: exchange code for %s/%s: %w", pending.Username, pending.Provider, err)
	}

	refreshToken := resp.RefreshToken
	if refreshToken == "" {
		if existing, ok, err := s.store.GetToken(ctx, s.masterKey, pending.Username, pending.Provider); err == nil && ok {
			refreshToken = existing.RefreshToken
		}
	}

	expiry := time.Now().Add(time.Duration(resp.ExpiresIn) * time.Second)
	if err := s.store.PutToken(ctx, s.masterKey, Token{
		Username:     pending.Username,
		Provider:     pending.Provider,
		AccessToken:  resp.AccessToken,
		RefreshToken: refreshToken,
		Scope:        resp.Scope,
		Expiry:       expiry,
	}); err != nil {
		return "", "", err
	}

	s.cache.Set(pending.Username, pending.Provider, resp.AccessToken, expiry)
	return pending.Username, pending.Provider, nil
}

// AccessToken returns a currently-valid access token from the in-memory
// cache ONLY — never performs network I/O. ok=false covers both "never
// authorized" and "cached token expired, refresher hasn't caught up yet"
// identically; callers on the hot request path must treat both as "not
// ready" and fail the tool call cleanly, not block.
func (s *Service) AccessToken(username, provider string) (string, bool) {
	return s.cache.Get(username, provider)
}

// RefreshNow forces a synchronous refresh — the one deliberate exception to
// "never block on network here": used only from a per-user MCP session's
// own connect closure (already running off the request path, in a
// background goroutine) when no cached token exists yet.
func (s *Service) RefreshNow(ctx context.Context, username, provider string) (string, bool, error) {
	p, ok := s.providers[provider]
	if !ok {
		return "", false, fmt.Errorf("oauth2: unknown provider %q", provider)
	}

	stored, ok, err := s.store.GetToken(ctx, s.masterKey, username, provider)
	if err != nil {
		return "", false, err
	}
	if !ok || stored.RefreshToken == "" {
		return "", false, nil
	}

	resp, err := RefreshAccessToken(ctx, s.httpClient, p, stored.RefreshToken)
	if err != nil {
		s.logger.Warn("oauth2: refresh failed", "user", username, "provider", provider, "error", err)
		return "", false, fmt.Errorf("oauth2: refresh token for %s/%s: %w", username, provider, err)
	}

	refreshToken := resp.RefreshToken
	if refreshToken == "" {
		refreshToken = stored.RefreshToken
	}
	expiry := time.Now().Add(time.Duration(resp.ExpiresIn) * time.Second)
	if err := s.store.PutToken(ctx, s.masterKey, Token{
		Username:     username,
		Provider:     provider,
		AccessToken:  resp.AccessToken,
		RefreshToken: refreshToken,
		Scope:        resp.Scope,
		Expiry:       expiry,
	}); err != nil {
		return "", false, err
	}

	s.cache.Set(username, provider, resp.AccessToken, expiry)
	s.logger.Info("oauth2: token refreshed", "user", username, "provider", provider, "scope", resp.Scope, "expiry", expiry)
	return resp.AccessToken, true, nil
}

// HasToken reports whether username has ever completed authorization for
// provider — used by executeTool's load_tool_group handling to distinguish
// "never authorized, tell the model to call oauth_authorize" from
// "authorized, but this process's per-user session isn't warm yet".
func (s *Service) HasToken(ctx context.Context, username, provider string) (bool, error) {
	return s.store.HasToken(ctx, username, provider)
}

// StartRefresher launches the background proactive-refresh loop: every
// tickInterval, polls store.ListDueForRefresh for tokens within
// defaultRefreshMargin of expiry and refreshes each, updating both the
// persisted row and the in-memory cache. Runs until ctx is cancelled; call
// in its own goroutine.
func (s *Service) StartRefresher(ctx context.Context, tickInterval time.Duration) {
	ticker := time.NewTicker(tickInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.refreshDue(ctx)
		}
	}
}

func (s *Service) refreshDue(ctx context.Context) {
	due, err := s.store.ListDueForRefresh(ctx, s.masterKey, defaultRefreshMargin)
	if err != nil {
		s.logger.Warn("oauth2: list due for refresh", "error", err)
		return
	}
	for _, t := range due {
		// Success/failure are both already logged inside RefreshNow itself
		// (covers this call site and the per-user MCP connect closure's
		// on-demand one identically), so nothing more to do with err here.
		_, _, _ = s.RefreshNow(ctx, t.Username, t.Provider)
	}
}

// RevokeToken deletes username's stored token for provider and drops it
// from the in-memory cache — not wired to any tool/UI yet, but the natural
// extension point for a future "disconnect my Google Calendar" action.
func (s *Service) RevokeToken(ctx context.Context, username, provider string) error {
	s.cache.Delete(username, provider)
	return s.store.DeleteToken(ctx, username, provider)
}
