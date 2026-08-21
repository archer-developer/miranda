package agentloop

import (
	"context"
	"fmt"
	"strings"
	"time"

	llm "github.com/archer-developer/miranda-llm"
)

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

// turnFailureReplies is the one hardcoded, non-LLM-generated reply text
// Miranda ever sends: what a user gets instead of total silence when
// runAgentLoop itself errors out (provider outage, mid-stream failure, or
// the turn's own deadline expiring — see Handle's call site). Keyed the
// same way as config.UserConfig.Language/the web UI's own locale files
// ("ru", "be", "en") since that's the only per-user language signal
// Miranda already tracks; there's no broader backend i18n system to hook
// into for a single fixed string like this one.
var turnFailureReplies = map[string]string{
	"ru": "Не получилось ответить — что-то пошло не так. Попробуй ещё раз.",
	"be": "Не атрымалася адказаць — нешта пайшло не так. Паспрабуй яшчэ раз.",
	"en": "Something went wrong and I couldn't finish that — please try again.",
}

// turnFailureReply returns the fallback reply text for userID's configured
// language, falling back to "ru" (config.Default's own DefaultLanguage) when
// there's no registry, no match, or an unrecognized language code — the same
// "better to fall back sensibly than say nothing" reasoning as userLocation.
func (o *Orchestrator) turnFailureReply(userID string) string {
	lang := "ru"
	if o.users != nil {
		if u, ok := o.users.Get(userID); ok && u.Language != "" {
			lang = u.Language
		}
	}
	if s, ok := turnFailureReplies[lang]; ok {
		return s
	}
	return turnFailureReplies["ru"]
}
