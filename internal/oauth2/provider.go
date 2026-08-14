// Package oauth2 implements a generic OAuth 2.1 authorization-code (+ PKCE)
// flow with encrypted refresh-token storage and background access-token
// refresh, so any MCP server that requires per-user OAuth2 consent (Google
// Calendar first, more later) can be wired in by adding one Provider entry
// and one config.MCPServer.OAuthProvider reference — no new Go code per
// provider. See docs/adr/oauth2-layer.md for the full design and the two
// deliberate divergences from internal/keyring's per-user model: tokens are
// encrypted under a server-held master key (not gated on keyring unlock, so
// background refresh and scheduled tasks keep working), and MCP sessions are
// multiplexed per (server, user) rather than sharing one connection with a
// swapped bearer token (see internal/mcp.Manager).
//
// File split: provider.go (this file, config type), pkce.go (RFC 7636),
// state.go (pending-authorization correlation store), store.go (encrypted
// SQLite persistence), cache.go (in-memory access-token cache), client.go
// (token endpoint HTTP calls), masterkey.go (server key loading), service.go
// (the one entry point internal/httpapi and internal/mcp call).
package oauth2

// Provider is one OAuth2 identity provider Miranda can authorize a user
// against — config-driven, resolved from config.OAuthProvider by
// cmd/miranda, independent of any particular MCP server.
type Provider struct {
	// Name identifies this provider, e.g. "google_calendar" — referenced by
	// config.MCPServer.OAuthProvider and by the oauth_authorize tool's
	// provider argument.
	Name string
	// Description is shown to the model/user, e.g. "Google Calendar".
	Description string
	// AuthorizeURL is the provider's authorization endpoint, e.g.
	// https://accounts.google.com/o/oauth2/v2/auth.
	AuthorizeURL string
	// TokenURL is the provider's token endpoint, e.g.
	// https://oauth2.googleapis.com/token.
	TokenURL string
	// ClientID is resolved once at startup from the provider config's
	// ClientIDEnv.
	ClientID string
	// ClientSecret is resolved once at startup from ClientSecretEnv. "" is a
	// valid value — a public/PKCE-only client omits it from token requests
	// entirely rather than sending an empty string (see client.go).
	ClientSecret string
	Scopes       []string
	// PKCE enables RFC 7636 code_verifier/code_challenge on the authorize and
	// token-exchange requests. Google's Calendar MCP server requires this
	// (OAuth 2.1) — true.
	PKCE bool
	// ExtraAuthorizeParams are appended to the authorize URL verbatim, e.g.
	// {"access_type": "offline", "prompt": "consent"} — required for Google
	// to issue a refresh_token on every consent, not just the first ever.
	ExtraAuthorizeParams map[string]string
}
