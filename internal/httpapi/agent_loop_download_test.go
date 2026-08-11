package httpapi

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRenderDownloadMarkersForChannel_WebUIPassesThroughRaw(t *testing.T) {
	text := "Here you go." + appendDownloadMarkers("", []downloadedFile{{fileID: "f1", filename: "a.txt", sizeBytes: 10}})
	got := renderDownloadMarkersForChannel(text, webUISource)
	require.Equal(t, text, got, "web UI must still receive the raw marker so chat.js can render a chip")
}

func TestRenderDownloadMarkersForChannel_OtherChannelsGetPlainText(t *testing.T) {
	text := "Here you go." + appendDownloadMarkers("", []downloadedFile{{fileID: "f1", filename: "a.txt", sizeBytes: 2048}})

	for _, source := range []string{"ha_assist", "telegram", "scheduled", ""} {
		got := renderDownloadMarkersForChannel(text, source)
		require.NotContains(t, got, "<download>", "source %q must never see the raw marker tag", source)
		require.NotContains(t, got, "</download>", "source %q must never see the raw marker tag", source)
		require.Contains(t, got, "a.txt", "source %q should still see the filename", source)
	}
}

// TestAppendDownloadMarkers_OmitsZeroSizeAndEmptyMIMEType guards a real
// display bug: a generically detected file (see detectRemoteFileLinks) often
// has no known size/mime — without omitempty on the marker JSON, a literal
// "size_bytes":0 would make downloadChip's "size != null" check in
// downloads.js render a bogus "· 0 B" suffix instead of omitting it.
func TestAppendDownloadMarkers_OmitsZeroSizeAndEmptyMIMEType(t *testing.T) {
	text := appendDownloadMarkers("", []downloadedFile{{fileID: "f1", filename: "unknown.bin"}})
	require.NotContains(t, text, `"size_bytes"`)
	require.NotContains(t, text, `"mime_type"`)
	require.Contains(t, text, `"filename":"unknown.bin"`)
}
