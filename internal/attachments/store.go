// Package attachments provides an in-memory cache for files uploaded through
// POST /api/upload. Records are kept for a configurable TTL (default one
// hour) so the owning turn can retrieve the bytes when building the LLM
// prompt, then evicted automatically by a background sweeper.
//
// Only files that can be inlined into the prompt (images for vision, text for
// context) have their Data bytes stored; binary blobs and PDFs are processed
// entirely inside the sandbox and are represented here only by metadata
// (Data == nil).
package attachments

import (
	"sync"
	"time"
)

// defaultTTL is how long a record lives in the store before the sweeper
// removes it. One hour is generous enough to cover a slow typist who
// attaches a file, walks away, and comes back to finish the message.
const defaultTTL = time.Hour

// Record holds the data for one uploaded file.
type Record struct {
	// UserID is the authenticated username of the session that uploaded
	// this file (empty for bearer-token auth, which has no per-user
	// identity). processAttachments checks this against the requesting
	// user so one household member cannot reference another member's
	// file_id in their own InputRequest.Attachments.
	UserID string
	// FileID is the opaque ID the sandbox assigned to the uploaded file,
	// used to reference it in subsequent upload_file MCP tool calls.
	FileID string
	// Filename is the original client-provided filename, shown to the
	// model in the context annotation (e.g. "<file:readme.txt>").
	Filename string
	// MIMEType is the file's detected MIME type.
	MIMEType string
	// Size is the file's byte length as reported by the sandbox.
	Size int64
	// Data holds the buffered file bytes for providers that inline content
	// (images for vision, text for context injection). Nil for binary
	// blobs and PDFs that the sandbox processes, which are accessed via
	// FileID rather than inlined into the prompt.
	Data []byte
	// TTL overrides the store's default TTL for this one record when
	// non-zero. Upload-staging records rely on the store default (built for
	// a turn still being composed); a download's ownership record instead
	// backs a link embedded durably in persisted conversation history, which
	// a user may revisit long after the default TTL would otherwise evict
	// it — see internal/httpapi's executeTool, which sets this for
	// download_file results.
	TTL time.Duration
	// StoredAt is when this record was inserted; compared against TTL (or
	// the store's default) by the background sweeper.
	StoredAt time.Time
}

// Store is a thread-safe in-memory cache of uploaded-file records with a
// background TTL sweeper. Create with NewStore; call Close when done.
type Store struct {
	mu      sync.RWMutex
	records map[string]Record
	ttl     time.Duration
	done    chan struct{}
}

// NewStore creates a Store with the given TTL and starts the background
// sweeper goroutine. Pass ttl = 0 to use the default (one hour). Call
// Close when the store is no longer needed to stop the sweeper cleanly.
func NewStore(ttl time.Duration) *Store {
	if ttl <= 0 {
		ttl = defaultTTL
	}
	s := &Store{
		records: make(map[string]Record),
		ttl:     ttl,
		done:    make(chan struct{}),
	}
	go s.sweep()
	return s
}

// Put adds or replaces a record in the store. StoredAt is set to the
// current time regardless of any value already in rec.
func (s *Store) Put(rec Record) {
	rec.StoredAt = time.Now()
	s.mu.Lock()
	s.records[rec.FileID] = rec
	s.mu.Unlock()
}

// Get returns the record for fileID, or (zero, false) if it is absent
// or has already been evicted by the sweeper.
func (s *Store) Get(fileID string) (Record, bool) {
	s.mu.RLock()
	rec, ok := s.records[fileID]
	s.mu.RUnlock()
	return rec, ok
}

// Delete removes a record immediately. A no-op for unknown ids.
func (s *Store) Delete(fileID string) {
	s.mu.Lock()
	delete(s.records, fileID)
	s.mu.Unlock()
}

// Close stops the background sweeper goroutine. The store must not be
// used after Close returns.
func (s *Store) Close() {
	close(s.done)
}

// sweep is the background goroutine that removes expired records once a
// minute.
func (s *Store) sweep() {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-s.done:
			return
		case <-ticker.C:
			s.evictExpired()
		}
	}
}

// evictExpired removes all records whose age exceeds their TTL — a record's
// own Record.TTL if set, otherwise the store's default.
func (s *Store) evictExpired() {
	now := time.Now()
	s.mu.Lock()
	for id, rec := range s.records {
		ttl := s.ttl
		if rec.TTL > 0 {
			ttl = rec.TTL
		}
		if rec.StoredAt.Before(now.Add(-ttl)) {
			delete(s.records, id)
		}
	}
	s.mu.Unlock()
}
