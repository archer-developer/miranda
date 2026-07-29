// Package gemini implements llm.Provider on top of the official
// google.golang.org/genai Go SDK, used exclusively for native Gemini
// models — it gets full native support for function calling combined with
// Grounding with Google Search in one request, unlike routing Gemini
// through the OpenAI-compatible shim (code execution is deliberately not
// wired — see nativeTools's doc comment, it's Vertex AI-only on the
// generateContent API this package calls). See internal/llm/anthropic for
// the sibling adapter this mirrors, and internal/tts/gemini.go for the
// API-key-rotation shape this shares (via internal/keyrotation) but
// broadens: quota, server, AND per-key auth errors rotate here, not just
// quota — see isRetryable.
package gemini

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"google.golang.org/genai"

	"github.com/archer-developer/miranda/internal/config"
	"github.com/archer-developer/miranda/internal/keyrotation"
	"github.com/archer-developer/miranda/internal/llm"
)

// apiBaseURL overrides every genai.Client's HTTPOptions.BaseURL. It's a
// var, not a const, so tests can point it at an httptest.Server instead of
// the real Gemini API — mirrors internal/tts/gemini.go's geminiAPIBaseURL.
// Empty (the default) means "use the SDK's real Gemini API endpoint".
var apiBaseURL string

// Provider is an llm.Provider backed by the native Gemini API. Unlike
// anthropic.Provider (one API key, one client), it holds one *genai.Client
// per configured key and rotates across them on a quota or server error —
// see pump/attempt below.
type Provider struct {
	name     string
	model    string
	clients  []*genai.Client // one per resolved API key, in configured order
	tools    config.GeminiToolsConfig
	rotation config.GeminiRotationConfig
	tracer   llm.Tracer
	logger   *slog.Logger
}

// New builds a Provider named name for the given Gemini model, resolving
// apiKeyEnvs to actual key values from the process environment (never
// stored in config.yaml — same convention as gemini_tts, see
// internal/tts.NewGeminiProvider) and building one genai.Client per
// resolved key up front. Building clients eagerly means a later rotation is
// just "try the next already-built client" — no per-request client
// reconstruction, and no risk of a client build failure surfacing mid-
// conversation instead of at startup. Fails fast if none of apiKeyEnvs
// resolve to a non-empty value, since every subsequent Chat call would
// otherwise fail identically anyway.
func New(ctx context.Context, name, model string, apiKeyEnvs []string, tools config.GeminiToolsConfig, rotation config.GeminiRotationConfig, logger *slog.Logger) (*Provider, error) {
	if logger == nil {
		logger = slog.Default()
	}
	if tools.ContextCaching {
		// Rejecting outright, rather than silently ignoring the flag, means
		// a config typo/aspiration doesn't silently no-op — see
		// GeminiToolsConfig.ContextCaching's doc comment for why Gemini's
		// CachedContent (an explicit, separately-managed resource) isn't
		// implemented yet.
		return nil, fmt.Errorf("gemini: context_caching is not implemented yet (see config.GeminiToolsConfig doc comment) — leave it false")
	}

	var clients []*genai.Client
	for _, env := range apiKeyEnvs {
		key := os.Getenv(env)
		if key == "" {
			continue
		}
		client, err := genai.NewClient(ctx, &genai.ClientConfig{
			APIKey:      key,
			Backend:     genai.BackendGeminiAPI,
			HTTPOptions: genai.HTTPOptions{BaseURL: apiBaseURL},
		})
		if err != nil {
			return nil, fmt.Errorf("gemini: build client for %s: %w", env, err)
		}
		clients = append(clients, client)
	}
	if len(clients) == 0 {
		return nil, fmt.Errorf("gemini: none of the configured api_key_envs %v are set", apiKeyEnvs)
	}

	return &Provider{
		name:     name,
		model:    model,
		clients:  clients,
		tools:    tools,
		rotation: rotation,
		logger:   logger,
	}, nil
}

func (p *Provider) Name() string { return p.name }

// SetTracer matches anthropic.Provider's — the router's traceable
// structural interface (see internal/llm/router.Router.SetTracer, which
// type-asserts for this method) forwards a tracer here.
func (p *Provider) SetTracer(t llm.Tracer) { p.tracer = t }

// Chat implements llm.Provider by streaming a GenerateContentStream
// response, forwarding text/tool-call chunks to the caller live as they
// arrive (token-by-token, like anthropic.Provider's pump) while rotating
// across configured API keys on a retryable error (see pump/attempt).
func (p *Provider) Chat(ctx context.Context, req llm.ChatRequest) (<-chan llm.StreamChunk, error) {
	system, contents := toGeminiContents(req.Messages)
	cfg := &genai.GenerateContentConfig{
		SystemInstruction: system,
		Tools:             p.buildTools(req.Tools),
	}

	out := make(chan llm.StreamChunk)
	go p.pump(ctx, contents, cfg, out)
	return out, nil
}

// pump rotates across p.clients (one per configured API key) via
// internal/keyrotation.Run — shared with internal/tts/gemini.go's
// callGemini, which needs the identical cycle/cooldown shape but a
// narrower (quota-only) retryability rule; isRetryable here is broader
// (quota AND 5xx AND per-key auth failures, see its doc comment) since a
// conversational agent turn can't just drop the whole turn on a transient
// upstream failure the way a single TTS chunk request can afford to fail
// loudly.
func (p *Provider) pump(ctx context.Context, contents []*genai.Content, cfg *genai.GenerateContentConfig, out chan<- llm.StreamChunk) {
	defer close(out)

	rotCfg := keyrotation.Config{
		Cycles:   p.rotation.MaxRetryCycles,
		Cooldown: time.Duration(p.rotation.CooldownSeconds) * time.Second,
	}
	err := keyrotation.Run(ctx, p.logger, "gemini", len(p.clients), rotCfg, isRetryable,
		func(ctx context.Context, i int) error {
			return p.attempt(ctx, p.clients[i], i, contents, cfg, out)
		},
	)
	// errAttemptWritten means attempt already wrote its own terminal Err
	// chunk before returning — writing a second one here would be a
	// duplicate. Any other non-nil error (the rotation genuinely exhausted
	// every key/cycle, or ctx was cancelled) still needs pump to report it,
	// since nothing else has.
	if err != nil && !errors.Is(err, errAttemptWritten) {
		out <- llm.StreamChunk{Err: err}
	}
}

// errAttemptWritten marks that attempt already sent its own terminal
// StreamChunk{Err: ...} to out before returning (see attempt's doc
// comment for the two cases that produce it) — pump checks for this
// sentinel specifically so it doesn't write a second Err chunk on top of
// the one attempt already sent. isRetryable naturally treats it as
// non-retryable too (it isn't a genai.APIError), so keyrotation.Run stops
// rotating and returns it straight back to pump.
var errAttemptWritten = errors.New("gemini: attempt already wrote its terminal error")

// attempt makes exactly one GenerateContentStream call against one client
// (one API key), forwarding TextDelta/ToolCall chunks to out live as they
// stream in — unlike a fully-buffered design, the first spoken word can
// reach the caller before Gemini finishes generating the rest of the
// reply, matching anthropic.Provider's live pump.
//
// The one safety rule this requires: once ANY chunk has been forwarded to
// out for this attempt, a later error on the SAME attempt must not be
// retried even if isRetryable(err) would otherwise say yes — rotating to
// another key re-runs the whole request from scratch, and streamOneTurn
// (internal/httpapi/agent_loop.go) speaks/publishes chunks live as they
// arrive with no way to "unsay" one, so a retry after partial output would
// duplicate or garble what the user already heard. attempt itself decides
// this (via the local forwarded bool) rather than pushing the decision
// into isRetryable, since isRetryable is a pure function of the error
// alone and has no way to know this attempt's forwarding state.
//
// Returns nil once the response completes cleanly (having already written
// Done to out). Returns errAttemptWritten in the two cases described above
// (forwarded-then-error, or a non-retryable error from the first event) —
// both already wrote their own Err chunk to out, so keyrotation.Run/pump
// must not retry or double-report. Returns the raw streamErr only when
// nothing has been forwarded yet AND isRetryable(streamErr) is true, so
// keyrotation.Run's own isRetryable check (the same package-level
// function, applied to the same value) agrees and tries the next key.
func (p *Provider) attempt(ctx context.Context, client *genai.Client, keyIndex int, contents []*genai.Content, cfg *genai.GenerateContentConfig, out chan<- llm.StreamChunk) error {
	start := time.Now()
	var textBuf strings.Builder // accumulated only for tracing, not for forwarding
	var toolCalls []llm.ToolCall
	forwarded := false

	for resp, streamErr := range client.Models.GenerateContentStream(ctx, p.model, contents, cfg) {
		if streamErr != nil {
			if forwarded {
				p.logger.Error("gemini: request failed mid-stream after partial output, not retrying", "provider", p.name, "key_index", keyIndex, "error", streamErr)
				wrapped := fmt.Errorf("gemini: %w", streamErr)
				p.trace(ctx, contents, cfg, textBuf.String(), toolCalls, wrapped)
				out <- llm.StreamChunk{Err: wrapped}
				return errAttemptWritten
			}
			if isRetryable(streamErr) {
				p.logger.Warn("gemini: key error, trying next key", "provider", p.name, "key_index", keyIndex, "error", streamErr)
				return streamErr
			}
			p.logger.Error("gemini: request failed", "provider", p.name, "key_index", keyIndex, "error", streamErr)
			wrapped := fmt.Errorf("gemini: %w", streamErr)
			p.trace(ctx, contents, cfg, "", nil, wrapped)
			out <- llm.StreamChunk{Err: wrapped}
			return errAttemptWritten
		}
		for _, cand := range resp.Candidates {
			if cand.Content == nil {
				continue
			}
			for _, part := range cand.Content.Parts {
				switch {
				case part.Text != "":
					textBuf.WriteString(part.Text)
					out <- llm.StreamChunk{TextDelta: part.Text}
					forwarded = true
				case part.FunctionCall != nil:
					tc := toLLMToolCall(part.FunctionCall, part.ThoughtSignature, len(toolCalls))
					toolCalls = append(toolCalls, tc)
					out <- llm.StreamChunk{ToolCall: &tc}
					forwarded = true
				}
			}
		}
	}

	p.logger.Info("gemini: request succeeded", "provider", p.name, "key_index", keyIndex, "duration_ms", time.Since(start).Milliseconds())
	p.trace(ctx, contents, cfg, textBuf.String(), toolCalls, nil)
	out <- llm.StreamChunk{Done: true}
	return nil
}

// toLLMToolCall converts a Gemini FunctionCall to llm.ToolCall,
// JSON-encoding Args (a map[string]any) into the raw-JSON-string shape
// llm.ToolCall.Arguments expects. Gemini doesn't always populate
// FunctionCall.ID (it's documented as optional) — executeTool,
// history.AppendToolCall, and router.appendEscalationTurn all key off
// llm.ToolCall.ID to match a later tool result back to this call, so an
// empty ID would silently break that matching; synthesize a deterministic
// one from the call's name and position when Gemini leaves it blank.
//
// thoughtSignature is the sibling Part's ThoughtSignature (not a field on
// FunctionCall itself) — confirmed against a real API response, not just
// SDK docs: replaying a function-call turn without echoing this back is a
// hard 400 ("Function call is missing a thought_signature..."), not just
// degraded quality as the SDK's own doc comments elsewhere might suggest.
// Base64-encoded into ProviderMetadata since it's opaque binary and
// llm.ToolCall carries only string fields.
func toLLMToolCall(fc *genai.FunctionCall, thoughtSignature []byte, index int) llm.ToolCall {
	id := fc.ID
	if id == "" {
		id = fmt.Sprintf("%s-%d", fc.Name, index)
	}
	argsJSON, err := json.Marshal(fc.Args)
	if err != nil {
		argsJSON = []byte("{}")
	}
	var providerMetadata string
	if len(thoughtSignature) > 0 {
		providerMetadata = base64.StdEncoding.EncodeToString(thoughtSignature)
	}
	return llm.ToolCall{ID: id, Name: fc.Name, Arguments: string(argsJSON), ProviderMetadata: providerMetadata}
}

// isRetryable reports whether err is worth rotating to the next key for:
//   - a quota/rate-limit error (HTTP 429, or APIError.Status ==
//     "RESOURCE_EXHAUSTED" — the same trigger internal/tts's gemini_tts
//     rotation already uses)
//   - a server-side error (HTTP 5xx) — broader than TTS's quota-only
//     rotation, per this adapter's requirement
//   - a per-key auth failure (HTTP 401 Unauthorized / 403 Forbidden) — this
//     is a property of THAT key specifically (revoked, disabled, or a
//     per-key restriction hit independent of quota), not of the request,
//     so a different configured key can plausibly still succeed. This is
//     distinct from a malformed/bad request (HTTP 400), which would fail
//     identically no matter which key sends it and stays non-retryable
//     below — rotating keys only helps when the fault is IN the key, not
//     in what's being asked.
//
// Any other error (a malformed request, or a network failure before any
// HTTP response came back) is not retried: cycling keys can't fix a
// request that's simply wrong.
func isRetryable(err error) bool {
	var apiErr genai.APIError
	if !errors.As(err, &apiErr) {
		return false
	}
	switch apiErr.Code {
	case http.StatusTooManyRequests, http.StatusUnauthorized, http.StatusForbidden:
		return true
	}
	if apiErr.Status == "RESOURCE_EXHAUSTED" {
		return true
	}
	return apiErr.Code >= 500 && apiErr.Code < 600
}

// trace serializes the actual request (contents + cfg) and response (text +
// tool calls) to p.tracer, mirroring anthropic.Provider.trace's contract —
// same pattern, just over Gemini's own types instead of Anthropic's.
func (p *Provider) trace(ctx context.Context, contents []*genai.Content, cfg *genai.GenerateContentConfig, text string, toolCalls []llm.ToolCall, err error) {
	if p.tracer == nil {
		return
	}
	reqPayload := struct {
		Contents []*genai.Content             `json:"contents"`
		Config   *genai.GenerateContentConfig `json:"config"`
	}{contents, cfg}
	reqJSON, marshalErr := json.MarshalIndent(reqPayload, "", "  ")
	if marshalErr != nil {
		reqJSON = []byte(fmt.Sprintf("(failed to marshal request: %v)", marshalErr))
	}
	var respJSON []byte
	if err == nil {
		respPayload := struct {
			Text      string         `json:"text"`
			ToolCalls []llm.ToolCall `json:"tool_calls,omitempty"`
		}{text, toolCalls}
		if respJSON, marshalErr = json.MarshalIndent(respPayload, "", "  "); marshalErr != nil {
			respJSON = []byte(fmt.Sprintf("(failed to marshal response: %v)", marshalErr))
		}
	}
	p.tracer.Trace(ctx, p.name, string(reqJSON), string(respJSON), err)
}

// toGeminiContents splits llm.Message history into Gemini's separate
// top-level system instruction and turn-by-turn Content list — the same
// split anthropic.toAnthropicMessages does for Claude's system param.
// RoleTool messages map onto Gemini's FunctionResponse part, which requires
// the original tool's Name (not just its call ID) — llm.Message{Role:
// RoleTool} only carries ToolCallID, so Name is recovered from the
// preceding assistant message's matching ToolCall as history is walked.
//
// A replayed FunctionCall part also needs its ThoughtSignature echoed back
// from tc.ProviderMetadata (base64-decoded) — confirmed against a real API
// response: Gemini returns a 400 ("Function call is missing a
// thought_signature...") on the very next turn to the SAME provider if
// it's dropped, not just degraded quality. Sent as-is (possibly empty) for
// a call with no ProviderMetadata to echo: older stored history predating
// this fix, a synthetic call built elsewhere (e.g.
// router.appendEscalationTurn's handoff acknowledgment), or a call
// replayed to a DIFFERENT Gemini provider after an escalation hop (e.g.
// gemini-lite's own earlier tool calls, replayed to gemini-strong) — a
// signature is only ever verified against the exact model that emitted
// it, so cross-model replay is untested territory this fix doesn't
// specifically address; if that turns out to also need special handling,
// it needs its own real-API verification pass, not a guess here.
func toGeminiContents(msgs []llm.Message) (*genai.Content, []*genai.Content) {
	var systemParts []*genai.Part
	var out []*genai.Content

	toolNames := make(map[string]string) // ToolCall.ID -> ToolCall.Name
	for _, m := range msgs {
		switch m.Role {
		case llm.RoleSystem:
			systemParts = append(systemParts, &genai.Part{Text: m.Content})

		case llm.RoleUser:
			out = append(out, &genai.Content{Role: genai.RoleUser, Parts: []*genai.Part{{Text: m.Content}}})

		case llm.RoleTool:
			out = append(out, &genai.Content{Role: genai.RoleUser, Parts: []*genai.Part{{
				FunctionResponse: &genai.FunctionResponse{
					ID:       m.ToolCallID,
					Name:     toolNames[m.ToolCallID],
					Response: map[string]any{"result": m.Content},
				},
			}}})

		case llm.RoleAssistant:
			var parts []*genai.Part
			if m.Content != "" {
				parts = append(parts, &genai.Part{Text: m.Content})
			}
			for _, tc := range m.ToolCalls {
				toolNames[tc.ID] = tc.Name
				var args map[string]any
				if tc.Arguments != "" {
					// Best-effort: malformed arguments just become a nil map
					// rather than failing the whole request — same posture
					// as anthropic.toAnthropicMessages's tool_use handling.
					_ = json.Unmarshal([]byte(tc.Arguments), &args)
				}
				var thoughtSignature []byte
				if tc.ProviderMetadata != "" {
					// Best-effort: a malformed/foreign-provider blob just
					// means no signature is echoed, not a hard failure —
					// mirrors the Arguments unmarshal above.
					thoughtSignature, _ = base64.StdEncoding.DecodeString(tc.ProviderMetadata)
				}
				parts = append(parts, &genai.Part{
					FunctionCall:     &genai.FunctionCall{ID: tc.ID, Name: tc.Name, Args: args},
					ThoughtSignature: thoughtSignature,
				})
			}
			out = append(out, &genai.Content{Role: genai.RoleModel, Parts: parts})
		}
	}

	var system *genai.Content
	if len(systemParts) > 0 {
		system = &genai.Content{Parts: systemParts}
	}
	return system, out
}

// buildTools assembles native tools (GoogleSearch/CodeExecution, each its
// own *genai.Tool entry per the REST API's rule that built-in tools can't
// share an entry with function declarations or each other) plus one
// *genai.Tool carrying every custom (MCP/built-in) tool's
// FunctionDeclaration. Unlike Claude's SDK, genai.FunctionDeclaration
// exposes a raw-JSON-Schema passthrough field (ParametersJsonSchema) that
// accepts llm.ToolDef.Parameters directly — confirmed via `go doc
// google.golang.org/genai.FunctionDeclaration` — so no Schema-struct
// conversion is needed even for MCP's often-nested tool schemas.
func (p *Provider) buildTools(custom []llm.ToolDef) []*genai.Tool {
	var out []*genai.Tool

	if len(custom) > 0 {
		decls := make([]*genai.FunctionDeclaration, 0, len(custom))
		for _, t := range custom {
			decls = append(decls, &genai.FunctionDeclaration{
				Name:                 t.Name,
				Description:          t.Description,
				ParametersJsonSchema: t.Parameters,
			})
		}
		out = append(out, &genai.Tool{FunctionDeclarations: decls})
	}

	out = append(out, p.nativeTools()...)
	return out
}

// nativeTools builds Gemini's own server-executed tools — mirrors
// anthropic.Provider.nativeTools. Unlike everything in buildTools' custom
// half, the model's use of these never round-trips through the
// Orchestrator's executeTool; Gemini resolves them server-side within the
// same streamed response.
//
// Code execution (genai.ToolCodeExecution) is deliberately NOT wired here:
// the SDK's source comment marks it, consistently with ~98 other
// genuinely-Vertex-only fields/types in that file, as unsupported outside
// Vertex AI on the classic generateContent/streamGenerateContent API this
// Provider calls via Models.GenerateContentStream. The working example on
// ai.google.dev that appears to contradict this targets a different,
// newer endpoint (the "Interactions API", v1beta/interactions,
// "tools":[{"type":"code_execution"}]) that this adapter doesn't call —
// it doesn't actually contradict the SDK's restriction on this code path.
// GoogleSearch has no such restriction (confirmed: no matching comment on
// its type or the fields this Provider sets), so grounding is unaffected.
func (p *Provider) nativeTools() []*genai.Tool {
	var out []*genai.Tool
	if p.tools.GoogleSearch {
		out = append(out, &genai.Tool{GoogleSearch: &genai.GoogleSearch{}})
	}
	return out
}
