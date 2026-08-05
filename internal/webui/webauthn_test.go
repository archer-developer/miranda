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

	assertion   *protocol.CredentialAssertion
	ceremonyID  string
	beginLogErr error
	loginUser   string
	loginErr    error

	credentials []webauthn.CredentialInfo
	listErr     error
	deleteErr   error

	gotRegUsername, gotRegCeremonyKey    string
	gotFinishUsername, gotFinishCeremony string
	gotFinishNickname                    string
	gotFinishBody                        []byte
	gotLoginFinishCeremonyID             string
	gotLoginFinishBody                   []byte
	gotListUsername                      string
	gotDeleteUsername                    string
	gotDeleteID                          []byte
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

func (f *fakeWebAuthnService) FinishDiscoverableLogin(ctx context.Context, ceremonyID string, body []byte) (string, error) {
	f.gotLoginFinishCeremonyID, f.gotLoginFinishBody = ceremonyID, body
	return f.loginUser, f.loginErr
}

func (f *fakeWebAuthnService) ListCredentials(ctx context.Context, username string) ([]webauthn.CredentialInfo, error) {
	f.gotListUsername = username
	return f.credentials, f.listErr
}

func (f *fakeWebAuthnService) DeleteCredential(ctx context.Context, username string, credentialID []byte) error {
	f.gotDeleteUsername, f.gotDeleteID = username, credentialID
	return f.deleteErr
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

	h, err := New(&fakeHistory{}, newFakeMemory(), fake, registry, sessions, "ru", "", testLogger())
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
