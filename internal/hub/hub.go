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

	// Sized to match h.bufferSize (config.WebUI.LogBufferSize), not a small
	// fixed constant: a single internal/llmtrace block publishes every one of
	// its lines (a large trace can run to hundreds of lines — see
	// internal/llmtrace/llmtrace.go) from one synchronous Hub.Publish loop,
	// far faster than the WS handler's Write goroutine can drain them over
	// the network (see internal/httpapi.Server.handleWSLogs). A too-small
	// channel used to fill up mid-burst and silently drop the rest of that
	// same event (Publish's docs above are explicit that a full channel is
	// skipped, not blocked on) — harmless for a scrolling plain-text pane
	// missing one line, but fatal for the Logs screen's "LLM trace" tab,
	// which only recognizes a call as complete once every one of its lines,
	// including the terminating blank line, has arrived (see
	// static/js/screens/logs-trace-parser.js) — a dropped line anywhere in
	// the block meant it could never render at all.
	sub := make(chan Event, h.bufferSize)
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
