package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/archer-developer/miranda/internal/tavily"
)

func TestWebFetchTool_Def(t *testing.T) {
	tool := NewWebFetch(tavily.New(""), nil)
	def := tool.Def()
	require.Equal(t, WebFetchToolName, def.Name)
	require.NotEmpty(t, def.Description)
}

func TestWebFetchTool_Call_ReturnsContent(t *testing.T) {
	client := newTestTavilyClient(t, func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		require.Equal(t, []any{"https://example.com/page"}, body["urls"])
		_ = json.NewEncoder(w).Encode(map[string]any{
			"results": []map[string]any{
				{"url": "https://example.com/page", "raw_content": "the page's text"},
			},
		})
	})

	tool := NewWebFetch(client, nil)
	result, err := tool.Call(context.Background(), `{"url":"https://example.com/page"}`)
	require.NoError(t, err)
	require.Equal(t, "the page's text", result)
}

func TestWebFetchTool_Call_TruncatesLongContent(t *testing.T) {
	long := strings.Repeat("а", maxFetchContentChars+500) // Cyrillic to exercise rune-safe truncation
	client := newTestTavilyClient(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"results": []map[string]any{{"url": "https://example.com/long", "raw_content": long}},
		})
	})

	tool := NewWebFetch(client, nil)
	result, err := tool.Call(context.Background(), `{"url":"https://example.com/long"}`)
	require.NoError(t, err)
	require.True(t, strings.HasSuffix(result, "... (truncated)"))
	require.LessOrEqual(t, len([]rune(result)), maxFetchContentChars+len("\n... (truncated)"))
}

func TestWebFetchTool_Call_FailedExtractReturnsError(t *testing.T) {
	client := newTestTavilyClient(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"results":        []map[string]any{},
			"failed_results": []map[string]any{{"url": "https://example.com/dead", "error": "timeout"}},
		})
	})

	tool := NewWebFetch(client, nil)
	_, err := tool.Call(context.Background(), `{"url":"https://example.com/dead"}`)
	require.Error(t, err)
	require.Contains(t, err.Error(), "timeout")
}

func TestWebFetchTool_Call_MissingURL(t *testing.T) {
	tool := NewWebFetch(tavily.New(""), nil)
	_, err := tool.Call(context.Background(), `{}`)
	require.Error(t, err)
}

func TestWebFetchTool_Call_LogsURLAndTimingAtDebugLevel(t *testing.T) {
	client := newTestTavilyClient(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"results": []map[string]any{{"url": "https://example.com/page", "raw_content": "the page's text"}},
		})
	})

	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	tool := NewWebFetch(client, logger)

	_, err := tool.Call(context.Background(), `{"url":"https://example.com/page"}`)
	require.NoError(t, err)

	logged := logBuf.String()
	require.Contains(t, logged, "web_fetch: request")
	require.Contains(t, logged, "https://example.com/page")
	require.Contains(t, logged, "web_fetch: response")
	require.Contains(t, logged, "duration_ms")
	require.Contains(t, logged, "fetched_chars")
}
