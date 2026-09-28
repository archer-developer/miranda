package webui

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/archer-developer/miranda/internal/config"
	"github.com/archer-developer/miranda/internal/notify"
	"github.com/archer-developer/miranda/internal/session"
	"github.com/archer-developer/miranda/internal/users"
)

// fakeNotify implements Notify with canned results/errors, recording the
// userID/args each method was called with — mirrors fakeWebAuthnService's
// shape in webauthn_test.go.
type fakeNotify struct {
	notifications []notify.Notification
	listErr       error
	unreadCount   int
	unreadErr     error
	markReadErr   error
	subscribeErr  error
	unsubErr      error
	webPush       bool
	vapidKey      string

	gotListUserID     string
	gotMarkReadUserID string
	gotUnreadUserID   string
	gotSubscribe      notify.Subscription
	gotUnsubEndpoint  string
	gotUnsubUserID    string
}

func (f *fakeNotify) ListForUser(ctx context.Context, userID string, limit int) ([]notify.Notification, error) {
	f.gotListUserID = userID
	return f.notifications, f.listErr
}

func (f *fakeNotify) UnreadCount(ctx context.Context, userID string) (int, error) {
	f.gotUnreadUserID = userID
	return f.unreadCount, f.unreadErr
}

func (f *fakeNotify) MarkAllRead(ctx context.Context, userID string) error {
	f.gotMarkReadUserID = userID
	return f.markReadErr
}

func (f *fakeNotify) Subscribe(ctx context.Context, sub notify.Subscription) error {
	f.gotSubscribe = sub
	return f.subscribeErr
}

func (f *fakeNotify) Unsubscribe(ctx context.Context, endpoint, userID string) error {
	f.gotUnsubEndpoint, f.gotUnsubUserID = endpoint, userID
	return f.unsubErr
}

func (f *fakeNotify) WebPushEnabled() bool   { return f.webPush }
func (f *fakeNotify) VAPIDPublicKey() string { return f.vapidKey }

// newTestHandlerWithNotify builds a Handler with the notification feed
// enabled (a fakeNotify), one configured user ("alex"/"555"), and returns
// the handler, its session store, and the fake so tests can set canned
// responses and assert on recorded calls.
func newTestHandlerWithNotify(t *testing.T, fake *fakeNotify) (*Handler, *session.Store) {
	t.Helper()
	registry, err := users.NewRegistry([]config.UserConfig{
		{Username: "alex", PasswordHash: mustHash(t, "555"), FullName: "Alex"},
	})
	require.NoError(t, err)
	sessions := session.NewStore(time.Hour)

	h, err := New(&fakeHistory{}, newFakeMemory(), nil, nil, fake, nil, registry, sessions, "ru", "", testLogger())
	require.NoError(t, err)
	return h, sessions
}

func TestNotificationRoutes_NotRegisteredWhenServiceIsNil(t *testing.T) {
	h, _ := newTestHandler(t, &fakeHistory{}) // nil Notify

	req := httptest.NewRequest(http.MethodGet, "/api/notifications", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	require.Equal(t, http.StatusNotFound, rec.Code)
}

func TestHandleGetNotifications_RequiresAuth(t *testing.T) {
	h, _ := newTestHandlerWithNotify(t, &fakeNotify{})

	req := httptest.NewRequest(http.MethodGet, "/api/notifications", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	require.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestHandleGetNotifications_ScopedToCallingUser(t *testing.T) {
	fake := &fakeNotify{notifications: []notify.Notification{{ID: "n1", Title: "Miranda", Body: "hello"}}}
	h, sessions := newTestHandlerWithNotify(t, fake)

	req := authedRequest(t, sessions, http.MethodGet, "/api/notifications")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "alex", fake.gotListUserID, "must scope to the logged-in user, never a client-supplied id")

	var out notificationsView
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&out))
	require.Len(t, out.Notifications, 1)
	require.Equal(t, "hello", out.Notifications[0].Body)
}

func TestHandlePostNotificationsRead_MarksCallingUserOnly(t *testing.T) {
	fake := &fakeNotify{}
	h, sessions := newTestHandlerWithNotify(t, fake)

	req := authedRequest(t, sessions, http.MethodPost, "/api/notifications/read")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	require.Equal(t, http.StatusNoContent, rec.Code)
	require.Equal(t, "alex", fake.gotMarkReadUserID)
}

func TestHandleGetUnreadCount_ReturnsCount(t *testing.T) {
	fake := &fakeNotify{unreadCount: 3}
	h, sessions := newTestHandlerWithNotify(t, fake)

	req := authedRequest(t, sessions, http.MethodGet, "/api/notifications/unread-count")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "alex", fake.gotUnreadUserID)

	var out unreadCountView
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&out))
	require.Equal(t, 3, out.Count)
}

func TestPushRoutes_NotRegisteredWhenWebPushDisabled(t *testing.T) {
	h, sessions := newTestHandlerWithNotify(t, &fakeNotify{webPush: false})

	req := authedRequest(t, sessions, http.MethodGet, "/api/push/vapid-key")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	require.Equal(t, http.StatusNotFound, rec.Code, "the notification feed being enabled must not imply push routes exist")
}

func TestHandleGetVAPIDKey_ReturnsPublicKey(t *testing.T) {
	fake := &fakeNotify{webPush: true, vapidKey: "pub-key-123"}
	h, sessions := newTestHandlerWithNotify(t, fake)

	req := authedRequest(t, sessions, http.MethodGet, "/api/push/vapid-key")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "pub-key-123", rec.Body.String())
}

// authedRequestWithBody is authedRequest plus a request body — the memory
// PUT tests (webui_test.go) build this by hand each time; factored out here
// since the push subscribe/unsubscribe tests below need it three times.
func authedRequestWithBody(t *testing.T, sessions *session.Store, method, target string, body []byte) *http.Request {
	t.Helper()
	token, err := sessions.Create("alex")
	require.NoError(t, err)
	req := httptest.NewRequest(method, target, bytes.NewReader(body))
	req.AddCookie(&http.Cookie{Name: session.CookieName, Value: token})
	return req
}

func TestHandlePostPushSubscribe_ScopedToCallingUser(t *testing.T) {
	fake := &fakeNotify{webPush: true}
	h, sessions := newTestHandlerWithNotify(t, fake)

	body := []byte(`{"endpoint":"https://push.example/1","keys":{"p256dh":"p","auth":"a"}}`)
	req := authedRequestWithBody(t, sessions, http.MethodPost, "/api/push/subscribe", body)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	require.Equal(t, http.StatusNoContent, rec.Code)
	require.Equal(t, "alex", fake.gotSubscribe.UserID, "must always subscribe as the logged-in user, regardless of request body")
	require.Equal(t, "https://push.example/1", fake.gotSubscribe.Endpoint)
	require.Equal(t, "p", fake.gotSubscribe.P256dh)
	require.Equal(t, "a", fake.gotSubscribe.Auth)
}

func TestHandleDeletePushSubscribe_NotFoundWhenNotOwned(t *testing.T) {
	fake := &fakeNotify{webPush: true, unsubErr: notify.ErrNotFound}
	h, sessions := newTestHandlerWithNotify(t, fake)

	body := []byte(`{"endpoint":"https://push.example/1"}`)
	req := authedRequestWithBody(t, sessions, http.MethodDelete, "/api/push/subscribe", body)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	require.Equal(t, http.StatusNotFound, rec.Code)
	require.Equal(t, "alex", fake.gotUnsubUserID, "ownership must be checked against the logged-in user, never trusted from the body")
}

func TestHandleDeletePushSubscribe_Success(t *testing.T) {
	fake := &fakeNotify{webPush: true}
	h, sessions := newTestHandlerWithNotify(t, fake)

	body := []byte(`{"endpoint":"https://push.example/1"}`)
	req := authedRequestWithBody(t, sessions, http.MethodDelete, "/api/push/subscribe", body)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	require.Equal(t, http.StatusNoContent, rec.Code)
	require.Equal(t, "https://push.example/1", fake.gotUnsubEndpoint)
}
