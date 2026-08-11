package httpapi

import (
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
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
	server.SetUploadHandler(0)

	return server, store, sandbox.URL
}

func TestHandleDownload_SessionCookieRejectsAnotherUsersFile(t *testing.T) {
	registry, err := users.NewRegistry([]config.UserConfig{{Username: "anna", PasswordHash: "x"}, {Username: "archer", PasswordHash: "x"}})
	require.NoError(t, err)
	sessions := session.NewStore(time.Hour)
	token, err := sessions.Create("archer")
	require.NoError(t, err)

	server, store, sandboxURL := newDownloadTestServer(t, registry, sessions, "secret")
	store.Put(attachments.Record{UserID: "anna", FileID: "f1", Filename: "report.pdf", RemoteURL: sandboxURL + "/f1"})

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

	server, store, sandboxURL := newDownloadTestServer(t, registry, sessions, "secret")
	store.Put(attachments.Record{UserID: "anna", FileID: "f1", Filename: "report.pdf", RemoteURL: sandboxURL + "/f1"})

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

	server, store, sandboxURL := newDownloadTestServer(t, registry, nil, "secret")
	store.Put(attachments.Record{UserID: "anna", FileID: "f1", Filename: "report.pdf", RemoteURL: sandboxURL + "/f1"})

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

	server, store, sandboxURL := newDownloadTestServer(t, registry, nil, "secret")
	store.Put(attachments.Record{UserID: "anna", FileID: "f1", Filename: "report.pdf", RemoteURL: sandboxURL + "/f1"})

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

	server, store, sandboxURL := newDownloadTestServer(t, registry, nil, "secret")
	store.Put(attachments.Record{UserID: "anna", FileID: "f1", Filename: "report.pdf", RemoteURL: sandboxURL + "/f1"})

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

	server := NewServer(o, o.hub, "", nil, nil, nil, nil)
	server.SetUploadHandler(0)

	ts := httptest.NewServer(server)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/api/files/f1")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusInternalServerError, resp.StatusCode, "must fail closed, not silently skip the ownership check, when attachStore is nil")
}

// GET /files/{id} (handleFilesServe) is the new, deliberately unauthenticated
// route external MCP servers pull an uploaded attachment's bytes from — see
// docs/file-staging-refactor.md. Distinct from GET /api/files/{file_id}
// (handleDownload) tested above.

func TestHandleFilesServe_ServesKnownFile(t *testing.T) {
	provider := llmtest.New("local")
	o, _, _ := newTestOrchestrator(t, provider)
	store := attachments.NewStore(time.Hour)
	t.Cleanup(store.Close)
	o.SetAttachmentStore(store)
	store.Put(attachments.Record{FileID: "abc123", Filename: "scan.pdf", MIMEType: "application/pdf", Data: []byte("%PDF-bytes")})

	server := NewServer(o, o.hub, "", nil, nil, nil, nil)
	server.SetUploadHandler(0)
	ts := httptest.NewServer(server)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/files/abc123")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, "application/pdf", resp.Header.Get("Content-Type"))
	body, _ := io.ReadAll(resp.Body)
	require.Equal(t, "%PDF-bytes", string(body))
}

func TestHandleFilesServe_UnknownIDReturns404(t *testing.T) {
	provider := llmtest.New("local")
	o, _, _ := newTestOrchestrator(t, provider)
	store := attachments.NewStore(time.Hour)
	t.Cleanup(store.Close)
	o.SetAttachmentStore(store)

	server := NewServer(o, o.hub, "", nil, nil, nil, nil)
	server.SetUploadHandler(0)
	ts := httptest.NewServer(server)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/files/does-not-exist")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusNotFound, resp.StatusCode)
}

// TestHandleUpload_StagesLocallyWithoutForwardingAnywhere guards the actual
// point of this refactor: POST /api/upload must never make an outbound
// request at all — it stages bytes straight into attachStore, with no
// remote target configured anywhere in this test — and the returned
// file_id must be immediately fetchable back from GET /files/{id}.
func TestHandleUpload_StagesLocallyWithoutForwardingAnywhere(t *testing.T) {
	provider := llmtest.New("local")
	o, _, _ := newTestOrchestrator(t, provider)
	store := attachments.NewStore(time.Hour)
	t.Cleanup(store.Close)
	o.SetAttachmentStore(store)

	server := NewServer(o, o.hub, "", nil, nil, nil, nil)
	server.SetUploadHandler(10 << 20)
	ts := httptest.NewServer(server)
	defer ts.Close()

	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	fw, err := mw.CreateFormFile("file", "scan.pdf")
	require.NoError(t, err)
	_, err = fw.Write([]byte("%PDF-bytes"))
	require.NoError(t, err)
	require.NoError(t, mw.Close())

	req, err := http.NewRequest(http.MethodPost, ts.URL+"/api/upload", &body)
	require.NoError(t, err)
	req.Header.Set("Content-Type", mw.FormDataContentType())

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var uploadResp UploadResponse
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&uploadResp))
	require.NotEmpty(t, uploadResp.FileID)
	require.Equal(t, "scan.pdf", uploadResp.Filename)

	filesResp, err := http.Get(ts.URL + "/files/" + uploadResp.FileID)
	require.NoError(t, err)
	defer filesResp.Body.Close()
	require.Equal(t, http.StatusOK, filesResp.StatusCode)
	got, _ := io.ReadAll(filesResp.Body)
	require.Equal(t, "%PDF-bytes", string(got))
}
