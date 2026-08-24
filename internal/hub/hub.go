// Package hub broadcasts structured log/event lines to subscribers (the web
// UI's WebSocket log tail, primarily) while keeping a bounded in-memory
// buffer, one per event source, so a new subscriber sees recent history
// immediately on connect.
package hub

import (
	"bytes"
	"encoding/json"
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

// SourceLimit bounds one hub source's replay buffer (see Hub.buffers).
// A zero value on either field means "no cap on that dimension"; leaving
// both zero means that source is fully unbounded — only do that
// deliberately, and never for a source fed by high-frequency or
// user-triggered bursts (see New's doc comment for why sources differ).
type SourceLimit struct {
	MaxCount int // 0 = unlimited event count
	MaxBytes int // 0 = unlimited total serialized size (see eventSize)
}

// sourceBuf is one event source's own replay history and trim bookkeeping.
type sourceBuf struct {
	events []Event
	sizes  []int // eventSize(events[i]), parallel to events
	bytes  int    // running total of sizes, kept in sync with events/sizes
}

// Hub fans Events out to any number of subscribers and retains, per source,
// the last events published to it so late subscribers (a browser tab opened
// mid-request) aren't blind.
type Hub struct {
	mu sync.Mutex

	// bufferSize is the capacity of every subscriber's channel (see
	// Subscribe) and the MaxCount fallback for any source with no entry in
	// limits.
	bufferSize int
	limits     map[string]SourceLimit

	buffers map[string]*sourceBuf
	order   []string // source names in first-seen order, for deterministic replay

	subscribers map[chan Event]func(Event) bool
}

// New creates a Hub whose subscriber channels have capacity bufferSize and
// whose per-source replay buffers are trimmed as directed by limits — a
// source with no entry there (or when limits is nil) falls back to a plain
// MaxCount cap of bufferSize, matching the Hub's original (pre-per-source)
// behavior.
//
// Different sources genuinely need different policies, which is why this
// isn't just one shared cap: app_log events are one short line apiece, so
// they're best bounded by total size (a "how many KB of recent log text"
// budget). llm_log events are different in kind — each one's Data is a
// whole miranda-llm/llmtrace/analyze.Block (full request/response,
// including the accumulated conversation history the provider was sent —
// see Hub.LLMTraceWriter), and a single call's trace can legitimately be
// large without that being a reason to drop it; what the Logs screen's
// "LLM trace" tab actually wants is a fixed number of recent *calls*
// (one UI row per block), regardless of any one call's size. A single
// shared byte/count cap can express neither of these precisely, and on a
// long-lived, chatty server the difference matters: every buffered event
// for a source is replayed to a /ws/logs subscriber in one synchronous
// burst on connect (see httpapi.Server.handleWSLogs), so an under-tuned
// cap on llm_log specifically can turn simply opening the web UI into a
// multi-megabyte payload that freezes the tab rendering it.
func New(bufferSize int, limits map[string]SourceLimit) *Hub {
	if bufferSize <= 0 {
		bufferSize = 1
	}
	return &Hub{
		bufferSize:  bufferSize,
		limits:      limits,
		buffers:     make(map[string]*sourceBuf),
		subscribers: make(map[chan Event]func(Event) bool),
	}
}

// eventSize estimates ev's serialized size for a source's byte-cap trim.
// Message covers app_log's plain-text lines; Data (marshaled the same way
// wsjson.Write will send it) covers llm_log's *analyze.Block payloads, which
// dominate real-world buffer size — see New's doc comment.
func eventSize(ev Event) int {
	n := len(ev.Message)
	if ev.Data != nil {
		if b, err := json.Marshal(ev.Data); err == nil {
			n += len(b)
		}
	}
	return n
}

// Publish broadcasts ev to every subscriber whose filter (see Subscribe)
// accepts it, and appends it to ev.Source's own replay buffer regardless of
// any filter (each source's buffer is a full, unfiltered tail of everything
// ever published to it — each Subscribe call re-filters across every
// source's buffer for its own replay). Subscribers with a full channel are
// skipped rather than blocking the publisher — a slow web UI tab must never
// stall the agent loop.
func (h *Hub) Publish(ev Event) {
	h.mu.Lock()
	defer h.mu.Unlock()

	buf, ok := h.buffers[ev.Source]
	if !ok {
		buf = &sourceBuf{}
		h.buffers[ev.Source] = buf
		h.order = append(h.order, ev.Source)
	}

	lim, ok := h.limits[ev.Source]
	if !ok {
		lim = SourceLimit{MaxCount: h.bufferSize}
	}

	size := eventSize(ev)
	buf.events = append(buf.events, ev)
	buf.sizes = append(buf.sizes, size)
	buf.bytes += size

	for (lim.MaxCount > 0 && len(buf.events) > lim.MaxCount) ||
		(lim.MaxBytes > 0 && buf.bytes > lim.MaxBytes && len(buf.events) > 1) {
		buf.bytes -= buf.sizes[0]
		buf.events = buf.events[1:]
		buf.sizes = buf.sizes[1:]
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
// of currently buffered events across every source (filtered the same way,
// if filter is set). Call the returned unsubscribe func when done.
//
// The replay is grouped by source (each source's own buffer, in its own
// chronological order, one source fully before the next in h.order's
// first-seen order) rather than one global chronological interleaving —
// safe today because every current consumer already keys off ev.Source
// before doing anything with an event: logs.js renders app_log/llm_log as
// separate tabs, and ws.js's own client-side buffer already buckets
// everything by source. A filter that needed strict cross-source ordering
// would need to sort replay itself.
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

	for _, source := range h.order {
		for _, ev := range h.buffers[source].events {
			if filter == nil || filter(ev) {
				replay = append(replay, ev)
			}
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
