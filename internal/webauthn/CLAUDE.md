# WebAuthn / passkey login (`internal/webauthn`)

Optional (`config.WebAuthnConfig.Enabled`, default false — no safe
default, requires `rp_id` and `rp_origins`).

## Structure

`internal/webauthn` wraps `github.com/go-webauthn/webauthn`:

- `Service` — orchestrates registration/login ceremonies.
- `Store` — persists credentials and each user's stable, random WebAuthn
  "user handle" in its own SQLite file (`Storage.WebAuthnSQLitePath`).
- `CeremonyStore` — holds transient challenge state between a ceremony's
  begin/finish calls.

`internal/webui` only talks to `Service`: registration from the profile
screen (`POST /api/webauthn/register/{begin,finish}`), and login from
`/login`'s biometric button. Login is *discoverable/usernameless*
(`BeginDiscoverableLogin`/`FinishDiscoverableLogin`): the browser's
platform authenticator resolves which resident credential to use before
Miranda knows who's signing in — there's no per-account signal available
on the anonymous login page (see `Store.LookupByCredentialID` and
`/login`'s client-side "remembered last method" heuristic).

## Android BackupEligible quirk (`Store.ReconcileFlags`)

Some Android platform authenticators (Google Password Manager passkeys)
report `BackupEligible=false` at registration, then `BackupEligible=true`
on every subsequent login once the credential finishes syncing to the
cloud. `go-webauthn` hard-fails any `BackupEligible` mismatch with no
config escape hatch (`go-webauthn/webauthn#240`).
`FinishDiscoverableLogin` resyncs the stored flags to the assertion's
actual value before validation runs — the library maintainer's own
documented workaround (`go-webauthn/webauthn#351`).

## Auto-retry on stale ceremony (`/login` biometric button)

The biometric button retries once automatically on a failure that lands in
under 800ms. A stale WebAuthn request left pending by an earlier ceremony
makes the *next* `navigator.credentials.get()` reject instantly (before
the OS picker opens); a second call right after clears it. This is a
distinct Android/Chrome quirk, unrelated to the BackupEligible issue
above — both were found debugging the same real bug report.
