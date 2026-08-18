package agentloop

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	llm "github.com/archer-developer/miranda-llm"
	"github.com/archer-developer/miranda/internal/attachments"
	"github.com/archer-developer/miranda/internal/hub"
	"github.com/archer-developer/miranda/internal/mcp"
)

// executeMCPTool is executeTool's fallthrough path: everything that isn't
// one of Miranda's own built-in tools or an internal/tools.Tool (see
// o.webTools) ends up here, resolved against the MCP tool manager. Kept
// separate from executeTool's builtin if-chain since this half is about one
// concern — coordinating with TTS, injecting per-server arguments, routing
// the call, and staging any file the result references — rather than
// dispatching by tool name.
func (o *Orchestrator) executeMCPTool(ctx context.Context, userID, conversationID string, tc llm.ToolCall, control *turnControl) string {
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
