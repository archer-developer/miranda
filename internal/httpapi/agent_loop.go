package httpapi

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/robfig/cron/v3"

	llm "github.com/archer-developer/miranda-llm"
	"github.com/archer-developer/miranda/internal/attachments"
	"github.com/archer-developer/miranda/internal/config"
	"github.com/archer-developer/miranda/internal/history"
	"github.com/archer-developer/miranda/internal/hub"
	"github.com/archer-developer/miranda/internal/mcp"
	"github.com/archer-developer/miranda/internal/schedule"
	"github.com/archer-developer/miranda/internal/tts"
	"github.com/archer-developer/miranda/internal/users"
)

// searchHistoryLimit bounds how many past conversations the search_history
// tool feeds back to the model in one call, so a broad query doesn't blow up
// the prompt.
const searchHistoryLimit = 8

// turnControl lets a tool call executed mid-agent-loop (end_conversation,
// forget_conversation) signal Handle to close or delete the conversation
// once the turn finishes, instead of doing it immediately: the destructive
// action must wait until after the final assistant reply is recorded (and,
// via TTS, spoken), or that write would be orphaned/lost.
type turnControl struct {
	endRequested    bool
	forgetRequested bool
	// loadedGroups accumulates which lazy MCP server names load_tool_group
	// has expanded so far THIS turn — reset implicitly every turn since
	// turnControl is constructed fresh in Handle. Never persisted to
	// history/memory: re-collapsing to the compact stub on the next turn is
	// intentional, not a bug — see docs/adr/lazy-mcp-tool-loading.md §3/§4.
	loadedGroups map[string]bool
	// groupsChanged is set by executeTool whenever loadedGroups grew this
	// iteration, so runAgentLoop knows to recompute the tool list before the
	// next call instead of doing it unconditionally every iteration.
	groupsChanged bool
	// downloadedFiles accumulates one entry per distinct remote file URI
	// executeTool's file-URI detector finds in a file-exposing server's
	// tool result this turn (see detectRemoteFileLinks) — Handle reads this
	// after the loop finishes and records it as the turn's structured
	// history.Message.Downloads (via toDownloadRefs), deterministically,
	// regardless of what the model's own reply text says.
	downloadedFiles []downloadedFile
	// remoteFileURLs dedups the detector's finds within this turn, keyed by
	// the exact remote URL — a document fetched twice in one turn (e.g.
	// get_document called again for the same documentId) must not get two
	// separate chips.
	remoteFileURLs map[string]bool
}

// hasRemoteFile reports whether url was already recorded this turn via
// recordRemoteFile.
func (c *turnControl) hasRemoteFile(url string) bool {
	return c.remoteFileURLs[url]
}

// recordRemoteFile marks url as seen this turn, for hasRemoteFile.
func (c *turnControl) recordRemoteFile(url string) {
	if c.remoteFileURLs == nil {
		c.remoteFileURLs = make(map[string]bool)
	}
	c.remoteFileURLs[url] = true
}

// recordDownloadedFile appends df to downloadedFiles, replacing — not
// skipping — any earlier entry recorded this turn with the same
// filename/size/mime. A second dedup layer on top of hasRemoteFile, needed
// because the sandbox mints a fresh file_id (and thus a fresh file_uri) on
// every download_file call even when the underlying file is unchanged, e.g.
// the model re-downloading the same generated document after a later tool
// call in the same turn — URL-only dedup misses that case since the two
// URLs genuinely differ. Replacing rather than skipping matters: the
// sandbox does not keep every previously-issued file_id resolvable
// indefinitely, so of two file_ids for what's nominally the same file, it's
// consistently the *earlier* one whose /files/<id> proxy request 404s by
// the time the chip is clicked — keeping the first (as an earlier version
// of this dedup did) surfaced one permanently broken chip instead of the
// one that still works. See git history for the incident.
func (c *turnControl) recordDownloadedFile(df downloadedFile) {
	for i, existing := range c.downloadedFiles {
		if existing.filename == df.filename && existing.sizeBytes == df.sizeBytes && existing.mimeType == df.mimeType {
			c.downloadedFiles[i] = df
			return
		}
	}
	c.downloadedFiles = append(c.downloadedFiles, df)
}

// downloadedFile is one file executeTool's detector found and staged this
// turn — everything toDownloadRefs needs to build the turn's structured
// history.DownloadRef entry for it. filename/sizeBytes/mimeType are
// best-effort (see detectRemoteFileLinks) and may be zero/empty; the web
// UI's downloadChip already degrades cleanly when they are.
type downloadedFile struct {
	fileID    string
	filename  string
	sizeBytes int64
	mimeType  string
}

// remoteFileLink is one URL executeTool found rooted at a file-exposing
// server's own FilesEndpoint inside an MCP tool's raw result text, plus
// whatever best-effort metadata could be read from sibling JSON fields next
// to it — see detectRemoteFileLinks. This is the one shape every
// file-exposing server's download flows into, sandbox included: there is
// deliberately no second, tool-specific shape.
type remoteFileLink struct {
	url       string
	filename  string
	mimeType  string
	sizeBytes int64
}

// detectRemoteFileLinks scans an MCP tool call's raw result text for every
// absolute URL rooted at endpoint.FilesURL — e.g. a "fileUri" field in
// miranda-medical-card's medical.get_document response
// ("https://127.0.0.1:8791/files/file_49948e03-...") — and returns one entry
// per distinct URL found, with best-effort filename/mime_type/size_bytes
// recovered two ways depending on the result's shape: for a JSON result,
// from sibling fields in whichever object the URL itself came from (see
// findSiblingObject); for a plain-text result — e.g.
// miranda-code-execution-sandbox's download_file, which reports
// "file_id: ...\nfile_uri: ...\nfilename: ...\n..." rather than JSON — from
// "key: value" lines instead (see keyValueStringField/keyValueInt64Field).
// Both paths share the same priority-ordered key lists
// (filenameKeys/mimeTypeKeys/sizeBytesKeys) since the two shapes happen to
// use the same field names for this data. Detection of the URL itself is
// purely by shape, never by tool name, so it works for any tool on a server
// opted into config.MCPServer.ExposeFiles, present or future, without
// Miranda needing to know that tool's schema — including the sandbox's own
// download_file, which this same function covers now that tool's result
// carries a full file_uri rather than a bare id.
//
// Only a URL whose prefix matches endpoint.FilesURL exactly is ever treated
// as a file reference — this is the security boundary between "a trusted,
// explicitly opted-in server's own file link" and an arbitrary URL a
// compromised/malicious tool result could otherwise smuggle in for
// handleDownload to proxy (with that server's own bearer token attached) to
// wherever the attacker chose. See config.MCPServer.ExposeFiles.
func detectRemoteFileLinks(result string, endpoint config.FileServerEndpoint) []remoteFileLink {
	prefix := strings.TrimRight(endpoint.FilesURL, "/") + "/"
	// RFC 3986 unreserved characters plus "%" for percent-encoding, so a
	// file id containing e.g. an extension suffix or an encoded character
	// isn't truncated mid-id (a truncated RemoteURL would still get staged
	// and chipped, then 404/502 on click — see git history).
	pattern := regexp.MustCompile(regexp.QuoteMeta(prefix) + `[A-Za-z0-9._~%-]+`)

	var doc interface{}
	hasJSON := json.Unmarshal([]byte(result), &doc) == nil

	seen := make(map[string]bool)
	var links []remoteFileLink
	for _, u := range pattern.FindAllString(result, -1) {
		if seen[u] {
			continue
		}
		seen[u] = true
		link := remoteFileLink{url: u}
		if hasJSON {
			if obj := findSiblingObject(doc, u); obj != nil {
				link.filename = stringSiblingField(obj, filenameKeys)
				link.mimeType = stringSiblingField(obj, mimeTypeKeys)
				link.sizeBytes = int64SiblingField(obj, sizeBytesKeys)
			}
		} else {
			link.filename = keyValueStringField(result, filenameKeys)
			link.mimeType = keyValueStringField(result, mimeTypeKeys)
			link.sizeBytes = keyValueInt64Field(result, sizeBytesKeys)
		}
		links = append(links, link)
	}
	return links
}

// filenameKeys/mimeTypeKeys/sizeBytesKeys are the priority-ordered field
// names treated as describing a file — as JSON object keys for
// findSiblingObject's caller (e.g. medical_card_medical.get_document's
// "title"), or as "key: value" line prefixes for
// keyValueStringField/keyValueInt64Field (e.g. the sandbox's download_file
// "filename: ..."/"mime_type: ..."/"size_bytes: ..." lines — deliberately
// the same lists, since both shapes happen to use these exact field names).
// A miss on all of them leaves that piece of metadata unset rather than
// guessed wrong; the download chip (downloadChip in downloads.js) already
// degrades cleanly when filename/size are empty/zero.
var (
	filenameKeys  = []string{"title", "filename", "fileName", "name", "documentTitle"}
	mimeTypeKeys  = []string{"mime_type", "mimeType", "contentType", "content_type"}
	sizeBytesKeys = []string{"size_bytes", "sizeBytes", "fileSize", "size"}
)

// findSiblingObject recursively walks a generically decoded JSON value and
// returns the object (map) that holds target as one of its own string
// field values — e.g. for {"title":"x","fileUri":target}, returns that
// whole map so the caller can then read whichever other fields it wants
// out of it. Returns nil if target isn't found anywhere.
func findSiblingObject(node interface{}, target string) map[string]interface{} {
	switch v := node.(type) {
	case map[string]interface{}:
		for _, val := range v {
			if s, ok := val.(string); ok && s == target {
				return v
			}
		}
		for _, val := range v {
			if obj := findSiblingObject(val, target); obj != nil {
				return obj
			}
		}
	case []interface{}:
		for _, item := range v {
			if obj := findSiblingObject(item, target); obj != nil {
				return obj
			}
		}
	}
	return nil
}

// stringSiblingField returns the first non-empty string value found in obj
// under any of keys, in order, or "" if none match.
func stringSiblingField(obj map[string]interface{}, keys []string) string {
	for _, k := range keys {
		if s, ok := obj[k].(string); ok && s != "" {
			return s
		}
	}
	return ""
}

// int64SiblingField returns the first positive numeric value found in obj
// under any of keys, in order, or 0 if none match. JSON numbers decode as
// float64 via encoding/json's default interface{} unmarshaling.
func int64SiblingField(obj map[string]interface{}, keys []string) int64 {
	for _, k := range keys {
		if n, ok := obj[k].(float64); ok && n > 0 {
			return int64(n)
		}
	}
	return 0
}

// keyValueStringField is detectRemoteFileLinks' non-JSON counterpart to
// stringSiblingField: it scans result line by line for the first
// "key: value" line (colon-space separated, the shape
// miranda-code-execution-sandbox's download_file and similar plain-text
// tool results use) whose key matches any of keys, in priority order, and
// returns that value. Unlike stringSiblingField there's no JSON object to
// scope the search to, so this scans the whole result — fine in practice,
// since every known plain-text result of this shape describes exactly one
// file. Returns "" if no line matches.
func keyValueStringField(result string, keys []string) string {
	for _, line := range strings.Split(result, "\n") {
		key, value, found := strings.Cut(line, ": ")
		if !found || value == "" {
			continue
		}
		for _, k := range keys {
			if key == k {
				return value
			}
		}
	}
	return ""
}

// keyValueInt64Field is keyValueStringField's numeric counterpart, mirroring
// int64SiblingField: returns the first positive integer value found on a
// matching "key: value" line, or 0 if none match or parse.
func keyValueInt64Field(result string, keys []string) int64 {
	for _, line := range strings.Split(result, "\n") {
		key, value, found := strings.Cut(line, ": ")
		if !found {
			continue
		}
		for _, k := range keys {
			if key != k {
				continue
			}
			if n, err := strconv.ParseInt(value, 10, 64); err == nil && n > 0 {
				return n
			}
		}
	}
	return 0
}

// toDownloadRefs converts the turn's detected files to the form
// history.AppendAssistantMessage/InputResponse/ChatEvent carry them in —
// structurally separate from the reply's own text (see
// history.Message.Downloads for why). Replacing the earlier design (an
// in-band "\n\n<download>{json}</download>" marker appended to the reply
// text itself) is what actually closes the failure mode that design had:
// once such a marker had been written into a conversation's history, it
// came back as plain assistant text in every later turn's model-facing
// context, and a model was observed pattern-matching that exact tag shape
// back into a new reply of its own — filling it in with the wrong id (e.g.
// the sandbox's raw internal file_id rather than the attachments-store id)
// and producing a second, broken chip alongside the real one. A model can
// only mimic a shape it has actually seen; since a downloaded file is never
// represented as any kind of tag in message content anymore — not in what
// gets replayed as history, not even in the current turn's own reply before
// this function's caller attaches it — there is nothing left in text for it
// to copy. See git history for the incident and the two narrower
// text-marker-patching attempts (strip-on-append, then keep-latest-wins
// dedup) this replaced.
func toDownloadRefs(files []downloadedFile) []history.DownloadRef {
	if len(files) == 0 {
		return nil
	}
	refs := make([]history.DownloadRef, len(files))
	for i, f := range files {
		refs[i] = history.DownloadRef{FileID: f.fileID, Filename: f.filename, SizeBytes: f.sizeBytes, MIMEType: f.mimeType}
	}
	return refs
}

// appendDownloadFootnotes appends one human-readable "📎 filename (size)"
// line per downloaded file to text, for channels with no client-side chip
// renderer to hand structured data to instead. Only the web UI's chat
// screen renders a chip from InputResponse/ChatEvent's own Downloads field
// (see toDownloadRefs) — every other channel (ha_assist speaks/shows the
// reply text as-is, Telegram sends it verbatim via the Bot API) needs the
// file mentioned in the text itself or the user has no way to know it
// exists.
func appendDownloadFootnotes(text string, files []downloadedFile, source string) string {
	if source == webUISource {
		return text
	}
	for _, f := range files {
		if f.sizeBytes > 0 {
			text += fmt.Sprintf("\n\n📎 %s (%s)", f.filename, formatByteSize(f.sizeBytes))
		} else {
			text += fmt.Sprintf("\n\n📎 %s", f.filename)
		}
	}
	return text
}

// formatByteSize renders a byte count as a short human-readable string,
// mirroring the web UI's formatFileSize (internal/webui/static/js/downloads.js)
// for the plain-text fallback appendDownloadFootnotes produces.
func formatByteSize(n int64) string {
	switch {
	case n < 1024:
		return fmt.Sprintf("%d B", n)
	case n < 1024*1024:
		return fmt.Sprintf("%d KB", n/1024)
	default:
		return fmt.Sprintf("%.1f MB", float64(n)/(1024*1024))
	}
}

// resolveConversation either continues the user's currently open
// conversation (loading its prior turns so the model has context) or starts
// a new one. Continuity is server-owned and keyed only on userID — not on
// any conversation_id a caller might send — so the idle timeout and the
// explicit end_conversation/forget_conversation tools are what actually
// govern session boundaries, regardless of which channel (HA, web UI,
// future Telegram/mobile) a turn arrives on.
func (o *Orchestrator) resolveConversation(ctx context.Context, userID, source string) (string, []llm.Message, error) {
	open, err := o.history.OpenConversation(ctx, userID)
	if err != nil {
		return "", nil, fmt.Errorf("orchestrator: query open conversation: %w", err)
	}
	if open == nil {
		convID, err := o.history.StartConversation(ctx, userID, source)
		if err != nil {
			return "", nil, fmt.Errorf("orchestrator: start conversation: %w", err)
		}
		return convID, nil, nil
	}

	stored, err := o.history.ConversationMessages(ctx, open.ID)
	if err != nil {
		return "", nil, fmt.Errorf("orchestrator: load conversation %s: %w", open.ID, err)
	}

	// Prior turns are replayed in full, including tool calls and their
	// results (see history.Message.ToolCallID / ToolCalls, populated by
	// AppendAssistantMessage / AppendToolResultMessage) — not just the plain
	// user/assistant text, so the model resuming this conversation sees
	// exactly the same tool activity it saw when it originally ran.
	messages := make([]llm.Message, 0, len(stored))
	for _, m := range stored {
		switch m.Role {
		case "user":
			messages = append(messages, llm.Message{Role: llm.RoleUser, Content: m.Content})
		case "assistant":
			msg := llm.Message{Role: llm.RoleAssistant, Content: m.Content}
			for _, tc := range m.ToolCalls {
				msg.ToolCalls = append(msg.ToolCalls, llm.ToolCall{ID: tc.ID, Name: tc.Name, Arguments: tc.Arguments, ProviderMetadata: tc.ProviderMetadata})
			}
			messages = append(messages, msg)
		case "tool":
			messages = append(messages, llm.Message{Role: llm.RoleTool, ToolCallID: m.ToolCallID, Content: m.Content})
		}
	}
	return open.ID, messages, nil
}

// buildSystemPrompt combines the base persona prompt with who is currently
// speaking and long-term memory, so both are available on every turn without
// re-deriving them from raw history. sharedMemory is household-wide facts
// injected first (from shared.md); userMemory is the per-user file that
// follows. Without the speaker identity, the model has no way to tell which
// of the household it is talking to — it can only guess from what gets said,
// which is exactly the kind of thing that should never need guessing.
//
// Both the display name and the raw userID are spelled out explicitly. The
// display name alone isn't enough: several external MCP servers are
// multi-tenant and take a technical user/user_id argument that is the
// userID itself, not the display name — e.g. yazio's "user" on every
// nutrition tool, or miranda-diary's "user_id" on add_record/search/remove
// (the latter doesn't even validate it against a known list, so a wrong
// value doesn't error, it just silently starts a new, unsearchable bucket).
// The display name and userID only coincide by lexical accident (e.g. "Аня"
// vs. "anna") for some household members and not others (e.g. "Саша" vs.
// "archer" — no relation at all). Without the userID spelled out, the model
// has been observed defaulting to whichever username it saw most recently in
// context rather than the current speaker's, silently misattributing tool
// calls (e.g. food log entries) to the wrong account. Miranda's own built-in
// tools don't have this problem — they take userID from the server-side
// session, never as a model-supplied argument — this is only for
// external/MCP tools whose schema asks the model for one explicitly.
//
// The rest of the household is spelled out too, not just the speaker: some
// MCP tools take a *target* user id distinct from the caller — e.g.
// miranda-medical-card's medical.ask "subjectId" ("whose data the question
// is about, if not the caller's own"). Asked about a third party by name
// ("what's Sasha's cholesterol?"), the model has nothing but that name to
// go on if the mapping isn't in context, and has been observed inventing a
// plausible-looking userID instead of leaving the field unset or asking —
// e.g. subjectId: "sasha" for a household where the actual username is
// "archer" (full_name "Саша"). Listing every configured user's username
// and display name up front removes the need to guess.
//
// The return value is split into two pieces instead of one concatenated
// string — stable (persona, speaker identity, shared+personal memory) and
// volatile (the current time, which changes on essentially every turn — see
// its own comment below). Handle sends these as two separate
// llm.Message{Role: RoleSystem} entries, stable first: llm.RoleSystem's own
// doc comment (miranda-llm/llm.go) and anthropic.toAnthropicMessages both
// document the convention this depends on — a provider that caches the
// system prompt (currently just Anthropic; see
// docs/adr/system-prompt-caching.md) places its cache breakpoint on the
// FIRST system block specifically, so the stable block keeps getting reused
// across turns even though the volatile one, appended after it, differs
// every single time.
func (o *Orchestrator) buildSystemPrompt(userID, sharedMemory, userMemory string) (stable, volatile string) {
	stable = o.baseSystemPrompt
	if name := o.currentUserName(userID); name != "" {
		stable += "\n\nСейчас с тобой разговаривает: " + name + " (технический userID: \"" + userID + "\"). " +
			"Если вызываешь MCP-тул с параметром user или userId (yazio, diary и любой другой multi-tenant тул) — " +
			"передавай туда именно эту строку, \"" + userID + "\", а не имя " + name + " и не чьё-либо ещё имя из контекста."
	}
	if roster := o.householdRoster(); roster != "" {
		stable += "\n\nВсе пользователи Miranda в этом доме (userID — имя):\n" + roster +
			"\nЕсли тул просит указать другого человека, а не тебя самого (например, \"subjectId\" у medical.ask — " +
			"\"чьи это данные, если не самого вызывающего\"), используй userID из этого списка, а не имя " +
			"и не выдумывай значение сам. Если человек, о котором спрашивают, не входит в список — уточни у собеседника."
	}
	if sharedMemory != "" {
		stable += "\n\nShared household memory:\n" + sharedMemory
	}
	if userMemory != "" {
		stable += "\n\nWhat you remember about this user:\n" + userMemory
	}

	// The user's current local time is injected so the model can correctly
	// interpret relative time references ("в 22:00", "через 10 минут") and
	// generate proper RFC3339 timestamps for create_scheduled_task. Kept in
	// its own volatile block rather than folded into stable: time.Now()
	// changes on every call, so mixing it into the cacheable prefix would
	// defeat that cache's reuse on every single turn — see this function's
	// own doc comment above.
	now := time.Now().In(o.userLocation(userID))
	volatile = "Текущее время пользователя: " + now.Format("2006-01-02 15:04 MST") + "."

	return stable, volatile
}

// cachedMemory is one conversation's shared+personal memory snapshot, read
// once and reused for the rest of that conversation — see
// conversationMemory.
type cachedMemory struct {
	shared, personal string
}

// conversationMemory returns convID's shared+personal memory, reading from
// disk only the first time this conversation is seen and reusing that
// snapshot for every later turn of the same conversation. This is safe
// because a fact remember_this writes mid-conversation is already visible
// to the model through that tool call's own result message, already
// present in this conversation's own message history — re-reading memory
// on every turn would only ever pick up a write from a DIFFERENT,
// concurrently open conversation (see docs/adr/system-prompt-caching.md for
// why that staleness window — bounded by Memory.SessionIdleTimeoutMinutes —
// is an accepted tradeoff), at the cost of hitting disk, and defeating the
// stable system-prompt block's cacheability (see buildSystemPrompt), on
// every single turn instead.
//
// clearConversationMemory must be called once this conversation ends or is
// forgotten, or the cache would otherwise hold a stale snapshot forever if
// the same conversation ID were ever reused (it isn't, in practice — see
// history.StartConversation) and would leak one entry per conversation for
// the life of the process otherwise.
func (o *Orchestrator) conversationMemory(userID, convID string) (shared, personal string, err error) {
	o.memoryMu.Lock()
	if cached, ok := o.memoryCache[convID]; ok {
		o.memoryMu.Unlock()
		return cached.shared, cached.personal, nil
	}
	o.memoryMu.Unlock()

	shared, err = o.memory.ReadShared()
	if err != nil {
		return "", "", err
	}
	personal, err = o.memory.Read(userID)
	if err != nil {
		return "", "", err
	}

	o.memoryMu.Lock()
	o.memoryCache[convID] = cachedMemory{shared: shared, personal: personal}
	o.memoryMu.Unlock()
	return shared, personal, nil
}

// clearConversationMemory evicts convID's cached memory snapshot (see
// conversationMemory) — called once a conversation ends (idle sweep or
// explicit end_conversation, both via summarizeConversation) or is deleted
// (forget_conversation), so a later conversation for the same user always
// starts from a fresh disk read rather than an entry that would otherwise
// sit in the map forever.
func (o *Orchestrator) clearConversationMemory(convID string) {
	o.memoryMu.Lock()
	delete(o.memoryCache, convID)
	o.memoryMu.Unlock()
}

// currentUserName resolves userID to a human-readable display name via the
// users registry, if one is configured. Falls back to the bare userID when
// there's no registry, no match (e.g. the "debug" fallback id, or ad-hoc
// curl/testing), or an empty id — better to name the raw id than to say
// nothing about who's speaking.
func (o *Orchestrator) currentUserName(userID string) string {
	if o.users == nil || userID == "" {
		return userID
	}
	if u, ok := o.users.Get(userID); ok {
		return u.DisplayName()
	}
	return userID
}

// householdRoster renders every configured user as one "username — name"
// line per Registry.All (already sorted by username, so the block is
// identical across turns and doesn't defeat the stable block's prompt-cache
// reuse — see buildSystemPrompt). Returns "" when there's no registry or no
// configured users, so callers can skip the surrounding paragraph entirely
// instead of emitting an empty list.
func (o *Orchestrator) householdRoster() string {
	if o.users == nil {
		return ""
	}
	all := o.users.All()
	if len(all) == 0 {
		return ""
	}
	var b strings.Builder
	for _, u := range all {
		b.WriteString("- ")
		b.WriteString(u.Username)
		b.WriteString(" — ")
		b.WriteString(u.DisplayName())
		b.WriteString("\n")
	}
	return b.String()
}

// userLocation returns the configured IANA timezone for userID, or time.Local
// when the user is not in the registry or has no timezone configured. Used by
// the scheduler to interpret cron expressions and format timestamps.
func (o *Orchestrator) userLocation(userID string) *time.Location {
	if o.users != nil {
		if u, ok := o.users.Get(userID); ok {
			return u.Location()
		}
	}
	return time.Local
}

// availableTools combines every connected MCP server's tools with the
// agent's built-in tools (remember_this, etc.). The escalation tool is NOT
// added here: since each provider in the router's fallback/escalation chain
// can configure its own escalation target and tool name (see
// config.LLMProvider.Escalation), only the Router knows which one applies
// to whichever provider is active at a given hop — it appends that
// provider's own escalation ToolDef to this base list right before each
// Chat() call (see miranda-llm/router.requestFor), and intercepts calls to
// it transparently, so it never reaches executeTool.
//
// Built-ins are collected first (into the closure-captured `tools`/`names`
// pair below) and MCP tools are filtered against that set afterward, rather
// than the reverse, because MCP tool names come from a live server
// (internal/mcp.Manager.Tools, prefixed "<serverName>_<toolName>") and
// aren't known until connect time — an MCP server whose prefixed name
// happens to collide with one of Miranda's own fixed tool names (e.g. a
// server named "web" exposing a tool "search") is dropped, with a warning,
// rather than silently shadowing (or being shadowed by) a built-in of the
// same name. Sending two ToolDefs with the same name to a provider isn't
// just confusing — Anthropic specifically rejects the request outright.
//
// control gates which lazy MCP servers' (config.MCPServer.Lazy) real tool
// schemas are included: a lazy server not yet named in control.loadedGroups
// contributes nothing here except a one-line entry inside the shared
// load_tool_group stub (see loadToolGroupStub); once the model calls
// load_tool_group with that server's name, executeTool records it in
// control.loadedGroups and runAgentLoop calls this again to splice in that
// server's real tools for the rest of the turn. Called with a control whose
// loadedGroups is empty/nil on a turn's first iteration (from Handle) — see
// docs/adr/lazy-mcp-tool-loading.md.
func (o *Orchestrator) availableTools(ctx context.Context, userID string, control *turnControl) []llm.ToolDef {
	var tools []llm.ToolDef
	names := make(map[string]bool)
	add := func(t llm.ToolDef) {
		tools = append(tools, t)
		names[t.Name] = true
	}

	if o.memoryCfg.ExplicitTool {
		add(llm.ToolDef{
			Name: rememberToolName,
			Description: "Remember a durable fact for future conversations. " +
				"By default (scope=\"personal\") the fact is saved to the current user's private memory. " +
				"Set scope=\"shared\" to save to shared household memory visible to all users — " +
				"use this only when the fact belongs to the household, not to one person " +
				"(e.g. \"у нас живёт кот Барсик\", \"wifi пароль: ...\").",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"fact": map[string]any{"type": "string"},
					"scope": map[string]any{
						"type":        "string",
						"enum":        []string{"personal", "shared"},
						"description": "\"personal\" (default) writes to the current user's memory; \"shared\" writes to household-wide shared memory.",
					},
				},
				"required": []string{"fact"},
			},
		})
	}

	if o.memoryCfg.SearchHistoryTool {
		add(llm.ToolDef{
			Name: searchHistoryToolName,
			Description: "Search this user's past conversations for something they said earlier — use it when " +
				"they reference an earlier conversation (e.g. \"помнишь мы говорили о...\", \"remember when we talked about...\").",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"query": map[string]any{
						"type":        "string",
						"description": "keywords to search for, in the same language the user used",
					},
				},
				"required": []string{"query"},
			},
		})
	}

	if o.memoryCfg.EndConversationTool {
		add(llm.ToolDef{
			Name: endConversationToolName,
			Description: "End the current conversation right now — use when the user explicitly asks to start a " +
				"new conversation (e.g. \"давай начнём новую беседу\", \"let's start a new conversation\"), " +
				"instead of waiting for the idle timeout to close it.",
			Parameters: map[string]any{"type": "object", "properties": map[string]any{}},
		})
	}

	if o.memoryCfg.ForgetConversationTool {
		add(llm.ToolDef{
			Name: forgetConversationToolName,
			Description: "Delete this entire conversation with no memory of it — use when the user explicitly asks " +
				"to forget this conversation or start completely from scratch (e.g. \"забудь\", \"забудь этот диалог\", " +
				"\"давай с начала\").",
			Parameters: map[string]any{"type": "object", "properties": map[string]any{}},
		})
	}

	if o.ttsCfg.SpeakReplyTool {
		add(llm.ToolDef{
			Name: speakReplyToolName,
			Description: "Speak text out loud through the physical speaker, even though this request didn't arrive " +
				"via the voice pipeline — use only when the user explicitly asks to hear something read aloud " +
				"(e.g. \"озвучь это\", \"расскажи голосом\", \"скажи вслух\", \"read that out loud\"). Pass the text to speak — normally " +
				"the same as your written reply, but reworded speech-friendly (no markdown, links, code) if the " +
				"reply itself wouldn't sound natural read verbatim.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"text": map[string]any{"type": "string", "description": "the text to speak aloud"},
					"device": map[string]any{
						"type":        "string",
						"description": "friendly name of the speaker to use (e.g. \"Станция Мини 3 Про\") — omit to use the default device",
					},
				},
				"required": []string{"text"},
			},
		})
	}

	if o.tts != nil && o.ttsCfg.StopSpeechTool {
		add(llm.ToolDef{
			Name: stopSpeechToolName,
			Description: "Stop speaking immediately — use when the user explicitly asks Miranda to stop talking " +
				"(e.g. \"хватит\", \"замолчи\", \"stop talking\") — clears anything still queued and silences " +
				"whatever is currently playing on the physical speaker.",
			Parameters: map[string]any{"type": "object", "properties": map[string]any{}},
		})
	}

	if o.oauth != nil {
		add(llm.ToolDef{
			Name: oauthAuthorizeToolName,
			Description: "Start connecting a third-party account (e.g. Google Calendar) so its tools can act on the " +
				"current user's own data — call this when the user asks to connect/link/authorize a service, or when " +
				"a previous tool call failed because this user hasn't authorized it yet. Returns a link the user must " +
				"open and approve; the link is also proactively sent to their Telegram when known, since a spoken " +
				"reply can't usefully read a URL aloud.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"provider": map[string]any{
						"type":        "string",
						"enum":        o.oauth.ProviderNames(),
						"description": "which service to connect, e.g. \"google_calendar\"",
					},
				},
				"required": []string{"provider"},
			},
		})
	}

	for _, t := range o.webTools {
		add(t.Def())
	}

	if o.telegram != nil && o.telegramCfg.SendMessageTool {
		add(llm.ToolDef{
			Name: sendTelegramToolName,
			Description: "Send a text message to a household member's Telegram — use when the user explicitly asks " +
				"to send something to a phone (e.g. \"отправь мне на телефон ...\", \"send that to my phone\", " +
				"\"отправь Ане на телефон ...\"). Only works for someone who has messaged the bot at least once.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"text": map[string]any{
						"type":        "string",
						"description": "the message to send",
					},
					"recipient": map[string]any{
						"type": "string",
						"description": "the household member's name, exactly as the user said it (e.g. \"Аня\") — " +
							"omit this to send to whoever is currently talking to you",
					},
				},
				"required": []string{"text"},
			},
		})
	}

	if o.schedule != nil {
		add(llm.ToolDef{
			Name: createScheduledTaskToolName,
			Description: "Schedule a free-text instruction to be carried out later, either once or on a " +
				"recurring basis — use when the user explicitly asks to be reminded/have something done " +
				"at a future time (e.g. \"сегодня в 22:00 напомни мне...\", \"каждое утро в 9:01 ...\"). " +
				"The instruction is replayed through you later exactly like a live message from the user — " +
				"at that point you decide which of your own tools (speak_reply, send_telegram, etc.) to call " +
				"to actually carry it out, so write it as a clear, self-contained instruction, not a summary.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"task": map[string]any{
						"type":        "string",
						"description": "the instruction to carry out when this fires, written exactly as you'd want to receive it as a live message",
					},
					"run_at": map[string]any{
						"type":        "string",
						"description": "RFC3339 datetime for a one-off task — use the user's current local timezone offset shown in the system prompt (e.g. \"2026-07-30T22:00:00+03:00\") — provide exactly one of run_at or schedule, never both",
					},
					"schedule": map[string]any{
						"type":        "string",
						"description": "5-field cron expression (minute hour day-of-month month day-of-week) for a recurring task — times are interpreted in the user's local timezone, e.g. \"1 9 * * *\" for every day at 09:01 local time, or \"20 22 * * 2\" for every Tuesday at 22:20 local time — provide exactly one of run_at or schedule, never both",
					},
				},
				"required": []string{"task"},
			},
		})

		add(llm.ToolDef{
			Name:        listScheduledTasksToolName,
			Description: "List this user's currently scheduled tasks (id, next run time, and instruction) — use when the user asks what's scheduled, or before deleting one.",
			Parameters:  map[string]any{"type": "object", "properties": map[string]any{}},
		})

		add(llm.ToolDef{
			Name:        deleteScheduledTaskToolName,
			Description: "Cancel a scheduled task by id (from list_scheduled_tasks) — use when the user asks to cancel/remove a reminder or scheduled routine.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"id": map[string]any{"type": "string", "description": "the task id, from list_scheduled_tasks"},
				},
				"required": []string{"id"},
			},
		})
	}

	addMCP := func(t llm.ToolDef) {
		if names[t.Name] {
			o.hub.Publish(hub.Event{Source: "error", Message: fmt.Sprintf(
				"mcp tool %q collides with a built-in tool of the same name — dropping the mcp one", t.Name)})
			return
		}
		tools = append(tools, t)
		names[t.Name] = true
	}

	// pending collects every lazy server not yet loaded this turn — each
	// contributes only a one-line entry inside the shared load_tool_group
	// stub below, not its own real tool schemas, until the model asks for
	// it. A server absent from o.lazyServerDescriptions (the common case:
	// lazy loading unconfigured, or this particular server isn't lazy) is
	// unaffected and always included via ToolsExcluding.
	//
	// Every lazy server's name goes into skip regardless of loaded state —
	// a loaded one is added explicitly via ToolsForServer just below, so it
	// must NOT also come back through ToolsExcluding's own listing, or it
	// would be fetched (and ListTools-RPC'd) twice, with the second
	// occurrence of each tool tripping addMCP's same-name collision guard
	// and logging a spurious "collides with a built-in tool" error.
	var pending []string
	skip := make(map[string]bool, len(o.lazyServerDescriptions))
	for name := range o.lazyServerDescriptions {
		skip[name] = true
		if control.loadedGroups[name] {
			for _, t := range o.tools.ToolsForServerAndUser(ctx, name, userID) {
				addMCP(t)
			}
		} else {
			pending = append(pending, name)
		}
	}
	for _, t := range o.tools.ToolsExcludingForUser(ctx, skip, userID) {
		addMCP(t)
	}
	if len(pending) > 0 {
		addMCP(o.loadToolGroupStub(pending))
	}

	return tools
}

// loadToolGroupStub builds the single load_tool_group ToolDef standing in
// for every not-yet-loaded lazy MCP server in pending — one line per domain,
// drawn from that server's config Description, so the model can decide
// which (if any) is worth loading before it ever sees that domain's real
// tool schemas. See docs/adr/lazy-mcp-tool-loading.md §2.6.
func (o *Orchestrator) loadToolGroupStub(pending []string) llm.ToolDef {
	sort.Strings(pending) // deterministic order across calls — stable prompt for identical state
	var desc strings.Builder
	desc.WriteString("Load the real tools for one of these domains before calling anything in it — you currently only see this one-line summary, not their actual tool schemas:\n")
	for _, name := range pending {
		fmt.Fprintf(&desc, "- %s: %s\n", name, o.lazyServerDescriptions[name])
	}
	return llm.ToolDef{
		Name:        loadToolGroupToolName,
		Description: desc.String(),
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"group": map[string]any{"type": "string", "enum": pending},
			},
			"required": []string{"group"},
		},
	}
}

// runAgentLoop drives the model until it produces a final text-only reply:
// each iteration streams a response, executes any requested tool calls, and
// feeds their results back in for the next iteration. providerUsed is
// threaded through every iteration via streamOneTurn/router.ChatPinned so
// that once a turn escalates (e.g. gemini-lite hands off to gemini-strong),
// every later iteration of THIS loop stays pinned to whichever provider
// answered last — a tool call the escalated model requested must be
// answered by that same model once the tool result comes back, not silently
// downgraded back to the chain's default provider on the very next
// iteration just because the tool call happened in between.
func (o *Orchestrator) runAgentLoop(ctx context.Context, userID, conversationID, source string, messages []llm.Message, tools []llm.ToolDef, control *turnControl) (string, string, error) {
	var providerUsed string

	for i := 0; i < maxToolIterations; i++ {
		text, toolCalls, err := o.streamOneTurn(ctx, source, messages, tools, &providerUsed)
		if err != nil {
			return "", "", err
		}

		if len(toolCalls) == 0 {
			return text, providerUsed, nil
		}

		// Strip image Parts from all user messages now that the model has
		// seen them on this first successful call. Subsequent iterations
		// re-send the full accumulated message history, and re-transmitting
		// large base64 blobs on every tool-call round-trip wastes both
		// tokens and latency — the model's context already holds the image.
		for j := range messages {
			messages[j].Parts = nil
		}

		o.recordAssistantToolCallMessage(ctx, userID, conversationID, text, toolCalls)
		messages = append(messages, llm.Message{Role: llm.RoleAssistant, Content: text, ToolCalls: toolCalls})
		for _, tc := range toolCalls {
			result := o.executeTool(ctx, userID, conversationID, tc, control)
			o.recordToolCall(ctx, userID, conversationID, tc, result)
			messages = append(messages, llm.Message{Role: llm.RoleTool, ToolCallID: tc.ID, Content: result})
		}

		// A load_tool_group call this iteration means the next call's tool
		// list must grow to include that server's real schemas — see
		// availableTools' control param and docs/adr/lazy-mcp-tool-loading.md.
		// Skipped when nothing changed so an ordinary iteration doesn't pay
		// for rebuilding the list (which, unlike the built-ins half, may hit
		// every connected MCP server) on every single turn.
		if control.groupsChanged {
			tools = o.availableTools(ctx, userID, control)
			control.groupsChanged = false
		}
	}

	return "", "", fmt.Errorf("orchestrator: exceeded %d tool-call iterations without a final reply", maxToolIterations)
}

// streamOneTurn consumes one router stream: text deltas are pushed through a
// tts.Accumulator so complete sentences get spoken as soon as they're
// available, and tool calls are collected for the caller to execute.
// providerUsed is read before the call to pin the request to whichever
// provider answered the previous iteration of this same agent loop (see
// runAgentLoop and router.ChatPinned) — "" on the loop's first iteration,
// which is a no-op and behaves exactly like an unpinned router.Chat.
//
// Only ha_assist gets its streamed text spoken live here — that's the one
// channel where the model's plain reply text is itself the thing to say out
// loud. Every other source stays silent on this path no matter what the
// model's text says; the only way those sources ever reach the Yandex
// Station is the model explicitly calling speak_reply with the text to
// speak (see executeTool) — a real tool call with a real argument, not a
// flag inferred from *which* turn happened to contain the model's answer.
// That sidesteps an entire class of bugs an earlier version of this code
// had: content blocks stream in the order the model produced them (text,
// then tool_use), so "speak whatever text this turn already streamed"
// requires guessing, after the fact, whether a turn's text was the answer
// or just a throwaway post-tool-call remark — get that guess wrong and the
// same text can end up spoken twice. Passing the text as the tool's own
// argument removes the guess entirely.
func (o *Orchestrator) streamOneTurn(ctx context.Context, source string, messages []llm.Message, tools []llm.ToolDef, providerUsed *string) (string, []llm.ToolCall, error) {
	stream, err := o.router.ChatPinned(ctx, llm.ChatRequest{Messages: messages, Tools: tools}, *providerUsed, func(name string) { *providerUsed = name })
	if err != nil {
		return "", nil, fmt.Errorf("orchestrator: chat: %w", err)
	}

	speakLive := source == users.SourceHAAssist

	var fullText string
	var toolCalls []llm.ToolCall
	acc := tts.NewAccumulator(o.chunkMaxChars)

	for chunk := range stream {
		if chunk.Err != nil {
			return "", nil, fmt.Errorf("orchestrator: stream: %w", chunk.Err)
		}
		if chunk.TextDelta != "" {
			fullText += chunk.TextDelta
			ready := acc.Push(chunk.TextDelta)
			if speakLive {
				o.speakChunks(ctx, ready)
			} else {
				o.publishChunks(ready)
			}
		}
		if chunk.ToolCall != nil {
			toolCalls = append(toolCalls, *chunk.ToolCall)
		}
	}
	final := acc.Flush()
	if speakLive {
		o.speakChunks(ctx, final)
	} else {
		o.publishChunks(final)
	}

	return fullText, toolCalls, nil
}

// publishChunks puts each chunk on the hub as an "assistant" event (chat/log
// visibility) without any TTS decision — used for every source other than
// ha_assist; see streamOneTurn.
func (o *Orchestrator) publishChunks(chunks []string) {
	for _, chunk := range chunks {
		o.hub.Publish(hub.Event{Source: "assistant", Message: chunk})
	}
}

// speakChunks publishes each chunk (see publishChunks) and dispatches it to
// TTS — only ever called for ha_assist turns (see streamOneTurn), the one
// channel where the reply needs to come out of a physical speaker, separate
// from — and in addition to — the text reply HA's own pipeline may speak
// (see README).
func (o *Orchestrator) speakChunks(ctx context.Context, chunks []string) {
	o.publishChunks(chunks)
	for _, chunk := range chunks {
		o.speakText(ctx, chunk)
	}
}

// speakText enqueues one already-voice-approved piece of text onto
// tts.primary's background Player (via Dispatcher.Speak) — it returns
// immediately without waiting for synthesis or physical playback, and any
// eventual failure is published to the hub by the Player itself (Source:
// "tts"), not returned here, so a broken speaker shouldn't stop the
// assistant from answering.
func (o *Orchestrator) speakText(ctx context.Context, text string) {
	if o.tts == nil || text == "" {
		return
	}
	o.tts.Speak(ctx, text)
}

// executeTool runs one tool call: locally (remember_this, search_history,
// end_conversation, forget_conversation), via an internal/tools.Tool
// (tavily_web_search, tavily_web_fetch — see o.webTools), or via the MCP tool manager.
// Errors are turned into a result string rather than aborting the turn, so
// the model can see what went wrong and react (apologize, retry
// differently) instead of the whole request failing.
func (o *Orchestrator) executeTool(ctx context.Context, userID, conversationID string, tc llm.ToolCall, control *turnControl) string {
	if tc.Name == rememberToolName {
		var args struct {
			Fact  string `json:"fact"`
			Scope string `json:"scope"`
		}
		if err := json.Unmarshal([]byte(tc.Arguments), &args); err != nil {
			return fmt.Sprintf("error: invalid arguments: %v", err)
		}
		if args.Scope == "shared" {
			if err := o.memory.RememberShared(args.Fact); err != nil {
				return fmt.Sprintf("error: %v", err)
			}
			return "remembered in shared household memory"
		}
		if err := o.memory.Remember(userID, args.Fact); err != nil {
			return fmt.Sprintf("error: %v", err)
		}
		return "remembered"
	}

	if tc.Name == searchHistoryToolName {
		var args struct {
			Query string `json:"query"`
		}
		if err := json.Unmarshal([]byte(tc.Arguments), &args); err != nil {
			return fmt.Sprintf("error: invalid arguments: %v", err)
		}
		results, err := o.history.SearchConversations(ctx, userID, args.Query, searchHistoryLimit)
		if err != nil {
			return fmt.Sprintf("error: %v", err)
		}
		if len(results) == 0 {
			return "no matching past conversations found"
		}
		var b strings.Builder
		for _, c := range results {
			fmt.Fprintf(&b, "[%s] %s\n", c.StartedAt.Format("2006-01-02"), c.Summary)
		}
		return b.String()
	}

	if tc.Name == loadToolGroupToolName {
		var args struct {
			Group string `json:"group"`
		}
		if err := json.Unmarshal([]byte(tc.Arguments), &args); err != nil {
			return fmt.Sprintf("error: invalid arguments: %v", err)
		}
		if _, ok := o.lazyServerDescriptions[args.Group]; !ok {
			return fmt.Sprintf("error: unknown tool group %q", args.Group)
		}
		// An OAuth-gated group (config.MCPServer.OAuthProvider != "") has no
		// live per-user MCP session until EnsureUserSession brings one up —
		// unlike every other lazy group, whose real tools come from the one
		// already-connected global client. Without this, the model's first
		// load_tool_group call for such a group would report success while
		// the very next availableTools() call still saw zero real tools for
		// it — see docs/adr/oauth2-layer.md.
		if ext, ok := o.mcpExtensions[args.Group]; ok && ext.OAuthProvider != "" && o.oauth != nil {
			authorized, err := o.oauth.HasToken(ctx, userID, ext.OAuthProvider)
			if err != nil {
				return fmt.Sprintf("error: %v", err)
			}
			if !authorized {
				return fmt.Sprintf("the user hasn't connected %q yet — call %s with provider=%q first", args.Group, oauthAuthorizeToolName, ext.OAuthProvider)
			}
			o.tools.EnsureUserSession(args.Group, userID, o.oauthReconnect, o.oauthMaxReconnect, o.oauthConnectTimeout, o.oauthConnectFunc(ext, args.Group, userID))
			// Bounded wait (not unbounded — a scoped exception to "no
			// blocking I/O in executeTool", like Service.RefreshNow: at most
			// once per user per process restart, not a per-call cost) so the
			// very next availableTools() call in this same turn has a real
			// chance of seeing a live client instead of falsely reporting
			// success on a session that isn't up yet.
			if !waitForUserSession(ctx, o.tools, args.Group, userID, 5*time.Second) {
				return fmt.Sprintf("still connecting to %q — try again in a moment", args.Group)
			}
		}
		if control.loadedGroups == nil {
			control.loadedGroups = map[string]bool{}
		}
		control.loadedGroups[args.Group] = true
		control.groupsChanged = true
		return fmt.Sprintf("tools for %q are now available — call the specific tool you need now", args.Group)
	}

	if tc.Name == endConversationToolName {
		control.endRequested = true
		return "conversation ended"
	}

	if tc.Name == forgetConversationToolName {
		control.forgetRequested = true
		return "conversation forgotten"
	}

	if tc.Name == speakReplyToolName {
		var args struct {
			Text   string `json:"text"`
			Device string `json:"device"`
		}
		if err := json.Unmarshal([]byte(tc.Arguments), &args); err != nil {
			return fmt.Sprintf("error: invalid arguments: %v", err)
		}
		if args.Text == "" {
			return "error: text is required"
		}
		if args.Device != "" && o.tts != nil {
			o.tts.SpeakTo(ctx, args.Text, args.Device)
		} else {
			o.speakText(ctx, args.Text)
		}
		return "spoken"
	}

	if tc.Name == stopSpeechToolName {
		if o.tts != nil {
			o.tts.Stop()
		}
		return "stopped"
	}

	if tc.Name == sendTelegramToolName {
		var args struct {
			Text      string `json:"text"`
			Recipient string `json:"recipient"`
		}
		if err := json.Unmarshal([]byte(tc.Arguments), &args); err != nil {
			return fmt.Sprintf("error: invalid arguments: %v", err)
		}

		targetUsername := userID
		if args.Recipient != "" {
			if o.users == nil {
				return fmt.Sprintf("error: no household member matches %q", args.Recipient)
			}
			target, ok := o.users.ResolveByDisplayName(args.Recipient)
			if !ok {
				return fmt.Sprintf("error: no household member matches %q", args.Recipient)
			}
			targetUsername = target.Username
		}

		if err := o.telegram.SendToUser(ctx, targetUsername, args.Text); err != nil {
			return fmt.Sprintf("error: %v", err)
		}
		return "sent"
	}

	if tc.Name == oauthAuthorizeToolName {
		var args struct {
			Provider string `json:"provider"`
		}
		if err := json.Unmarshal([]byte(tc.Arguments), &args); err != nil {
			return fmt.Sprintf("error: invalid arguments: %v", err)
		}
		authorizeURL, err := o.oauth.StartAuthorization(ctx, userID, args.Provider)
		if err != nil {
			return fmt.Sprintf("error: %v", err)
		}
		// Best-effort, mirrors send_telegram's own "silently skip if no chat
		// id known" shape — a raw URL is useless spoken aloud (ha_assist), so
		// push it out-of-band regardless of which channel actually asked, in
		// addition to the reply text below (which works fine for web
		// UI/typed Telegram).
		if o.telegram != nil {
			_ = o.telegram.SendToUser(ctx, userID, "Follow this link to connect "+args.Provider+": "+authorizeURL)
		}
		return "Send the user this link to complete authorization: " + authorizeURL
	}

	if tc.Name == createScheduledTaskToolName {
		var args struct {
			Task     string `json:"task"`
			RunAt    string `json:"run_at"`
			Schedule string `json:"schedule"`
		}
		if err := json.Unmarshal([]byte(tc.Arguments), &args); err != nil {
			return fmt.Sprintf("error: invalid arguments: %v", err)
		}
		if args.Task == "" {
			return "error: task is required"
		}
		if (args.RunAt == "") == (args.Schedule == "") {
			return "error: provide exactly one of run_at or schedule"
		}

		task := schedule.Task{UserID: userID, Prompt: args.Task}
		if args.Schedule != "" {
			sched, err := cron.ParseStandard(args.Schedule)
			if err != nil {
				return fmt.Sprintf("error: invalid schedule expression: %v", err)
			}
			// Interpret the cron expression in the user's local timezone so
			// "1 9 * * *" means 09:01 in the user's time, not the server's.
			if specSched, ok := sched.(*cron.SpecSchedule); ok {
				specSched.Location = o.userLocation(userID)
			}
			task.CronExpr = args.Schedule
			task.NextRunAt = sched.Next(time.Now())
		} else {
			runAt, err := time.Parse(time.RFC3339, args.RunAt)
			if err != nil {
				return fmt.Sprintf("error: invalid run_at (expected RFC3339): %v", err)
			}
			if !runAt.After(time.Now()) {
				return "error: run_at is in the past"
			}
			task.RunAt = &runAt
			task.NextRunAt = runAt
		}

		id, err := o.schedule.Create(ctx, task)
		if err != nil {
			return fmt.Sprintf("error: %v", err)
		}
		return "scheduled: " + id
	}

	if tc.Name == listScheduledTasksToolName {
		tasksList, err := o.schedule.ListForUser(ctx, userID)
		if err != nil {
			return fmt.Sprintf("error: %v", err)
		}
		if len(tasksList) == 0 {
			return "no scheduled tasks"
		}
		userLoc := o.userLocation(userID)
		var b strings.Builder
		for _, t := range tasksList {
			fmt.Fprintf(&b, "[%s] next: %s — %s\n", t.ID, t.NextRunAt.In(userLoc).Format(time.RFC3339), t.Prompt)
		}
		return b.String()
	}

	if tc.Name == deleteScheduledTaskToolName {
		var args struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal([]byte(tc.Arguments), &args); err != nil {
			return fmt.Sprintf("error: invalid arguments: %v", err)
		}
		if err := o.schedule.Delete(ctx, args.ID, userID); err != nil {
			if err == schedule.ErrNotFound {
				return "error: no such scheduled task"
			}
			return fmt.Sprintf("error: %v", err)
		}
		return "deleted"
	}

	for _, t := range o.webTools {
		if t.Def().Name != tc.Name {
			continue
		}
		result, err := t.Call(ctx, tc.Arguments)
		if err != nil {
			return fmt.Sprintf("error: %v", err)
		}
		return result
	}

	// For alice MCP tool calls that carry a "device" argument (the target
	// speaker's friendly name), coordinate with the TTS Player before firing:
	// drain any queued TTS first (WaitIdle), then confirm the physical station
	// is idle via alice_state polling (WaitEntityIdle). This prevents a model
	// that batches speak_reply + an alice command from having the alice command
	// interrupt the TTS mid-utterance. Both steps are best-effort: errors are
	// published to the hub but never abort the tool call.
	if o.tts != nil && o.speakerHA != nil {
		if device := aliceToolDevice(tc.Arguments); device != "" {
			o.tts.WaitIdle(ctx)
			if entityID, err := o.speakerHA.ResolveMediaPlayer(ctx, device); err == nil {
				if err := o.tts.WaitEntityIdle(ctx, entityID); err != nil && ctx.Err() == nil {
					o.hub.Publish(hub.Event{Source: "error", Message: "speaker coordination: " + err.Error()})
				}
			}
		}
	}

	// Build the wire payload as a local variable, never overwriting
	// tc.Arguments itself — runAgentLoop already called
	// recordAssistantToolCallMessage with the model's original, unmutated
	// toolCalls before this iteration's executeTool call, and calls
	// recordToolCall with that same outer, unmutated tc right after this
	// call returns (Go passes tc into executeTool by value, so a local
	// mutation here is invisible to the caller's copy either way). Keeping
	// the mutation local-only is what keeps a real key from ever reaching
	// history's SQLite tables or miranda-llm/llmtrace's llm.log. See
	// docs/encryption.md.

	// Resolved once and reused by every block below that needs to know
	// tc.Name's owning server (and, where relevant, its bare tool name, or
	// that server's bundle of opted-in behaviors) instead of each
	// re-running the same prefix-matching scan over o.tools' server list.
	toolServer, toolName, toolServerOK := o.tools.ServerAndTool(tc.Name)
	var toolExt MCPServerExtension
	if toolServerOK {
		toolExt = o.mcpExtensions[toolServer]
	}

	callArgs := tc.Arguments
	if o.keyring != nil {
		// key stays nil unless tc.Name targets a whitelisted server AND
		// userID's key is currently unlocked — setEncryptionKeyArg treats a
		// nil key as "strip", which also covers the not-whitelisted case:
		// a compromised/malicious MCP tool description must never be able
		// to trick the model into forwarding a previously-seen key value to
		// a different, non-whitelisted server. argName follows the same
		// resolution: it's only switched to the target server's own
		// configured EncryptionKeyArg() when that server is whitelisted, so
		// the strip path for a non-whitelisted call always falls back to
		// the package default name below.
		var key []byte
		argName := defaultEncryptionKeyArgName
		if toolExt.EncryptionKeyArg != "" {
			argName = toolExt.EncryptionKeyArg
			key, _ = o.keyring.Get(userID)
		}
		var decodeOK bool
		callArgs, decodeOK = setEncryptionKeyArg(tc.Arguments, argName, key)
		if !decodeOK && len(key) > 0 {
			o.hub.Publish(hub.Event{Source: "error", Message: "keyring: tool call arguments were not valid JSON, encryption key not attached to " + tc.Name})
		}
	}

	// Session-id injection is independent of the keyring block above — it
	// operates on callArgs' current value (already keyring-adjusted, if
	// applicable) rather than tc.Arguments, so the two compose for a tool
	// call that happens to be whitelisted for both. Config-driven and
	// generic across any MCP server/tool pair (o.sessionIDAllowed, built by
	// cmd/miranda from every config.MCPServer.SessionIDTools), not specific
	// to any one server — see docs/medical-card-session-injection.md.
	if toolExt.SessionIDArg != "" && toolExt.SessionIDTools[toolName] {
		var setOK bool
		callArgs, setOK = setSessionIDArg(callArgs, toolExt.SessionIDArg, conversationID)
		if !setOK {
			o.hub.Publish(hub.Event{Source: "error", Message: "session id not attached to " + tc.Name + ": arguments were not valid JSON"})
		}
	}

	// An OAuth-gated server routes to userID's own per-user MCP session
	// (lazily brought up here if this is the first call this process has
	// made for this user) rather than the one globally-shared connection
	// every other server uses — see docs/adr/oauth2-layer.md for why a
	// shared connection with a swapped bearer token isn't safe here.
	var result string
	var err error
	if toolServerOK && toolExt.OAuthProvider != "" && o.oauth != nil {
		o.tools.EnsureUserSession(toolServer, userID, o.oauthReconnect, o.oauthMaxReconnect, o.oauthConnectTimeout, o.oauthConnectFunc(toolExt, toolServer, userID))
		result, err = o.tools.CallForUser(ctx, tc.Name, callArgs, userID)
	} else {
		result, err = o.tools.Call(ctx, tc.Name, callArgs)
	}
	if err != nil {
		if o.logger != nil {
			o.logger.Warn("mcp: tool call failed", "tool", tc.Name, "server", toolServer, "user", userID, "error", err)
		}
		return fmt.Sprintf("error: %v", err)
	}

	// Every file-exposing MCP server (config.MCPServer.ExposeFiles,
	// including the sandbox — it has no dedicated path of its own) goes
	// through this one detector: scan the raw result for a URL rooted at
	// that server's own FilesEndpoint (e.g. miranda-medical-card's
	// medical.get_document returning a "fileUri"), regardless of which
	// tool produced it, stage a download record, and queue it to be
	// attached to the turn's final reply as structured data (see
	// history.Message.Downloads).
	if toolExt.FilesEndpoint != nil {
		endpoint := *toolExt.FilesEndpoint
		for _, link := range detectRemoteFileLinks(result, endpoint) {
			if control.hasRemoteFile(link.url) {
				continue
			}
			control.recordRemoteFile(link.url)
			fileID, err := attachments.NewFileID()
			if err != nil {
				continue
			}
			control.recordDownloadedFile(downloadedFile{
				fileID: fileID, filename: link.filename, sizeBytes: link.sizeBytes, mimeType: link.mimeType,
			})
			if o.attachStore != nil {
				o.attachStore.Put(attachments.Record{
					UserID:      userID,
					FileID:      fileID,
					Filename:    link.filename,
					MIMEType:    link.mimeType,
					Size:        link.sizeBytes,
					RemoteURL:   link.url,
					RemoteToken: endpoint.Token,
					TTL:         o.downloadRecordTTL,
				})
			}
		}
	}

	return result
}

// oauthConnectFunc builds the per-user mcp.Connect closure EnsureUserSession
// retries on its own backoff schedule — pulls a currently-valid access
// token fresh from the in-memory cache on every invocation (never a
// captured/stale value, so a rotated/refreshed token is always picked up on
// the very next reconnect attempt), falling back to a synchronous
// RefreshNow only here, inside a background goroutine, never on
// executeTool's own call path. serverName is passed explicitly rather than
// read off ext, since MCPServerExtension is keyed by server name in the map
// the caller already has (toolServer/args.Group) and carries no back
// reference to its own key.
func (o *Orchestrator) oauthConnectFunc(ext MCPServerExtension, serverName, userID string) func(ctx context.Context) (mcp.Client, error) {
	return func(ctx context.Context) (mcp.Client, error) {
		token, ok := o.oauth.AccessToken(userID, ext.OAuthProvider)
		if !ok {
			var err error
			token, ok, err = o.oauth.RefreshNow(ctx, userID, ext.OAuthProvider)
			if err != nil {
				return nil, fmt.Errorf("oauth2: refresh failed for %s/%s: %w", userID, ext.OAuthProvider, err)
			}
			if !ok {
				return nil, fmt.Errorf("oauth2: %s has not authorized %s yet", userID, ext.OAuthProvider)
			}
		}
		return mcp.Connect(ctx, serverName, ext.MCPServerURL, token)
	}
}

// waitForUserSession polls mgr.HasUserClient(name, userID) until it's true
// or timeout elapses — used by executeTool's load_tool_group handling to
// give a freshly-started EnsureUserSession connect attempt a real chance to
// land before the model's very next tool-listing call, see its own call
// site for why this bounded wait is an acceptable, scoped exception to
// executeTool otherwise never blocking on network I/O.
func waitForUserSession(ctx context.Context, mgr *mcp.Manager, name, userID string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		if mgr.HasUserClient(name, userID) {
			return true
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			return false
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// aliceToolDevice extracts the "device" string field from a tool call's JSON
// arguments, returning "" if the field is absent, not a string, or the JSON
// is malformed. Used by executeTool to detect alice MCP tool calls that
// target a speaker by friendly name.
func aliceToolDevice(args string) string {
	var m struct {
		Device string `json:"device"`
	}
	if err := json.Unmarshal([]byte(args), &m); err != nil {
		return ""
	}
	return m.Device
}

// defaultEncryptionKeyArgName is the tool-call argument name used to strip
// a model-supplied key field from a call to a server that isn't (or isn't
// currently known to be) whitelisted — see executeTool's argName
// resolution. A whitelisted server's own injected argument name instead
// comes from that server's config.MCPServer.EncryptionKeyArg(), which may
// override this default (e.g. the external miranda-diary repo's tools use
// "record_encryption_key", not this default).
const defaultEncryptionKeyArgName = "encryption_key"

// setEncryptionKeyArg sets or strips argName in args' JSON. A non-empty key
// sets it (lowercase hex-encoded, 64 chars for a 32-byte key — the encoding
// the external miranda-diary repo's schema requires, confirmed against its
// own parseEncryptionKey; the same real-world incident that necessitated
// the per-server EncryptionKeyArg() name override also surfaced this: an
// earlier version of this function base64-encoded instead, which a diary
// account with encryption disabled silently accepted (the raw value is
// wholly ignored server-side when disabled) until encryption was actually
// turned on for that account and every call started failing "must be 64
// lowercase hex characters"), overwriting any value the model may have set
// itself. A nil/empty key strips it instead — defense against a
// compromised/malicious MCP tool description tricking the model into
// forwarding a previously-seen key value somewhere it shouldn't go.
//
// Always decodes args (no substring pre-check): JSON allows any character,
// including every letter of argName, to be written as a \uXXXX escape, so a
// literal-substring fast path can be silently dodged by a tool call whose
// key field is spelled with an escape — exactly the prompt-injection
// scenario this function exists to defend against. The post-decode "field
// not present" check below is what actually avoids a wasted re-encode for
// the overwhelmingly common case (any non-whitelisted or locked-key call),
// safely, since it runs after the field has genuinely been looked for in
// the parsed object rather than guessed at from raw text.
//
// ok is false only when args itself isn't valid JSON — the caller should
// treat that as "the key was not attached" and log it when key was
// non-empty, since it means an intended injection silently didn't happen
// (a fully malformed tool call is rare and will fail against the MCP server
// on its own terms regardless, but a merely-unparseable-here edge case
// deserves a trace, not silence).
func setEncryptionKeyArg(args, argName string, key []byte) (result string, ok bool) {
	m, decodeOK := decodeToolArgs(args)
	if !decodeOK {
		return args, false
	}
	if len(key) == 0 {
		if _, present := m[argName]; !present {
			return args, true
		}
		delete(m, argName)
	} else {
		m[argName] = hex.EncodeToString(key)
	}
	return encodeToolArgs(m, args), true
}

// setSessionIDArg sets argName to sessionID in args' JSON, unconditionally
// overwriting any value the model may have set itself — see
// docs/medical-card-session-injection.md §1 for why the model must never be
// trusted to supply this value on its own. Unlike setEncryptionKeyArg, there
// is no "strip" branch: sessionID is conversationID, which executeTool's
// caller (runAgentLoop) only ever receives already resolved (see
// resolveConversation), so it is never empty here.
func setSessionIDArg(args, argName, sessionID string) (result string, ok bool) {
	m, decodeOK := decodeToolArgs(args)
	if !decodeOK {
		return args, false
	}
	m[argName] = sessionID
	return encodeToolArgs(m, args), true
}

func decodeToolArgs(args string) (map[string]any, bool) {
	var m map[string]any
	if err := json.Unmarshal([]byte(args), &m); err != nil {
		return nil, false
	}
	return m, true
}

// encodeToolArgs marshals m back to JSON, falling back to the original
// (pre-decode) args string on a marshal error — this should be unreachable
// since m was itself produced by unmarshaling valid JSON, but never worth
// silently losing a tool call's arguments over.
func encodeToolArgs(m map[string]any, original string) string {
	b, err := json.Marshal(m)
	if err != nil {
		return original
	}
	return string(b)
}

// recordAssistantToolCallMessage persists the assistant's turn that
// requested toolCalls (text may be empty when the model replies with only
// tool calls), so resolveConversation can replay it on a later turn instead
// of dropping it. Best-effort: a failure here is logged but must not abort
// the turn — the caller already has the tool calls in hand to execute.
func (o *Orchestrator) recordAssistantToolCallMessage(ctx context.Context, userID, conversationID, text string, toolCalls []llm.ToolCall) {
	refs := make([]history.ToolCallRef, len(toolCalls))
	for i, tc := range toolCalls {
		refs[i] = history.ToolCallRef{ID: tc.ID, Name: tc.Name, Arguments: tc.Arguments, ProviderMetadata: tc.ProviderMetadata}
	}
	// nil downloads: a mid-loop tool-calling turn never carries a file
	// reference — those are only ever flushed once, on the turn's final
	// reply (Handle, after runAgentLoop returns) — see history.Message.Downloads.
	msgID, err := o.history.AppendAssistantMessage(ctx, conversationID, text, refs, nil)
	if err != nil {
		o.hub.Publish(hub.Event{Source: "error", Message: "record assistant tool-call message: " + err.Error()})
		return
	}
	// Published even though the chat UI doesn't render tool-call turns today
	// (see chat.js's chatMessages filter) — sent so a future debug view can
	// show tool activity live without any backend change, matching how
	// GET /api/dialogs/{id} already returns these rows unfiltered.
	o.publishChatMessage(userID, conversationID, history.Message{ID: msgID, ConversationID: conversationID, Role: "assistant", Content: text, ToolCalls: refs})
}

func (o *Orchestrator) recordToolCall(ctx context.Context, userID, conversationID string, tc llm.ToolCall, result string) {
	msgID, err := o.history.AppendToolResultMessage(ctx, conversationID, tc.ID, result)
	if err != nil {
		o.hub.Publish(hub.Event{Source: "error", Message: "record tool call: " + err.Error()})
		return
	}
	if err := o.history.AppendToolCall(ctx, msgID, tc.Name, "", tc.Arguments, result); err != nil {
		o.hub.Publish(hub.Event{Source: "error", Message: "record tool call detail: " + err.Error()})
	}
	o.publishChatMessage(userID, conversationID, history.Message{ID: msgID, ConversationID: conversationID, Role: "tool", Content: result, ToolCallID: tc.ID})
}
