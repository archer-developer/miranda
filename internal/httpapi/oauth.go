package httpapi

import (
	"fmt"
	"html"
	"net/http"

	"github.com/archer-developer/miranda/internal/oauth2"
)

// OAuthCallback bundles everything Server needs to receive Google's (or any
// other configured provider's) OAuth2 redirect back to Miranda. Pass it to
// Server.SetOAuthCallback once built (see cmd/miranda) — leaving it unset
// (nil, the default) means the route is never registered, the same
// nil-disables-the-feature convention SetTelegramWebhook/SetUploadHandler
// use. See docs/adr/oauth2-layer.md.
type OAuthCallback struct {
	// PathPrefix is where the provider redirects back to, e.g.
	// "/oauth/callback" (see config.OAuthConfig.CallbackPath) — the
	// provider's own name is appended as a path segment, matching the exact
	// redirect_uri oauth2.Service.StartAuthorization built.
	PathPrefix string
	Service    *oauth2.Service
}

// SetOAuthCallback wires the optional OAuth2 callback route in, mirroring
// SetTelegramWebhook's post-construction style — call at most once, before
// the server starts accepting connections; a nil oc is a no-op (OAuth2
// layer disabled).
func (s *Server) SetOAuthCallback(oc *OAuthCallback) {
	if oc == nil {
		return
	}
	s.oauthCallback = oc
	s.mux.HandleFunc("GET "+oc.PathPrefix+"/{provider}", s.handleOAuthCallback)
}

// handleOAuthCallback receives one provider's OAuth2 redirect: it validates
// and consumes the single-use state value (oauth2.Service.CompleteAuthorization
// does this), exchanges the authorization code for tokens, and renders a
// minimal static confirmation page — there is no session cookie on this
// request to authenticate against (the user may have opened the
// authorization link from an entirely different device/channel than the one
// that started it, e.g. Telegram on a phone), so the single-use state value
// is this route's only credential, playing the same role the Telegram
// webhook's secret header plays for that route.
func (s *Server) handleOAuthCallback(w http.ResponseWriter, r *http.Request) {
	pathProvider := r.PathValue("provider")
	q := r.URL.Query()

	if errParam := q.Get("error"); errParam != "" {
		renderOAuthResult(w, false, "Authorization was not completed: "+errParam)
		return
	}

	state, code := q.Get("state"), q.Get("code")
	if state == "" || code == "" {
		http.Error(w, "missing state or code", http.StatusBadRequest)
		return
	}

	username, provider, err := s.oauthCallback.Service.CompleteAuthorization(r.Context(), state, code)
	if err != nil {
		s.logger.Warn("oauth: callback failed", "path_provider", pathProvider, "error", err)
		renderOAuthResult(w, false, "Authorization failed — the link may have expired; ask Miranda to send a new one.")
		return
	}
	if provider != pathProvider {
		// The state record (not the URL path) is authoritative for which
		// provider this belongs to — a mismatch here means a tampered or
		// copy-pasted callback URL, not a real failure (the token exchange
		// above already succeeded against the right provider), but still
		// worth logging.
		s.logger.Warn("oauth: callback path provider does not match state's provider", "path", pathProvider, "state_provider", provider)
	}

	s.logger.Info("oauth: authorization completed", "provider", provider, "user", username)
	renderOAuthResult(w, true, "Connected — you can return to Miranda.")
}

// renderOAuthResult writes a minimal static HTML page — no template engine
// needed for two fixed messages, and html.EscapeString on message is cheap
// insurance even though every current caller only ever passes a fixed
// string (never a value derived from the request).
func renderOAuthResult(w http.ResponseWriter, ok bool, message string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if !ok {
		w.WriteHeader(http.StatusBadRequest)
	}
	title := "Authorization failed"
	if ok {
		title = "Authorization successful"
	}
	_, _ = fmt.Fprintf(w, `<!doctype html><html><head><meta charset="utf-8"><title>%s</title></head>`+
		`<body style="font-family:sans-serif;text-align:center;padding:4rem 1rem"><h1>%s</h1><p>%s</p></body></html>`,
		html.EscapeString(title), html.EscapeString(title), html.EscapeString(message))
}
