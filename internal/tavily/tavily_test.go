package tavily

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

func newTestClient(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return NewWithAPIBase("test-key", server.URL)
}

func TestSearch_PostsQueryAndAuthHeader(t *testing.T) {
	var gotPath string
	var gotAuth string
	var gotBody map[string]any

	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		require.NoError(t, json.NewDecoder(r.Body).Decode(&gotBody))
		_ = json.NewEncoder(w).Encode(map[string]any{
			"results": []map[string]any{
				{"title": "Example", "url": "https://example.com", "content": "snippet", "score": 0.9},
			},
		})
	})

	resp, err := c.Search(context.Background(), "who won the game", 3)
	require.NoError(t, err)
	require.Equal(t, "/search", gotPath)
	require.Equal(t, "Bearer test-key", gotAuth)
	require.Equal(t, "who won the game", gotBody["query"])
	require.Equal(t, float64(3), gotBody["max_results"])
	require.Len(t, resp.Results, 1)
	require.Equal(t, "Example", resp.Results[0].Title)
	require.Equal(t, "https://example.com", resp.Results[0].URL)
}

func TestSearch_OmitsMaxResultsWhenZero(t *testing.T) {
	var gotBody map[string]any
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, json.NewDecoder(r.Body).Decode(&gotBody))
		_ = json.NewEncoder(w).Encode(map[string]any{"results": []map[string]any{}})
	})

	_, err := c.Search(context.Background(), "query", 0)
	require.NoError(t, err)
	require.NotContains(t, gotBody, "max_results")
}

func TestSearch_NonOKStatusReturnsError(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_ = json.NewEncoder(w).Encode(map[string]any{"detail": "invalid API key"})
	})

	_, err := c.Search(context.Background(), "query", 0)
	require.Error(t, err)
	require.Contains(t, err.Error(), "invalid API key")
}

func TestExtract_PostsURLAndTextFormat(t *testing.T) {
	var gotPath string
	var gotBody map[string]any

	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		require.NoError(t, json.NewDecoder(r.Body).Decode(&gotBody))
		_ = json.NewEncoder(w).Encode(map[string]any{
			"results": []map[string]any{
				{"url": "https://example.com/page", "raw_content": "page text"},
			},
		})
	})

	resp, err := c.Extract(context.Background(), "https://example.com/page")
	require.NoError(t, err)
	require.Equal(t, "/extract", gotPath)
	require.Equal(t, []any{"https://example.com/page"}, gotBody["urls"])
	require.Equal(t, "text", gotBody["format"])
	require.Len(t, resp.Results, 1)
	require.Equal(t, "page text", resp.Results[0].RawContent)
}

func TestExtract_ReturnsFailedResults(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"results": []map[string]any{},
			"failed_results": []map[string]any{
				{"url": "https://example.com/dead", "error": "timeout"},
			},
		})
	})

	resp, err := c.Extract(context.Background(), "https://example.com/dead")
	require.NoError(t, err)
	require.Empty(t, resp.Results)
	require.Len(t, resp.FailedResults, 1)
	require.Equal(t, "timeout", resp.FailedResults[0].Error)
}
