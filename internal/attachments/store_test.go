package attachments

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestStore_DefaultTTLEviction(t *testing.T) {
	s := &Store{records: make(map[string]Record), ttl: time.Hour}
	s.records["expired"] = Record{FileID: "expired", StoredAt: time.Now().Add(-2 * time.Hour)}
	s.records["fresh"] = Record{FileID: "fresh", StoredAt: time.Now()}

	s.evictExpired()

	_, expiredFound := s.Get("expired")
	_, freshFound := s.Get("fresh")
	require.False(t, expiredFound)
	require.True(t, freshFound)
}

// A download ownership record (see internal/httpapi's executeTool) sets a
// much longer per-record TTL than the store's own short upload-oriented
// default, since its marker is embedded durably in persisted conversation
// history and can be revisited long after the default TTL would otherwise
// evict it.
func TestStore_PerRecordTTLOverridesStoreDefault(t *testing.T) {
	s := &Store{records: make(map[string]Record), ttl: time.Minute}
	s.records["long-lived"] = Record{
		FileID:   "long-lived",
		StoredAt: time.Now().Add(-30 * time.Minute),
		TTL:      24 * time.Hour,
	}

	s.evictExpired()

	_, found := s.Get("long-lived")
	require.True(t, found, "a record with its own longer TTL must survive past the store's default TTL")
}
