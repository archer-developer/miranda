package httpapi

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/archer-developer/miranda/internal/attachments"
	"github.com/archer-developer/miranda/internal/llm"
)

// textAttachmentThreshold is the maximum number of bytes of a text file that
// get inlined verbatim into the user message. Content beyond this is
// truncated with a "[...truncated...]" marker to avoid blowing up the prompt.
const textAttachmentThreshold = 10_000

// processAttachments builds the enriched user message content from the bare
// user text and the list of pre-uploaded file attachments (resolved via
// o.attachStore). It returns:
//
//   - userContent: the text to store in history and send as the LLM message's
//     Content field. For images this is the bare user text plus an
//     "[Изображение: ...]" placeholder so history has a hint; for text files
//     it includes the file content wrapped in <file:name>…</file>. Every
//     attachment (including images/text) also gets an
//     <attachment>{json}</attachment> marker appended — see
//     appendAttachmentMarker — carrying the fileURI any tool can fetch the
//     real bytes from, plus filename/size/mime for the web UI to render a
//     chip from, mirroring how appendDownloadMarkers
//     (internal/httpapi/agent_loop.go) already does this for the outbound
//     direction. Using a structured, boundary-delimited marker rather than
//     matching specific prose is deliberate: an earlier version relied on
//     the web UI regex-matching an exact Russian sentence, which silently
//     broke (attachment chip disappeared, raw instructional text leaked
//     into the chat bubble instead) the moment that sentence's wording
//     changed — see docs/file-staging-refactor.md and
//     internal/webui/static/js/screens/chat.js's extractFileAttachments.
//
//   - imageParts: base64 image blocks for vision-capable providers. These are
//     only used in the current turn's LLM message (llm.Message.Parts) and are
//     never stored in history — future replay turns won't re-send image bytes.
//
// userID is the identity of the requesting user; only attachments uploaded by
// that same session are accepted — a different user's file_id is treated as
// not found to prevent cross-user data leakage.
//
// If o.attachStore is nil (file_upload.enabled is false), Attachments in req
// are silently ignored and userContent == userText, imageParts == nil.
func (o *Orchestrator) processAttachments(userID, userText string, atts []Attachment) (userContent string, imageParts []llm.ContentPart) {
	if o.attachStore == nil || len(atts) == 0 {
		return userText, nil
	}

	var sb strings.Builder
	sb.WriteString(userText)

	for _, att := range atts {
		rec, ok := o.attachStore.Get(att.FileID)
		// Treat a mismatched owner the same as not-found: don't reveal
		// that the file_id exists but belongs to someone else.
		if !ok || (rec.UserID != "" && userID != "" && rec.UserID != userID) {
			// TTL expired, invalid file_id, or wrong owner — surface this to
			// the model as an inline notice rather than silently dropping the
			// attachment, so it can acknowledge the issue to the user.
			fmt.Fprintf(&sb, "\n\n[Файл %q не найден — возможно, истёк срок хранения или передан неверный ID]", att.Filename)
			continue
		}

		switch {
		case mimeTypePrefix(rec.MIMEType) == "image" && rec.Data != nil:
			// Vision: send the image as an inline block to the LLM and add a
			// short history placeholder so future replay turns know an image
			// was discussed, even though they won't see the pixels themselves.
			imageParts = append(imageParts, llm.ContentPart{
				ImageBase64: base64.StdEncoding.EncodeToString(rec.Data),
				MIMEType:    rec.MIMEType,
			})
			fmt.Fprintf(&sb, "\n\n[Изображение: %q (%s)]", rec.Filename, rec.MIMEType)
			appendAttachmentMarker(&sb, rec, o.fileURI(rec.FileID),
				"Файл также доступен по адресу %s, если какому-то инструменту нужен сам файл, а не только его содержимое здесь.")

		case isTextMIME(rec.MIMEType) && rec.Data != nil:
			// Inline text: embed the file content in the message so the model
			// can reason over it directly without calling any tool.
			content := string(rec.Data)
			if len(rec.Data) > textAttachmentThreshold {
				content = string(rec.Data[:textAttachmentThreshold]) + "\n[...truncated...]"
			}
			fmt.Fprintf(&sb, "\n\n<file:%s>\n%s\n</file>", rec.Filename, content)
			appendAttachmentMarker(&sb, rec, o.fileURI(rec.FileID),
				"Файл также доступен по адресу %s, если какому-то инструменту нужен сам файл, а не только его содержимое выше.")

		default:
			// Binary blob (PDF, archive, executable, …): there is nothing to
			// inline, only the marker below. The model must never try to
			// construct or embed the file's bytes itself in a tool call —
			// whichever tool it calls should fetch the file from uri on its
			// own (see docs/file-staging-refactor.md for why this replaced
			// an earlier design that routed everything through the
			// sandbox).
			appendAttachmentMarker(&sb, rec, o.fileURI(rec.FileID),
				"Если вызываемому инструменту нужен этот файл — передай ему именно %s (обычно это аргумент вроде fileUri в его схеме); никогда не пытайся передать содержимое файла напрямую в аргументах вызова.")
		}
	}

	return sb.String(), imageParts
}

// attachmentMarker is the JSON payload of an <attachment>...</attachment>
// marker appendAttachmentMarker writes — deliberately the same field
// shape/naming as appendDownloadMarkers' <download> marker
// (internal/httpapi/agent_loop.go), so both ends of the web UI only need
// one mental model for "a server-emitted file marker".
type attachmentMarker struct {
	Filename  string `json:"filename"`
	MIMEType  string `json:"mime_type"`
	SizeBytes int64  `json:"size_bytes"`
	URI       string `json:"uri,omitempty"`
	Note      string `json:"note,omitempty"`
}

// appendAttachmentMarker writes one <attachment>{json}</attachment> marker
// for rec to sb — the web UI's sole, wording-independent source of truth
// for rendering an attachment chip
// (internal/webui/static/js/screens/chat.js's extractFileAttachments looks
// only for this tag pair, never for specific prose), and, via note (a
// format string with one %s for the real address, so the instruction reads
// naturally), what tells the model what to actually do with the file.
// note is left "" (its %s never filled in) when uri itself is "" —
// file_upload.enabled but public_base_url unset, which only happens in
// tests; cmd/miranda refuses to start with that combination in production.
func appendAttachmentMarker(sb *strings.Builder, rec attachments.Record, uri, note string) {
	if uri != "" {
		note = fmt.Sprintf(note, uri)
	} else {
		note = ""
	}
	payload, err := json.Marshal(attachmentMarker{
		Filename:  rec.Filename,
		MIMEType:  rec.MIMEType,
		SizeBytes: rec.Size,
		URI:       uri,
		Note:      note,
	})
	if err != nil {
		return
	}
	fmt.Fprintf(sb, "\n\n<attachment>%s</attachment>", payload)
}

// fileURI returns the address any tool can fetch fileID's bytes from — see
// internal/httpapi's handleFilesServe (GET /files/{id}) and
// config.FileUploadConfig.PublicBaseURL. Empty only when no base URL is
// configured, which in practice means file_upload.enabled is false (the
// attachStore == nil check in processAttachments already short-circuits
// that case) — cmd/miranda refuses to start with file_upload enabled and
// no public_base_url set, so this is otherwise never empty outside tests
// that construct an Orchestrator directly without calling
// SetFilesPublicBaseURL.
func (o *Orchestrator) fileURI(fileID string) string {
	if o.filesPublicBaseURL == "" {
		return ""
	}
	return strings.TrimRight(o.filesPublicBaseURL, "/") + "/files/" + fileID
}

// mimeTypePrefix returns the primary type component of a MIME type — the
// part before "/" (e.g. "image" from "image/png", "text" from "text/plain").
func mimeTypePrefix(mimeType string) string {
	if idx := strings.Index(mimeType, "/"); idx >= 0 {
		return mimeType[:idx]
	}
	return mimeType
}

// isTextMIME reports whether mimeType should be inlined as plain text in the
// prompt rather than treated as an opaque binary blob for sandbox processing.
func isTextMIME(mimeType string) bool {
	if mimeTypePrefix(mimeType) == "text" {
		return true
	}
	switch mimeType {
	case "application/json", "application/xml", "application/javascript",
		"application/x-yaml", "application/yaml":
		return true
	}
	return false
}
