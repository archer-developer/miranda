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

	speakOne := func(ctx context.Context, text string) error {
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

	p := newPlayer(&stopTrackingHA{}, nil, speakOne, nil, nil)
	p.Enqueue("первый")
	p.Enqueue("второй")
	p.Enqueue("третий")

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

// TestPlayer_StopClearsQueueAndCancelsInFlightChunk exercises the full
// Stop contract: text still sitting in the queue is dropped, whatever chunk
// speakOne is mid-processing has its context cancelled, and every
// configured entity gets a media_player.media_stop call regardless of
// whether anything was actually in flight.
func TestPlayer_StopClearsQueueAndCancelsInFlightChunk(t *testing.T) {
	started := make(chan struct{})
	var sawCancellation bool
	var mu sync.Mutex

	speakOne := func(ctx context.Context, text string) error {
		close(started)
		<-ctx.Done()
		mu.Lock()
		sawCancellation = true
		mu.Unlock()
		return ctx.Err()
	}

	ha := &stopTrackingHA{}
	p := newPlayer(ha, []string{"media_player.kitchen", "media_player.bedroom"}, speakOne, nil, nil)
	p.Enqueue("in flight when Stop is called")
	p.Enqueue("still queued, must be dropped")

	<-started // the first chunk is now blocked inside speakOne, waiting on ctx.Done()
	p.Stop(context.Background())

	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return sawCancellation
	}, time.Second, time.Millisecond)

	p.mu.Lock()
	queueLen := len(p.queue)
	p.mu.Unlock()
	require.Zero(t, queueLen, "Stop must drop anything still queued")

	require.ElementsMatch(t, []string{"media_player.kitchen", "media_player.bedroom"}, ha.StopCalls())
}

// TestPlayer_StopWithNothingInFlightStillStopsThePhysicalStation guards
// against only wiring Stop's context-cancellation half: even when the
// queue is already empty (nothing to cancel), Stop must still call
// media_stop, since audio already finished rendering client-side could
// still be audibly playing on the physical device.
func TestPlayer_StopWithNothingInFlightStillStopsThePhysicalStation(t *testing.T) {
	ha := &stopTrackingHA{}
	p := newPlayer(ha, []string{"media_player.kitchen"}, func(ctx context.Context, text string) error { return nil }, nil, nil)

	p.Stop(context.Background())

	require.Equal(t, []string{"media_player.kitchen"}, ha.StopCalls())
}
