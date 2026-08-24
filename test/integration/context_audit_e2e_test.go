//go:build context_audit_e2e

// This file is excluded from the normal `go test ./...` sweep (build tag
// context_audit_e2e) — it loads the real config/*.yaml, makes one real call
// to a configured LLM provider (needs a real API key), and best-effort
// connects to whatever MCP servers are configured. Run on demand:
//
//	go test -tags context_audit_e2e ./test/integration/... -run TestContextAudit -v
//
// What it audits: the exact llm.ChatRequest (system prompt + tool schemas +
// message history) the agent loop assembles for the FIRST provider call of
// a brand-new turn from user "debug" — i.e. the context every turn starts
// paying for, before any tool-call iteration grows it further. It asserts
// hard ceilings on tool count and context size, and dumps the full request
// (every iteration of the turn, not just the first) plus the reply to a
// JSON file under logs/context_audit for manual review.
//
// Known gaps vs. the real deployment (cmd/miranda's run()): Telegram,
// OAuth, attachments, and a real TTS dispatcher are NOT wired up here, so
// oauth_authorize/send_telegram/stop_speech (and any OAuth-gated MCP
// server) won't appear in the dump even if enabled in the real config —
// wiring them needs a webhook, a master key, and a real speaker
// respectively, which this on-demand audit deliberately avoids. Everything
// else (built-in memory/schedule/speak_reply tools, Tavily web tools, and
// every non-OAuth MCP server reachable within a few seconds) is real.
//
// Missing config/*.yaml, no llm.providers, or a failing provider build
// (e.g. missing API key) cause a clean t.Skip, not a failure — same posture
// as calendar_e2e_test.go.
package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	anthropicsdk "github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/anthropics/anthropic-sdk-go/packages/param"
	"google.golang.org/genai"

	llm "github.com/archer-developer/miranda-llm"
	"github.com/archer-developer/miranda-llm/anthropic"
	"github.com/archer-developer/miranda-llm/gemini"
	"github.com/archer-developer/miranda-llm/openaicompat"
	"github.com/archer-developer/miranda-llm/router"
	agentloop "github.com/archer-developer/miranda/internal/agent_loop"
	"github.com/archer-developer/miranda/internal/config"
	"github.com/archer-developer/miranda/internal/envfile"
	"github.com/archer-developer/miranda/internal/history"
	"github.com/archer-developer/miranda/internal/hub"
	"github.com/archer-developer/miranda/internal/mcp"
	"github.com/archer-developer/miranda/internal/memory"
	"github.com/archer-developer/miranda/internal/schedule"
	"github.com/archer-developer/miranda/internal/tavily"
	"github.com/archer-developer/miranda/internal/tools"
)

const (
	contextAuditUserID = "debug"

	// Hard ceilings for the FIRST request of a turn — the context every
	// single turn pays for regardless of how the conversation unfolds
	// afterward. These are deliberately well under the model's real context
	// window (Haiku 4.5: 200K tokens; see internal/config/CLAUDE.md and the
	// project's LLM config) — the risk this test guards against is silent
	// bloat hurting latency/cost on a voice assistant, not hitting the
	// model's actual ceiling.
	maxToolCount           = 40
	maxContextTokens       = 12000 // real input-token count from the provider's own count-tokens API — see realTokenCount
	mcpConnectAuditTimeout = 5 * time.Second
)

// envDefault returns the environment variable named key, or fallback if
// unset/empty. Named distinctly from calendar_e2e_test.go's envOrDefault
// since both files can in principle be compiled together (-tags "calendar_e2e context_audit_e2e").
func envDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// recordedCall is one captured llm.Provider.Chat invocation: the exact
// request the caller built, and what came back.
type recordedCall struct {
	ProviderName string          `json:"provider"`
	Timestamp    time.Time       `json:"timestamp"`
	Request      llm.ChatRequest `json:"request"`
	ResponseText string          `json:"response_text,omitempty"`
	ToolCall     *llm.ToolCall   `json:"response_tool_call,omitempty"`
	Err          string          `json:"error,omitempty"`
}

// callRecorder collects every recordedCall across every wrapped provider,
// in call order — router.Router tries providers sequentially (fallback/
// escalation), never concurrently, but the mutex keeps this safe regardless.
type callRecorder struct {
	mu    sync.Mutex
	calls []recordedCall
}

func (c *callRecorder) add(call recordedCall) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = append(c.calls, call)
}

// recordingProvider wraps a real llm.Provider and transparently tees every
// Chat request/response through a shared callRecorder, so the test can
// inspect exactly what was sent to a REAL provider (not a scripted fake)
// without needing router.SetTracer wiring.
type recordingProvider struct {
	inner llm.Provider
	rec   *callRecorder
}

func (r *recordingProvider) Name() string { return r.inner.Name() }

func (r *recordingProvider) Chat(ctx context.Context, req llm.ChatRequest) (<-chan llm.StreamChunk, error) {
	ch, err := r.inner.Chat(ctx, req)
	if err != nil {
		r.rec.add(recordedCall{ProviderName: r.inner.Name(), Timestamp: time.Now(), Request: req, Err: err.Error()})
		return ch, err
	}
	out := make(chan llm.StreamChunk)
	go func() {
		defer close(out)
		var text strings.Builder
		var toolCall *llm.ToolCall
		var callErr error
		for chunk := range ch {
			if chunk.TextDelta != "" {
				text.WriteString(chunk.TextDelta)
			}
			if chunk.ToolCall != nil {
				toolCall = chunk.ToolCall
			}
			if chunk.Err != nil {
				callErr = chunk.Err
			}
			out <- chunk
		}
		entry := recordedCall{ProviderName: r.inner.Name(), Timestamp: time.Now(), Request: req, ResponseText: text.String(), ToolCall: toolCall}
		if callErr != nil {
			entry.Err = callErr.Error()
		}
		r.rec.add(entry)
	}()
	return out, nil
}

// buildRecordingProviders mirrors cmd/miranda.buildProviders but wraps each
// constructed provider in a recordingProvider sharing rec, so every real
// call this test drives is captured.
func buildRecordingProviders(ctx context.Context, configs []config.LLMProvider, logger *slog.Logger, rec *callRecorder) ([]llm.Provider, error) {
	var providers []llm.Provider
	for _, c := range configs {
		var p llm.Provider
		switch c.Type {
		case "anthropic":
			p = anthropic.New(c.Name, c.Model, firstAuditAPIKey(c.APIKeyEnvs), anthropic.ToolsConfig(c.AnthropicTools))
		case "openai_compat":
			p = openaicompat.New(c.Name, c.BaseURL, c.Model, firstAuditAPIKey(c.APIKeyEnvs))
		case "gemini":
			gp, err := gemini.New(ctx, c.Name, c.Model, c.APIKeyEnvs, gemini.ToolsConfig(c.GeminiTools), gemini.RotationConfig(c.GeminiRotation), logger)
			if err != nil {
				return nil, fmt.Errorf("build gemini provider %q: %w", c.Name, err)
			}
			p = gp
		default:
			return nil, fmt.Errorf("unknown llm provider type %q for provider %q", c.Type, c.Name)
		}
		providers = append(providers, &recordingProvider{inner: p, rec: rec})
	}
	if len(providers) == 0 {
		return nil, fmt.Errorf("no llm.providers configured")
	}
	return providers, nil
}

func firstAuditAPIKey(envs []string) string {
	for _, e := range envs {
		if v := os.Getenv(e); v != "" {
			return v
		}
	}
	return ""
}

// buildAuditEscalations mirrors cmd/miranda.buildEscalations (unexported
// there, so duplicated here).
func buildAuditEscalations(configs []config.LLMProvider) map[string]router.EscalationConfig {
	m := make(map[string]router.EscalationConfig, len(configs))
	for _, c := range configs {
		m[c.Name] = router.EscalationConfig(c.Escalation)
	}
	return m
}

// buildAuditWebTools mirrors cmd/miranda.buildWebTools.
func buildAuditWebTools(cfg config.TavilyConfig, logger *slog.Logger) ([]tools.Tool, error) {
	if !cfg.WebSearch.Enabled && !cfg.WebFetch.Enabled {
		return nil, nil
	}
	apiKey := os.Getenv(cfg.APIKeyEnv)
	if apiKey == "" {
		return nil, fmt.Errorf("tavily.web_search/web_fetch enabled but %s is not set", cfg.APIKeyEnv)
	}
	client := tavily.New(apiKey)
	var out []tools.Tool
	if cfg.WebSearch.Enabled {
		out = append(out, tools.NewWebSearch(client, cfg.WebSearch, logger))
	}
	if cfg.WebFetch.Enabled {
		out = append(out, tools.NewWebFetch(client, logger))
	}
	return out, nil
}

// realTokenCount returns the ACTUAL input-token count for req, as reported
// by that provider's own count-tokens endpoint — not a character/byte
// approximation. Byte length is a poor proxy for token cost on non-Latin
// scripts: BPE/SentencePiece vocabularies are trained mostly on
// English/Latin corpora, so Cyrillic text (this project's system prompt is
// almost entirely Russian) tokenizes far less efficiently — often closer to
// 1.5-2.5 characters per token instead of English's ~4. Guessing at a
// char-count threshold either over- or under-estimates real cost depending
// on language mix; asking the provider directly removes the guesswork.
//
// Only "anthropic" and "gemini" are supported (the two provider types this
// project's config actually uses — see config/llm.yaml). "openai_compat"
// has no standardized token-counting endpoint across arbitrary
// OpenAI-compatible backends, so ok is false and the caller should treat
// the token-based assertion as not applicable rather than failing on it.
func realTokenCount(ctx context.Context, providerType, apiKey, model string, req llm.ChatRequest) (tokens int64, ok bool, err error) {
	switch providerType {
	case "anthropic":
		n, err := anthropicCountTokens(ctx, apiKey, model, req)
		return n, true, err
	case "gemini":
		n, err := geminiCountTokens(ctx, apiKey, model, req)
		return n, true, err
	default:
		return 0, false, nil
	}
}

// toolNameByCallID walks messages once and returns every assistant
// tool-call's ID mapped to its Name, so a later RoleTool message (which
// only carries ToolCallID, not the tool's name — see llm.Message's own doc
// comment) can be labeled correctly when building a provider-native
// tool-result content block for token counting.
func toolNameByCallID(messages []llm.Message) map[string]string {
	m := make(map[string]string)
	for _, msg := range messages {
		for _, tc := range msg.ToolCalls {
			m[tc.ID] = tc.Name
		}
	}
	return m
}

// toStringSlice converts a JSON-Schema "required" array to []string
// regardless of whether it originated as a Go literal (our own built-in
// tools build it as []string directly — see tool_catalog.go) or was
// decoded from JSON (every MCP-server tool schema decodes an array as
// []any, since encoding/json has no way to know the element type ahead of
// time).
func toStringSlice(v any) []string {
	switch vv := v.(type) {
	case []string:
		return vv
	case []any:
		out := make([]string, 0, len(vv))
		for _, e := range vv {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		return out
	default:
		return nil
	}
}

// anthropicCountTokens calls POST /v1/messages/count_tokens against the
// REAL Anthropic API (a real API key is required — this is not a fake
// call) with req translated into the SDK's own request shape.
func anthropicCountTokens(ctx context.Context, apiKey, model string, req llm.ChatRequest) (int64, error) {
	var opts []option.RequestOption
	if apiKey != "" {
		opts = append(opts, option.WithAPIKey(apiKey))
	}
	client := anthropicsdk.NewClient(opts...)

	var systemText strings.Builder
	var messages []anthropicsdk.MessageParam
	for _, m := range req.Messages {
		switch m.Role {
		case llm.RoleSystem:
			if systemText.Len() > 0 {
				systemText.WriteString("\n\n")
			}
			systemText.WriteString(m.Content)
		case llm.RoleUser:
			messages = append(messages, anthropicsdk.NewUserMessage(anthropicsdk.NewTextBlock(m.Content)))
		case llm.RoleAssistant:
			var blocks []anthropicsdk.ContentBlockParamUnion
			if m.Content != "" {
				blocks = append(blocks, anthropicsdk.NewTextBlock(m.Content))
			}
			for _, tc := range m.ToolCalls {
				var input any = json.RawMessage(tc.Arguments)
				blocks = append(blocks, anthropicsdk.NewToolUseBlock(tc.ID, input, tc.Name))
			}
			if len(blocks) == 0 {
				blocks = append(blocks, anthropicsdk.NewTextBlock(""))
			}
			messages = append(messages, anthropicsdk.NewAssistantMessage(blocks...))
		case llm.RoleTool:
			messages = append(messages, anthropicsdk.NewUserMessage(anthropicsdk.NewToolResultBlock(m.ToolCallID, m.Content, false)))
		}
	}

	var toolParams []anthropicsdk.MessageCountTokensToolUnionParam
	for _, td := range req.Tools {
		schema := anthropicsdk.ToolInputSchemaParam{}
		if td.Parameters != nil {
			schema.Properties = td.Parameters["properties"]
			schema.Required = toStringSlice(td.Parameters["required"])
		}
		toolParams = append(toolParams, anthropicsdk.MessageCountTokensToolUnionParam{OfTool: &anthropicsdk.ToolParam{
			Name:        td.Name,
			Description: param.NewOpt(td.Description),
			InputSchema: schema,
		}})
	}

	params := anthropicsdk.MessageCountTokensParams{
		Model:    anthropicsdk.Model(model),
		Messages: messages,
		Tools:    toolParams,
	}
	if systemText.Len() > 0 {
		params.System = anthropicsdk.MessageCountTokensParamsSystemUnion{OfString: param.NewOpt(systemText.String())}
	}

	resp, err := client.Messages.CountTokens(ctx, params)
	if err != nil {
		return 0, fmt.Errorf("anthropic count_tokens: %w", err)
	}
	return resp.InputTokens, nil
}

// geminiCountTokens calls the REAL Gemini count-tokens endpoint with req
// translated into the genai SDK's own request shape.
func geminiCountTokens(ctx context.Context, apiKey, model string, req llm.ChatRequest) (int64, error) {
	client, err := genai.NewClient(ctx, &genai.ClientConfig{APIKey: apiKey, Backend: genai.BackendGeminiAPI})
	if err != nil {
		return 0, fmt.Errorf("build genai client: %w", err)
	}

	byCallID := toolNameByCallID(req.Messages)

	var systemParts []*genai.Part
	var contents []*genai.Content
	for _, m := range req.Messages {
		switch m.Role {
		case llm.RoleSystem:
			systemParts = append(systemParts, genai.NewPartFromText(m.Content))
		case llm.RoleUser:
			contents = append(contents, genai.NewContentFromText(m.Content, genai.RoleUser))
		case llm.RoleAssistant:
			var parts []*genai.Part
			if m.Content != "" {
				parts = append(parts, genai.NewPartFromText(m.Content))
			}
			for _, tc := range m.ToolCalls {
				var args map[string]any
				_ = json.Unmarshal([]byte(tc.Arguments), &args)
				parts = append(parts, &genai.Part{FunctionCall: &genai.FunctionCall{ID: tc.ID, Name: tc.Name, Args: args}})
			}
			if len(parts) == 0 {
				parts = append(parts, genai.NewPartFromText(""))
			}
			contents = append(contents, genai.NewContentFromParts(parts, genai.RoleModel))
		case llm.RoleTool:
			name := byCallID[m.ToolCallID]
			if name == "" {
				name = "tool_result" // ID didn't match any assistant tool_use in this same request — first-turn-of-conversation edge case
			}
			contents = append(contents, &genai.Content{Role: genai.RoleUser, Parts: []*genai.Part{{
				FunctionResponse: &genai.FunctionResponse{ID: m.ToolCallID, Name: name, Response: map[string]any{"output": m.Content}},
			}}})
		}
	}

	// Both CountTokensConfig.SystemInstruction AND .Tools are Vertex-only —
	// the Gemini Developer API (this project's Backend:
	// genai.BackendGeminiAPI, i.e. a plain API key, not Vertex) rejects
	// either one outright ("... parameter is only supported in Gemini
	// Enterprise Agent Platform mode, not in Gemini Developer API mode"),
	// confirmed against the real API. So neither field is set here — system
	// text and the tool schemas (serialized the same way they'd actually
	// be sent) are instead folded in as a leading user-role Content. This
	// undercounts slightly relative to a real Chat() call, which formats
	// system instructions and native function declarations with some
	// protocol overhead this plain-text stand-in doesn't add — but it runs
	// the REAL tokenizer over the REAL text, which is what actually varies
	// with language (Cyrillic vs. Latin) — the whole reason this replaced
	// the byte-count heuristic. Treat the result as a close lower bound,
	// not an exact match to billed tokens.
	var leadingParts []*genai.Part
	leadingParts = append(leadingParts, systemParts...)
	if len(req.Tools) > 0 {
		toolsJSON, err := json.Marshal(req.Tools)
		if err != nil {
			return 0, fmt.Errorf("marshal tools for gemini token counting: %w", err)
		}
		leadingParts = append(leadingParts, genai.NewPartFromText(string(toolsJSON)))
	}
	if len(leadingParts) > 0 {
		contents = append([]*genai.Content{{Role: genai.RoleUser, Parts: leadingParts}}, contents...)
	}

	resp, err := client.Models.CountTokens(ctx, model, contents, &genai.CountTokensConfig{})
	if err != nil {
		return 0, fmt.Errorf("gemini count_tokens: %w", err)
	}
	return int64(resp.TotalTokens), nil
}

// connectAuditMCPServers best-effort connects every enabled, non-OAuth MCP
// server within a short timeout each, so the audit's tool count reflects
// real MCP tools when they're reachable. Unlike cmd/miranda.connectMCP,
// this does NOT retry in the background — a server that's down right now
// is simply reported as a warning and excluded, which is the right
// behavior for a one-shot audit (no point waiting on a flaky dependency).
// OAuth-gated servers are always excluded (correctly — they contribute
// nothing to a fresh turn's tool list until that user has authorized them;
// see tool_catalog.go's own doc comment on lazy groups).
func connectAuditMCPServers(t *testing.T, servers []config.MCPServer) ([]mcp.Client, []string) {
	t.Helper()
	var clients []mcp.Client
	var warnings []string
	for _, s := range servers {
		if !s.Enabled {
			continue
		}
		if s.OAuthProvider != "" {
			warnings = append(warnings, fmt.Sprintf("mcp server %q is OAuth-gated — excluded from this audit (correctly lazy until a user authorizes it)", s.Name))
			continue
		}
		token := ""
		if s.TokenEnv != "" {
			token = os.Getenv(s.TokenEnv)
		}
		ctx, cancel := context.WithTimeout(context.Background(), mcpConnectAuditTimeout)
		client, err := mcp.Connect(ctx, s.Name, s.URL, token)
		cancel()
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("mcp server %q: connect failed within %s: %v — excluded from this audit's tool count", s.Name, mcpConnectAuditTimeout, err))
			continue
		}
		clients = append(clients, client)
	}
	return clients, warnings
}

// auditTurnDump is one captured request/response, shaped for the JSON dump.
type auditTurnDump struct {
	Iteration        int           `json:"iteration"`
	ProviderName     string        `json:"provider"`
	Timestamp        time.Time     `json:"timestamp"`
	SystemMessages   []string      `json:"system_messages"`
	Messages         []llm.Message `json:"messages"`
	Tools            []llm.ToolDef `json:"tools"`
	ToolCount        int           `json:"tool_count"`
	ResponseText     string        `json:"response_text,omitempty"`
	ResponseToolCall *llm.ToolCall `json:"response_tool_call,omitempty"`
	Error            string        `json:"error,omitempty"`
}

// auditFirstTurnSummary is the set of measurements the hard assertions run
// against, duplicated into the dump so a human reading the file later sees
// exactly what the test checked and against what limits.
type auditFirstTurnSummary struct {
	ToolCount      int `json:"tool_count"`
	ToolCountLimit int `json:"tool_count_limit"`
	// SystemPromptChars/ToolsJSONChars are UTF-8 byte lengths (Go's len()
	// on a string) — informational only, NOT what the hard assertion
	// checks. Kept for quick eyeballing in the dump; they're a poor proxy
	// for token cost on this project's mostly-Russian system prompt (see
	// realTokenCount's doc comment) and are NOT character counts either.
	SystemPromptChars int `json:"system_prompt_bytes"`
	ToolsJSONChars    int `json:"tools_json_bytes"`
	// ContextTokens is the REAL input-token count for this exact request
	// (system + tools + messages combined), from the provider's own
	// count-tokens API — see realTokenCount. Zero + TokenCountAvailable
	// false means the provider type doesn't support it (currently only
	// "openai_compat").
	ContextTokens             int64    `json:"context_tokens"`
	ContextTokensLimit        int64    `json:"context_tokens_limit"`
	TokenCountAvailable       bool     `json:"token_count_available"`
	TokenCountMethod          string   `json:"token_count_method,omitempty"`
	DuplicateToolNames        []string `json:"duplicate_tool_names,omitempty"`
	ToolsWithEmptyDescription []string `json:"tools_with_empty_description,omitempty"`
}

type auditReport struct {
	GeneratedAt  time.Time             `json:"generated_at"`
	UserID       string                `json:"user_id"`
	Prompt       string                `json:"prompt"`
	ConfigDir    string                `json:"config_dir"`
	ProviderUsed string                `json:"provider_used"`
	FinalReply   string                `json:"final_reply"`
	KnownGaps    []string              `json:"known_gaps"`
	MCPWarnings  []string              `json:"mcp_warnings,omitempty"`
	FirstTurn    auditFirstTurnSummary `json:"first_turn_summary"`
	Turns        []auditTurnDump       `json:"turns"`
}

var auditKnownGaps = []string{
	"Telegram is not wired — send_telegram won't appear even if telegram.enabled=true in config",
	"OAuth is not wired — oauth_authorize and every OAuth-gated MCP server won't appear even if configured",
	"No real TTS dispatcher — stop_speech won't appear even if tts.stop_speech_tool=true (speak_reply is unaffected, it doesn't require a dispatcher)",
	"Attachments/keyring are not wired — irrelevant to a text-only turn with no uploaded files",
}

// TestContextAudit_DebugUser drives one real turn as user "debug" through
// the real (non-fake) LLM provider(s) configured in config/*.yaml, then
// hard-asserts on the size/shape of the very first request the agent loop
// assembled, and dumps the full turn (every request/response) to
// logs/context_audit/*.json for manual review — see this file's own
// top-of-file doc comment for scope and known gaps.
func TestContextAudit_DebugUser(t *testing.T) {
	_ = envfile.Load("../../.env")

	configDir := envDefault("MIRANDA_CONFIG_DIR", "../../config")
	paths, err := filepath.Glob(filepath.Join(configDir, "*.yaml"))
	require.NoError(t, err)
	if len(paths) == 0 {
		t.Skipf("no config/*.yaml found under %s — this test audits a real deployment config; nothing to run against", configDir)
	}

	cfg, err := config.Load(paths...)
	require.NoError(t, err)
	if len(cfg.LLM.Providers) == 0 {
		t.Skip("no llm.providers configured — nothing to audit")
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	rec := &callRecorder{}
	providers, err := buildRecordingProviders(t.Context(), cfg.LLM.Providers, logger, rec)
	if err != nil {
		t.Skipf("could not build LLM providers (likely a missing API key): %v", err)
	}

	llmRouter, err := router.New(providers, buildAuditEscalations(cfg.LLM.Providers), cfg.LLM.DefaultProvider)
	require.NoError(t, err)

	mcpClients, mcpWarnings := connectAuditMCPServers(t, cfg.MCP.Servers)
	toolManager := mcp.NewManager(logger, mcpClients...)

	historyStore, err := history.Open(filepath.Join(t.TempDir(), "miranda.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = historyStore.Close() })

	// Isolated temp-dir memory store, deliberately NOT the real deployment's
	// data/memory — this audits the STRUCTURE the agent loop assembles
	// (system prompt template, tool set) for a fresh "debug" user, not any
	// real household member's accumulated memory content.
	memoryStore, err := memory.New(t.TempDir())
	require.NoError(t, err)

	scheduleStore, err := schedule.Open(filepath.Join(t.TempDir(), "schedule.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = scheduleStore.Close() })

	eventHub := hub.New(200, nil)

	orchestrator := agentloop.NewOrchestrator(
		llmRouter, toolManager, historyStore, memoryStore, nil, eventHub, nil,
		cfg.Agent, cfg.Memory, cfg.TTS, cfg.TTS.YandexStation.ChunkMaxChars, contextAuditUserID,
	)
	if lazy := cfg.LazyMCPServers(); len(lazy) > 0 {
		orchestrator.SetLazyMCPServers(lazy)
	}
	if cfg.Schedule.Enabled {
		orchestrator.SetSchedule(scheduleStore)
	}
	if webTools, err := buildAuditWebTools(cfg.Tavily, logger); err != nil {
		t.Logf("web tools not wired: %v", err)
	} else if len(webTools) > 0 {
		orchestrator.SetWebTools(webTools)
	}

	const prompt = "Не вызывай никакие инструменты. Представься одним коротким предложением и скажи, какой сейчас день недели."
	resp, err := orchestrator.Handle(t.Context(), agentloop.InputRequest{
		Source: "cli",
		UserID: contextAuditUserID,
		Text:   prompt,
	})
	require.NoError(t, err)
	require.NotEmpty(t, resp.Reply)

	rec.mu.Lock()
	calls := append([]recordedCall(nil), rec.calls...)
	rec.mu.Unlock()
	require.NotEmpty(t, calls, "no LLM request was captured — check provider/router wiring")

	first := calls[0]

	var systemMessages []string
	for _, m := range first.Request.Messages {
		if m.Role == llm.RoleSystem {
			systemMessages = append(systemMessages, m.Content)
		}
	}
	systemChars := 0
	for _, s := range systemMessages {
		systemChars += len(s)
	}

	toolsJSON, err := json.Marshal(first.Request.Tools)
	require.NoError(t, err)
	toolsChars := len(toolsJSON)

	seen := make(map[string]int)
	var emptyDesc []string
	for _, td := range first.Request.Tools {
		seen[td.Name]++
		if strings.TrimSpace(td.Description) == "" {
			emptyDesc = append(emptyDesc, td.Name)
		}
	}
	var dupes []string
	for name, n := range seen {
		if n > 1 {
			dupes = append(dupes, name)
		}
	}
	sort.Strings(dupes)
	sort.Strings(emptyDesc)

	// Look up the config entry for whichever provider actually answered
	// this request — router.Router picks one of possibly several
	// configured providers, and each provider's Type/Model/APIKeyEnvs
	// determine how (and whether) realTokenCount can measure it.
	providerConfigByName := make(map[string]config.LLMProvider, len(cfg.LLM.Providers))
	for _, pc := range cfg.LLM.Providers {
		providerConfigByName[pc.Name] = pc
	}
	pc, foundProviderConfig := providerConfigByName[first.ProviderName]
	require.Truef(t, foundProviderConfig, "captured request came from provider %q, not found in cfg.LLM.Providers", first.ProviderName)

	contextTokens, tokenCountOK, tokenErr := realTokenCount(t.Context(), pc.Type, firstAuditAPIKey(pc.APIKeyEnvs), pc.Model, first.Request)
	tokenMethod := ""
	switch {
	case !tokenCountOK:
		tokenMethod = fmt.Sprintf("unavailable for provider type %q", pc.Type)
	case tokenErr == nil:
		tokenMethod = pc.Type + "_count_tokens_api"
	}

	summary := auditFirstTurnSummary{
		ToolCount:                 len(first.Request.Tools),
		ToolCountLimit:            maxToolCount,
		SystemPromptChars:         systemChars,
		ToolsJSONChars:            toolsChars,
		ContextTokens:             contextTokens,
		ContextTokensLimit:        maxContextTokens,
		TokenCountAvailable:       tokenCountOK,
		TokenCountMethod:          tokenMethod,
		DuplicateToolNames:        dupes,
		ToolsWithEmptyDescription: emptyDesc,
	}

	var turns []auditTurnDump
	for i, c := range calls {
		var sysMsgs []string
		for _, m := range c.Request.Messages {
			if m.Role == llm.RoleSystem {
				sysMsgs = append(sysMsgs, m.Content)
			}
		}
		turns = append(turns, auditTurnDump{
			Iteration:        i,
			ProviderName:     c.ProviderName,
			Timestamp:        c.Timestamp,
			SystemMessages:   sysMsgs,
			Messages:         c.Request.Messages,
			Tools:            c.Request.Tools,
			ToolCount:        len(c.Request.Tools),
			ResponseText:     c.ResponseText,
			ResponseToolCall: c.ToolCall,
			Error:            c.Err,
		})
	}

	report := auditReport{
		GeneratedAt:  time.Now(),
		UserID:       contextAuditUserID,
		Prompt:       prompt,
		ConfigDir:    configDir,
		ProviderUsed: resp.ProviderUsed,
		FinalReply:   resp.Reply,
		KnownGaps:    auditKnownGaps,
		MCPWarnings:  mcpWarnings,
		FirstTurn:    summary,
		Turns:        turns,
	}

	dumpPath := writeAuditReport(t, configDir, report)
	t.Logf("full context dumped to %s (tools=%d, context_tokens=%d via %s, system=%dB, tools_json=%dB)",
		dumpPath, summary.ToolCount, summary.ContextTokens, summary.TokenCountMethod, summary.SystemPromptChars, summary.ToolsJSONChars)
	for _, w := range mcpWarnings {
		t.Logf("mcp warning: %s", w)
	}

	// --- hard assertions — see this file's const block for why these numbers ---
	require.Emptyf(t, dupes, "duplicate tool names in the turn-start request: %v — see %s", dupes, dumpPath)
	require.Emptyf(t, emptyDesc, "tools with empty description: %v — see %s", emptyDesc, dumpPath)
	require.LessOrEqualf(t, summary.ToolCount, maxToolCount,
		"too many tools loaded at turn start (%d > %d) — see %s", summary.ToolCount, maxToolCount, dumpPath)
	if tokenCountOK {
		// A real error from the provider's own count-tokens API (as
		// opposed to "this provider type doesn't support it") is worth
		// failing loudly on — it usually means either a real API/config
		// problem or a bug in this file's request-shape translation, not
		// something to silently skip past.
		require.NoErrorf(t, tokenErr, "real token counting via %s failed — see %s", tokenMethod, dumpPath)
		require.LessOrEqualf(t, summary.ContextTokens, int64(maxContextTokens),
			"turn-start context too large (%d > %d real tokens, via %s) — see %s", summary.ContextTokens, maxContextTokens, tokenMethod, dumpPath)
	} else {
		t.Logf("real token counting not available for provider type %q — skipping the token-size assertion (tool-count/consistency checks above still ran)", pc.Type)
	}
}

// writeAuditReport writes report as indented JSON under
// <repo root>/logs/context_audit/, resolving the repo root the same way
// configDir was resolved (configDir defaults to "../../config", i.e. two
// levels up from this test's own package directory).
func writeAuditReport(t *testing.T, configDir string, report auditReport) string {
	t.Helper()
	repoRoot := filepath.Dir(configDir)
	dumpDir := filepath.Join(repoRoot, "logs", "context_audit")
	require.NoError(t, os.MkdirAll(dumpDir, 0o755))

	filename := fmt.Sprintf("context_audit_%s_%s.json", report.UserID, report.GeneratedAt.UTC().Format("20060102T150405Z"))
	path := filepath.Join(dumpDir, filename)

	data, err := json.MarshalIndent(report, "", "  ")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, data, 0o644))
	return path
}
