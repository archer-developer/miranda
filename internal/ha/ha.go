// Package ha is a minimal Home Assistant REST API client: calling services
// (media_player.play_media, tts.speak, ...) and reading entity state. It's
// intentionally small — the agent's tool access to HA goes through the HA
// MCP server (internal/mcp); this client exists only for the TTS dispatch
// path, which needs direct service calls and state polling that don't fit
// the MCP tool-calling shape well.
package ha

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Client is a small HTTP client for Home Assistant's REST API.
type Client struct {
	baseURL string
	token   string
	http    *http.Client
}

// New builds a Client against baseURL (e.g. "http://homeassistant.local:8123")
// authenticating with a long-lived access token.
func New(baseURL, token string) *Client {
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		token:   token,
		http:    &http.Client{Timeout: 15 * time.Second},
	}
}

// CallService invokes domain.service (e.g. "media_player"/"play_media")
// with data as the JSON service call body.
func (c *Client) CallService(ctx context.Context, domain, service string, data map[string]any) error {
	body, err := json.Marshal(data)
	if err != nil {
		return fmt.Errorf("ha: marshal service call body: %w", err)
	}

	url := fmt.Sprintf("%s/api/services/%s/%s", c.baseURL, domain, service)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("ha: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("ha: call %s.%s: %w", domain, service, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("ha: call %s.%s: status %d: %s", domain, service, resp.StatusCode, string(respBody))
	}
	return nil
}

type stateResponse struct {
	State string `json:"state"`
}

// State returns entityID's current state string (e.g. "idle", "playing").
func (c *Client) State(ctx context.Context, entityID string) (string, error) {
	url := fmt.Sprintf("%s/api/states/%s", c.baseURL, entityID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", fmt.Errorf("ha: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)

	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("ha: get state of %s: %w", entityID, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("ha: get state of %s: status %d: %s", entityID, resp.StatusCode, string(respBody))
	}

	var sr stateResponse
	if err := json.NewDecoder(resp.Body).Decode(&sr); err != nil {
		return "", fmt.Errorf("ha: decode state response for %s: %w", entityID, err)
	}
	return sr.State, nil
}
