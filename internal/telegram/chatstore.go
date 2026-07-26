package telegram

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// ChatStore persists the Miranda username -> Telegram chat id mapping
// learned from incoming webhook messages, as a single small JSON file. The
// Bot API gives no way to look up a chat id for a username, or to message a
// user who has never started a chat with the bot — so unlike HAUserID,
// which is set once in config.yaml, this mapping can only be learned at
// runtime and has to be persisted for the send_telegram tool to work
// across restarts.
type ChatStore struct {
	path string

	// mu serializes reads/writes so concurrent webhook deliveries can't
	// race and clobber each other's update to the underlying file.
	mu   sync.Mutex
	data map[string]int64
}

// OpenChatStore loads path, creating its parent directory if needed. A
// missing file is not an error — it just means no chat ids are known yet
// (nobody has messaged the bot).
func OpenChatStore(path string) (*ChatStore, error) {
	if dir := filepath.Dir(path); dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("telegram: create dir for chat store: %w", err)
		}
	}

	s := &ChatStore{path: path, data: make(map[string]int64)}
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return s, nil
		}
		return nil, fmt.Errorf("telegram: read chat store %s: %w", path, err)
	}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &s.data); err != nil {
			return nil, fmt.Errorf("telegram: parse chat store %s: %w", path, err)
		}
	}
	return s, nil
}

// Save records username's current chat id. Called on every incoming
// webhook message — cheap and idempotent, and the only place a mapping is
// ever learned or updated (e.g. if a user blocks and later re-adds the
// bot from a different chat).
func (s *ChatStore) Save(username string, chatID int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if existing, ok := s.data[username]; ok && existing == chatID {
		return nil
	}
	s.data[username] = chatID
	return s.writeLocked()
}

// Get returns username's known chat id, if they've ever messaged the bot.
func (s *ChatStore) Get(username string) (int64, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id, ok := s.data[username]
	return id, ok
}

func (s *ChatStore) writeLocked() error {
	raw, err := json.MarshalIndent(s.data, "", "  ")
	if err != nil {
		return fmt.Errorf("telegram: marshal chat store: %w", err)
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return fmt.Errorf("telegram: write chat store: %w", err)
	}
	// Atomic rename so a crash mid-write never leaves a truncated file.
	if err := os.Rename(tmp, s.path); err != nil {
		return fmt.Errorf("telegram: rename chat store into place: %w", err)
	}
	return nil
}
