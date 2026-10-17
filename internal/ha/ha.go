// Package ha is a minimal Home Assistant REST API client with one job: map a
// speaker's friendly name (as shown in HA's UI and used by the speak_reply
// tool's "device" argument) to the entity_id the miranda-tts service needs.
// Voicing itself is not done here — miranda-tts calls Home Assistant on its
// own; Miranda only resolves the target. The agent's tool access to HA goes
// through the HA MCP server (internal/mcp).
package ha

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// speakerDomains are the entity domains miranda-tts can speak on:
// media_player (e.g. a Yandex Station) and assist_satellite (e.g. a Home
// Assistant Voice PE). A friendly name found in both resolves to the
// media_player, since that was the only kind supported before satellites.
var speakerDomains = []string{"media_player.", "assist_satellite."}

// Client is a small HTTP client for Home Assistant's REST API.
type Client struct {
	baseURL string
	token   string
	http    *http.Client

	// speakerCacheMu guards speakerCache; held during the one-time load and
	// on every subsequent lookup — reads are infrequent enough that an
	// RWMutex would only add complexity without measurable gain.
	speakerCacheMu sync.Mutex
	// speakerCache maps speaker friendly names to entity_ids, populated
	// lazily on the first ResolveSpeaker call and never invalidated.
	speakerCache map[string]string
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

// ResolveSpeaker maps a speaker's friendly name (as returned by
// ha_GetLiveState and used in HA scripts, e.g. "Станция Мини 3 Про") to its
// entity_id (e.g. "media_player.yandex_station_m0pskn2001y7gw" or
// "assist_satellite.home_assistant_voice_09fdc5_assist_satellite"). The
// mapping is fetched from HA on the first call and cached for the lifetime
// of the process — speakers are renamed rarely enough that a process restart
// is an acceptable way to pick up a name change.
func (c *Client) ResolveSpeaker(ctx context.Context, friendlyName string) (string, error) {
	c.speakerCacheMu.Lock()
	defer c.speakerCacheMu.Unlock()

	if c.speakerCache == nil {
		if err := c.loadSpeakerCache(ctx); err != nil {
			return "", err
		}
	}

	entityID, ok := c.speakerCache[friendlyName]
	if !ok {
		return "", fmt.Errorf("ha: no media_player or assist_satellite entity with friendly name %q", friendlyName)
	}
	return entityID, nil
}

// loadSpeakerCache fetches all entity states from HA, filters to the speaker
// domains, and builds the friendly_name→entity_id map stored in
// c.speakerCache. Must be called with c.speakerCacheMu held; on error the
// cache stays nil so the next ResolveSpeaker call retries the fetch.
func (c *Client) loadSpeakerCache(ctx context.Context) error {
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
	defer func() { _ = resp.Body.Close() }()

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

	// Walk domains in priority order so a name present in several domains
	// resolves deterministically (media_player first).
	cache := make(map[string]string)
	for _, domain := range speakerDomains {
		for _, s := range states {
			name := s.Attributes.FriendlyName
			if !strings.HasPrefix(s.EntityID, domain) || name == "" {
				continue
			}
			if _, taken := cache[name]; !taken {
				cache[name] = s.EntityID
			}
		}
	}
	c.speakerCache = cache
	return nil
}
