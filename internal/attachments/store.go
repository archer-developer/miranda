// Package attachments provides an in-memory cache for files uploaded through
// POST /api/upload. Records are kept for a configurable TTL (default one
// hour) so the owning turn can retrieve the bytes when building the LLM
// prompt, then evicted automatically by a background sweeper.
//
// Every upload-staged record keeps its raw Data bytes, regardless of MIME
// type: images/text are inlined into the prompt from it, and binary blobs
// (PDFs, archives, ...) are served back out of it on demand by
// internal/httpapi's GET /files/{id} route — Miranda is the canonical host
// for an uploaded file's bytes, and hands the model a URI under that route
// rather than pushing the bytes anywhere itself (see
// docs/file-staging-refactor.md). The one exception is a download_file
// *ownership* record (set by executeTool, not by an upload) — that only
// ever needs UserID for an access check, so it never populates Data.
package attachments

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sync"
	"time"
)

// NewFileID generates a fresh, unguessable id for a newly-staged attachment
// — the id space GET /files/{id} (internal/httpapi's handleFilesServe)
// serves from, and by extension the only "credential" that route checks
// (see its own doc comment). Mirrors internal/telegram.RandomSecret's exact
// pattern: 24 random bytes, hex-encoded, well beyond brute-force range for
// the record's TTL-bounded lifetime.
func NewFileID() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("attachments: generate file id: %w", err)
	}
	return hex.EncodeToString(b), nil
}

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
	// FileID is Miranda's own opaque id for the uploaded file (see
	// NewFileID), served back out at GET /files/{id} and used to build the
	// fileURI the model sees for it.
	FileID string
	// Filename is the original client-provided filename, shown to the
	// model in the context annotation (e.g. "<file:readme.txt>") and set as
	// GET /files/{id}'s Content-Disposition.
	Filename string
	// MIMEType is the file's detected MIME type.
	MIMEType string
	// Size is the file's byte length.
	Size int64
	// Data holds the buffered file bytes: inlined directly into the prompt
	// for providers that support it (images for vision, text for context
	// injection), and/or served back out on demand at GET /files/{id} to
	// whichever external tool the model handed the resulting fileURI to
	// (see package doc comment). Nil only for a download_file
	// ownership record, which never had bytes of its own to buffer.
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
