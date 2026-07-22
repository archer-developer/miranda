// Package history stores the raw dialog log — conversations, messages, and
// tool calls — in an embedded SQLite database (via the pure-Go
// modernc.org/sqlite driver, so cross-compilation never needs a C toolchain).
// This is the "go back to a dialog / search what I said" store; distilled
// long-term facts live separately in internal/memory as markdown.
package history

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/google/uuid"
	_ "modernc.org/sqlite" // registers the "sqlite" database/sql driver
)

// Message is one stored turn in a conversation.
type Message struct {
	ID             int64     `json:"id"`
	ConversationID string    `json:"conversation_id"`
	Role           string    `json:"role"`
	Content        string    `json:"content"`
	CreatedAt      time.Time `json:"created_at"`
}

// Conversation is one stored dialog session.
type Conversation struct {
	ID        string     `json:"id"`
	UserID    string     `json:"user_id"`
	Source    string     `json:"source"`
	StartedAt time.Time  `json:"started_at"`
	EndedAt   *time.Time `json:"ended_at,omitempty"`
}

// Store is a SQLite-backed dialog history database.
type Store struct {
	db *sql.DB
}

// Open creates (if needed) and opens the SQLite database at path, applying
// the schema. busy_timeout is set so concurrent readers/writers from the
// same process (API handler + web UI) back off instead of erroring
// immediately on "database is locked".
func Open(path string) (*Store, error) {
	if dir := filepath.Dir(path); dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("history: create dir %s: %w", dir, err)
		}
	}

	dsn := path + "?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("history: open %s: %w", path, err)
	}
	// SQLite only supports one writer at a time; a single connection avoids
	// SQLITE_BUSY errors from this process racing against itself.
	db.SetMaxOpenConns(1)

	s := &Store{db: db}
	if err := s.migrate(context.Background()); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

// Close closes the underlying database connection.
func (s *Store) Close() error {
	return s.db.Close()
}

func (s *Store) migrate(ctx context.Context) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS users (
			id TEXT PRIMARY KEY,
			display_name TEXT,
			created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE TABLE IF NOT EXISTS conversations (
			id TEXT PRIMARY KEY,
			user_id TEXT NOT NULL,
			source TEXT NOT NULL,
			started_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			ended_at DATETIME
		)`,
		`CREATE INDEX IF NOT EXISTS idx_conversations_user ON conversations(user_id, started_at)`,
		`CREATE TABLE IF NOT EXISTS messages (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			conversation_id TEXT NOT NULL,
			role TEXT NOT NULL,
			content TEXT NOT NULL,
			created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE INDEX IF NOT EXISTS idx_messages_conversation ON messages(conversation_id, created_at)`,
		`CREATE TABLE IF NOT EXISTS tool_calls (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			message_id INTEGER NOT NULL,
			tool_name TEXT NOT NULL,
			mcp_server TEXT,
			request_json TEXT,
			response_json TEXT,
			created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
		)`,
		// Full-text search over message content, kept in sync via triggers so
		// callers never have to remember to update it separately.
		`CREATE VIRTUAL TABLE IF NOT EXISTS messages_fts USING fts5(
			content, content='messages', content_rowid='id'
		)`,
		`CREATE TRIGGER IF NOT EXISTS messages_ai AFTER INSERT ON messages BEGIN
			INSERT INTO messages_fts(rowid, content) VALUES (new.id, new.content);
		END`,
		`CREATE TRIGGER IF NOT EXISTS messages_ad AFTER DELETE ON messages BEGIN
			INSERT INTO messages_fts(messages_fts, rowid, content) VALUES('delete', old.id, old.content);
		END`,
		`CREATE TRIGGER IF NOT EXISTS messages_au AFTER UPDATE ON messages BEGIN
			INSERT INTO messages_fts(messages_fts, rowid, content) VALUES('delete', old.id, old.content);
			INSERT INTO messages_fts(rowid, content) VALUES (new.id, new.content);
		END`,
	}
	for _, stmt := range stmts {
		if _, err := s.db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("history: migrate: %w", err)
		}
	}
	return nil
}

// StartConversation records a new conversation for userID and returns its
// generated id. If userID hasn't been seen before, it's registered too.
func (s *Store) StartConversation(ctx context.Context, userID, source string) (string, error) {
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO users (id) VALUES (?) ON CONFLICT(id) DO NOTHING`, userID); err != nil {
		return "", fmt.Errorf("history: register user: %w", err)
	}

	id := uuid.NewString()
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO conversations (id, user_id, source) VALUES (?, ?, ?)`, id, userID, source); err != nil {
		return "", fmt.Errorf("history: start conversation: %w", err)
	}
	return id, nil
}

// EndConversation marks conversationID as finished.
func (s *Store) EndConversation(ctx context.Context, conversationID string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE conversations SET ended_at = CURRENT_TIMESTAMP WHERE id = ?`, conversationID)
	if err != nil {
		return fmt.Errorf("history: end conversation: %w", err)
	}
	return nil
}

// AppendMessage records one turn and returns its row id (used to attach
// tool_calls to it via AppendToolCall).
func (s *Store) AppendMessage(ctx context.Context, conversationID, role, content string) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO messages (conversation_id, role, content) VALUES (?, ?, ?)`,
		conversationID, role, content)
	if err != nil {
		return 0, fmt.Errorf("history: append message: %w", err)
	}
	return res.LastInsertId()
}

// AppendToolCall records one tool invocation associated with messageID.
func (s *Store) AppendToolCall(ctx context.Context, messageID int64, toolName, mcpServer, requestJSON, responseJSON string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO tool_calls (message_id, tool_name, mcp_server, request_json, response_json) VALUES (?, ?, ?, ?, ?)`,
		messageID, toolName, mcpServer, requestJSON, responseJSON)
	if err != nil {
		return fmt.Errorf("history: append tool call: %w", err)
	}
	return nil
}

// ConversationMessages returns all messages in conversationID, oldest first.
func (s *Store) ConversationMessages(ctx context.Context, conversationID string) ([]Message, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, conversation_id, role, content, created_at FROM messages WHERE conversation_id = ? ORDER BY id ASC`,
		conversationID)
	if err != nil {
		return nil, fmt.Errorf("history: query messages: %w", err)
	}
	defer func() { _ = rows.Close() }()
	return scanMessages(rows)
}

// RecentConversations returns userID's most recent conversations, newest first.
func (s *Store) RecentConversations(ctx context.Context, userID string, limit int) ([]Conversation, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, user_id, source, started_at, ended_at FROM conversations
		 WHERE user_id = ? ORDER BY started_at DESC LIMIT ?`, userID, limit)
	if err != nil {
		return nil, fmt.Errorf("history: query conversations: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []Conversation
	for rows.Next() {
		var c Conversation
		var endedAt sql.NullTime
		if err := rows.Scan(&c.ID, &c.UserID, &c.Source, &c.StartedAt, &endedAt); err != nil {
			return nil, fmt.Errorf("history: scan conversation: %w", err)
		}
		if endedAt.Valid {
			c.EndedAt = &endedAt.Time
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// SearchMessages full-text searches userID's message history for query
// (FTS5 query syntax) and returns matches, most recent first.
func (s *Store) SearchMessages(ctx context.Context, userID, query string, limit int) ([]Message, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT m.id, m.conversation_id, m.role, m.content, m.created_at
		FROM messages_fts
		JOIN messages m ON m.id = messages_fts.rowid
		JOIN conversations c ON c.id = m.conversation_id
		WHERE messages_fts MATCH ? AND c.user_id = ?
		ORDER BY m.created_at DESC
		LIMIT ?`, query, userID, limit)
	if err != nil {
		return nil, fmt.Errorf("history: search messages: %w", err)
	}
	defer func() { _ = rows.Close() }()
	return scanMessages(rows)
}

func scanMessages(rows *sql.Rows) ([]Message, error) {
	var out []Message
	for rows.Next() {
		var m Message
		if err := rows.Scan(&m.ID, &m.ConversationID, &m.Role, &m.Content, &m.CreatedAt); err != nil {
			return nil, fmt.Errorf("history: scan message: %w", err)
		}
		out = append(out, m)
	}
	return out, rows.Err()
}
