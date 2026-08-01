package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"strings"
	"time"

	"github.com/archer-developer/miranda/internal/attachments"
)

// sandboxUploadTimeout caps how long handleUpload will wait for the sandbox
// to accept and stage the file. The sandbox may be under CPU/IO pressure from
// concurrent container execs; 60 seconds is generous but still bounded.
const sandboxUploadTimeout = 60 * time.Second

// UploadResponse is the JSON body POST /api/upload returns to the web UI on
// a successful upload. It mirrors the sandbox's POST /files response shape so
// the client can store the file_id for inclusion in a subsequent
// InputRequest.Attachments list.
type UploadResponse struct {
	FileID    string `json:"file_id"`
	Filename  string `json:"filename"`
	SizeBytes int64  `json:"size_bytes"`
	MIMEType  string `json:"mime_type"`
}

// handleUpload proxies a multipart file upload to the sandbox, caches the
// result in the orchestrator's attachment store, and returns the sandbox-
// assigned file_id to the caller.
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

	// Forward to the sandbox with a bounded timeout so a stalled sandbox
	// doesn't hold this goroutine indefinitely.
	uploadCtx, cancel := context.WithTimeout(r.Context(), sandboxUploadTimeout)
	defer cancel()

	sandboxResp, err := s.forwardToSandbox(uploadCtx, filename, partMIME, fileBytes)
	if err != nil {
		s.logger.Error("upload: sandbox forward failed", "error", err, "filename", filename)
		http.Error(w, "failed to upload file to sandbox: "+err.Error(), http.StatusBadGateway)
		return
	}

	// Cache the file data for the upcoming turn. Images and text files have
	// their bytes stored for inlining into the prompt; binary blobs (including
	// PDFs) are referenced only by file_id — the sandbox handles them.
	// UserID is bound to the record so processAttachments can reject a
	// different user's attempt to reference this file_id.
	rec := attachments.Record{
		UserID:   sessionUser,
		FileID:   sandboxResp.FileID,
		Filename: sandboxResp.Filename,
		MIMEType: sandboxResp.MIMEType,
		Size:     sandboxResp.SizeBytes,
	}
	if shouldBufferData(sandboxResp.MIMEType) {
		rec.Data = fileBytes
	}
	s.orchestrator.attachStore.Put(rec)

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(*sandboxResp)
}

// forwardToSandbox sends the file bytes to the sandbox's POST /files endpoint
// and returns the parsed response on success. ctx is used as the request
// context so the caller's sandboxUploadTimeout deadline is enforced.
//
// mimeType is forwarded as the file part's Content-Type rather than letting
// multipart.Writer default to application/octet-stream — the sandbox prefers
// the browser-detected type over content-sniffing for its MIME response field,
// which processAttachments later uses to decide whether to inline or sandbox-route.
func (s *Server) forwardToSandbox(ctx context.Context, filename, mimeType string, data []byte) (*UploadResponse, error) {
	var reqBody bytes.Buffer
	mw := multipart.NewWriter(&reqBody)

	// Use CreatePart (not CreateFormFile) so we can set a custom Content-Type.
	// CreateFormFile hardcodes application/octet-stream for every part,
	// discarding the browser-detected mimeType entirely.
	h := make(textproto.MIMEHeader)
	h.Set("Content-Disposition", fmt.Sprintf(`form-data; name="file"; filename=%q`, filename))
	h.Set("Content-Type", mimeType)
	fw, err := mw.CreatePart(h)
	if err != nil {
		return nil, fmt.Errorf("create form part: %w", err)
	}
	if _, err := fw.Write(data); err != nil {
		return nil, fmt.Errorf("write form part: %w", err)
	}
	if err := mw.Close(); err != nil {
		return nil, fmt.Errorf("close multipart writer: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.upload.sandboxURL, &reqBody)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	if s.upload.sandboxToken != "" {
		req.Header.Set("Authorization", "Bearer "+s.upload.sandboxToken)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("send request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	// Sandbox returns 201 Created on success (per NewFileUploadHandler).
	if resp.StatusCode != http.StatusCreated {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("sandbox returned HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(errBody)))
	}

	var result UploadResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode sandbox response: %w", err)
	}
	return &result, nil
}

// shouldBufferData reports whether the file's raw bytes should be kept in the
// in-memory attachments.Store for inline prompt injection (images for vision,
// text for context) or discarded after forwarding to the sandbox.
//
// Delegates to isTextMIME and mimeTypePrefix (defined in attachments.go, same
// package) so a new text-like MIME type added to isTextMIME is automatically
// buffered here without a separate edit — the two functions stay in sync.
func shouldBufferData(mimeType string) bool {
	return isTextMIME(mimeType) || mimeTypePrefix(mimeType) == "image"
}
