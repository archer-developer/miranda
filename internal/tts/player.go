package tts

import (
	"context"
	"errors"
	"log/slog"
	"sync"

	"github.com/archer-developer/miranda/internal/hub"
)

// speakItem is one unit of work in the Player's queue: text to synthesize
// and entityID to play it through (the resolved media_player entity_id,
// already looked up by the Dispatcher before enqueueing — the Player itself
// has no knowledge of device names or HA).
type speakItem struct {
	text     string
	entityID string
}

// Player is a background-worker queue that decouples "enqueue this text to
// be spoken" from "block until it's actually audible" — Dispatcher.Speak
// calls Enqueue and returns immediately, so a slow or quota-throttled
// synthesis call (or the physical station's real playback duration) never
// stalls the agent loop's turn. A single worker goroutine drains the queue
// one chunk at a time, calling speakOne for each — the same one-at-a-time
// ordering the old synchronous Dispatcher.Speak's semaphore gave two
// overlapping turns, now free (the queue itself serializes everything
// without an explicit lock around each Speak call).
type Player struct {
	mu    sync.Mutex
	cond  *sync.Cond
	queue []speakItem
	// cancel is set (under mu) to the cancel func of whichever chunk's
	// context is currently being processed by run, and cleared back to nil
	// once that call returns — Stop uses it to interrupt work in flight.
	cancel context.CancelFunc
	closed bool

	speakOne func(ctx context.Context, text, entityID string) error

	hub    *hub.Hub
	logger *slog.Logger
}

// newPlayer builds a Player and starts its worker goroutine.
func newPlayer(speakOne func(ctx context.Context, text, entityID string) error, h *hub.Hub, logger *slog.Logger) *Player {
	if logger == nil {
		logger = slog.Default()
	}
	p := &Player{
		speakOne: speakOne,
		hub:      h,
		logger:   logger,
	}
	p.cond = sync.NewCond(&p.mu)
	go p.run()
	return p
}

// Enqueue appends text+entityID to the queue and returns immediately — see
// Player's doc comment. Safe to call concurrently and while the worker is
// mid-chunk.
func (p *Player) Enqueue(text, entityID string) {
	p.mu.Lock()
	p.queue = append(p.queue, speakItem{text: text, entityID: entityID})
	p.mu.Unlock()
	p.cond.Signal()
}

// run is the worker goroutine: pull one chunk off the queue at a time,
// synthesize/play it via speakOne, and report any failure to the hub
// (Source: "tts") — matching what the old synchronous Dispatcher.Speak
// callers used to do themselves on error, since nothing downstream of
// Enqueue can return an error to the agent loop anymore.
func (p *Player) run() {
	for {
		p.mu.Lock()
		for len(p.queue) == 0 && !p.closed {
			p.cond.Wait()
		}
		if p.closed && len(p.queue) == 0 {
			p.mu.Unlock()
			return
		}
		item := p.queue[0]
		p.queue = p.queue[1:]

		ctx, cancel := context.WithCancel(context.Background())
		p.cancel = cancel
		p.mu.Unlock()

		err := p.speakOne(ctx, item.text, item.entityID)

		p.mu.Lock()
		p.cancel = nil
		p.cond.Broadcast() // wake any WaitIdle callers blocked on this chunk
		p.mu.Unlock()
		cancel() // release the context's resources now that this chunk is done either way

		// context.Canceled means Stop interrupted this chunk deliberately —
		// not a real failure worth surfacing as a "tts" hub warning.
		if err != nil && !errors.Is(err, context.Canceled) {
			p.publish("speak failed: " + err.Error())
		}
	}
}

// Stop clears any not-yet-spoken queued text and interrupts whatever chunk
// is currently being synthesized/played. The station will finish its current
// short chunk on its own — Miranda's chunks are short enough that the brief
// tail is acceptable, and avoids having to maintain a separate entity list
// just for media_stop calls.
func (p *Player) Stop() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.queue = nil
	if p.cancel != nil {
		p.cancel()
	}
}

// WaitIdle blocks until the Player's queue is empty and no chunk is currently
// being synthesized/played — i.e. the background worker is truly idle.
// Returns immediately if already idle, and honours ctx cancellation.
//
// A helper goroutine broadcasts on the cond when ctx is cancelled, because
// sync.Cond.Wait has no built-in timeout: without that broadcast, a caller
// waiting while the player is busy would be stuck until the next chunk
// completes naturally.
func (p *Player) WaitIdle(ctx context.Context) {
	wakeCtx, cancelWake := context.WithCancel(ctx)
	defer cancelWake()
	go func() {
		<-wakeCtx.Done()
		p.cond.Broadcast()
	}()

	p.mu.Lock()
	defer p.mu.Unlock()
	for (len(p.queue) > 0 || p.cancel != nil) && ctx.Err() == nil {
		p.cond.Wait()
	}
}

// publish reports a failure to the hub, if one is configured — nil-safe so
// tests/callers that don't care about hub events can pass a nil *hub.Hub.
func (p *Player) publish(message string) {
	if p.hub == nil {
		return
	}
	p.hub.Publish(hub.Event{Source: "tts", Message: message})
}
