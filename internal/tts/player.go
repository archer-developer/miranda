package tts

import (
	"context"
	"errors"
	"log/slog"
	"sync"

	"github.com/archer-developer/miranda/internal/hub"
)

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
	queue []string
	// cancel is set (under mu) to the cancel func of whichever chunk's
	// context is currently being processed by run, and cleared back to nil
	// once that call returns — Stop uses it to interrupt work in flight.
	cancel context.CancelFunc
	closed bool

	speakOne func(ctx context.Context, text string) error

	ha       HAClient
	entities []string

	hub    *hub.Hub
	logger *slog.Logger
}

// newPlayer builds a Player and starts its worker goroutine. entities is
// the set of physical Yandex Station media_player entities Stop calls
// media_player.media_stop on — the one part of "stop whatever's playing"
// that's about the physical device, not about which provider queued the
// audio in the first place.
func newPlayer(ha HAClient, entities []string, speakOne func(ctx context.Context, text string) error, h *hub.Hub, logger *slog.Logger) *Player {
	if logger == nil {
		logger = slog.Default()
	}
	p := &Player{
		speakOne: speakOne,
		ha:       ha,
		entities: entities,
		hub:      h,
		logger:   logger,
	}
	p.cond = sync.NewCond(&p.mu)
	go p.run()
	return p
}

// Enqueue appends text to the queue and returns immediately — see Player's
// doc comment. Safe to call concurrently and while the worker is mid-chunk.
func (p *Player) Enqueue(text string) {
	p.mu.Lock()
	p.queue = append(p.queue, text)
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
		text := p.queue[0]
		p.queue = p.queue[1:]

		ctx, cancel := context.WithCancel(context.Background())
		p.cancel = cancel
		p.mu.Unlock()

		err := p.speakOne(ctx, text)

		p.mu.Lock()
		p.cancel = nil
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
// is currently being synthesized/played, then (regardless of whether
// anything was actually in flight) tells every configured Yandex Station
// entity to stop via media_player.media_stop — cancelling our own context
// alone doesn't un-play audio the speaker has already started rendering, so
// that HA call is what actually silences the physical device.
func (p *Player) Stop(ctx context.Context) {
	p.mu.Lock()
	p.queue = nil
	if p.cancel != nil {
		p.cancel()
	}
	p.mu.Unlock()

	for _, entity := range p.entities {
		if err := p.ha.CallService(ctx, "media_player", "media_stop", map[string]any{"entity_id": entity}); err != nil {
			p.publish("stop failed for " + entity + ": " + err.Error())
		}
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
