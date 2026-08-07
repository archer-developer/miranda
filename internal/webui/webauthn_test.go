package webui

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/stretchr/testify/require"

	"github.com/archer-developer/miranda/internal/config"
	"github.com/archer-developer/miranda/internal/session"
	"github.com/archer-developer/miranda/internal/users"
	"github.com/archer-developer/miranda/internal/webauthn"
)

var errBoom = errors.New("boom")

// fakeWebAuthnService implements WebAuthnService with canned
// results/errors, recording the arguments each method was called with so
// tests can assert the handlers wire request data through correctly.
type fakeWebAuthnService struct {
	creation   *protocol.CredentialCreation
	beginErr   error
	finishInfo webauthn.CredentialInfo
	finishErr  error

	assertion         *protocol.CredentialAssertion
	ceremonyID        string
	beginLogErr       error
	loginUser         string
	loginCredentialID []byte
	loginPRFOutput    []byte
	loginErr          error

	credentials []webauthn.CredentialInfo
	listErr     error
	deleteErr   error

	probeAssertion    *protocol.CredentialAssertion
	probeBeginErr     error
	probeCredentialID []byte
	probePRFOutput    []byte
	probeFinishErr    error

	gotRegUsername, gotRegCeremonyKey    string
	gotFinishUsername, gotFinishCeremony string
	gotFinishNickname                    string
	gotFinishBody                        []byte
	gotLoginFinishCeremonyID             string
	gotLoginFinishBody                   []byte
	gotListUsername                      string
	gotDeleteUsername                    string
	gotDeleteID                          []byte
	gotProbeBeginUsername                string
	gotProbeBeginCeremonyKey             string
	gotProbeBeginCredentialID            []byte
	gotProbeFinishUsername               string
	gotProbeFinishCeremonyKey            string
	gotProbeFinishBody                   []byte
}

func (f *fakeWebAuthnService) BeginRegistration(ctx context.Context, username, ceremonyKey string) (*protocol.CredentialCreation, error) {
	f.gotRegUsername, f.gotRegCeremonyKey = username, ceremonyKey
	return f.creation, f.beginErr
}

func (f *fakeWebAuthnService) FinishRegistration(ctx context.Context, username, ceremonyKey, nickname string, body []byte) (webauthn.CredentialInfo, error) {
	f.gotFinishUsername, f.gotFinishCeremony, f.gotFinishNickname, f.gotFinishBody = username, ceremonyKey, nickname, body
	return f.finishInfo, f.finishErr
}

func (f *fakeWebAuthnService) BeginDiscoverableLogin(ctx context.Context) (*protocol.CredentialAssertion, string, error) {
	return f.assertion, f.ceremonyID, f.beginLogErr
}

func (f *fakeWebAuthnService) FinishDiscoverableLogin(ctx context.Context, ceremonyID string, body []byte) (string, []byte, []byte, error) {
	f.gotLoginFinishCeremonyID, f.gotLoginFinishBody = ceremonyID, body
	return f.loginUser, f.loginCredentialID, f.loginPRFOutput, f.loginErr
}

func (f *fakeWebAuthnService) ListCredentials(ctx context.Context, username string) ([]webauthn.CredentialInfo, error) {
	f.gotListUsername = username
	return f.credentials, f.listErr
}

func (f *fakeWebAuthnService) DeleteCredential(ctx context.Context, username string, credentialID []byte) error {
	f.gotDeleteUsername, f.gotDeleteID = username, credentialID
	return f.deleteErr
}

func (f *fakeWebAuthnService) BeginKeyProbe(ctx context.Context, username, ceremonyKey string, credentialID []byte) (*protocol.CredentialAssertion, error) {
	f.gotProbeBeginUsername, f.gotProbeBeginCeremonyKey, f.gotProbeBeginCredentialID = username, ceremonyKey, credentialID
	return f.probeAssertion, f.probeBeginErr
}

func (f *fakeWebAuthnService) FinishKeyProbe(ctx context.Context, username, ceremonyKey string, body []byte) ([]byte, []byte, error) {
	f.gotProbeFinishUsername, f.gotProbeFinishCeremonyKey, f.gotProbeFinishBody = username, ceremonyKey, body
	return f.probeCredentialID, f.probePRFOutput, f.probeFinishErr
}

// fakeKeyringService implements KeyringService with canned errors,
// recording the arguments each method was called with.
type fakeKeyringService struct {
	unlockPasswordErr error
	unlockPRFErr      error
	addSlotErr        error
	removeSlotErr     error

	gotUnlockPasswordUsername, gotUnlockPasswordPassword string
	gotUnlockPRFUsername                                 string
	gotUnlockPRFCredentialID, gotUnlockPRFOutput         []byte
	gotAddSlotUsername                                   string
	gotAddSlotCredentialID, gotAddSlotPRFOutput          []byte
	gotLockUsername                                      string
	gotRemoveSlotUsername                                string
	gotRemoveSlotCredentialID                            []byte
}

func (f *fakeKeyringService) UnlockWithPassword(ctx context.Context, username, password string) error {
	f.gotUnlockPasswordUsername, f.gotUnlockPasswordPassword = username, password
	return f.unlockPasswordErr
}

func (f *fakeKeyringService) UnlockWithPRF(ctx context.Context, username string, credentialID, prfOutput []byte) error {
	f.gotUnlockPRFUsername, f.gotUnlockPRFCredentialID, f.gotUnlockPRFOutput = username, credentialID, prfOutput
	return f.unlockPRFErr
}

func (f *fakeKeyringService) AddPasskeySlot(ctx context.Context, username string, credentialID, prfOutput []byte) error {
	f.gotAddSlotUsername, f.gotAddSlotCredentialID, f.gotAddSlotPRFOutput = username, credentialID, prfOutput
	return f.addSlotErr
}

func (f *fakeKeyringService) Lock(username string) {
	f.gotLockUsername = username
}

func (f *fakeKeyringService) RemoveSlotForCredential(ctx context.Context, username string, credentialID []byte) error {
	f.gotRemoveSlotUsername, f.gotRemoveSlotCredentialID = username, credentialID
	return f.removeSlotErr
}

// newTestHandlerWithWebAuthn builds a Handler with passkeys enabled (a
// fakeWebAuthnService), one configured user ("alex"/"555"), and returns the
// handler, its session store, and the fake so tests can set canned
// responses and assert on recorded calls.
func newTestHandlerWithWebAuthn(t *testing.T, fake *fakeWebAuthnService) (*Handler, *session.Store) {
	t.Helper()
	registry, err := users.NewRegistry([]config.UserConfig{
		{Username: "alex", PasswordHash: mustHash(t, "555"), FullName: "Alex"},
	})
	require.NoError(t, err)
	sessions := session.NewStore(time.Hour)

	h, err := New(&fakeHistory{}, newFakeMemory(), fake, nil, registry, sessions, "ru", "", testLogger())
	require.NoError(t, err)
	return h, sessions
}

// newTestHandlerWithWebAuthnAndKeyring is newTestHandlerWithWebAuthn plus a
// fakeKeyringService, for tests covering the login/logout/registration
// hooks that unlock/lock/add a wrapped-key slot.
func newTestHandlerWithWebAuthnAndKeyring(t *testing.T, fake *fakeWebAuthnService, keyringFake *fakeKeyringService) (*Handler, *session.Store) {
	t.Helper()
	registry, err := users.NewRegistry([]config.UserConfig{
		{Username: "alex", PasswordHash: mustHash(t, "555"), FullName: "Alex"},
	})
	require.NoError(t, err)
	sessions := session.NewStore(time.Hour)

	h, err := New(&fakeHistory{}, newFakeMemory(), fake, keyringFake, registry, sessions, "ru", "", testLogger())
	require.NoError(t, err)
	return h, sessions
}

func TestWebAuthnRoutes_NotRegisteredWhenServiceIsNil(t *testing.T) {
	h, _ := newTestHandler(t, &fakeHistory{}) // nil WebAuthnService

	req := httptest.NewRequest(http.MethodPost, "/api/webauthn/login/begin", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	require.Equal(t, http.StatusNotFound, rec.Code)
}

func TestHandleWebAuthnRegisterBegin_RequiresAuth(t *testing.T) {
	h, _ := newTestHandlerWithWebAuthn(t, &fakeWebAuthnService{})

	req := httptest.NewRequest(http.MethodPost, "/api/webauthn/register/begin", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	require.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestHandleWebAuthnRegisterBegin_ReturnsCreationOptionsForLoggedInUser(t *testing.T) {
	fake := &fakeWebAuthnService{creation: &protocol.CredentialCreation{}}
	h, sessions := newTestHandlerWithWebAuthn(t, fake)

	token, err := sessions.Create("alex")
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, "/api/webauthn/register/begin", nil)
	req.AddCookie(&http.Cookie{Name: session.CookieName, Value: token})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "alex", fake.gotRegUsername)
	require.Equal(t, token, fake.gotRegCeremonyKey, "registration must be keyed by the caller's own session token")
}

func TestHandleWebAuthnRegisterFinish_PassesNicknameAndRawBodyThrough(t *testing.T) {
	fake := &fakeWebAuthnService{finishInfo: webauthn.CredentialInfo{ID: "cred-1", Nickname: "iPhone"}}
	h, sessions := newTestHandlerWithWebAuthn(t, fake)

	token, err := sessions.Create("alex")
	require.NoError(t, err)

	body := []byte(`{"id":"abc","nickname":"iPhone"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/webauthn/register/finish", bytes.NewReader(body))
	req.AddCookie(&http.Cookie{Name: session.CookieName, Value: token})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "alex", fake.gotFinishUsername)
	require.Equal(t, token, fake.gotFinishCeremony)
	require.Equal(t, "iPhone", fake.gotFinishNickname)
	require.Equal(t, body, fake.gotFinishBody, "the full original body must reach Service, not just the nickname field")

	var out webauthn.CredentialInfo
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&out))
	require.Equal(t, "cred-1", out.ID)
}

func TestHandleWebAuthnRegisterFinish_ReturnsBadRequestOnServiceError(t *testing.T) {
	fake := &fakeWebAuthnService{finishErr: errBoom}
	h, sessions := newTestHandlerWithWebAuthn(t, fake)

	token, err := sessions.Create("alex")
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, "/api/webauthn/register/finish", bytes.NewReader([]byte(`{}`)))
	req.AddCookie(&http.Cookie{Name: session.CookieName, Value: token})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	require.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestHandleWebAuthnLoginBegin_IsUnauthenticated(t *testing.T) {
	fake := &fakeWebAuthnService{assertion: &protocol.CredentialAssertion{}, ceremonyID: "ceremony-1"}
	h, _ := newTestHandlerWithWebAuthn(t, fake)

	req := httptest.NewRequest(http.MethodPost, "/api/webauthn/login/begin", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)

	var out struct {
		CeremonyID string          `json:"ceremonyId"`
		PublicKey  json.RawMessage `json:"publicKey"`
	}
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&out))
	require.Equal(t, "ceremony-1", out.CeremonyID)
	require.NotNil(t, out.PublicKey, "the standard WebAuthn options must be flattened to the top level, not nested under another key")
}

func TestHandleWebAuthnLoginFinish_IssuesSessionCookieOnSuccess(t *testing.T) {
	fake := &fakeWebAuthnService{loginUser: "alex"}
	h, sessions := newTestHandlerWithWebAuthn(t, fake)

	body := []byte(`{"ceremonyId":"ceremony-1","id":"abc"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/webauthn/login/finish", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "ceremony-1", fake.gotLoginFinishCeremonyID)
	require.Equal(t, body, fake.gotLoginFinishBody)

	cookies := rec.Result().Cookies()
	var sessionCookie *http.Cookie
	for _, c := range cookies {
		if c.Name == session.CookieName {
			sessionCookie = c
		}
	}
	require.NotNil(t, sessionCookie, "a successful passkey login must issue the same session cookie as password login")

	username, ok := sessions.Validate(sessionCookie.Value)
	require.True(t, ok)
	require.Equal(t, "alex", username)
}

func TestHandleWebAuthnLoginFinish_UnlocksKeyringWithPRFOutput(t *testing.T) {
	fake := &fakeWebAuthnService{loginUser: "alex", loginCredentialID: []byte{1, 2, 3}, loginPRFOutput: []byte{4, 5, 6}}
	keyringFake := &fakeKeyringService{}
	h, _ := newTestHandlerWithWebAuthnAndKeyring(t, fake, keyringFake)

	body := []byte(`{"ceremonyId":"ceremony-1","id":"abc"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/webauthn/login/finish", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "alex", keyringFake.gotUnlockPRFUsername)
	require.Equal(t, []byte{1, 2, 3}, keyringFake.gotUnlockPRFCredentialID)
	require.Equal(t, []byte{4, 5, 6}, keyringFake.gotUnlockPRFOutput)
}

func TestHandleWebAuthnLoginFinish_KeyringFailureDoesNotBlockLogin(t *testing.T) {
	fake := &fakeWebAuthnService{loginUser: "alex"}
	keyringFake := &fakeKeyringService{unlockPRFErr: errBoom}
	h, _ := newTestHandlerWithWebAuthnAndKeyring(t, fake, keyringFake)

	body := []byte(`{"ceremonyId":"ceremony-1","id":"abc"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/webauthn/login/finish", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, "a keyring error must never turn into a login failure")
}

func TestHandleWebAuthnLoginFinish_RejectsMissingCeremonyID(t *testing.T) {
	fake := &fakeWebAuthnService{}
	h, _ := newTestHandlerWithWebAuthn(t, fake)

	req := httptest.NewRequest(http.MethodPost, "/api/webauthn/login/finish", bytes.NewReader([]byte(`{}`)))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	require.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestHandleWebAuthnLoginFinish_ServiceErrorIsUnauthorized(t *testing.T) {
	fake := &fakeWebAuthnService{loginErr: errBoom}
	h, _ := newTestHandlerWithWebAuthn(t, fake)

	body := []byte(`{"ceremonyId":"ceremony-1"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/webauthn/login/finish", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	require.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestHandleWebAuthnListCredentials_RequiresAuthAndScopesByUsername(t *testing.T) {
	fake := &fakeWebAuthnService{credentials: []webauthn.CredentialInfo{{ID: "cred-1", Nickname: "iPhone"}}}
	h, sessions := newTestHandlerWithWebAuthn(t, fake)

	req := httptest.NewRequest(http.MethodGet, "/api/webauthn/credentials", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusUnauthorized, rec.Code)

	token, err := sessions.Create("alex")
	require.NoError(t, err)
	req = httptest.NewRequest(http.MethodGet, "/api/webauthn/credentials", nil)
	req.AddCookie(&http.Cookie{Name: session.CookieName, Value: token})
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "alex", fake.gotListUsername)

	var out []webauthn.CredentialInfo
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&out))
	require.Len(t, out, 1)
}

func TestHandleWebAuthnDeleteCredential_DecodesBase64URLIDAndScopesByUsername(t *testing.T) {
	fake := &fakeWebAuthnService{}
	h, sessions := newTestHandlerWithWebAuthn(t, fake)

	token, err := sessions.Create("alex")
	require.NoError(t, err)

	rawID := []byte{1, 2, 3, 4, 5}
	encoded := base64.RawURLEncoding.EncodeToString(rawID)
	req := httptest.NewRequest(http.MethodDelete, "/api/webauthn/credentials/"+encoded, nil)
	req.AddCookie(&http.Cookie{Name: session.CookieName, Value: token})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	require.Equal(t, http.StatusNoContent, rec.Code)
	require.Equal(t, "alex", fake.gotDeleteUsername)
	require.Equal(t, rawID, fake.gotDeleteID)
}

func TestHandleWebAuthnDeleteCredential_RemovesOrphanedKeyringSlot(t *testing.T) {
	fake := &fakeWebAuthnService{}
	keyringFake := &fakeKeyringService{}
	h, sessions := newTestHandlerWithWebAuthnAndKeyring(t, fake, keyringFake)

	token, err := sessions.Create("alex")
	require.NoError(t, err)

	rawID := []byte{1, 2, 3, 4, 5}
	encoded := base64.RawURLEncoding.EncodeToString(rawID)
	req := httptest.NewRequest(http.MethodDelete, "/api/webauthn/credentials/"+encoded, nil)
	req.AddCookie(&http.Cookie{Name: session.CookieName, Value: token})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	require.Equal(t, http.StatusNoContent, rec.Code)
	require.Equal(t, "alex", keyringFake.gotRemoveSlotUsername)
	require.Equal(t, rawID, keyringFake.gotRemoveSlotCredentialID)
}

func TestWebAuthnProbeRoutes_NotRegisteredWithoutKeyring(t *testing.T) {
	fake := &fakeWebAuthnService{}
	h, sessions := newTestHandlerWithWebAuthn(t, fake) // webauthn set, keyring nil

	token, err := sessions.Create("alex")
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, "/api/webauthn/register/probe-begin", bytes.NewReader([]byte(`{"credentialId":"abc"}`)))
	req.AddCookie(&http.Cookie{Name: session.CookieName, Value: token})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	require.Equal(t, http.StatusNotFound, rec.Code, "the probe routes only make sense when there's a keyring to hand PRF output to")
}

func TestHandleWebAuthnRegisterProbeBegin_ReturnsAssertionForRequestedCredential(t *testing.T) {
	rawID := []byte{9, 9, 9}
	fake := &fakeWebAuthnService{probeAssertion: &protocol.CredentialAssertion{}}
	keyringFake := &fakeKeyringService{}
	h, sessions := newTestHandlerWithWebAuthnAndKeyring(t, fake, keyringFake)

	token, err := sessions.Create("alex")
	require.NoError(t, err)

	body := []byte(`{"credentialId":"` + base64.RawURLEncoding.EncodeToString(rawID) + `"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/webauthn/register/probe-begin", bytes.NewReader(body))
	req.AddCookie(&http.Cookie{Name: session.CookieName, Value: token})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "alex", fake.gotProbeBeginUsername)
	require.Equal(t, token, fake.gotProbeBeginCeremonyKey)
	require.Equal(t, rawID, fake.gotProbeBeginCredentialID)
}

func TestHandleWebAuthnRegisterProbeBegin_RequiresAuth(t *testing.T) {
	fake := &fakeWebAuthnService{}
	keyringFake := &fakeKeyringService{}
	h, _ := newTestHandlerWithWebAuthnAndKeyring(t, fake, keyringFake)

	req := httptest.NewRequest(http.MethodPost, "/api/webauthn/register/probe-begin", bytes.NewReader([]byte(`{"credentialId":"abc"}`)))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	require.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestHandleWebAuthnRegisterProbeFinish_AddsPasskeySlotOnPRFOutput(t *testing.T) {
	// clientClaimedID is what the request body's own credentialId field
	// says; validatedID is what FinishKeyProbe reports it actually
	// cryptographically verified the assertion against. They're
	// deliberately different here to prove AddPasskeySlot binds to the
	// latter, never the former — see FinishKeyProbe's doc comment for why a
	// client's own claimed credentialId must never drive which keyring slot
	// a PRF output gets wrapped under.
	clientClaimedID := []byte{9, 9, 9}
	validatedID := []byte{1, 2, 3}
	fake := &fakeWebAuthnService{probeCredentialID: validatedID, probePRFOutput: []byte{7, 7, 7}}
	keyringFake := &fakeKeyringService{}
	h, sessions := newTestHandlerWithWebAuthnAndKeyring(t, fake, keyringFake)

	token, err := sessions.Create("alex")
	require.NoError(t, err)

	body := []byte(`{"credentialId":"` + base64.RawURLEncoding.EncodeToString(clientClaimedID) + `"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/webauthn/register/probe-finish", bytes.NewReader(body))
	req.AddCookie(&http.Cookie{Name: session.CookieName, Value: token})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	require.Equal(t, http.StatusNoContent, rec.Code)
	require.Equal(t, "alex", keyringFake.gotAddSlotUsername)
	require.Equal(t, validatedID, keyringFake.gotAddSlotCredentialID, "must bind the keyring slot to the server-validated credential id, not the client's claimed one")
	require.Equal(t, []byte{7, 7, 7}, keyringFake.gotAddSlotPRFOutput)
}

func TestHandleWebAuthnRegisterProbeFinish_NoPRFOutputIsUnprocessable(t *testing.T) {
	rawID := []byte{9, 9, 9}
	fake := &fakeWebAuthnService{} // probePRFOutput left nil: authenticator didn't support PRF
	keyringFake := &fakeKeyringService{}
	h, sessions := newTestHandlerWithWebAuthnAndKeyring(t, fake, keyringFake)

	token, err := sessions.Create("alex")
	require.NoError(t, err)

	body := []byte(`{"credentialId":"` + base64.RawURLEncoding.EncodeToString(rawID) + `"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/webauthn/register/probe-finish", bytes.NewReader(body))
	req.AddCookie(&http.Cookie{Name: session.CookieName, Value: token})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	require.Equal(t, http.StatusUnprocessableEntity, rec.Code, "a probe failure must never be reported as the passkey registration itself failing")
	require.Empty(t, keyringFake.gotAddSlotUsername, "must not call AddPasskeySlot with no PRF output")
}

func TestHandleWebAuthnRegisterProbeFinish_CeremonyErrorIsBadRequest(t *testing.T) {
	fake := &fakeWebAuthnService{probeFinishErr: errBoom}
	keyringFake := &fakeKeyringService{}
	h, sessions := newTestHandlerWithWebAuthnAndKeyring(t, fake, keyringFake)

	token, err := sessions.Create("alex")
	require.NoError(t, err)

	body := []byte(`{"credentialId":"` + base64.RawURLEncoding.EncodeToString([]byte{1}) + `"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/webauthn/register/probe-finish", bytes.NewReader(body))
	req.AddCookie(&http.Cookie{Name: session.CookieName, Value: token})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	require.Equal(t, http.StatusBadRequest, rec.Code)
}
