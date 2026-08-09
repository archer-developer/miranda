package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/archer-developer/miranda/internal/attachments"
)

// sandboxDownloadTimeout caps how long handleDownload will wait on the
// sandbox, including the time spent streaming the body back to the caller —
// longer than sandboxUploadTimeout because a download_file result can be a
// video or other large artifact (sandbox default max_download_size_bytes is
// 200 MB) rather than the smaller uploads this endpoint's sibling handles.
// 30 minutes gives a ~200MB transfer room to complete over a slow/mobile
// client (5 minutes required an unrealistic ~700KB/s sustained minimum);
// the deadline still bounds the request so a genuinely stalled sandbox or
// client can't hold the connection open indefinitely.
const sandboxDownloadTimeout = 30 * time.Minute

// UploadResponse is the JSON body POST /api/upload returns to the web UI on
// a successful upload: Miranda's own attachStore-assigned file_id (see
// attachments.NewFileID), for the client to include in a subsequent
// InputRequest.Attachments list.
type UploadResponse struct {
	FileID    string `json:"file_id"`
	Filename  string `json:"filename"`
	SizeBytes int64  `json:"size_bytes"`
	MIMEType  string `json:"mime_type"`
}

// handleUpload reads a multipart file upload straight into the
// orchestrator's attachment store — Miranda is the file's canonical host
// now, it never forwards the bytes anywhere at upload time (see
// docs/file-staging-refactor.md). processAttachments later turns the
// returned file_id into a fileURI under
// config.FileUploadConfig.PublicBaseURL that any tool needing the bytes
// fetches for itself via a plain HTTP GET.
//
// Auth: same dual bearer-token / session-cookie check as /api/v1/input.
// Content-Type: multipart/form-data; the file must be in a field named "file".
// Size limit: config.FileUploadConfig.MaxFileSizeBytes (applied to the raw
// file bytes, not the full multipart envelope).
//
// On success the handler replies with HTTP 200 and a JSON UploadResponse
// body; the caller is expected to include the returned file_id in the next
// InputRequest.Attachments list.
func (s *Server) handleUpload(w http.ResponseWriter, r *http.Request) {
	sessionUser, ok := s.authorize(r)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if s.upload == nil {
		// Safety valve: the route should not be registered when upload is nil,
		// but guard here so a misconfigured mux never panics.
		http.Error(w, "file upload is not configured", http.StatusNotImplemented)
		return
	}
	if s.orchestrator.attachStore == nil {
		http.Error(w, "attachment store is not configured", http.StatusInternalServerError)
		return
	}

	// Limit the total multipart read to maxBytes + a small overhead for the
	// multipart envelope itself (boundary, headers) — the actual file byte
	// count is checked separately after reading the part.
	r.Body = http.MaxBytesReader(w, r.Body, s.upload.maxBytes+4096)

	mediaType, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || !strings.HasPrefix(mediaType, "multipart/") {
		http.Error(w, "expected multipart/form-data", http.StatusBadRequest)
		return
	}

	mr := multipart.NewReader(r.Body, params["boundary"])
	var filePart *multipart.Part
	for {
		p, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			http.Error(w, "failed to parse multipart body: "+err.Error(), http.StatusBadRequest)
			return
		}
		if p.FormName() == "file" {
			filePart = p
			break
		}
		// Skip non-file parts (e.g. a stray text field).
		_ = p.Close()
	}
	if filePart == nil {
		http.Error(w, "missing \"file\" part in multipart body", http.StatusBadRequest)
		return
	}
	defer func() { _ = filePart.Close() }()

	// Read file bytes into memory, capped by the configured limit.
	limitedReader := &io.LimitedReader{R: filePart, N: s.upload.maxBytes + 1}
	fileBytes, err := io.ReadAll(limitedReader)
	if err != nil {
		http.Error(w, "failed to read file: "+err.Error(), http.StatusBadRequest)
		return
	}
	if int64(len(fileBytes)) > s.upload.maxBytes {
		http.Error(w, fmt.Sprintf("file exceeds the %d-byte limit", s.upload.maxBytes), http.StatusRequestEntityTooLarge)
		return
	}

	filename := filePart.FileName()
	if filename == "" {
		filename = "upload"
	}

	// Detect MIME type from the Content-Type header on the part first
	// (the browser sets this from the file's OS type), falling back to
	// "application/octet-stream" if absent or unparseable.
	partMIME := filePart.Header.Get("Content-Type")
	if partMIME == "" {
		partMIME = "application/octet-stream"
	}
	// Strip any parameters (e.g. charset) — the sandbox and vision APIs
	// want the bare type/subtype only.
	if mt, _, err := mime.ParseMediaType(partMIME); err == nil {
		partMIME = mt
	}

	fileID, err := attachments.NewFileID()
	if err != nil {
		s.logger.Error("upload: generate file id failed", "error", err)
		http.Error(w, "failed to generate file id", http.StatusInternalServerError)
		return
	}

	// Bytes are buffered for as long as the record's TTL allows — not just
	// for inlining into the prompt (images for vision, text for context),
	// but so GET /files/{id} (handleFilesServe) can serve them to whichever
	// external tool the model hands the resulting fileURI to (see
	// docs/file-staging-refactor.md). UserID is bound to the record so
	// processAttachments rejects a different user's attempt to reference
	// this file_id.
	rec := attachments.Record{
		UserID:   sessionUser,
		FileID:   fileID,
		Filename: filename,
		MIMEType: partMIME,
		Size:     int64(len(fileBytes)),
		Data:     fileBytes,
	}
	s.orchestrator.attachStore.Put(rec)

	resp := UploadResponse{FileID: fileID, Filename: filename, SizeBytes: rec.Size, MIMEType: partMIME}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// handleFilesServe serves a locally-staged attachment's raw bytes at
// GET /files/{id} — the endpoint any external MCP server's tool fetches a
// fileURI from (see docs/file-staging-refactor.md and
// config.FileUploadConfig.PublicBaseURL). Deliberately unauthenticated,
// mirroring internal/tts.HTTPHandler's GET /tts-audio/{filename} (the same
// "a LAN service needs to fetch a Miranda-hosted resource by URL" problem,
// already solved there the same way): the id's own randomness
// (attachments.NewFileID, crypto/rand-backed) plus the store's TTL is the
// security boundary, not a session or bearer token — this route serves
// backend services pulling a file they were handed a capability URL for,
// not a user's browser (that's the separate, authenticated
// GET /api/files/{file_id} — see handleDownload).
func (s *Server) handleFilesServe(w http.ResponseWriter, r *http.Request) {
	if s.orchestrator.attachStore == nil {
		http.Error(w, "file staging is not configured", http.StatusNotImplemented)
		return
	}
	id := r.PathValue("id")
	rec, found := s.orchestrator.attachStore.Get(id)
	if !found {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if rec.Data == nil {
		// Shouldn't happen for an upload-staged record (handleUpload always
		// buffers Data) — only a download_file ownership record omits it,
		// and those aren't reachable through this id space by construction.
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", rec.MIMEType)
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", rec.Filename))
	_, _ = w.Write(rec.Data)
}

// handleDownload proxies a GET request for a file the model retrieved from a
// sandbox session via the download_file MCP tool (see
// ../../miranda-code-execution-sandbox/CLAUDE.md's "File download flow").
// The file_id path value is what that tool call returned and what
// appendDownloadMarkers (internal/httpapi/agent_loop.go) embeds in a
// <download>...</download> marker in the assistant's reply — the web UI chat
// screen (chat.js's extractDownloadBlocks) turns that marker into a chip
// linking straight to this route.
//
// Auth: same dual bearer-token / session-cookie check as /api/v1/input and
// POST /api/upload. The file's recorded owner (o.attachStore, populated by
// executeTool at download_file call time with the actual resolved userID,
// not just the session identity — see executeTool) must match the
// requesting user — a mismatched or unknown file_id 404s rather than
// revealing whether it exists, the same IDOR-safe pattern GET
// /api/dialogs/{id} uses. Session-cookie auth identifies the requester as
// the logged-in user, same as every other endpoint. Bearer-token auth has
// no identity of its own, so (mirroring how POST /api/v1/input lets a
// bearer-token caller supply InputRequest.UserID) the caller must pass a
// "user_id" query parameter to be recognized as anyone in particular; with
// none given, requestingUser is "" and only ever matches an ownerless
// record (rec.UserID == ""), the same fail-closed default processAttachments
// applies for an anonymous bearer-token upload.
//
// Unlike handleUpload, the staged file on the sandbox side is NOT deleted by
// this GET (mirroring the sandbox's own GET /files/{id} semantics — see that
// repo's CLAUDE.md), so a dropped connection or retry can re-fetch the same
// file_id until the sandbox's own TTL sweeper removes it.
func (s *Server) handleDownload(w http.ResponseWriter, r *http.Request) {
	sessionUser, ok := s.authorize(r)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if s.upload == nil {
		// Safety valve: the route should not be registered when upload/download
		// is nil, but guard here so a misconfigured mux never panics.
		http.Error(w, "file download is not configured", http.StatusNotImplemented)
		return
	}
	if s.orchestrator.attachStore == nil {
		http.Error(w, "attachment store is not configured", http.StatusInternalServerError)
		return
	}

	fileID := r.PathValue("file_id")
	if fileID == "" {
		http.Error(w, "missing file_id", http.StatusBadRequest)
		return
	}

	requestingUser := sessionUser
	if requestingUser == "" && s.users != nil {
		requestingUser = s.users.ResolveUserID(r.URL.Query().Get("source"), r.URL.Query().Get("user_id"))
	}

	rec, found := s.orchestrator.attachStore.Get(fileID)
	if !found || (rec.UserID != "" && rec.UserID != requestingUser) {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	downloadCtx, cancel := context.WithTimeout(r.Context(), sandboxDownloadTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(downloadCtx, http.MethodGet, s.upload.sandboxURL+"/"+url.PathEscape(fileID), nil)
	if err != nil {
		http.Error(w, "failed to build sandbox request: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if s.upload.sandboxToken != "" {
		req.Header.Set("Authorization", "Bearer "+s.upload.sandboxToken)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		s.logger.Error("download: sandbox request failed", "error", err, "file_id", fileID)
		http.Error(w, "failed to fetch file from sandbox: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		http.Error(w, fmt.Sprintf("sandbox returned HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(errBody))), resp.StatusCode)
		return
	}

	// Forward only the specific headers a file download needs — never copy
	// resp.Header wholesale, which would let the sandbox dictate arbitrary
	// response headers (e.g. Set-Cookie) to this handler's caller.
	if ct := resp.Header.Get("Content-Type"); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	if cd := resp.Header.Get("Content-Disposition"); cd != "" {
		w.Header().Set("Content-Disposition", cd)
	}
	if cl := resp.Header.Get("Content-Length"); cl != "" {
		w.Header().Set("Content-Length", cl)
	}
	if _, err := io.Copy(w, resp.Body); err != nil {
		s.logger.Warn("download: failed streaming file to client", "error", err, "file_id", fileID)
	}
}
