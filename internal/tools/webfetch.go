package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	llm "github.com/archer-developer/miranda-llm"
	"github.com/archer-developer/miranda/internal/tavily"
)

// WebFetchToolName mirrors WebSearchToolName's reasoning — named
// "tavily_web_fetch" so it can coexist with Anthropic's own native web_fetch
// on the same request (see WebSearchToolName's doc comment for the full
// rationale).
const WebFetchToolName = "tavily_web_fetch"

// maxFetchContentChars bounds how much of a fetched page's text is handed
// back to the model — a defensive cap independent of whatever Tavily's own
// extract_depth/chunking already trims, so one unusually large page can't
// blow up a turn's context.
const maxFetchContentChars = 8000

// WebFetchTool implements Tool by calling Tavily's /extract endpoint —
// reusing Tavily rather than Miranda doing its own HTTP GET + HTML-to-text
// extraction, so web_search and web_fetch share one dependency, one API
// key, and one failure mode (see internal/tavily.Client.Extract).
type WebFetchTool struct {
	client *tavily.Client
	logger *slog.Logger
	// def is computed once in NewWebFetch rather than rebuilt on every
	// Def() call — see WebSearchTool.def's doc comment for why.
	def llm.ToolDef
}

// NewWebFetch builds a WebFetchTool. logger is used at debug level only
// (see Call) — the URL requested and a summary of what came back — nil
// falls back to slog.Default(), same convention as NewWebSearch.
func NewWebFetch(client *tavily.Client, logger *slog.Logger) *WebFetchTool {
	if logger == nil {
		logger = slog.Default()
	}
	return &WebFetchTool{
		client: client,
		logger: logger,
		def: llm.ToolDef{
			Name: WebFetchToolName,
			Description: "Fetch the text content of a specific URL — a link the user gave you, or one from a " +
				"tavily_web_search result. Use tavily_web_search first if you don't already have a specific URL to fetch.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"url": map[string]any{
						"type":        "string",
						"description": "the URL to fetch",
					},
				},
				"required": []string{"url"},
			},
		},
	}
}

// Def implements Tool.
func (t *WebFetchTool) Def() llm.ToolDef { return t.def }

// Call implements Tool: it runs args.URL through Tavily's /extract endpoint
// and returns the page's text content, truncated to maxFetchContentChars.
func (t *WebFetchTool) Call(ctx context.Context, argumentsJSON string) (string, error) {
	var args struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal([]byte(argumentsJSON), &args); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}
	if args.URL == "" {
		return "", fmt.Errorf("url is required")
	}

	start := time.Now()
	t.logger.Debug("tavily_web_fetch: request", "url", args.URL)

	resp, err := t.client.Extract(ctx, args.URL)
	if err != nil {
		t.logger.Debug("tavily_web_fetch: failed", "url", args.URL, "duration_ms", time.Since(start).Milliseconds(), "error", err)
		return "", err
	}
	if len(resp.Results) == 0 {
		reason := "no content extracted"
		if len(resp.FailedResults) > 0 {
			reason = resp.FailedResults[0].Error
		}
		t.logger.Debug("tavily_web_fetch: no content", "url", args.URL, "duration_ms", time.Since(start).Milliseconds(), "reason", reason)
		return "", fmt.Errorf("could not fetch %s: %s", args.URL, reason)
	}

	content := resp.Results[0].RawContent
	runes := []rune(content)
	fetchedChars := len(runes)
	truncated := fetchedChars > maxFetchContentChars
	if truncated {
		content = string(runes[:maxFetchContentChars]) + "\n... (truncated)"
	}
	t.logger.Debug("tavily_web_fetch: response", "url", args.URL, "duration_ms", time.Since(start).Milliseconds(),
		"fetched_chars", fetchedChars, "truncated", truncated)
	return content, nil
}
