// Package tavily is a minimal client for the Tavily API
// (https://docs.tavily.com), used to give Miranda its own web_search and
// web_fetch tools (see internal/tools) instead of relying on a specific LLM
// provider's own native web tool (Claude's anthropic_tools, or Gemini's
// gemini_tools.google_search) — those run only on their own provider and,
// in Gemini's case, ran into a hard architectural wall: Grounding with
// Google Search is entirely unavailable on the free-tier Gemini Developer
// API, so any request that triggered it failed with RESOURCE_EXHAUSTED even
// on the very first call of the day (see internal/llm/gemini's rotation —
// it retried across every key and cooldown cycle for nothing, since the
// quota for that specific tool is zero, not merely low). A single
// self-hosted implementation, callable by every provider in the chain
// through the ordinary custom-tool path, sidesteps that per-provider
// quota entirely and works identically regardless of which model handles
// the turn.
//
// This package has no dependency on internal/httpapi or the LLM types —
// internal/tools is what adapts Search/Extract to the Tool interface the
// agent loop calls.
package tavily

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/archer-developer/miranda/internal/httpx"
)

// defaultAPIBase is the real Tavily API origin. Overridable per Client (see
// NewWithAPIBase) only so tests can point at an httptest.Server instead —
// mirrors internal/telegram.Client's apiBase field.
const defaultAPIBase = "https://api.tavily.com"

// Client is a small HTTP client for the two Tavily endpoints Miranda's
// tools need: search and extract (URL content fetch).
type Client struct {
	apiKey  string
	apiBase string
	http    *http.Client
}

// New builds a Client authenticating with apiKey (from
// https://app.tavily.com, read by the caller out of the environment
// variable named by config.TavilyConfig.APIKeyEnv — never stored in
// config.yaml directly, same convention as every other secret in this
// codebase).
func New(apiKey string) *Client {
	return NewWithAPIBase(apiKey, defaultAPIBase)
}

// NewWithAPIBase is New, but against apiBase instead of the real Tavily
// API — for tests.
func NewWithAPIBase(apiKey, apiBase string) *Client {
	return &Client{
		apiKey:  apiKey,
		apiBase: apiBase,
		http:    &http.Client{Timeout: 30 * time.Second},
	}
}

// SearchResult is one entry of SearchResponse.Results.
type SearchResult struct {
	Title   string  `json:"title"`
	URL     string  `json:"url"`
	Content string  `json:"content"`
	Score   float64 `json:"score"`
}

// SearchResponse is the subset of Tavily's POST /search response body this
// client cares about.
type SearchResponse struct {
	Results []SearchResult `json:"results"`
}

// Search runs a web search for query, asking Tavily for at most maxResults
// results (Tavily's own default is 5 if this is left at 0).
func (c *Client) Search(ctx context.Context, query string, maxResults int) (SearchResponse, error) {
	payload := map[string]any{"query": query}
	if maxResults > 0 {
		payload["max_results"] = maxResults
	}

	var resp SearchResponse
	if err := c.call(ctx, "/search", payload, &resp); err != nil {
		return SearchResponse{}, err
	}
	return resp, nil
}

// ExtractResult is one entry of ExtractResponse.Results.
type ExtractResult struct {
	URL        string `json:"url"`
	RawContent string `json:"raw_content"`
}

// ExtractFailure is one entry of ExtractResponse.FailedResults — a URL
// Tavily itself could not fetch (dead link, blocked by the target site,
// timed out, etc.), distinct from an HTTP/transport error talking to
// Tavily's own API.
type ExtractFailure struct {
	URL   string `json:"url"`
	Error string `json:"error"`
}

// ExtractResponse is the subset of Tavily's POST /extract response body
// this client cares about.
type ExtractResponse struct {
	Results       []ExtractResult  `json:"results"`
	FailedResults []ExtractFailure `json:"failed_results"`
}

// Extract fetches and returns the readable content of url via Tavily's own
// fetch/render pipeline, as plain text — this is what backs Miranda's
// web_fetch tool, and deliberately reuses Tavily rather than Miranda doing
// its own net/http GET + HTML-to-text extraction, so both tools share one
// dependency, one API key, and one failure mode to handle.
func (c *Client) Extract(ctx context.Context, url string) (ExtractResponse, error) {
	payload := map[string]any{"urls": []string{url}, "format": "text"}

	var resp ExtractResponse
	if err := c.call(ctx, "/extract", payload, &resp); err != nil {
		return ExtractResponse{}, err
	}
	return resp, nil
}

// maxErrorBodyRunes bounds how much of a non-JSON error body (e.g. an
// intermediate proxy/WAF's HTML error page, returned instead of Tavily's
// documented {"detail": ...} shape) gets embedded in the error call
// returns — that error string ends up persisted as a tool-result message in
// conversation history and re-sent to the LLM on every later turn (see
// internal/httpapi/agent_loop.go's executeTool), so it needs the same kind
// of defensive cap internal/tools/webfetch.go already applies to a
// successfully fetched page's content, not an unbounded copy of whatever
// came back.
const maxErrorBodyRunes = 2000

// call POSTs a JSON payload to a Tavily endpoint, authenticating via the
// Bearer Authorization header (Tavily's documented auth scheme, distinct
// from Telegram's token-in-URL-path convention), and decodes the response
// into out. The request/response transport plumbing (including the
// response size cap) is shared with internal/telegram's client via
// internal/httpx.PostJSON — only the response envelope (Tavily's
// {"detail": ...} on error) is specific to this client.
func (c *Client) call(ctx context.Context, path string, payload, out any) error {
	headers := map[string]string{"Authorization": "Bearer " + c.apiKey}
	status, respBody, err := httpx.PostJSON(ctx, c.http, c.apiBase+path, headers, payload, httpx.DefaultMaxResponseBytes)
	if err != nil {
		return fmt.Errorf("tavily: %s: %w", path, err)
	}

	if status < 200 || status >= 300 {
		var apiErr struct {
			Detail any `json:"detail"`
		}
		_ = json.Unmarshal(respBody, &apiErr)
		if apiErr.Detail != nil {
			return fmt.Errorf("tavily: %s failed (status %d): %v", path, status, apiErr.Detail)
		}
		return fmt.Errorf("tavily: %s failed (status %d): %s", path, status, truncateRunes(string(respBody), maxErrorBodyRunes))
	}

	if err := json.Unmarshal(respBody, out); err != nil {
		return fmt.Errorf("tavily: decode %s response: %w", path, err)
	}
	return nil
}

// truncateRunes cuts s to at most n runes (not bytes, so a multi-byte
// character never gets split mid-encoding), appending a marker when it
// does.
func truncateRunes(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n]) + "... (truncated)"
}
