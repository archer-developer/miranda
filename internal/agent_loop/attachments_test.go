package agentloop

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/png"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	llm "github.com/archer-developer/miranda-llm"
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

	content, imageParts, attachmentRefs := o.processAttachments("alex", "отправь в медкарту", []Attachment{{FileID: "abc123", Filename: "scan.pdf"}})

	require.Nil(t, imageParts)
	require.Contains(t, content, "http://192.168.1.50:8787/files/abc123")
	require.NotContains(t, content, "sandbox", "must not mention the old sandbox-specific instruction")
	require.NotContains(t, content, "create_session", "must not mention the old sandbox tool-call sequence")
	require.Len(t, attachmentRefs, 1)
	require.Equal(t, "scan.pdf", attachmentRefs[0].Filename)
	require.Empty(t, attachmentRefs[0].ThumbnailDataURL, "non-image attachments never get a thumbnail")
}

func TestProcessAttachments_ImageGetsBothInlineAndFileURI(t *testing.T) {
	o, store := newAttachmentTestOrchestrator(t, "http://192.168.1.50:8787")
	store.Put(attachments.Record{UserID: "alex", FileID: "img1", Filename: "photo.png", MIMEType: "image/png", Data: []byte("pngbytes")})

	content, imageParts, attachmentRefs := o.processAttachments("alex", "что на фото?", []Attachment{{FileID: "img1", Filename: "photo.png"}})

	require.Len(t, imageParts, 1, "still inlined for vision")
	require.Contains(t, content, "http://192.168.1.50:8787/files/img1", "also gets a fetchable URI for tools that need the file itself")
	require.Len(t, attachmentRefs, 1)
	require.Empty(t, attachmentRefs[0].ThumbnailDataURL, "undecodable test fixture bytes: thumbnail generation fails gracefully")
}

func TestProcessAttachments_ImageGetsDurableThumbnail(t *testing.T) {
	o, store := newAttachmentTestOrchestrator(t, "http://192.168.1.50:8787")
	store.Put(attachments.Record{UserID: "alex", FileID: "img1", Filename: "photo.png", MIMEType: "image/png", Data: fakePNG(t, 480, 320)})

	_, _, attachmentRefs := o.processAttachments("alex", "что на фото?", []Attachment{{FileID: "img1", Filename: "photo.png"}})

	require.Len(t, attachmentRefs, 1)
	require.True(t, strings.HasPrefix(attachmentRefs[0].ThumbnailDataURL, "data:image/jpeg;base64,"),
		"a decodable image gets a durable inline thumbnail regardless of the attachments store's own TTL")
}

// TestProcessAttachments_ImageIsResizedForModel guards modelImageMaxPx: a
// phone-camera-sized image must come out smaller before it's base64-encoded
// for the LLM, not sent at its original size (see attachments.go's doc
// comment on modelImageMaxPx/modelImageJPEGQuality for the token/traffic
// reasoning).
func TestProcessAttachments_ImageIsResizedForModel(t *testing.T) {
	o, store := newAttachmentTestOrchestrator(t, "http://192.168.1.50:8787")
	original := fakePNG(t, 4032, 3024)
	store.Put(attachments.Record{UserID: "alex", FileID: "img1", Filename: "photo.jpg", MIMEType: "image/jpeg", Data: original})

	_, imageParts, _ := o.processAttachments("alex", "что на фото?", []Attachment{{FileID: "img1", Filename: "photo.jpg"}})

	require.Len(t, imageParts, 1)
	require.Less(t, len(imageParts[0].ImageBase64), len(original), "resized image must be smaller than the original")
	require.Equal(t, "image/jpeg", imageParts[0].MIMEType, "resize re-encodes as JPEG regardless of source format")

	decoded, err := base64.StdEncoding.DecodeString(imageParts[0].ImageBase64)
	require.NoError(t, err)
	cfg, _, err := image.DecodeConfig(bytes.NewReader(decoded))
	require.NoError(t, err)
	require.LessOrEqual(t, cfg.Width, modelImageMaxPx)
	require.LessOrEqual(t, cfg.Height, modelImageMaxPx)
}

// TestOrchestrator_ImagePartsSurviveToolCallRoundTrip is the regression test
// for the production bug this fixed: a photo question that needs a tool
// call before the final answer (e.g. this household's medical_card lookup,
// always triggered for "what can I eat") used to get its image silently
// stripped from the message history after the *first* Chat call — every
// following call, including the one that composes the actual answer, saw
// only the "[Изображение: ...]" text placeholder and had to fabricate an
// answer with no pixels at all. Confirmed against the real Gemini API by
// replaying a captured production request with and without the image
// present: 4/4 hallucinated answers without it, 12/12 correct with it (see
// commit fixing agent_loop.go's runAgentLoop). This test asserts the image
// Part is still attached on the *second* Chat call — the one immediately
// after a tool call — not just the first.
func TestOrchestrator_ImagePartsSurviveToolCallRoundTrip(t *testing.T) {
	provider := llmtest.New("local",
		llmtest.Response{ToolCall: &llm.ToolCall{ID: "call-1", Name: "remember_this", Arguments: `{"fact":"ate an apple"}`}},
		llmtest.Response{Text: "На фото — яблоко."},
	)
	o, _, _ := newTestOrchestrator(t, provider)
	store := attachments.NewStore(time.Hour)
	t.Cleanup(store.Close)
	o.SetAttachmentStore(store)
	o.SetFilesPublicBaseURL("http://192.168.1.50:8787")
	store.Put(attachments.Record{UserID: "alex", FileID: "img1", Filename: "apple.jpg", MIMEType: "image/jpeg", Data: fakePNG(t, 480, 320)})

	resp, err := o.Handle(context.Background(), InputRequest{
		Source: "cli", UserID: "alex", Text: "что на фото?",
		Attachments: []Attachment{{FileID: "img1", Filename: "apple.jpg"}},
	})
	require.NoError(t, err)
	require.Equal(t, "На фото — яблоко.", resp.Reply)

	// Only Requests[1] (the call after the tool round-trip) is checked:
	// FakeProvider.Requests records the same slice header runAgentLoop
	// mutates in place, so an in-place `messages[j].Parts = nil` (the old
	// bug) retroactively blanks Requests[0]'s Parts too once inspected
	// here — Requests[1] is still the one that isolates "did the second
	// call actually carry the image."
	require.Len(t, provider.Requests, 2, "one call, one tool-call round-trip, one final answer")
	requireHasImagePart(t, provider.Requests[1].Messages, "call after the tool round-trip must STILL include the image")
}

func requireHasImagePart(t *testing.T, messages []llm.Message, msg string) {
	t.Helper()
	for _, m := range messages {
		for _, p := range m.Parts {
			if p.ImageBase64 != "" {
				return
			}
		}
	}
	t.Fatal(msg)
}

func TestProcessAttachments_TextFileGetsBothInlineAndFileURI(t *testing.T) {
	o, store := newAttachmentTestOrchestrator(t, "http://192.168.1.50:8787")
	store.Put(attachments.Record{UserID: "alex", FileID: "txt1", Filename: "notes.txt", MIMEType: "text/plain", Data: []byte("hello world")})

	content, _, _ := o.processAttachments("alex", "", []Attachment{{FileID: "txt1", Filename: "notes.txt"}})

	require.Contains(t, content, "hello world", "still inlined")
	require.Contains(t, content, "http://192.168.1.50:8787/files/txt1")
}

func TestProcessAttachments_NoFileURIWhenPublicBaseURLUnset(t *testing.T) {
	o, store := newAttachmentTestOrchestrator(t, "")
	store.Put(attachments.Record{UserID: "alex", FileID: "abc123", Filename: "scan.pdf", MIMEType: "application/pdf", Data: []byte("%PDF-bytes")})

	content, _, _ := o.processAttachments("alex", "отправь в медкарту", []Attachment{{FileID: "abc123", Filename: "scan.pdf"}})

	require.NotContains(t, content, "/files/abc123")
}

// fakePNG encodes a minimal solid-color PNG of the given dimensions — real
// enough for image.Decode/imageutil.ThumbnailJPEG to succeed on, unlike the
// other tests' placeholder "pngbytes" fixture.
func fakePNG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	var buf bytes.Buffer
	require.NoError(t, png.Encode(&buf, img))
	return buf.Bytes()
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

	content, _, _ := o.processAttachments("alex", "отправь в медкарту", []Attachment{{FileID: "abc123", Filename: "scan.pdf"}})

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
