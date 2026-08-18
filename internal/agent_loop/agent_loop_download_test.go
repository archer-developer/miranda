package agentloop

import (
	"encoding/json"
	"testing"

	"github.com/archer-developer/miranda/internal/history"
	"github.com/stretchr/testify/require"
)

func TestToDownloadRefs_OmitsZeroSizeAndEmptyMIMEType(t *testing.T) {
	refs := toDownloadRefs([]downloadedFile{{fileID: "f1", filename: "unknown.bin"}})
	require.Equal(t, []history.DownloadRef{{FileID: "f1", Filename: "unknown.bin"}}, refs)

	b, err := json.Marshal(refs)
	require.NoError(t, err)
	// omitempty on SizeBytes/MIMEType matters now that a generically
	// detected file (see detectRemoteFileLinks) often has neither —
	// downloadChip (downloads.js) checks size != null to decide whether to
	// render a size suffix, which only works if a missing value is
	// actually absent from the JSON rather than present as a literal 0.
	require.NotContains(t, string(b), `"size_bytes"`)
	require.NotContains(t, string(b), `"mime_type"`)
}

func TestToDownloadRefs_NilForNoFiles(t *testing.T) {
	require.Nil(t, toDownloadRefs(nil))
}

func TestAppendDownloadFootnotes_WebUIUntouched(t *testing.T) {
	text := "Here you go."
	got := appendDownloadFootnotes(text, []downloadedFile{{fileID: "f1", filename: "a.txt", sizeBytes: 10}}, WebUISource)
	require.Equal(t, text, got, "the web UI renders chips from InputResponse.Downloads, not from Reply's text")
}

func TestAppendDownloadFootnotes_OtherChannelsGetPlainText(t *testing.T) {
	text := "Here you go."
	for _, source := range []string{"ha_assist", "telegram", "scheduled", ""} {
		got := appendDownloadFootnotes(text, []downloadedFile{{fileID: "f1", filename: "a.txt", sizeBytes: 2048}}, source)
		require.Contains(t, got, "a.txt", "source %q should still see the filename", source)
	}
}

// TestTurnControl_RecordDownloadedFile_LatestWinsOnSameFileDifferentURL
// guards a real, previously-confirmed bug (git history, commit 821a482): the
// sandbox mints a fresh file_id (and thus a fresh file_uri) on every
// download_file call even for the same underlying file, so URL-only dedup
// (hasRemoteFile) isn't enough on its own — a second call for a file with
// the same filename/size/mime as one already recorded this turn must
// replace it, not sit alongside it as a second chip. It must be the *later*
// file_id that survives: of two file_ids for nominally the same file, the
// sandbox's earlier one is the one that stops resolving by the time the
// chip is clicked, not the later one — keeping the first would leave a
// single, permanently-broken chip instead of the one that still works.
func TestTurnControl_RecordDownloadedFile_LatestWinsOnSameFileDifferentURL(t *testing.T) {
	c := &turnControl{}
	first := downloadedFile{fileID: "f1", filename: "autumn.docx", sizeBytes: 36864, mimeType: "application/vnd.openxmlformats-officedocument.wordprocessingml.document"}
	c.recordDownloadedFile(first)
	require.Equal(t, []downloadedFile{first}, c.downloadedFiles)

	second := downloadedFile{fileID: "f2", filename: "autumn.docx", sizeBytes: 36864, mimeType: "application/vnd.openxmlformats-officedocument.wordprocessingml.document"}
	c.recordDownloadedFile(second)
	require.Equal(t, []downloadedFile{second}, c.downloadedFiles, "the later file_id must replace the earlier one, not sit alongside it")

	different := downloadedFile{fileID: "f3", filename: "medical_profile.pdf", sizeBytes: 60335, mimeType: "application/pdf"}
	c.recordDownloadedFile(different)
	require.Equal(t, []downloadedFile{second, different}, c.downloadedFiles, "a genuinely different file must be kept as its own chip")
}
