package tools

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/archer-developer/miranda/internal/config"
	"github.com/archer-developer/miranda/internal/tavily"
)

func newTestTavilyClient(t *testing.T, handler http.HandlerFunc) *tavily.Client {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return tavily.NewWithAPIBase("test-key", server.URL)
}

func TestWebSearchTool_Def(t *testing.T) {
	tool := NewWebSearch(tavily.New(""), config.WebSearchToolConfig{MaxResults: 5})
	def := tool.Def()
	require.Equal(t, WebSearchToolName, def.Name)
	require.NotEmpty(t, def.Description)
}

func TestWebSearchTool_Call_FormatsResults(t *testing.T) {
	client := newTestTavilyClient(t, func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		require.Equal(t, "current bitcoin price", body["query"])
		require.Equal(t, float64(3), body["max_results"])
		_ = json.NewEncoder(w).Encode(map[string]any{
			"results": []map[string]any{
				{"title": "Bitcoin Price", "url": "https://example.com/btc", "content": "$100,000", "score": 0.95},
			},
		})
	})

	tool := NewWebSearch(client, config.WebSearchToolConfig{MaxResults: 3})
	result, err := tool.Call(context.Background(), `{"query":"current bitcoin price"}`)
	require.NoError(t, err)
	require.Contains(t, result, "Bitcoin Price")
	require.Contains(t, result, "https://example.com/btc")
	require.Contains(t, result, "$100,000")
}

func TestWebSearchTool_Call_NoResults(t *testing.T) {
	client := newTestTavilyClient(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"results": []map[string]any{}})
	})

	tool := NewWebSearch(client, config.WebSearchToolConfig{})
	result, err := tool.Call(context.Background(), `{"query":"something obscure"}`)
	require.NoError(t, err)
	require.Equal(t, "no results found", result)
}

func TestWebSearchTool_Call_MissingQuery(t *testing.T) {
	tool := NewWebSearch(tavily.New(""), config.WebSearchToolConfig{})
	_, err := tool.Call(context.Background(), `{}`)
	require.Error(t, err)
}

func TestWebSearchTool_Call_InvalidArguments(t *testing.T) {
	tool := NewWebSearch(tavily.New(""), config.WebSearchToolConfig{})
	_, err := tool.Call(context.Background(), `not json`)
	require.Error(t, err)
}
