package agentloop

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	llm "github.com/archer-developer/miranda-llm"
	"github.com/archer-developer/miranda-llm/llmtest"
)

// gatedTTS is a miranda-tts stand-in whose speak call blocks (like the real
// one, which returns only when playback ends) until the test releases it, so
// tests can observe what runs concurrently.
type gatedTTS struct {
	gate chan struct{}

	mu        sync.Mutex
	active    int
	maxActive int
	speaks    []string // "<entity>:<text>" in the order calls started
	stops     []string
}

func newGatedTTS() *gatedTTS { return &gatedTTS{gate: make(chan struct{}, 16)} }

func (g *gatedTTS) Name() string { return ttsServerName }

func (g *gatedTTS) ListTools(context.Context) ([]llm.ToolDef, error) {
	return []llm.ToolDef{{Name: "speak"}, {Name: "stop"}}, nil
}

func (g *gatedTTS) CallTool(ctx context.Context, name, argsJSON string) (string, error) {
	var a map[string]string
	_ = json.Unmarshal([]byte(argsJSON), &a)
	if name == "stop" {
		g.mu.Lock()
		g.stops = append(g.stops, a["entity_id"])
		g.mu.Unlock()
		return "stopped", nil
	}

	g.mu.Lock()
	g.active++
	g.maxActive = max(g.maxActive, g.active)
	g.speaks = append(g.speaks, a["entity_id"]+":"+a["text"])
	g.mu.Unlock()
	defer func() {
		g.mu.Lock()
		g.active--
		g.mu.Unlock()
	}()

	select {
	case <-g.gate:
	case <-ctx.Done():
	}
	return "played", nil
}

func (g *gatedTTS) Close() error { return nil }

func (g *gatedTTS) release() { g.gate <- struct{}{} }

func (g *gatedTTS) snapshot() (speaks, stops []string, maxActive int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.speaks...), append([]string(nil), g.stops...), g.maxActive
}

func newQueueTestOrchestrator(t *testing.T) (*Orchestrator, *gatedTTS) {
	t.Helper()
	o, _ := newTestOrchestratorWithTTS(t, llmtest.New("local"))
	g := newGatedTTS()
	o.tools.SetClient(g)
	return o, g
}

func eventuallySpeaks(t *testing.T, g *gatedTTS, want ...string) {
	t.Helper()
	require.Eventually(t, func() bool {
		speaks, _, _ := g.snapshot()
		return assert.ObjectsAreEqual(want, speaks)
	}, 2*time.Second, 5*time.Millisecond, "speak calls started")
}

func TestSpeakOnEntity_SameEntityPlaysInOrderNeverConcurrently(t *testing.T) {
	o, g := newQueueTestOrchestrator(t)
	ctx := context.Background()

	o.speakOnEntity(ctx, "media_player.kitchen", "раз")
	o.speakOnEntity(ctx, "media_player.kitchen", "два")
	o.speakOnEntity(ctx, "media_player.kitchen", "три")

	eventuallySpeaks(t, g, "media_player.kitchen:раз")
	time.Sleep(50 * time.Millisecond)
	speaks, _, _ := g.snapshot()
	require.Len(t, speaks, 1, "the second utterance waits for the first to finish")

	g.release()
	eventuallySpeaks(t, g, "media_player.kitchen:раз", "media_player.kitchen:два")
	g.release()
	eventuallySpeaks(t, g, "media_player.kitchen:раз", "media_player.kitchen:два", "media_player.kitchen:три")
	g.release()

	_, _, maxActive := g.snapshot()
	assert.Equal(t, 1, maxActive)
	require.Eventually(t, func() bool { return len(o.speakingEntities()) == 0 }, 2*time.Second, 5*time.Millisecond)
}

func TestSpeakOnEntity_DifferentEntitiesDoNotWaitForEachOther(t *testing.T) {
	o, g := newQueueTestOrchestrator(t)
	ctx := context.Background()

	o.speakOnEntity(ctx, "media_player.kitchen", "кухня")
	o.speakOnEntity(ctx, "assist_satellite.bedroom", "спальня")

	require.Eventually(t, func() bool {
		_, _, maxActive := g.snapshot()
		return maxActive == 2
	}, 2*time.Second, 5*time.Millisecond, "both speakers should be playing at once")
	g.release()
	g.release()
}

func TestStopSpeech_DropsQueuedUtterancesAndStopsTheSpeaker(t *testing.T) {
	o, g := newQueueTestOrchestrator(t)
	ctx := context.Background()
	o.speakOnEntity(ctx, "media_player.kitchen", "раз")
	o.speakOnEntity(ctx, "media_player.kitchen", "два")
	eventuallySpeaks(t, g, "media_player.kitchen:раз")

	assert.Equal(t, "stopped", o.stopSpeech(ctx))
	g.release() // the running speak returns, as it would once the real stop cut playback

	time.Sleep(100 * time.Millisecond)
	speaks, stops, _ := g.snapshot()
	assert.Equal(t, []string{"media_player.kitchen:раз"}, speaks, "the queued utterance must not start after stop")
	assert.Equal(t, []string{"media_player.kitchen"}, stops)
	require.Eventually(t, func() bool { return len(o.speakingEntities()) == 0 }, 2*time.Second, 5*time.Millisecond)
}

func TestStopSpeech_StillReachesAnEntityWhoseSpeakCallAlreadyReturned(t *testing.T) {
	o, g := newQueueTestOrchestrator(t)
	ctx := context.Background()
	o.speakOnEntity(ctx, "assist_satellite.bedroom", "привет")
	eventuallySpeaks(t, g, "assist_satellite.bedroom:привет")
	g.release() // the speak call returns (e.g. timed out) while playback may continue
	require.Eventually(t, func() bool { return len(o.speakingEntities()) == 0 }, 2*time.Second, 5*time.Millisecond)

	o.stopSpeech(ctx)

	_, stops, _ := g.snapshot()
	assert.ElementsMatch(t, []string{"assist_satellite.bedroom", "media_player.kitchen"}, stops,
		"the recently spoken-on entity and the default device both get a stop")
}
