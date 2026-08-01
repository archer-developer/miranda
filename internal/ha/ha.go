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
	"sync"
	"time"
)

// Client is a small HTTP client for Home Assistant's REST API.
type Client struct {
	baseURL string
	token   string
	http    *http.Client

	// playerCacheMu guards playerCache; held during the one-time load and on
	// every subsequent lookup — reads are infrequent enough that an RWMutex
	// would only add complexity without measurable gain.
	playerCacheMu sync.Mutex
	// playerCache maps media_player friendly names to entity_ids, populated
	// lazily on the first ResolveMediaPlayer call and never invalidated.
	playerCache map[string]string
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
	State      string `json:"state"`
	Attributes struct {
		AliceState string `json:"alice_state"`
	} `json:"attributes"`
}

// AliceState returns entityID's alice_state attribute (e.g. "IDLE", "NONE"
// while speaking — note the uppercase, unlike the entity's own top-level
// state) rather than the entity's top-level media_player state. The
// top-level state tracks whatever the underlying media session is doing
// (music playing/paused/idle) and is not a reliable signal for whether a
// TTS announcement is still being spoken — confirmed by direct observation:
// a Yandex Station left playing background music reports its top-level
// state as "paused" throughout a TTS announcement, never "idle", while
// alice_state cleanly transitions IDLE -> NONE -> IDLE around the
// announcement regardless of what the underlying media session is doing.
func (c *Client) AliceState(ctx context.Context, entityID string) (string, error) {
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
	return sr.Attributes.AliceState, nil
}

// ResolveMediaPlayer maps a media_player entity's friendly name (as returned
// by ha_GetLiveState and used in HA scripts, e.g. "Станция Мини 3 Про") to
// its entity_id (e.g. "media_player.yandex_station_m0pskn2001y7gw"). The
// mapping is fetched from HA on the first call and cached for the lifetime of
// the process — media_player entities are renamed rarely enough that a process
// restart is an acceptable way to pick up a name change.
func (c *Client) ResolveMediaPlayer(ctx context.Context, friendlyName string) (string, error) {
	c.playerCacheMu.Lock()
	defer c.playerCacheMu.Unlock()

	if c.playerCache == nil {
		if err := c.loadPlayerCache(ctx); err != nil {
			return "", err
		}
	}

	entityID, ok := c.playerCache[friendlyName]
	if !ok {
		return "", fmt.Errorf("ha: no media_player entity with friendly name %q", friendlyName)
	}
	return entityID, nil
}

// loadPlayerCache fetches all entity states from HA, filters to the
// media_player domain, and builds the friendly_name→entity_id map stored in
// c.playerCache. Must be called with c.playerCacheMu held; on error the cache
// stays nil so the next ResolveMediaPlayer call retries the fetch.
func (c *Client) loadPlayerCache(ctx context.Context) error {
	url := fmt.Sprintf("%s/api/states", c.baseURL)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("ha: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("ha: get states: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("ha: get states: status %d: %s", resp.StatusCode, string(body))
	}

	var states []struct {
		EntityID   string `json:"entity_id"`
		Attributes struct {
			FriendlyName string `json:"friendly_name"`
		} `json:"attributes"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&states); err != nil {
		return fmt.Errorf("ha: decode states: %w", err)
	}

	cache := make(map[string]string)
	for _, s := range states {
		if !strings.HasPrefix(s.EntityID, "media_player.") || s.Attributes.FriendlyName == "" {
			continue
		}
		cache[s.Attributes.FriendlyName] = s.EntityID
	}
	c.playerCache = cache
	return nil
}
