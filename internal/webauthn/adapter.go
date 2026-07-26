package webauthn

import (
	webauthnlib "github.com/go-webauthn/webauthn/webauthn"

	"github.com/archer-developer/miranda/internal/users"
)

// credentialUser adapts a users.User (Miranda's config-driven identity)
// plus its loaded WebAuthn state to satisfy the library's User interface.
// internal/users deliberately has no dependency on this package or the
// webauthn library, so the dependency points one way:
// internal/webauthn -> internal/users, never the reverse.
type credentialUser struct {
	users.User
	handle      []byte
	credentials []webauthnlib.Credential
}

// WebAuthnID returns the stable, random per-(rpid,username) user handle
// from webauthn_users — never derived from the username itself. The
// library's own docs say this "should be completely random," and its
// validateLogin step compares this value against the authenticator-supplied
// user handle on every login (both the identified and discoverable paths),
// so it must be correct and stable regardless of which lookup path found
// the user.
func (u credentialUser) WebAuthnID() []byte { return u.handle }

// WebAuthnName is the account identifier shown to the user by their
// browser/OS during registration and login ceremonies.
func (u credentialUser) WebAuthnName() string { return u.Username }

// WebAuthnDisplayName reuses users.User.DisplayName() (full name, falling
// back to the bare username) — the same "who is this" text used elsewhere
// in the web UI.
func (u credentialUser) WebAuthnDisplayName() string { return u.DisplayName() }

// WebAuthnCredentials returns every passkey username has registered, loaded
// from Store.CredentialsForUser by whoever constructs this adapter.
func (u credentialUser) WebAuthnCredentials() []webauthnlib.Credential { return u.credentials }

var _ webauthnlib.User = credentialUser{}
