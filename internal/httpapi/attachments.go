package httpapi

import (
	"encoding/base64"
	"fmt"
	"strings"

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
//     it includes the file content wrapped in <file:name>…</file>; for
//     binary/PDF files it includes a sandbox-access instruction.
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
			// Placeholder in the text so history captures that an image was
			// part of this turn, even though the pixel data isn't stored.
			fmt.Fprintf(&sb, "\n\n[Изображение: %q (%s)]", rec.Filename, rec.MIMEType)

		case isTextMIME(rec.MIMEType) && rec.Data != nil:
			// Inline text: embed the file content in the message so the model
			// can reason over it directly without calling any tool.
			content := string(rec.Data)
			if len(rec.Data) > textAttachmentThreshold {
				content = string(rec.Data[:textAttachmentThreshold]) + "\n[...truncated...]"
			}
			fmt.Fprintf(&sb, "\n\n<file:%s>\n%s\n</file>", rec.Filename, content)

		default:
			// Binary blob (PDF, archive, executable, …): the model must use
			// the sandbox MCP tools to process it. Provide the file_id and
			// clear step-by-step instructions so it knows exactly what to call.
			fmt.Fprintf(&sb,
				"\n\nФайл %q (%s, %d байт) загружен в sandbox и доступен для обработки. "+
					"Используй инструменты create_session → upload_file(session_id=..., file_id=%q) → execute_in_session, "+
					"чтобы работать с ним программно.",
				rec.Filename, rec.MIMEType, rec.Size, rec.FileID)
		}
	}

	return sb.String(), imageParts
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
