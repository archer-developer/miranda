package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	llm "github.com/archer-developer/miranda-llm"
	"github.com/archer-developer/miranda/internal/config"
	"github.com/archer-developer/miranda/internal/tavily"
)

// WebSearchToolName is the function name advertised to every LLM provider.
// Named "tavily_web_search" (not "web_search") so it can coexist with
// Anthropic's own native web_search tool on the same request — Anthropic
// requires unique tool names, and the Claude provider may have
// anthropic_tools.web_search enabled at the same time (see
// config.AnthropicToolsConfig). Claude sees both and prefers its own
// server-side native tool; Gemini and other providers only see this one.
const WebSearchToolName = "tavily_web_search"

// WebSearchTool implements Tool by calling Tavily's /search endpoint.
type WebSearchTool struct {
	client     *tavily.Client
	maxResults int
	logger     *slog.Logger
	// def is computed once in NewWebSearch rather than rebuilt on every
	// Def() call — its content is fixed for the process's lifetime (it
	// doesn't depend on anything that changes at runtime), and Def() is
	// called on every single turn (availableTools) plus every tool
	// dispatch (executeTool), so building the nested Parameters map fresh
	// each time would be pure repeated allocation for an identical result.
	def llm.ToolDef
}

// NewWebSearch builds a WebSearchTool. cfg.Enabled is the caller's concern
// (cmd/miranda only constructs this when true) — this constructor doesn't
// re-check it. logger is used at debug level only (see Call) — the query
// sent to Tavily and a summary of what came back, to debug why a turn did
// or didn't find what the model expected without needing to reproduce it
// against the real API; nil falls back to slog.Default(), same convention
// as miranda-llm/gemini.New/internal/mcp.NewManager.
func NewWebSearch(client *tavily.Client, cfg config.WebSearchToolConfig, logger *slog.Logger) *WebSearchTool {
	if logger == nil {
		logger = slog.Default()
	}
	return &WebSearchTool{
		client:     client,
		maxResults: cfg.MaxResults,
		logger:     logger,
		def: llm.ToolDef{
			Name: WebSearchToolName,
			Description: "Search the live web for information you don't already know or that changes over time " +
				"(current events, prices, schedules, facts beyond your training data). Returns a list of results, " +
				"each with a title, URL, and a short content snippet — use tavily_web_fetch on a result's URL if you need " +
				"the full page rather than just the snippet.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"query": map[string]any{
						"type":        "string",
						"description": "the search query, in the same language as the user's question",
					},
				},
				"required": []string{"query"},
			},
		},
	}
}

// Def implements Tool.
func (t *WebSearchTool) Def() llm.ToolDef { return t.def }

// Call implements Tool: it runs args.Query against Tavily's /search
// endpoint and formats each result as a numbered title/URL/snippet block.
func (t *WebSearchTool) Call(ctx context.Context, argumentsJSON string) (string, error) {
	var args struct {
		Query string `json:"query"`
	}
	if err := json.Unmarshal([]byte(argumentsJSON), &args); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}
	if args.Query == "" {
		return "", fmt.Errorf("query is required")
	}

	start := time.Now()
	t.logger.Debug("tavily_web_search: request", "query", args.Query, "max_results", t.maxResults)

	resp, err := t.client.Search(ctx, args.Query, t.maxResults)
	if err != nil {
		t.logger.Debug("tavily_web_search: failed", "query", args.Query, "duration_ms", time.Since(start).Milliseconds(), "error", err)
		return "", err
	}
	if len(resp.Results) == 0 {
		t.logger.Debug("tavily_web_search: no results", "query", args.Query, "duration_ms", time.Since(start).Milliseconds())
		return "no results found", nil
	}

	urls := make([]string, len(resp.Results))
	var b strings.Builder
	for i, r := range resp.Results {
		urls[i] = r.URL
		fmt.Fprintf(&b, "%d. %s\n%s\n%s\n\n", i+1, r.Title, r.URL, r.Content)
	}
	result := strings.TrimRight(b.String(), "\n")
	t.logger.Debug("tavily_web_search: response", "query", args.Query, "duration_ms", time.Since(start).Milliseconds(),
		"result_count", len(resp.Results), "urls", urls, "result_chars", len(result))
	return result, nil
}
