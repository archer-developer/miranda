// Package hub broadcasts structured log/event lines to subscribers (the web
// UI's WebSocket log tail, primarily) while keeping a bounded in-memory
// buffer so a new subscriber sees recent history immediately on connect.
package hub

import "sync"

// Event is one broadcastable log/event line. Fields are deliberately generic
// so any pipeline stage (router, tool calls, TTS) can publish through it.
type Event struct {
	Source  string `json:"source"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

// Hub fans Events out to any number of subscribers and retains the last N
// events so late subscribers (a browser tab opened mid-request) aren't blind.
type Hub struct {
	mu          sync.Mutex
	bufferSize  int
	buffer      []Event
	subscribers map[chan Event]struct{}
}

// New creates a Hub that retains up to bufferSize recent events for replay to
// new subscribers.
func New(bufferSize int) *Hub {
	if bufferSize <= 0 {
		bufferSize = 1
	}
	return &Hub{
		bufferSize:  bufferSize,
		subscribers: make(map[chan Event]struct{}),
	}
}

// Publish broadcasts ev to all current subscribers and appends it to the
// replay buffer. Subscribers with a full channel are skipped rather than
// blocking the publisher — a slow web UI tab must never stall the agent loop.
func (h *Hub) Publish(ev Event) {
	h.mu.Lock()
	defer h.mu.Unlock()

	h.buffer = append(h.buffer, ev)
	if len(h.buffer) > h.bufferSize {
		h.buffer = h.buffer[len(h.buffer)-h.bufferSize:]
	}

	for sub := range h.subscribers {
		select {
		case sub <- ev:
		default:
		}
	}
}

// Subscribe registers a new subscriber and returns its channel plus a replay
// of currently buffered events. Call the returned unsubscribe func when done.
func (h *Hub) Subscribe() (ch <-chan Event, replay []Event, unsubscribe func()) {
	h.mu.Lock()
	defer h.mu.Unlock()

	sub := make(chan Event, 64)
	h.subscribers[sub] = struct{}{}

	replay = make([]Event, len(h.buffer))
	copy(replay, h.buffer)

	unsub := func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		if _, ok := h.subscribers[sub]; ok {
			delete(h.subscribers, sub)
			close(sub)
		}
	}

	return sub, replay, unsub
}
