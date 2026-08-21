// Package hub broadcasts structured log/event lines to subscribers (the web
// UI's WebSocket log tail, primarily) while keeping a bounded in-memory
// buffer so a new subscriber sees recent history immediately on connect.
package hub

import (
	"bytes"
	"strings"
	"sync"
)

// Event is one broadcastable log/event line. Fields are deliberately generic
// so any pipeline stage (router, tool calls, TTS) can publish through it.
type Event struct {
	Source  string `json:"source"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
	// UserID scopes an event to one user's chat (see httpapi's ChatEvent /
	// GET /ws/chat/{username}) — left empty for the app/error/log events
	// broadcast to every subscriber over /ws/logs regardless of who's
	// looking, so the Hub itself never needs to know about per-user
	// scoping: that filtering happens entirely in the WS handler that reads
	// from Subscribe().
	UserID string `json:"user_id,omitempty"`
}

// Hub fans Events out to any number of subscribers and retains the last N
// events so late subscribers (a browser tab opened mid-request) aren't blind.
type Hub struct {
	mu          sync.Mutex
	bufferSize  int
	buffer      []Event
	subscribers map[chan Event]func(Event) bool
}

// New creates a Hub that retains up to bufferSize recent events for replay to
// new subscribers.
func New(bufferSize int) *Hub {
	if bufferSize <= 0 {
		bufferSize = 1
	}
	return &Hub{
		bufferSize:  bufferSize,
		subscribers: make(map[chan Event]func(Event) bool),
	}
}

// Publish broadcasts ev to every subscriber whose filter (see Subscribe)
// accepts it, and appends it to the replay buffer regardless of any filter
// (the buffer is a shared, unfiltered tail of everything ever published —
// each Subscribe call re-filters it for its own replay). Subscribers with a
// full channel are skipped rather than blocking the publisher — a slow web
// UI tab must never stall the agent loop.
func (h *Hub) Publish(ev Event) {
	h.mu.Lock()
	defer h.mu.Unlock()

	h.buffer = append(h.buffer, ev)
	if len(h.buffer) > h.bufferSize {
		h.buffer = h.buffer[len(h.buffer)-h.bufferSize:]
	}

	for sub, filter := range h.subscribers {
		if filter != nil && !filter(ev) {
			continue
		}
		select {
		case sub <- ev:
		default:
		}
	}
}

// Subscribe registers a new subscriber and returns its channel plus a replay
// of currently buffered events (filtered the same way, if filter is set).
// Call the returned unsubscribe func when done.
//
// filter, if non-nil, is checked in Publish *before* an event is ever sent
// to this subscriber's channel — pass one whenever a subscriber only cares
// about a slice of the hub's traffic (e.g. GET /ws/chat/{username} only
// wants Source == "chat" events for its own UserID). Without a filter, a
// subscriber's fixed-size channel (see sizing note below) is shared by
// every event published anywhere in the app; a burst of traffic the
// subscriber doesn't even care about can fill it and silently crowd out
// the events it does, since Publish skips rather than blocks a full
// channel. Pass nil to receive everything unfiltered (e.g. /ws/logs, which
// really does want the full firehose).
func (h *Hub) Subscribe(filter func(Event) bool) (ch <-chan Event, replay []Event, unsubscribe func()) {
	h.mu.Lock()
	defer h.mu.Unlock()

	// Sized to match h.bufferSize (config.WebUI.LogBufferSize), not a small
	// fixed constant: app_log's plain Writer still publishes one Event per
	// raw line, and a burst of those (a busy request) could otherwise fill
	// a too-small channel and silently drop the rest of it (Publish's docs
	// above are explicit that a full channel is skipped, not blocked on) —
	// harmless for app_log's scrolling plain-text pane missing one line, but
	// was fatal for the Logs screen's "LLM trace" tab back when llm_log was
	// also published one raw line at a time: a call was only recognized as
	// complete once every one of its lines, including the terminating blank
	// line, had arrived, so a single dropped line anywhere in a large trace
	// meant it could never render at all. llm_log now publishes one Event
	// per already-reassembled call instead (see Hub.LLMTraceWriter), which
	// removed that specific failure mode, but app_log's own burst risk is
	// reason enough to keep this the same size rather than a small constant.
	sub := make(chan Event, h.bufferSize)
	h.subscribers[sub] = filter

	for _, ev := range h.buffer {
		if filter == nil || filter(ev) {
			replay = append(replay, ev)
		}
	}

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

// Writer returns an io.Writer that publishes each complete line written to it
// as an Event{Source: source, Message: line} — so a log file's io.MultiWriter
// can fan its output into the hub alongside disk/stdout, letting the web UI's
// log viewer screen show live app/LLM-trace log lines over the same /ws/logs
// connection used for chat/tool events, with no separate WS endpoint and no
// file-tailing (which would have to handle lumberjack's rotation renames).
func (h *Hub) Writer(source string) *Writer {
	return &Writer{hub: h, source: source}
}

// Writer is a line-buffering io.Writer adapter; see Hub.Writer.
type Writer struct {
	hub    *Hub
	source string

	mu  sync.Mutex
	buf bytes.Buffer
}

// Write implements io.Writer. Partial lines are buffered until a trailing
// newline completes them, so a caller that writes in arbitrary-sized chunks
// (rather than one full line per call) still produces one Event per line.
func (w *Writer) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	w.buf.Write(p)
	for {
		line, err := w.buf.ReadString('\n')
		if err != nil {
			// No newline yet: put the partial line back and wait for more.
			w.buf.Reset()
			w.buf.WriteString(line)
			break
		}
		w.hub.Publish(Event{Source: w.source, Message: strings.TrimSuffix(line, "\n")})
	}
	return len(p), nil
}
