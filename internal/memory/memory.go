// Package memory manages per-user long-term memory as markdown files
// (data/memory/<user_id>.md). Unlike internal/history's raw dialog log, this
// holds distilled, durable facts about a user (preferences, recurring
// patterns) that get included in the system prompt on every turn.
//
// Two independent writers can update a user's file: an explicit
// remember_this tool call in the moment, and an end-of-session summarization
// pass that merges new durable facts the model didn't explicitly flag.
package memory

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Store reads and writes per-user memory files under a root directory.
type Store struct {
	dir string

	// mu serializes writes per user so a remember_this call and an
	// end-of-session summarization can't race and clobber each other.
	mu sync.Mutex
}

// New creates a Store rooted at dir, creating the directory if needed.
func New(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("memory: create dir %s: %w", dir, err)
	}
	return &Store{dir: dir}, nil
}

func (s *Store) path(userID string) string {
	return filepath.Join(s.dir, userID+".md")
}

// sharedPath is shared.md's fixed location — household-wide memory has no
// userID key of its own, so unlike path(userID) this takes no argument.
func (s *Store) sharedPath() string {
	return filepath.Join(s.dir, "shared.md")
}

// Read returns userID's current memory content, or "" if they have none yet.
func (s *Store) Read(userID string) (string, error) {
	return s.readFile(s.path(userID))
}

// ReadShared returns the content of shared.md — household-wide facts injected
// into every user's system prompt before their personal memory. Returns "" if
// the file doesn't exist yet (not an error; it's optional).
func (s *Store) ReadShared() (string, error) {
	return s.readFile(s.sharedPath())
}

// readFile is the shared implementation behind Read/ReadShared/readLocked:
// a missing file is an empty memory document, not an error — every memory
// file (per-user or shared) starts out absent until something is first
// remembered into it.
func (s *Store) readFile(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", fmt.Errorf("memory: read %s: %w", path, err)
	}
	return string(data), nil
}

// Remember appends fact under a "## Remembered" section, timestamped. This
// is the explicit remember_this tool's write path: additive, never rewrites
// existing content, so it can't lose information even if called from a
// half-finished conversation.
func (s *Store) Remember(userID, fact string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	current, err := s.readLocked(userID)
	if err != nil {
		return err
	}

	entry := fmt.Sprintf("- (%s) %s\n", time.Now().Format("2006-01-02"), strings.TrimSpace(fact))
	updated := appendUnderSection(current, "## Remembered", entry)
	return s.writeLocked(userID, updated)
}

// RememberShared appends fact under a "## Remembered" section of shared.md,
// timestamped. Shared memory is household-wide and visible in every user's
// system prompt — use it for facts that belong to the household rather than
// one specific person (e.g. "у нас живёт кот Барсик").
func (s *Store) RememberShared(fact string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	current, err := s.readFile(s.sharedPath())
	if err != nil {
		return err
	}

	entry := fmt.Sprintf("- (%s) %s\n", time.Now().Format("2006-01-02"), strings.TrimSpace(fact))
	updated := appendUnderSection(current, "## Remembered", entry)
	return s.writeFile(s.sharedPath(), updated)
}

// Write overwrites userID's entire memory file verbatim with content. This
// is the web UI editor's write path: unlike Remember/ReplaceSection, which
// each only ever touch one section so the model's automated writes can't
// clobber each other, a human editing their own memory in the UI is
// expected to see and control the whole document at once.
func (s *Store) Write(userID, content string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.writeLocked(userID, content)
}

// ReplaceSection overwrites the content of a named "## <section>" with body,
// leaving every other section untouched. This is the summarization pass's
// write path: it re-derives one section (e.g. "## Preferences") from the
// latest conversation and merges it in without touching sections it didn't
// analyze (like the append-only "## Remembered" log).
func (s *Store) ReplaceSection(userID, section, body string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	current, err := s.readLocked(userID)
	if err != nil {
		return err
	}

	updated := replaceSection(current, "## "+section, strings.TrimSpace(body)+"\n")
	return s.writeLocked(userID, updated)
}

func (s *Store) readLocked(userID string) (string, error) {
	return s.readFile(s.path(userID))
}

func (s *Store) writeLocked(userID, content string) error {
	return s.writeFile(s.path(userID), content)
}

// writeFile is the shared implementation behind writeLocked/RememberShared:
// write-to-tmp then rename, so a crash mid-write never leaves a truncated
// memory file in place — callers must hold s.mu (writeFile itself doesn't).
func (s *Store) writeFile(path, content string) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(content), 0o644); err != nil {
		return fmt.Errorf("memory: write %s: %w", path, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("memory: rename into place for %s: %w", path, err)
	}
	return nil
}

// appendUnderSection appends entry (already newline-terminated) to the named
// section, creating the section at the end of the document if absent.
func appendUnderSection(doc, heading, entry string) string {
	sections := splitSections(doc)
	for i, sec := range sections {
		if sec.heading == heading {
			if sec.body != "" && !strings.HasSuffix(sec.body, "\n") {
				sec.body += "\n"
			}
			sec.body += entry
			sections[i] = sec
			return joinSections(sections)
		}
	}
	sections = append(sections, section{heading: heading, body: entry})
	return joinSections(sections)
}

// replaceSection overwrites the named section's body, creating it at the end
// if absent.
func replaceSection(doc, heading, body string) string {
	sections := splitSections(doc)
	for i, sec := range sections {
		if sec.heading == heading {
			sections[i] = section{heading: heading, body: body}
			return joinSections(sections)
		}
	}
	sections = append(sections, section{heading: heading, body: body})
	return joinSections(sections)
}

// section is one "## Heading\n<body>" block of a memory document. An empty
// heading holds any preamble text before the first "## " heading.
type section struct {
	heading string
	body    string
}

func splitSections(doc string) []section {
	if strings.TrimSpace(doc) == "" {
		return nil
	}

	var sections []section
	lines := strings.Split(doc, "\n")
	// strings.Split on a newline-terminated string always produces a trailing
	// empty element that isn't a real blank line — drop it so each round-trip
	// through splitSections/joinSections doesn't accumulate an extra "\n".
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	cur := section{}
	for _, line := range lines {
		if strings.HasPrefix(line, "## ") {
			if cur.heading != "" || cur.body != "" {
				sections = append(sections, cur)
			}
			cur = section{heading: strings.TrimRight(line, "\n")}
			continue
		}
		cur.body += line + "\n"
	}
	sections = append(sections, cur)
	return sections
}

func joinSections(sections []section) string {
	var b strings.Builder
	for _, sec := range sections {
		if sec.heading != "" {
			b.WriteString(sec.heading)
			b.WriteString("\n")
		}
		b.WriteString(sec.body)
	}
	return b.String()
}
