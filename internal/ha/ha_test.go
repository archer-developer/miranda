package ha

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
)

const statesJSON = `[
 {"entity_id":"media_player.station","attributes":{"friendly_name":"Станция Мини 3 Про"}},
 {"entity_id":"assist_satellite.voice_pe","attributes":{"friendly_name":"Voice PE Спальня"}},
 {"entity_id":"media_player.shared","attributes":{"friendly_name":"Общая"}},
 {"entity_id":"assist_satellite.shared","attributes":{"friendly_name":"Общая"}},
 {"entity_id":"light.kitchen","attributes":{"friendly_name":"Кухня"}},
 {"entity_id":"media_player.nameless","attributes":{}}
]`

func newStatesServer(t *testing.T, calls *atomic.Int32) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		require.Equal(t, "Bearer secret-token", r.Header.Get("Authorization"))
		require.Equal(t, "/api/states", r.URL.Path)
		_, _ = w.Write([]byte(statesJSON))
	}))
	t.Cleanup(server.Close)
	return server
}

func TestResolveSpeaker_FindsMediaPlayersAndSatellites(t *testing.T) {
	var calls atomic.Int32
	c := New(newStatesServer(t, &calls).URL, "secret-token")

	got, err := c.ResolveSpeaker(context.Background(), "Станция Мини 3 Про")
	require.NoError(t, err)
	require.Equal(t, "media_player.station", got)

	got, err = c.ResolveSpeaker(context.Background(), "Voice PE Спальня")
	require.NoError(t, err)
	require.Equal(t, "assist_satellite.voice_pe", got)
}

func TestResolveSpeaker_MediaPlayerWinsOnNameClash(t *testing.T) {
	var calls atomic.Int32
	c := New(newStatesServer(t, &calls).URL, "secret-token")

	got, err := c.ResolveSpeaker(context.Background(), "Общая")
	require.NoError(t, err)
	require.Equal(t, "media_player.shared", got)
}

func TestResolveSpeaker_IgnoresOtherDomainsAndFetchesOnce(t *testing.T) {
	var calls atomic.Int32
	c := New(newStatesServer(t, &calls).URL, "secret-token")

	_, err := c.ResolveSpeaker(context.Background(), "Кухня")
	require.Error(t, err)
	_, _ = c.ResolveSpeaker(context.Background(), "Станция Мини 3 Про")

	require.Equal(t, int32(1), calls.Load(), "states are fetched once and cached")
}

func TestResolveSpeaker_HTTPErrorIsRetriedNextCall(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		_, _ = w.Write([]byte(statesJSON))
	}))
	defer server.Close()
	c := New(server.URL, "t")

	_, err := c.ResolveSpeaker(context.Background(), "Общая")
	require.Error(t, err)

	got, err := c.ResolveSpeaker(context.Background(), "Общая")
	require.NoError(t, err)
	require.Equal(t, "media_player.shared", got)
}
