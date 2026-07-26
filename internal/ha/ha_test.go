package ha

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCallService_SendsAuthorizedRequest(t *testing.T) {
	var gotAuth, gotPath string
	var gotBody map[string]any

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		require.NoError(t, json.NewDecoder(r.Body).Decode(&gotBody))
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	c := New(server.URL, "secret-token")
	err := c.CallService(context.Background(), "media_player", "play_media", map[string]any{
		"entity_id": "media_player.kitchen",
	})
	require.NoError(t, err)
	require.Equal(t, "Bearer secret-token", gotAuth)
	require.Equal(t, "/api/services/media_player/play_media", gotPath)
	require.Equal(t, "media_player.kitchen", gotBody["entity_id"])
}

func TestCallService_NonSuccessStatusReturnsError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"message":"invalid entity"}`))
	}))
	defer server.Close()

	c := New(server.URL, "token")
	err := c.CallService(context.Background(), "media_player", "play_media", nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "invalid entity")
}

func TestAliceState_ReturnsAliceStateAttribute(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/api/states/media_player.kitchen", r.URL.Path)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"state":      "paused", // the top-level media_player state -- must be ignored
			"attributes": map[string]any{"alice_state": "NONE"},
		})
	}))
	defer server.Close()

	c := New(server.URL, "token")
	state, err := c.AliceState(context.Background(), "media_player.kitchen")
	require.NoError(t, err)
	require.Equal(t, "NONE", state)
}
