package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/archer-developer/miranda/internal/config"
	"github.com/archer-developer/miranda/internal/llm"
	"github.com/archer-developer/miranda/internal/tavily"
)

// WebSearchToolName is the function name advertised to every LLM provider —
// deliberately the same name Claude/Gemini's own native tools use
// (anthropic_tools.web_search, gemini_tools.google_search's rough
// equivalent), since config/llm.yaml turns those off in favor of this one:
// a provider offering both its own native tool AND a custom function tool
// with the same name is a real conflict (Anthropic, for one, requires
// unique tool names on a single request), not just redundant.
const WebSearchToolName = "web_search"

// WebSearchTool implements Tool by calling Tavily's /search endpoint.
type WebSearchTool struct {
	client     *tavily.Client
	maxResults int
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
// re-check it.
func NewWebSearch(client *tavily.Client, cfg config.WebSearchToolConfig) *WebSearchTool {
	return &WebSearchTool{
		client:     client,
		maxResults: cfg.MaxResults,
		def: llm.ToolDef{
			Name: WebSearchToolName,
			Description: "Search the live web for information you don't already know or that changes over time " +
				"(current events, prices, schedules, facts beyond your training data). Returns a list of results, " +
				"each with a title, URL, and a short content snippet — use web_fetch on a result's URL if you need " +
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

	resp, err := t.client.Search(ctx, args.Query, t.maxResults)
	if err != nil {
		return "", err
	}
	if len(resp.Results) == 0 {
		return "no results found", nil
	}

	var b strings.Builder
	for i, r := range resp.Results {
		fmt.Fprintf(&b, "%d. %s\n%s\n%s\n\n", i+1, r.Title, r.URL, r.Content)
	}
	return strings.TrimRight(b.String(), "\n"), nil
}
