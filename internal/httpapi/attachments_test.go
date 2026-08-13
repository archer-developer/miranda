package httpapi

import (
	"encoding/json"
	"regexp"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/archer-developer/miranda-llm/llmtest"
	"github.com/archer-developer/miranda/internal/attachments"
)

func newAttachmentTestOrchestrator(t *testing.T, publicBaseURL string) (*Orchestrator, *attachments.Store) {
	t.Helper()
	provider := llmtest.New("local")
	o, _, _ := newTestOrchestrator(t, provider)
	store := attachments.NewStore(time.Hour)
	t.Cleanup(store.Close)
	o.SetAttachmentStore(store)
	o.SetFilesPublicBaseURL(publicBaseURL)
	return o, store
}

func TestProcessAttachments_BinaryFileGetsFileURINotSandboxInstructions(t *testing.T) {
	o, store := newAttachmentTestOrchestrator(t, "http://192.168.1.50:8787")
	store.Put(attachments.Record{UserID: "alex", FileID: "abc123", Filename: "scan.pdf", MIMEType: "application/pdf", Size: 10, Data: []byte("%PDF-bytes")})

	content, imageParts := o.processAttachments("alex", "отправь в медкарту", []Attachment{{FileID: "abc123", Filename: "scan.pdf"}})

	require.Nil(t, imageParts)
	require.Contains(t, content, "http://192.168.1.50:8787/files/abc123")
	require.NotContains(t, content, "sandbox", "must not mention the old sandbox-specific instruction")
	require.NotContains(t, content, "create_session", "must not mention the old sandbox tool-call sequence")
}

func TestProcessAttachments_ImageGetsBothInlineAndFileURI(t *testing.T) {
	o, store := newAttachmentTestOrchestrator(t, "http://192.168.1.50:8787")
	store.Put(attachments.Record{UserID: "alex", FileID: "img1", Filename: "photo.png", MIMEType: "image/png", Data: []byte("pngbytes")})

	content, imageParts := o.processAttachments("alex", "что на фото?", []Attachment{{FileID: "img1", Filename: "photo.png"}})

	require.Len(t, imageParts, 1, "still inlined for vision")
	require.Contains(t, content, "http://192.168.1.50:8787/files/img1", "also gets a fetchable URI for tools that need the file itself")
}

func TestProcessAttachments_TextFileGetsBothInlineAndFileURI(t *testing.T) {
	o, store := newAttachmentTestOrchestrator(t, "http://192.168.1.50:8787")
	store.Put(attachments.Record{UserID: "alex", FileID: "txt1", Filename: "notes.txt", MIMEType: "text/plain", Data: []byte("hello world")})

	content, _ := o.processAttachments("alex", "", []Attachment{{FileID: "txt1", Filename: "notes.txt"}})

	require.Contains(t, content, "hello world", "still inlined")
	require.Contains(t, content, "http://192.168.1.50:8787/files/txt1")
}

func TestProcessAttachments_NoFileURIWhenPublicBaseURLUnset(t *testing.T) {
	o, store := newAttachmentTestOrchestrator(t, "")
	store.Put(attachments.Record{UserID: "alex", FileID: "abc123", Filename: "scan.pdf", MIMEType: "application/pdf", Data: []byte("%PDF-bytes")})

	content, _ := o.processAttachments("alex", "отправь в медкарту", []Attachment{{FileID: "abc123", Filename: "scan.pdf"}})

	require.NotContains(t, content, "/files/abc123")
}

func TestFileURI_TrimsTrailingSlashOnBaseURL(t *testing.T) {
	o, _ := newAttachmentTestOrchestrator(t, "http://192.168.1.50:8787/")
	require.Equal(t, "http://192.168.1.50:8787/files/abc123", o.fileURI("abc123"))
}

var attachmentMarkerPattern = regexp.MustCompile(`\n\n<attachment>([\s\S]*?)</attachment>`)

// TestProcessAttachments_EmitsWellFormedAttachmentMarker guards the actual
// regression this replaced: the web UI (internal/webui/static/js/downloads.js's
// extractAttachmentBlocks) parses a structured <attachment>{json}</attachment>
// marker, not specific prose — this locks down that shape so a future wording
// change to the human-readable "note" can never again silently break chip
// rendering the way the old regex-matched-Russian-prose approach did.
func TestProcessAttachments_EmitsWellFormedAttachmentMarker(t *testing.T) {
	o, store := newAttachmentTestOrchestrator(t, "http://192.168.1.50:8787")
	store.Put(attachments.Record{UserID: "alex", FileID: "abc123", Filename: "scan.pdf", MIMEType: "application/pdf", Size: 411910, Data: []byte("%PDF-bytes")})

	content, _ := o.processAttachments("alex", "отправь в медкарту", []Attachment{{FileID: "abc123", Filename: "scan.pdf"}})

	matches := attachmentMarkerPattern.FindAllStringSubmatch(content, -1)
	require.Len(t, matches, 1, "exactly one <attachment> marker for one attachment")

	var marker struct {
		Filename  string `json:"filename"`
		MIMEType  string `json:"mime_type"`
		SizeBytes int64  `json:"size_bytes"`
		URI       string `json:"uri"`
		Note      string `json:"note"`
	}
	require.NoError(t, json.Unmarshal([]byte(matches[0][1]), &marker))
	require.Equal(t, "scan.pdf", marker.Filename)
	require.Equal(t, "application/pdf", marker.MIMEType)
	require.EqualValues(t, 411910, marker.SizeBytes)
	require.Equal(t, "http://192.168.1.50:8787/files/abc123", marker.URI)
	require.Contains(t, marker.Note, marker.URI, "note must have the %s placeholder actually filled in")
}
