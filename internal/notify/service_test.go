package notify

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	webpush "github.com/SherClockHolmes/webpush-go"
	"github.com/stretchr/testify/require"
)

// stubHTTPClient lets service_test.go exercise Service.pushToSubscriptions'
// status-code handling without hitting a real push service — it satisfies
// webpush.HTTPClient (just Do(*http.Request) (*http.Response, error)).
type stubHTTPClient struct {
	statusByEndpoint map[string]int // request URL -> status to return
	calls            []string       // endpoints actually POSTed to, in order
	authHeaders      []string       // this call's Authorization header, same order as calls
}

func (c *stubHTTPClient) Do(req *http.Request) (*http.Response, error) {
	c.calls = append(c.calls, req.URL.String())
	c.authHeaders = append(c.authHeaders, req.Header.Get("Authorization"))
	status := c.statusByEndpoint[req.URL.String()]
	if status == 0 {
		status = http.StatusCreated
	}
	return &http.Response{StatusCode: status, Body: io.NopCloser(bytes.NewReader(nil))}, nil
}

// vapidJWTSubject decodes the unverified "sub" claim out of a
// `vapid t=<jwt>, k=<key>` Authorization header — enough to confirm what
// normalizeVAPIDSubject actually produced without needing to verify the
// ES256 signature.
func vapidJWTSubject(t *testing.T, authHeader string) string {
	t.Helper()
	tPart, _, ok := strings.Cut(authHeader, ", k=")
	require.True(t, ok, "unexpected Authorization header shape: %s", authHeader)
	jwtStr := strings.TrimPrefix(tPart, "vapid t=")
	segments := strings.Split(jwtStr, ".")
	require.Len(t, segments, 3, "JWT must have 3 dot-separated segments")
	payload, err := base64.RawURLEncoding.DecodeString(segments[1])
	require.NoError(t, err)
	var claims struct {
		Sub string `json:"sub"`
	}
	require.NoError(t, json.Unmarshal(payload, &claims))
	return claims.Sub
}

func newTestService(t *testing.T, webpushCfg *WebPushConfig) (*Service, *Store) {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), "notify.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	return NewService(store, webpushCfg, nil), store
}

func TestNotify_FeedOnly_PersistsWithoutPush(t *testing.T) {
	ctx := context.Background()
	svc, store := newTestService(t, nil) // webpush disabled

	n, err := svc.Notify(ctx, "alice", "Miranda", "hello", "tool")
	require.NoError(t, err)
	require.NotEmpty(t, n.ID)

	got, err := store.ListForUser(ctx, "alice", 0)
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, "hello", got[0].Body)
}

func TestNotify_PushesToEveryRegisteredSubscription(t *testing.T) {
	ctx := context.Background()
	stub := &stubHTTPClient{}
	svc, store := newTestService(t, &WebPushConfig{
		VAPIDPublicKey:  "pub",
		VAPIDPrivateKey: mustGenerateVAPIDPrivateKey(t),
		Subject:         "mailto:test@example.com",
		HTTPClient:      stub,
	})

	require.NoError(t, store.UpsertSubscription(ctx, Subscription{Endpoint: "https://push.example/1", UserID: "alice", P256dh: validP256dh, Auth: validAuth}))
	require.NoError(t, store.UpsertSubscription(ctx, Subscription{Endpoint: "https://push.example/2", UserID: "alice", P256dh: validP256dh, Auth: validAuth}))
	// A subscription belonging to someone else must never receive alice's push.
	require.NoError(t, store.UpsertSubscription(ctx, Subscription{Endpoint: "https://push.example/bob", UserID: "bob", P256dh: validP256dh, Auth: validAuth}))

	_, err := svc.Notify(ctx, "alice", "Miranda", "dinner's ready", "tool")
	require.NoError(t, err)

	require.ElementsMatch(t, []string{"https://push.example/1", "https://push.example/2"}, stub.calls)
}

func TestNotify_PrunesDeadSubscriptionOn410(t *testing.T) {
	ctx := context.Background()
	stub := &stubHTTPClient{statusByEndpoint: map[string]int{"https://push.example/dead": http.StatusGone}}
	svc, store := newTestService(t, &WebPushConfig{
		VAPIDPublicKey:  "pub",
		VAPIDPrivateKey: mustGenerateVAPIDPrivateKey(t),
		Subject:         "mailto:test@example.com",
		HTTPClient:      stub,
	})
	require.NoError(t, store.UpsertSubscription(ctx, Subscription{Endpoint: "https://push.example/dead", UserID: "alice", P256dh: validP256dh, Auth: validAuth}))

	_, err := svc.Notify(ctx, "alice", "Miranda", "text", "tool")
	require.NoError(t, err, "a dead subscription must never fail the whole Notify call")

	got, err := store.SubscriptionsForUser(ctx, "alice")
	require.NoError(t, err)
	require.Empty(t, got, "a 410 response must prune the subscription so it's never retried again")
}

func TestNotify_OneFailingSubscriptionDoesNotBlockAnother(t *testing.T) {
	ctx := context.Background()
	stub := &stubHTTPClient{statusByEndpoint: map[string]int{"https://push.example/404": http.StatusNotFound}}
	svc, store := newTestService(t, &WebPushConfig{
		VAPIDPublicKey:  "pub",
		VAPIDPrivateKey: mustGenerateVAPIDPrivateKey(t),
		Subject:         "mailto:test@example.com",
		HTTPClient:      stub,
	})
	require.NoError(t, store.UpsertSubscription(ctx, Subscription{Endpoint: "https://push.example/404", UserID: "alice", P256dh: validP256dh, Auth: validAuth}))
	require.NoError(t, store.UpsertSubscription(ctx, Subscription{Endpoint: "https://push.example/ok", UserID: "alice", P256dh: validP256dh, Auth: validAuth}))

	_, err := svc.Notify(ctx, "alice", "Miranda", "text", "tool")
	require.NoError(t, err)

	require.ElementsMatch(t, []string{"https://push.example/404", "https://push.example/ok"}, stub.calls, "the 404 subscription must not stop the send loop from reaching the other one")

	got, err := store.SubscriptionsForUser(ctx, "alice")
	require.NoError(t, err)
	require.Len(t, got, 1, "only the 404'd endpoint should be pruned")
	require.Equal(t, "https://push.example/ok", got[0].Endpoint)
}

func TestNormalizeVAPIDSubject(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"mailto lowercase", "mailto:foo@bar.com", "foo@bar.com"},
		{"mailto mixed case prefix", "Mailto:foo@bar.com", "foo@bar.com"},
		{"bare email untouched", "foo@bar.com", "foo@bar.com"},
		{"https url untouched", "https://example.com/contact", "https://example.com/contact"},
		{"empty string untouched", "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			require.Equal(t, c.want, normalizeVAPIDSubject(c.in))
		})
	}
}

// TestNotify_VAPIDSubjectNotDoubled is the regression test for the actual
// bug: webpush-go's own getVAPIDAuthorizationHeader re-prepends "mailto:"
// to any Subscriber that doesn't already start with "https:" — passing our
// config's "mailto:..." subject straight through used to double it into
// "mailto:mailto:...", which Apple's push service (web.push.apple.com)
// rejects with 403 "BadJwtToken" (Google's FCM tolerated it, which is why
// this only ever broke iOS/Safari, never Android/Chrome — see
// normalizeVAPIDSubject's doc comment). This decodes the real JWT
// webpush-go sends and checks the "sub" claim webpush-go itself produced,
// so it would have caught the bug even if normalizeVAPIDSubject's own unit
// test above were wrong about what webpush-go does with its input.
func TestNotify_VAPIDSubjectNotDoubled(t *testing.T) {
	ctx := context.Background()
	stub := &stubHTTPClient{}
	svc, store := newTestService(t, &WebPushConfig{
		VAPIDPublicKey:  "pub",
		VAPIDPrivateKey: mustGenerateVAPIDPrivateKey(t),
		Subject:         "mailto:test@example.com",
		HTTPClient:      stub,
	})
	require.NoError(t, store.UpsertSubscription(ctx, Subscription{Endpoint: "https://push.example/1", UserID: "alice", P256dh: validP256dh, Auth: validAuth}))

	_, err := svc.Notify(ctx, "alice", "Miranda", "text", "tool")
	require.NoError(t, err)

	require.Len(t, stub.authHeaders, 1)
	require.Equal(t, "mailto:test@example.com", vapidJWTSubject(t, stub.authHeaders[0]))
}

func mustGenerateVAPIDPrivateKey(t *testing.T) string {
	t.Helper()
	priv, _, err := webpush.GenerateVAPIDKeys()
	require.NoError(t, err)
	return priv
}

// validP256dh/validAuth are syntactically valid (right length, base64url)
// placeholder subscriber keys — SendNotificationWithContext decodes and
// runs real ECDH/HKDF on them before ever making the HTTP request our stub
// intercepts, so they need to actually decode and land on the P-256 curve,
// even though no real browser or push service is involved in these tests.
// Taken from webpush-go's own test fixtures (webpush_test.go) — a known
// point actually on the P-256 curve, since SendNotificationWithContext
// rejects anything that isn't before our stub HTTP client ever sees the
// request.
const (
	validP256dh = "BNNL5ZaTfK81qhXOx23-wewhigUeFb632jN6LvRWCFH1ubQr77FE_9qV1FuojuRmHP42zmf34rXgW80OvUVDgTk"
	validAuth   = "zqbxT6JKstKSY9JKibZLSQ"
)
