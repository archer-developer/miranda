// Package history stores the raw dialog log — conversations, messages, and
// tool calls — in an embedded SQLite database (via the pure-Go
// modernc.org/sqlite driver, so cross-compilation never needs a C toolchain).
// This is the "go back to a dialog / search what I said" store; distilled
// long-term facts live separately in internal/memory as markdown.
package history

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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
	// ToolCallID is set on Role == "tool" messages: the id of the ToolCall
	// (see ToolCalls below) that this message's Content answers. Mirrors
	// llm.Message.ToolCallID so a stored turn can be replayed to the model
	// exactly as it was originally sent.
	ToolCallID string `json:"tool_call_id,omitempty"`
	// ToolCalls is set on Role == "assistant" messages that requested one or
	// more tool invocations, mirroring llm.Message.ToolCalls. Stored
	// alongside the assistant's own message (rather than only on the
	// resulting "tool" message) so a resumed conversation can rebuild the
	// exact assistant/tool message pair the model originally produced.
	ToolCalls []ToolCallRef `json:"tool_calls,omitempty"`
}

// ToolCallRef is the durable record of one tool invocation the model
// requested: enough to rebuild an llm.ToolCall when replaying stored
// history, without internal/history depending on internal/llm.
type ToolCallRef struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
	// ProviderMetadata mirrors llm.ToolCall.ProviderMetadata — an opaque,
	// provider-specific blob (e.g. Gemini's thought signature) that must
	// survive a stored conversation being replayed on a later turn, or a
	// provider that requires it will hard-error on that replay. Omitted
	// (empty string) for providers that don't set it; stored here as a
	// plain JSON string field, so old rows without it decode fine.
	ProviderMetadata string `json:"provider_metadata,omitempty"`
}

// Conversation is one stored dialog session.
type Conversation struct {
	ID        string     `json:"id"`
	UserID    string     `json:"user_id"`
	Source    string     `json:"source"`
	StartedAt time.Time  `json:"started_at"`
	EndedAt   *time.Time `json:"ended_at,omitempty"`
	// Summary is a short recap of what was discussed, filled in when the
	// conversation ends (idle timeout or an explicit end_conversation tool
	// call) — see EndConversationWithSummary. Empty for a still-open
	// conversation. Distinct from internal/memory's distilled "Preferences":
	// this is per-conversation, that's cross-conversation.
	Summary string `json:"summary,omitempty"`
	// SystemPrompt is the system prompt used for this conversation's most
	// recent turn (base persona + memory snapshot at the time), kept in sync
	// on every turn via SetSystemPrompt. Never sent back to the model — it's
	// stored purely so a conversation's context is fully inspectable later.
	SystemPrompt string `json:"system_prompt,omitempty"`
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

	if err := s.ensureColumn(ctx, "conversations", "summary", "TEXT"); err != nil {
		return err
	}
	if err := s.ensureColumn(ctx, "conversations", "system_prompt", "TEXT"); err != nil {
		return err
	}
	// tool_call_id / tool_calls_json let a stored conversation be replayed to
	// the model exactly as it happened, instead of dropping tool-call turns
	// when resuming — see AppendAssistantMessage, AppendToolResultMessage,
	// and resolveConversation in internal/httpapi.
	if err := s.ensureColumn(ctx, "messages", "tool_call_id", "TEXT"); err != nil {
		return err
	}
	if err := s.ensureColumn(ctx, "messages", "tool_calls_json", "TEXT"); err != nil {
		return err
	}
	return nil
}

// ensureColumn adds column to table if it isn't already there. SQLite's
// CREATE TABLE IF NOT EXISTS above is a no-op on an already-migrated
// database, so columns added after the initial release need this explicit
// existence check instead — ALTER TABLE ADD COLUMN has no IF NOT EXISTS
// form in the SQLite versions modernc.org/sqlite has historically tracked.
func (s *Store) ensureColumn(ctx context.Context, table, column, sqlType string) error {
	rows, err := s.db.QueryContext(ctx, fmt.Sprintf("PRAGMA table_info(%s)", table))
	if err != nil {
		return fmt.Errorf("history: inspect %s columns: %w", table, err)
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var cid int
		var name, colType string
		var notNull, pk int
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &colType, &notNull, &dflt, &pk); err != nil {
			return fmt.Errorf("history: scan %s column info: %w", table, err)
		}
		if name == column {
			return nil
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("history: read %s column info: %w", table, err)
	}

	if _, err := s.db.ExecContext(ctx, fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", table, column, sqlType)); err != nil {
		return fmt.Errorf("history: add column %s.%s: %w", table, column, err)
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

// EndConversation marks conversationID as finished, with no summary (used
// when there was nothing worth summarizing — see EndConversationWithSummary
// for the normal case).
func (s *Store) EndConversation(ctx context.Context, conversationID string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE conversations SET ended_at = CURRENT_TIMESTAMP WHERE id = ?`, conversationID)
	if err != nil {
		return fmt.Errorf("history: end conversation: %w", err)
	}
	return nil
}

// EndConversationWithSummary marks conversationID as finished and records
// its recap, atomically — the summarization pass's normal write path.
func (s *Store) EndConversationWithSummary(ctx context.Context, conversationID, summary string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE conversations SET ended_at = CURRENT_TIMESTAMP, summary = ? WHERE id = ?`, summary, conversationID)
	if err != nil {
		return fmt.Errorf("history: end conversation with summary: %w", err)
	}
	return nil
}

// SetSystemPrompt records the system prompt used for conversationID's most
// recent turn. Called once per turn so it always reflects the latest
// base-persona + memory snapshot actually sent to the model.
func (s *Store) SetSystemPrompt(ctx context.Context, conversationID, prompt string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE conversations SET system_prompt = ? WHERE id = ?`, prompt, conversationID)
	if err != nil {
		return fmt.Errorf("history: set system prompt: %w", err)
	}
	return nil
}

// DeleteConversation permanently removes conversationID and everything
// attached to it (messages, tool calls). This is the explicit
// forget_conversation tool's write path: unlike ending a conversation, it
// leaves nothing behind for summarization to ever pick up — the FTS index
// is kept in sync automatically by the messages table's AFTER DELETE
// trigger (see migrate).
func (s *Store) DeleteConversation(ctx context.Context, conversationID string) error {
	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM tool_calls WHERE message_id IN (SELECT id FROM messages WHERE conversation_id = ?)`,
		conversationID); err != nil {
		return fmt.Errorf("history: delete tool calls: %w", err)
	}
	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM messages WHERE conversation_id = ?`, conversationID); err != nil {
		return fmt.Errorf("history: delete messages: %w", err)
	}
	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM conversations WHERE id = ?`, conversationID); err != nil {
		return fmt.Errorf("history: delete conversation: %w", err)
	}
	return nil
}

// OpenConversation returns userID's currently open conversation (ended_at IS
// NULL), or nil if they have none. This is the sole continuity mechanism for
// a turn: the server — not whatever conversation_id a caller might echo
// back — decides whether to continue an existing conversation or start a
// new one, so the idle timeout and explicit end/forget tools actually
// govern session boundaries.
func (s *Store) OpenConversation(ctx context.Context, userID string) (*Conversation, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, user_id, source, started_at, ended_at, COALESCE(summary, ''), COALESCE(system_prompt, '')
		FROM conversations
		WHERE user_id = ? AND ended_at IS NULL
		ORDER BY started_at DESC
		LIMIT 1`, userID)

	c, err := scanConversation(row)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, fmt.Errorf("history: query open conversation: %w", err)
	}
	return c, nil
}

// GetConversation fetches a single conversation by id, or nil if it doesn't
// exist (e.g. it was already deleted via DeleteConversation).
func (s *Store) GetConversation(ctx context.Context, conversationID string) (*Conversation, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, user_id, source, started_at, ended_at, COALESCE(summary, ''), COALESCE(system_prompt, '')
		FROM conversations WHERE id = ?`, conversationID)

	c, err := scanConversation(row)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, fmt.Errorf("history: get conversation: %w", err)
	}
	return c, nil
}

func scanConversation(row *sql.Row) (*Conversation, error) {
	var c Conversation
	var endedAt sql.NullTime
	if err := row.Scan(&c.ID, &c.UserID, &c.Source, &c.StartedAt, &endedAt, &c.Summary, &c.SystemPrompt); err != nil {
		return nil, err
	}
	if endedAt.Valid {
		c.EndedAt = &endedAt.Time
	}
	return &c, nil
}

// IdleConversations returns still-open conversations (ended_at IS NULL)
// whose most recent message is at least idleFor old, so the background
// summarization sweeper (see cmd/miranda) can treat them as finished. The
// cutoff is applied in Go rather than in SQL: SQLite's CURRENT_TIMESTAMP
// writes "YYYY-MM-DD HH:MM:SS" while the driver formats a bound time.Time
// parameter differently, and comparing the two as strings would silently
// misorder results.
func (s *Store) IdleConversations(ctx context.Context, idleFor time.Duration) ([]Conversation, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT c.id, c.user_id, c.source, c.started_at, c.ended_at, MAX(m.created_at)
		FROM conversations c
		JOIN messages m ON m.conversation_id = c.id
		WHERE c.ended_at IS NULL
		GROUP BY c.id`)
	if err != nil {
		return nil, fmt.Errorf("history: query idle conversations: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []Conversation
	for rows.Next() {
		var c Conversation
		var endedAt sql.NullTime
		// MAX(m.created_at) has no declared column type, so the driver can't
		// auto-convert it to time.Time the way it does for a plain DATETIME
		// column — scan it as text and parse it in the same layout SQLite's
		// CURRENT_TIMESTAMP writes ("YYYY-MM-DD HH:MM:SS", UTC, no fraction).
		var lastMessageAtText string
		if err := rows.Scan(&c.ID, &c.UserID, &c.Source, &c.StartedAt, &endedAt, &lastMessageAtText); err != nil {
			return nil, fmt.Errorf("history: scan idle conversation: %w", err)
		}
		if endedAt.Valid {
			c.EndedAt = &endedAt.Time
		}
		lastMessageAt, err := time.Parse("2006-01-02 15:04:05", lastMessageAtText)
		if err != nil {
			return nil, fmt.Errorf("history: parse last message time for conversation %s: %w", c.ID, err)
		}
		if time.Since(lastMessageAt) >= idleFor {
			out = append(out, c)
		}
	}
	return out, rows.Err()
}

// AppendMessage records one plain turn (no tool-call metadata) and returns
// its row id (used to attach tool_calls to it via AppendToolCall). Use
// AppendAssistantMessage instead when the assistant's turn requested tool
// calls, and AppendToolResultMessage for the resulting "tool" turn, so both
// carry the metadata needed to replay them later.
func (s *Store) AppendMessage(ctx context.Context, conversationID, role, content string) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO messages (conversation_id, role, content) VALUES (?, ?, ?)`,
		conversationID, role, content)
	if err != nil {
		return 0, fmt.Errorf("history: append message: %w", err)
	}
	return res.LastInsertId()
}

// AppendAssistantMessage records an assistant turn, optionally along with
// the tool calls it requested (toolCalls may be empty for a plain text
// reply). Storing the tool calls on the assistant's own message — rather
// than only on the "tool" result message that follows — is what lets
// resolveConversation rebuild the exact assistant/tool message pair on
// resume, instead of dropping tool-call turns entirely.
func (s *Store) AppendAssistantMessage(ctx context.Context, conversationID, content string, toolCalls []ToolCallRef) (int64, error) {
	var toolCallsJSON sql.NullString
	if len(toolCalls) > 0 {
		b, err := json.Marshal(toolCalls)
		if err != nil {
			return 0, fmt.Errorf("history: encode tool calls: %w", err)
		}
		toolCallsJSON = sql.NullString{String: string(b), Valid: true}
	}
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO messages (conversation_id, role, content, tool_calls_json) VALUES (?, 'assistant', ?, ?)`,
		conversationID, content, toolCallsJSON)
	if err != nil {
		return 0, fmt.Errorf("history: append assistant message: %w", err)
	}
	return res.LastInsertId()
}

// AppendToolResultMessage records the "tool" turn answering toolCallID —
// the counterpart to the ToolCallRef stored on the preceding assistant
// message by AppendAssistantMessage.
func (s *Store) AppendToolResultMessage(ctx context.Context, conversationID, toolCallID, content string) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO messages (conversation_id, role, content, tool_call_id) VALUES (?, 'tool', ?, ?)`,
		conversationID, content, toolCallID)
	if err != nil {
		return 0, fmt.Errorf("history: append tool result message: %w", err)
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

// ConversationMessages returns all messages in conversationID, oldest first,
// including tool-call metadata (see Message.ToolCallID / Message.ToolCalls)
// so the full turn — not just its text — can be reconstructed.
func (s *Store) ConversationMessages(ctx context.Context, conversationID string) ([]Message, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, conversation_id, role, content, created_at, COALESCE(tool_call_id, ''), COALESCE(tool_calls_json, '')
		 FROM messages WHERE conversation_id = ? ORDER BY id ASC`,
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
		`SELECT id, user_id, source, started_at, ended_at, COALESCE(summary, ''), COALESCE(system_prompt, '')
		 FROM conversations WHERE user_id = ? ORDER BY started_at DESC LIMIT ?`, userID, limit)
	if err != nil {
		return nil, fmt.Errorf("history: query conversations: %w", err)
	}
	defer func() { _ = rows.Close() }()
	return scanConversations(rows)
}

// SearchConversations full-text searches userID's finished conversations for
// query and returns distinct matches, most recently started first, each
// with its stored Summary — this is what the search_history tool surfaces,
// so recalling an earlier dialog injects its recap rather than a dump of
// raw messages. Only ended conversations are searched: the current open
// conversation is already in the model's context for this turn. query is
// free text, not FTS5 syntax (see ftsQuery).
func (s *Store) SearchConversations(ctx context.Context, userID, query string, limit int) ([]Conversation, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT c.id, c.user_id, c.source, c.started_at, c.ended_at, COALESCE(c.summary, ''), COALESCE(c.system_prompt, '')
		FROM conversations c
		WHERE c.ended_at IS NOT NULL AND c.user_id = ? AND c.id IN (
			SELECT m.conversation_id
			FROM messages_fts
			JOIN messages m ON m.id = messages_fts.rowid
			WHERE messages_fts MATCH ?
		)
		ORDER BY c.started_at DESC
		LIMIT ?`, userID, ftsQuery(query), limit)
	if err != nil {
		return nil, fmt.Errorf("history: search conversations: %w", err)
	}
	defer func() { _ = rows.Close() }()
	return scanConversations(rows)
}

func scanConversations(rows *sql.Rows) ([]Conversation, error) {
	var out []Conversation
	for rows.Next() {
		var c Conversation
		var endedAt sql.NullTime
		if err := rows.Scan(&c.ID, &c.UserID, &c.Source, &c.StartedAt, &endedAt, &c.Summary, &c.SystemPrompt); err != nil {
			return nil, fmt.Errorf("history: scan conversation: %w", err)
		}
		if endedAt.Valid {
			c.EndedAt = &endedAt.Time
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// SearchMessages full-text searches userID's message history for query and
// returns matches, most recent first. query is treated as free text, not
// FTS5 query syntax, so arbitrary punctuation (this is called with
// model-generated input, via the search_history tool) can't be
// misinterpreted as an FTS5 operator and error out the search.
func (s *Store) SearchMessages(ctx context.Context, userID, query string, limit int) ([]Message, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT m.id, m.conversation_id, m.role, m.content, m.created_at, COALESCE(m.tool_call_id, ''), COALESCE(m.tool_calls_json, '')
		FROM messages_fts
		JOIN messages m ON m.id = messages_fts.rowid
		JOIN conversations c ON c.id = m.conversation_id
		WHERE messages_fts MATCH ? AND c.user_id = ?
		ORDER BY m.created_at DESC
		LIMIT ?`, ftsQuery(query), userID, limit)
	if err != nil {
		return nil, fmt.Errorf("history: search messages: %w", err)
	}
	defer func() { _ = rows.Close() }()
	return scanMessages(rows)
}

// ftsQuery turns free text into a safe FTS5 MATCH expression: each word is
// wrapped as a quoted phrase, with embedded quotes escaped by doubling (the
// FTS5 convention), so punctuation in the input can't be parsed as FTS5
// query syntax.
func ftsQuery(text string) string {
	fields := strings.Fields(text)
	quoted := make([]string, len(fields))
	for i, f := range fields {
		quoted[i] = `"` + strings.ReplaceAll(f, `"`, `""`) + `"`
	}
	return strings.Join(quoted, " ")
}

func scanMessages(rows *sql.Rows) ([]Message, error) {
	var out []Message
	for rows.Next() {
		var m Message
		var toolCallsJSON string
		if err := rows.Scan(&m.ID, &m.ConversationID, &m.Role, &m.Content, &m.CreatedAt, &m.ToolCallID, &toolCallsJSON); err != nil {
			return nil, fmt.Errorf("history: scan message: %w", err)
		}
		if toolCallsJSON != "" {
			if err := json.Unmarshal([]byte(toolCallsJSON), &m.ToolCalls); err != nil {
				return nil, fmt.Errorf("history: decode tool calls for message %d: %w", m.ID, err)
			}
		}
		out = append(out, m)
	}
	return out, rows.Err()
}
