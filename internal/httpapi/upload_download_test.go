package httpapi

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/archer-developer/miranda/internal/attachments"
	"github.com/archer-developer/miranda/internal/config"
	"github.com/archer-developer/miranda/internal/llm/llmtest"
	"github.com/archer-developer/miranda/internal/session"
	"github.com/archer-developer/miranda/internal/users"
)

// fakeSandboxFiles serves GET /{file_id} the same way the sandbox's own
// GET /files/{id} does, for handleDownload to proxy against.
func fakeSandboxFiles(t *testing.T, body string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte(body))
	}))
}

func newDownloadTestServer(t *testing.T, registry *users.Registry, sessions *session.Store, authToken string) (*Server, *attachments.Store, string) {
	t.Helper()

	provider := llmtest.New("local")
	o, _, _ := newTestOrchestrator(t, provider)

	store := attachments.NewStore(time.Hour)
	t.Cleanup(store.Close)
	o.SetAttachmentStore(store)

	sandbox := fakeSandboxFiles(t, "file contents")
	t.Cleanup(sandbox.Close)

	server := NewServer(o, o.hub, authToken, nil, nil, registry, sessions)
	server.SetUploadHandler(sandbox.URL, "", 0)

	return server, store, sandbox.URL
}

func TestHandleDownload_SessionCookieRejectsAnotherUsersFile(t *testing.T) {
	registry, err := users.NewRegistry([]config.UserConfig{{Username: "anna", PasswordHash: "x"}, {Username: "archer", PasswordHash: "x"}})
	require.NoError(t, err)
	sessions := session.NewStore(time.Hour)
	token, err := sessions.Create("archer")
	require.NoError(t, err)

	server, store, _ := newDownloadTestServer(t, registry, sessions, "secret")
	store.Put(attachments.Record{UserID: "anna", FileID: "f1", Filename: "report.pdf"})

	ts := httptest.NewServer(server)
	defer ts.Close()

	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/files/f1", nil)
	req.AddCookie(&http.Cookie{Name: session.CookieName, Value: token})
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusNotFound, resp.StatusCode, "a logged-in user must not be able to fetch another household member's file")
}

func TestHandleDownload_SessionCookieAllowsOwnFile(t *testing.T) {
	registry, err := users.NewRegistry([]config.UserConfig{{Username: "anna", PasswordHash: "x"}})
	require.NoError(t, err)
	sessions := session.NewStore(time.Hour)
	token, err := sessions.Create("anna")
	require.NoError(t, err)

	server, store, _ := newDownloadTestServer(t, registry, sessions, "secret")
	store.Put(attachments.Record{UserID: "anna", FileID: "f1", Filename: "report.pdf"})

	ts := httptest.NewServer(server)
	defer ts.Close()

	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/files/f1", nil)
	req.AddCookie(&http.Cookie{Name: session.CookieName, Value: token})
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	got, _ := io.ReadAll(resp.Body)
	require.Equal(t, "file contents", string(got))
}

// Before this fix, a bearer-token request skipped the ownership check
// entirely (sessionUser == "" short-circuited the whole condition), so any
// caller holding the token could fetch any household member's downloaded
// file. Now a bearer-token caller must identify itself via ?user_id=, the
// same way InputRequest.UserID lets it identify itself for POST /api/v1/input.
func TestHandleDownload_BearerTokenWithoutUserIDCannotFetchOwnedFile(t *testing.T) {
	registry, err := users.NewRegistry([]config.UserConfig{{Username: "anna", PasswordHash: "x"}})
	require.NoError(t, err)

	server, store, _ := newDownloadTestServer(t, registry, nil, "secret")
	store.Put(attachments.Record{UserID: "anna", FileID: "f1", Filename: "report.pdf"})

	ts := httptest.NewServer(server)
	defer ts.Close()

	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/files/f1", nil)
	req.Header.Set("Authorization", "Bearer secret")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusNotFound, resp.StatusCode)
}

func TestHandleDownload_BearerTokenWithMatchingUserIDCanFetchOwnedFile(t *testing.T) {
	registry, err := users.NewRegistry([]config.UserConfig{{Username: "anna", PasswordHash: "x"}})
	require.NoError(t, err)

	server, store, _ := newDownloadTestServer(t, registry, nil, "secret")
	store.Put(attachments.Record{UserID: "anna", FileID: "f1", Filename: "report.pdf"})

	ts := httptest.NewServer(server)
	defer ts.Close()

	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/files/f1?user_id=anna", nil)
	req.Header.Set("Authorization", "Bearer secret")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
}

func TestHandleDownload_BearerTokenWithWrongUserIDCannotFetchOwnedFile(t *testing.T) {
	registry, err := users.NewRegistry([]config.UserConfig{{Username: "anna", PasswordHash: "x"}, {Username: "archer", PasswordHash: "x"}})
	require.NoError(t, err)

	server, store, _ := newDownloadTestServer(t, registry, nil, "secret")
	store.Put(attachments.Record{UserID: "anna", FileID: "f1", Filename: "report.pdf"})

	ts := httptest.NewServer(server)
	defer ts.Close()

	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/files/f1?user_id=archer", nil)
	req.Header.Set("Authorization", "Bearer secret")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusNotFound, resp.StatusCode)
}

func TestHandleDownload_NilAttachStoreFailsClosed(t *testing.T) {
	provider := llmtest.New("local")
	o, _, _ := newTestOrchestrator(t, provider)
	// Deliberately never call o.SetAttachmentStore.

	sandbox := fakeSandboxFiles(t, "file contents")
	defer sandbox.Close()

	server := NewServer(o, o.hub, "", nil, nil, nil, nil)
	server.SetUploadHandler(sandbox.URL, "", 0)

	ts := httptest.NewServer(server)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/api/files/f1")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusInternalServerError, resp.StatusCode, "must fail closed, not silently skip the ownership check, when attachStore is nil")
}
