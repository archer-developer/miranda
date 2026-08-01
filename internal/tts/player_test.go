package tts

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// stopTrackingHA is a minimal HAClient that only records
// media_player.media_stop calls, for Player.Stop tests.
type stopTrackingHA struct {
	mu        sync.Mutex
	stopCalls []string // entity ids media_stop was called for
	playErr   error    // if set, CallService for play_media returns this
}

func (f *stopTrackingHA) CallService(ctx context.Context, domain, service string, data map[string]any) error {
	if domain == "media_player" && service == "media_stop" {
		f.mu.Lock()
		f.stopCalls = append(f.stopCalls, data["entity_id"].(string))
		f.mu.Unlock()
		return nil
	}
	if service == "play_media" && f.playErr != nil {
		return f.playErr
	}
	return nil
}

func (f *stopTrackingHA) AliceState(ctx context.Context, entityID string) (string, error) {
	return "IDLE", nil
}

func (f *stopTrackingHA) ResolveMediaPlayer(ctx context.Context, friendlyName string) (string, error) {
	return "media_player." + friendlyName, nil
}

func (f *stopTrackingHA) StopCalls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.stopCalls))
	copy(out, f.stopCalls)
	return out
}

// TestPlayer_EnqueueProcessesInOrderOneAtATime guards the property that
// replaced the old synchronous Dispatcher.Speak's semaphore: Enqueue returns
// immediately, but the background worker still processes queued chunks
// strictly one at a time and in FIFO order, so two overlapping turns'
// speech never interleaves on the shared speaker.
func TestPlayer_EnqueueProcessesInOrderOneAtATime(t *testing.T) {
	var mu sync.Mutex
	var processed []string
	var inFlight int
	maxConcurrent := 0

	speakOne := func(ctx context.Context, text, entityID string) error {
		mu.Lock()
		inFlight++
		if inFlight > maxConcurrent {
			maxConcurrent = inFlight
		}
		mu.Unlock()

		time.Sleep(5 * time.Millisecond) // give a real overlap a chance to happen if serialization were broken

		mu.Lock()
		processed = append(processed, text)
		inFlight--
		mu.Unlock()
		return nil
	}

	p := newPlayer(speakOne, nil, nil)
	p.Enqueue("первый", "media_player.kitchen")
	p.Enqueue("второй", "media_player.kitchen")
	p.Enqueue("третий", "media_player.kitchen")

	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(processed) == 3
	}, time.Second, time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, []string{"первый", "второй", "третий"}, processed)
	require.Equal(t, 1, maxConcurrent)
}

// TestPlayer_StopClearsQueueAndCancelsInFlightChunk exercises the Stop
// contract: text still sitting in the queue is dropped and whatever chunk
// speakOne is mid-processing has its context cancelled.
func TestPlayer_StopClearsQueueAndCancelsInFlightChunk(t *testing.T) {
	started := make(chan struct{})
	var sawCancellation bool
	var mu sync.Mutex

	speakOne := func(ctx context.Context, text, entityID string) error {
		close(started)
		<-ctx.Done()
		mu.Lock()
		sawCancellation = true
		mu.Unlock()
		return ctx.Err()
	}

	p := newPlayer(speakOne, nil, nil)
	p.Enqueue("in flight when Stop is called", "media_player.kitchen")
	p.Enqueue("still queued, must be dropped", "media_player.kitchen")

	<-started // the first chunk is now blocked inside speakOne, waiting on ctx.Done()
	p.Stop()

	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return sawCancellation
	}, time.Second, time.Millisecond)

	p.mu.Lock()
	queueLen := len(p.queue)
	p.mu.Unlock()
	require.Zero(t, queueLen, "Stop must drop anything still queued")
}
