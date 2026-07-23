package webui

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/archer-developer/miranda/internal/config"
	"github.com/archer-developer/miranda/internal/history"
	"github.com/archer-developer/miranda/internal/session"
	"github.com/archer-developer/miranda/internal/users"
)

type fakeHistory struct {
	conversations []history.Conversation
	messages      []history.Message
}

func (f *fakeHistory) RecentConversations(ctx context.Context, userID string, limit int) ([]history.Conversation, error) {
	return f.conversations, nil
}

func (f *fakeHistory) ConversationMessages(ctx context.Context, conversationID string) ([]history.Message, error) {
	return f.messages, nil
}

func mustHash(t *testing.T, password string) string {
	t.Helper()
	hash, err := users.HashPassword(password)
	require.NoError(t, err)
	return hash
}

// newTestHandler builds a Handler with one configured user ("alex"/"555")
// and returns it alongside its Registry/Store so tests can create sessions
// directly without going through the login form.
func newTestHandler(t *testing.T, fake *fakeHistory) (*Handler, *session.Store) {
	t.Helper()
	registry, err := users.NewRegistry([]config.UserConfig{
		{Username: "alex", PasswordHash: mustHash(t, "555"), FullName: "Alex"},
	})
	require.NoError(t, err)
	sessions := session.NewStore(time.Hour)

	h, err := New(fake, registry, sessions, "ru", "")
	require.NoError(t, err)
	return h, sessions
}

func authedRequest(t *testing.T, sessions *session.Store, method, target string) *http.Request {
	t.Helper()
	token, err := sessions.Create("alex")
	require.NoError(t, err)
	req := httptest.NewRequest(method, target, nil)
	req.AddCookie(&http.Cookie{Name: session.CookieName, Value: token})
	return req
}

func TestHandleIndex_RedirectsToLoginWhenUnauthenticated(t *testing.T) {
	h, _ := newTestHandler(t, &fakeHistory{})

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	require.Equal(t, http.StatusSeeOther, rec.Code)
	require.Equal(t, "/login", rec.Header().Get("Location"))
}

func TestHandleIndex_ServesDashboardWhenAuthenticated(t *testing.T) {
	h, sessions := newTestHandler(t, &fakeHistory{})

	req := authedRequest(t, sessions, http.MethodGet, "/")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	require.Contains(t, rec.Body.String(), "Miranda")
	require.Contains(t, rec.Body.String(), "Alex") // display name rendered in the header
}

func TestHandleIndex_LocalizesToRussianByDefault(t *testing.T) {
	h, sessions := newTestHandler(t, &fakeHistory{})

	req := authedRequest(t, sessions, http.MethodGet, "/")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	require.Contains(t, rec.Body.String(), "Живой лог") // live_log_title in ru.json
}

func TestHandleIndex_LanguageCookieOverridesDefault(t *testing.T) {
	h, sessions := newTestHandler(t, &fakeHistory{})

	req := authedRequest(t, sessions, http.MethodGet, "/")
	req.AddCookie(&http.Cookie{Name: langCookieName, Value: "en"})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	require.Contains(t, rec.Body.String(), "Live log")
}

func TestLoginFlow_CorrectCredentialsGrantsSession(t *testing.T) {
	h, _ := newTestHandler(t, &fakeHistory{})

	form := url.Values{"username": {"alex"}, "password": {"555"}}
	req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	require.Equal(t, http.StatusSeeOther, rec.Code)
	require.Equal(t, "/", rec.Header().Get("Location"))

	cookies := rec.Result().Cookies()
	var sessionCookie *http.Cookie
	for _, c := range cookies {
		if c.Name == session.CookieName {
			sessionCookie = c
		}
	}
	require.NotNil(t, sessionCookie, "expected a session cookie to be set")
	require.NotEmpty(t, sessionCookie.Value)
}

func TestLoginFlow_WrongPasswordRedirectsWithError(t *testing.T) {
	h, _ := newTestHandler(t, &fakeHistory{})

	form := url.Values{"username": {"alex"}, "password": {"wrong"}}
	req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	require.Equal(t, http.StatusSeeOther, rec.Code)
	require.Equal(t, "/login?error=1", rec.Header().Get("Location"))

	for _, c := range rec.Result().Cookies() {
		require.NotEqual(t, session.CookieName, c.Name)
	}
}

func TestLogout_DestroysSession(t *testing.T) {
	h, sessions := newTestHandler(t, &fakeHistory{})

	token, err := sessions.Create("alex")
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodPost, "/logout", nil)
	req.AddCookie(&http.Cookie{Name: session.CookieName, Value: token})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	require.Equal(t, http.StatusSeeOther, rec.Code)
	require.Equal(t, "/login", rec.Header().Get("Location"))

	_, ok := sessions.Validate(token)
	require.False(t, ok, "session should be destroyed after logout")
}

func TestHandleStatic_ServesCompiledStylesheetWithoutAuth(t *testing.T) {
	h, _ := newTestHandler(t, &fakeHistory{})

	req := httptest.NewRequest(http.MethodGet, "/static/css/styles.css", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	require.Contains(t, rec.Body.String(), "tailwindcss")
}

func TestServesLocalAvatarFiles(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "alex.png"), []byte("fake-png-bytes"), 0o644))

	registry, err := users.NewRegistry([]config.UserConfig{{Username: "alex", PasswordHash: mustHash(t, "555")}})
	require.NoError(t, err)
	sessions := session.NewStore(time.Hour)
	h, err := New(&fakeHistory{}, registry, sessions, "ru", dir)
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodGet, "/static/avatars/alex.png", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "fake-png-bytes", rec.Body.String())
}

func TestHandleDialogs_RequiresAuth(t *testing.T) {
	h, _ := newTestHandler(t, &fakeHistory{})

	req := httptest.NewRequest(http.MethodGet, "/api/dialogs?user_id=alex", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	require.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestHandleDialogs_RequiresUserID(t *testing.T) {
	h, sessions := newTestHandler(t, &fakeHistory{})

	req := authedRequest(t, sessions, http.MethodGet, "/api/dialogs")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	require.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestHandleDialogs_ReturnsConversationsJSON(t *testing.T) {
	fake := &fakeHistory{conversations: []history.Conversation{
		{ID: "conv-1", UserID: "alex", Source: "cli", StartedAt: time.Now()},
	}}
	h, sessions := newTestHandler(t, fake)

	req := authedRequest(t, sessions, http.MethodGet, "/api/dialogs?user_id=alex")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	var out []history.Conversation
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&out))
	require.Len(t, out, 1)
	require.Equal(t, "conv-1", out[0].ID)
}

func TestHandleDialogMessages_ReturnsMessagesJSON(t *testing.T) {
	fake := &fakeHistory{messages: []history.Message{
		{ID: 1, Role: "user", Content: "привет"},
	}}
	h, sessions := newTestHandler(t, fake)

	req := authedRequest(t, sessions, http.MethodGet, "/api/dialogs/conv-1")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	var out []history.Message
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&out))
	require.Len(t, out, 1)
	require.Equal(t, "привет", out[0].Content)
}
