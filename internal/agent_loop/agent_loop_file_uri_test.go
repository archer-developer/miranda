package agentloop

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	llm "github.com/archer-developer/miranda-llm"
	"github.com/archer-developer/miranda-llm/llmtest"
	"github.com/archer-developer/miranda/internal/attachments"
	"github.com/archer-developer/miranda/internal/config"
	"github.com/archer-developer/miranda/internal/mcp/mcptest"
)

func TestDetectRemoteFileLinks_FindsURLAndSiblingTitle(t *testing.T) {
	endpoint := config.FileServerEndpoint{FilesURL: "https://127.0.0.1:8791/files"}
	result := `{"documentId":"doc_1","title":"МРТ пояснично-крестцового отдела","fileUri":"https://127.0.0.1:8791/files/file_abc123"}`

	links := detectRemoteFileLinks(result, endpoint)
	require.Len(t, links, 1)
	require.Equal(t, "https://127.0.0.1:8791/files/file_abc123", links[0].url)
	require.Equal(t, "МРТ пояснично-крестцового отдела", links[0].filename)
}

func TestDetectRemoteFileLinks_DedupesRepeatedURL(t *testing.T) {
	endpoint := config.FileServerEndpoint{FilesURL: "https://127.0.0.1:8791/files"}
	result := `["https://127.0.0.1:8791/files/file_abc123","https://127.0.0.1:8791/files/file_abc123"]`

	links := detectRemoteFileLinks(result, endpoint)
	require.Len(t, links, 1)
}

func TestDetectRemoteFileLinks_IgnoresURLFromUnrelatedHost(t *testing.T) {
	endpoint := config.FileServerEndpoint{FilesURL: "https://127.0.0.1:8791/files"}
	result := `{"fileUri":"https://evil.example.com/files/file_abc123"}`

	links := detectRemoteFileLinks(result, endpoint)
	require.Empty(t, links, "a URL not rooted at this server's own FilesEndpoint must never be treated as a file reference")
}

func TestDetectRemoteFileLinks_NoFilenameWhenResultIsNotJSON(t *testing.T) {
	endpoint := config.FileServerEndpoint{FilesURL: "https://127.0.0.1:8791/files"}
	result := "see https://127.0.0.1:8791/files/file_abc123 for the scan"

	links := detectRemoteFileLinks(result, endpoint)
	require.Len(t, links, 1)
	require.Equal(t, "", links[0].filename)
}

// TestDetectRemoteFileLinks_PlainTextKeyValueFallback covers the
// non-JSON shape miranda-code-execution-sandbox's download_file actually
// returns ("file_id: ...\nfile_uri: ...\nfilename: ...\nsize_bytes:
// ...\nmime_type: ...") — filename/mime_type/size_bytes must still be
// recovered via keyValueStringField/keyValueInt64Field, the same way the
// JSON sibling-object path recovers them for miranda-medical-card.
func TestDetectRemoteFileLinks_PlainTextKeyValueFallback(t *testing.T) {
	endpoint := config.FileServerEndpoint{FilesURL: "http://192.168.1.50:8788/files"}
	result := "file_id: file_abc123\n" +
		"file_uri: http://192.168.1.50:8788/files/file_abc123\n" +
		"filename: output.mp4\n" +
		"size_bytes: 4096\n" +
		"mime_type: video/mp4\n" +
		"To retrieve the file content, make an authenticated HTTP GET request " +
		"to /files/file_abc123 (Authorization: Bearer <token>) on this MCP server's host."

	links := detectRemoteFileLinks(result, endpoint)
	require.Len(t, links, 1)
	require.Equal(t, "http://192.168.1.50:8788/files/file_abc123", links[0].url)
	require.Equal(t, "output.mp4", links[0].filename)
	require.Equal(t, "video/mp4", links[0].mimeType)
	require.Equal(t, int64(4096), links[0].sizeBytes)
}

// TestExecuteTool_GenericFileURIDetection_ProxiesMedicalCardDocument exercises
// the full path this feature exists for: a get_document-shaped tool (no
// dedicated download tool, unlike the sandbox) returns a fileUri pointing at
// its own MCP server's internal address — executeTool must intercept it,
// stage a Miranda-hosted download record carrying that server's own bearer
// token, and surface it via InputResponse.Downloads (structured, never
// folded into Reply's text — see history.Message.Downloads) so the web UI
// renders a working chip instead of the model relaying a dead,
// unauthenticated internal link.
func TestExecuteTool_GenericFileURIDetection_ProxiesMedicalCardDocument(t *testing.T) {
	medical := mcptest.New("medical_card", llm.ToolDef{Name: "get_document"}).
		WithResult("get_document", `{"documentId":"doc_1","title":"МРТ от 19.03.2025","fileUri":"https://127.0.0.1:8791/files/file_abc123"}`)
	provider := llmtest.New("local",
		llmtest.Response{ToolCall: &llm.ToolCall{ID: "call-1", Name: "medical_card_get_document", Arguments: `{"userId":"archer","documentId":"doc_1"}`}},
		llmtest.Response{Text: "Вот файл исследования."},
	)
	o, _, _ := newTestOrchestrator(t, provider, medical)

	store := attachments.NewStore(0)
	t.Cleanup(store.Close)
	o.SetAttachmentStore(store)
	medicalEndpoint := config.FileServerEndpoint{FilesURL: "https://127.0.0.1:8791/files", Token: "medical-card-token"}
	o.SetMCPServerExtensions(map[string]MCPServerExtension{
		"medical_card": {FilesEndpoint: &medicalEndpoint},
	}, time.Hour)

	resp, err := o.Handle(context.Background(), InputRequest{Source: WebUISource, UserID: "archer", Text: "покажи файл МРТ"})
	require.NoError(t, err)
	require.NotContains(t, resp.Reply, "<download>", "downloads are never folded into the reply text — see InputResponse.Downloads")
	require.NotContains(t, resp.Reply, "127.0.0.1:8791", "the internal MCP server address must never reach the reply text")

	// Exactly one record should have been staged, carrying the medical_card
	// server's own token — not the raw internal URL exposed to the model,
	// which never gets to see this token at all.
	require.Len(t, resp.Downloads, 1)
	fileID := resp.Downloads[0].FileID
	require.NotEmpty(t, fileID)

	rec, found := store.Get(fileID)
	require.True(t, found)
	require.Equal(t, "archer", rec.UserID)
	require.Equal(t, "https://127.0.0.1:8791/files/file_abc123", rec.RemoteURL)
	require.Equal(t, "medical-card-token", rec.RemoteToken)
	require.Equal(t, "МРТ от 19.03.2025", rec.Filename)
}

// TestExecuteTool_GenericFileURIDetection_SkipsNonOptedInServer guards the
// security boundary: a server absent from SetFileExposingServers must never
// have its tool results scanned, even if a result happens to contain a URL
// shaped just like a real files endpoint.
func TestExecuteTool_GenericFileURIDetection_SkipsNonOptedInServer(t *testing.T) {
	other := mcptest.New("other", llm.ToolDef{Name: "get_document"}).
		WithResult("get_document", `{"fileUri":"https://127.0.0.1:9000/files/file_xyz"}`)
	provider := llmtest.New("local",
		llmtest.Response{ToolCall: &llm.ToolCall{ID: "call-1", Name: "other_get_document", Arguments: `{}`}},
		llmtest.Response{Text: "Done."},
	)
	o, _, _ := newTestOrchestrator(t, provider, other)

	store := attachments.NewStore(0)
	t.Cleanup(store.Close)
	o.SetAttachmentStore(store)
	// "other" is deliberately absent from this map.
	medicalEndpoint := config.FileServerEndpoint{FilesURL: "https://127.0.0.1:8791/files", Token: "medical-card-token"}
	o.SetMCPServerExtensions(map[string]MCPServerExtension{
		"medical_card": {FilesEndpoint: &medicalEndpoint},
	}, time.Hour)

	resp, err := o.Handle(context.Background(), InputRequest{Source: WebUISource, UserID: "archer", Text: "get the document"})
	require.NoError(t, err)
	require.Empty(t, resp.Downloads, "a non-opted-in server's result must never be scanned for a file URI")
}
