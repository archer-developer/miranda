package agentloop

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os/exec"
	"strings"
	"sync"
	"time"

	llm "github.com/archer-developer/miranda-llm"
	"github.com/archer-developer/miranda/internal/chunk"
	"github.com/archer-developer/miranda/internal/claudecode"
	"github.com/archer-developer/miranda/internal/config"
	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// claudeCodeProviderName is what a claude_code turn reports as
// InputResponse.ProviderUsed and tags its llm.log blocks with.
const claudeCodeProviderName = "claude_code"

// mirandaMCPServerName is the MCP server name each Claude Code process sees
// Miranda's tools under — the CLI exposes them to the model as
// "mcp__miranda__<tool>".
const mirandaMCPServerName = "miranda"

// relistTimeout bounds how long a turn waits for the CLI to re-fetch a
// changed tool list before sending the message anyway — see awaitRelist.
const relistTimeout = 2 * time.Second

// relistGrace is how long a turn still waits after the CLI's tools/list
// request was answered: the CLI applies the fetched list asynchronously, so
// a message sent the instant the response leaves Miranda still saw the old
// list. 50ms was enough against the real CLI on a laptop in every run; 200ms
// leaves headroom for a slower server. Paid only on a turn whose tool set
// changed (switching between ha_assist and other sources, lazy groups).
const relistGrace = 200 * time.Millisecond

// claudeCodeEngine is the claude_code orchestrator (see turnEngine and
// docs/adr/claude-code-orchestrator.md): one long-lived headless Claude Code
// process per user does the reasoning and tool selection, and calls Miranda's
// own tools back through a loopback MCP endpoint ("Miranda MCP") served by
// this engine. Every tool call still goes through Orchestrator.executeTool
// and recordToolCall — exactly the code path the miranda orchestrator uses —
// so no tool logic exists twice.
type claudeCodeEngine struct {
	o      *Orchestrator
	cfg    config.ClaudeCodeConfig
	tracer llm.Tracer
	logger *slog.Logger

	// listener/httpSrv serve Miranda MCP on 127.0.0.1 only; baseURL is the
	// URL every process's --mcp-config points at.
	listener net.Listener
	httpSrv  *http.Server
	baseURL  string

	mu sync.Mutex
	// sessions holds each user's current process, keyed by userID.
	sessions map[string]*ccSession
	// byToken maps each live session's bearer token to it, for Miranda
	// MCP's per-request lookup — the token is what binds an inbound tool
	// call to a user, so a process can only ever act as its own user.
	byToken map[string]*ccSession
	// userLocks serialize turns per user: a session is only ever replaced
	// or reaped while its user's lock is held, so a turn never has its
	// process swapped out from under it.
	userLocks map[string]*sync.Mutex

	stop chan struct{}
}

// ccSession is one user's running Claude Code process plus its Miranda MCP
// server instance.
type ccSession struct {
	userID string
	// convID/promptHash are what the process was started for: a turn for a
	// different conversation, or with a different stable system prompt
	// (memory changed), restarts the process — see claudeCodeEngine.session.
	convID     string
	promptHash string
	token      string
	proc       *claudecode.Process
	server     *mcp.Server

	mu sync.Mutex
	// active is the turn currently being served; nil between turns, when
	// Miranda MCP refuses every call.
	active *ccTurn
	// registered maps each tool name on server to a fingerprint of its
	// definition, so syncTools only touches what changed (every change
	// sends the CLI a tools/list_changed notification).
	registered map[string]string
	lastUsed   time.Time
	// initTools is what the CLI reported offering the model on the latest
	// turn (its "system/init" event, "mcp__miranda__"-prefixed) — kept for
	// diagnostics and tests: it shows whether a tools/list_changed refresh
	// actually landed before the turn began.
	initTools []string
	// relisted is non-nil while a tools/list_changed notification is
	// outstanding: syncTools creates it when it changes the tool set, and
	// Miranda MCP closes it as soon as the CLI re-fetches tools/list — see
	// awaitRelist.
	relisted chan struct{}

	// toolMu serializes tool execution within a session: the CLI may run
	// several MCP calls in parallel, but turnControl (and the history
	// ordering of tool call → result) assumes one at a time.
	toolMu sync.Mutex
}

// ccTurn is the per-turn state Miranda MCP's tool handlers need: the turn's
// own ctx (deadline, conversation id, anomaly recorder), source and control.
type ccTurn struct {
	ctx     context.Context
	source  string
	control *turnControl
	// allowed is the tool set availableTools offered this turn — a call to
	// anything else (e.g. a tool the CLI still lists because a
	// list_changed notification hasn't landed yet) is refused.
	allowed map[string]bool
}

// EnableClaudeCode switches this Orchestrator to the claude_code
// orchestrator (agent.orchestrator: claude_code). tracer should be the same
// tracer the router writes llm.log through (redact.Tracer wrapping
// llmtrace.ContextTracer), so a claude_code turn's blocks land in the same
// file, masked the same way, and reach the per-turn anomaly Recorder. Call
// once from cmd/miranda before serving; Close stops it.
//
// Fails if the CLI can't be found. Whether it is logged in is only logged
// (a warning), since a login can be fixed without restarting Miranda.
func (o *Orchestrator) EnableClaudeCode(cfg config.ClaudeCodeConfig, tracer llm.Tracer) error {
	if _, err := exec.LookPath(cfg.Binary); err != nil {
		return fmt.Errorf("orchestrator: claude_code: binary %q not found: %w", cfg.Binary, err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("orchestrator: claude_code: listen for miranda mcp: %w", err)
	}
	e := &claudeCodeEngine{
		o:         o,
		cfg:       cfg,
		tracer:    tracer,
		logger:    o.logger,
		listener:  ln,
		baseURL:   "http://" + ln.Addr().String() + "/mcp",
		sessions:  make(map[string]*ccSession),
		byToken:   make(map[string]*ccSession),
		userLocks: make(map[string]*sync.Mutex),
		stop:      make(chan struct{}),
	}
	mux := http.NewServeMux()
	mux.Handle("/mcp", e.mcpHandler())
	e.httpSrv = &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = e.httpSrv.Serve(ln) }()
	if cfg.ProcessIdleMinutes > 0 {
		go e.reapIdle(time.Duration(cfg.ProcessIdleMinutes) * time.Minute)
	}
	e.checkLogin()
	o.engine = e
	return nil
}

// checkLogin logs how the CLI is authenticated, warning when it isn't —
// every turn would otherwise fail with an opaque process exit.
func (e *claudeCodeEngine) checkLogin() {
	if e.logger == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	st, err := claudecode.CheckAuth(ctx, e.cfg.Binary, e.cfg.AllowAPIKeyEnv)
	if err != nil || !st.LoggedIn {
		e.logger.Warn("orchestrator: claude_code: CLI is not logged in — run `claude auth login` as the user Miranda runs as", "binary", e.cfg.Binary, "error", err)
		return
	}
	e.logger.Info("orchestrator: claude_code enabled", "authMethod", st.AuthMethod, "subscription", st.SubscriptionType, "model", e.cfg.Model, "mcp", e.baseURL)
}

// mcpHandler serves Miranda MCP. The bearer token picks the session's own
// *mcp.Server, so each process sees exactly its user's tools.
func (e *claudeCodeEngine) mcpHandler() http.Handler {
	h := mcp.NewStreamableHTTPHandler(func(r *http.Request) *mcp.Server {
		if s := e.sessionByToken(r); s != nil {
			return s.server
		}
		return nil
	}, nil)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if e.sessionByToken(r) == nil {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		h.ServeHTTP(w, r)
	})
}

func (e *claudeCodeEngine) sessionByToken(r *http.Request) *ccSession {
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || token == "" {
		return nil
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	s := e.byToken[token]
	if s == nil || subtle.ConstantTimeCompare([]byte(s.token), []byte(token)) != 1 {
		return nil
	}
	return s
}

func (e *claudeCodeEngine) lockUser(userID string) func() {
	e.mu.Lock()
	l, ok := e.userLocks[userID]
	if !ok {
		l = &sync.Mutex{}
		e.userLocks[userID] = l
	}
	e.mu.Unlock()
	l.Lock()
	return l.Unlock
}

// runTurn implements turnEngine.
func (e *claudeCodeEngine) runTurn(ctx context.Context, t engineTurn) (string, string, error) {
	unlock := e.lockUser(t.userID)
	defer unlock()

	// Lazy groups can't be expanded mid-turn here — see
	// turnControl.loadAllGroups.
	t.control.loadAllGroups = true
	tools := e.o.availableTools(ctx, t.userID, t.control)
	sess, fresh, err := e.session(t, tools)
	if err != nil {
		return "", "", err
	}
	sess.syncTools(e, tools)
	if !fresh {
		sess.awaitRelist(ctx)
	}
	sess.begin(&ccTurn{ctx: ctx, source: t.source, control: t.control, allowed: toolNameSet(tools)})
	defer sess.end()

	msg := claudecode.UserMessage{Text: composeClaudeCodeMessage(t, fresh)}
	for _, p := range t.imageParts {
		msg.Images = append(msg.Images, claudecode.Image{MIMEType: p.MIMEType, Base64: p.ImageBase64})
	}

	tr := newCCTraceBuilder(t.stableSystem, msg.Text, tools)
	acc := chunk.NewAccumulator(publishChunkMaxChars)
	res, err := sess.proc.Turn(ctx, msg, claudecode.Handler{
		OnInit: func(names []string) {
			sess.mu.Lock()
			sess.initTools = names
			sess.mu.Unlock()
		},
		OnTextDelta: func(d string) { e.o.publishChunks(acc.Push(d)) },
		OnAssistant: func(_ string, b claudecode.ContentBlock) { tr.addResponse(b) },
		OnToolResults: func(bs []claudecode.ContentBlock) {
			tr.flush(ctx, e.tracer, nil)
			tr.nextRequest(bs)
		},
	})
	e.o.publishChunks(acc.Flush())

	t.outcome.IterationCount = res.NumTurns
	if err != nil {
		t.outcome.TimedOut = errors.Is(err, context.DeadlineExceeded)
		tr.flush(ctx, e.tracer, err)
		// A failed/cancelled turn leaves the process killed or in an
		// unknown state — drop it; the next turn starts a fresh one and
		// replays the conversation from history.
		e.dropSession(t.userID, sess)
		return "", "", fmt.Errorf("orchestrator: claude_code: %w", err)
	}
	tr.flush(ctx, e.tracer, nil)

	switch {
	case res.Subtype == "error_max_turns":
		t.outcome.HitIterationCap = true
		return "", "", fmt.Errorf("orchestrator: claude_code: exceeded %d turns without a final reply", e.cfg.MaxTurns)
	case res.IsError:
		return "", "", fmt.Errorf("orchestrator: claude_code: turn failed (%s): %s", res.Subtype, res.Text)
	case strings.TrimSpace(res.Text) == "":
		// Same reasoning as runAgentLoop's empty-reply check: never forward a
		// silent blank reply.
		return "", "", fmt.Errorf("orchestrator: claude_code: empty final reply")
	}
	return res.Text, claudeCodeProviderName, nil
}

// session returns the user's process for this turn, starting one when there
// is none, it died, or it was started for another conversation or another
// stable system prompt. fresh reports a new process — which has none of the
// conversation's earlier turns in context yet. Caller holds the user lock.
func (e *claudeCodeEngine) session(t engineTurn, tools []llm.ToolDef) (sess *ccSession, fresh bool, err error) {
	hash := sha256.Sum256([]byte(t.stableSystem))
	promptHash := hex.EncodeToString(hash[:])

	e.mu.Lock()
	cur := e.sessions[t.userID]
	e.mu.Unlock()
	if cur != nil && cur.proc.Alive() && cur.convID == t.convID && cur.promptHash == promptHash {
		return cur, false, nil
	}
	if cur != nil {
		e.dropSession(t.userID, cur)
	}

	token, err := randomToken()
	if err != nil {
		return nil, false, err
	}
	s := &ccSession{
		userID:     t.userID,
		convID:     t.convID,
		promptHash: promptHash,
		token:      token,
		server:     mcp.NewServer(&mcp.Implementation{Name: mirandaMCPServerName, Version: "1"}, nil),
		registered: make(map[string]string),
		lastUsed:   time.Now(),
	}
	s.server.AddReceivingMiddleware(s.observeRelist)
	// Tools must be registered before the process starts: the CLI lists
	// them once while initializing.
	s.syncTools(e, tools)
	e.mu.Lock()
	e.byToken[token] = s
	e.mu.Unlock()

	proc, err := claudecode.Start(claudecode.Options{
		Binary:       e.cfg.Binary,
		Model:        e.cfg.Model,
		SystemPrompt: t.stableSystem,
		MCPServers: map[string]claudecode.MCPServer{
			mirandaMCPServerName: {Type: "http", URL: e.baseURL, Headers: map[string]string{"Authorization": "Bearer " + token}},
		},
		BuiltinTools:   e.cfg.BuiltinTools,
		AllowedTools:   []string{"mcp__" + mirandaMCPServerName + "__*"},
		MaxTurns:       e.cfg.MaxTurns,
		AllowAPIKeyEnv: e.cfg.AllowAPIKeyEnv,
		ExtraArgs:      e.cfg.ExtraArgs,
	})
	if err != nil {
		e.mu.Lock()
		delete(e.byToken, token)
		e.mu.Unlock()
		return nil, false, fmt.Errorf("orchestrator: claude_code: %w", err)
	}
	s.proc = proc
	e.mu.Lock()
	e.sessions[t.userID] = s
	e.mu.Unlock()
	if e.logger != nil {
		e.logger.Info("orchestrator: claude_code: started process", "userId", t.userID, "conversationId", t.convID)
	}
	return s, true, nil
}

// dropSession stops s and forgets it (if it is still the user's current one).
func (e *claudeCodeEngine) dropSession(userID string, s *ccSession) {
	e.mu.Lock()
	if e.sessions[userID] == s {
		delete(e.sessions, userID)
	}
	delete(e.byToken, s.token)
	e.mu.Unlock()
	if s.proc != nil {
		_ = s.proc.Close()
	}
}

// reapIdle closes processes that served no turn for idle — skipping any
// user with a turn in flight (TryLock), which is by definition not idle.
func (e *claudeCodeEngine) reapIdle(idle time.Duration) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-e.stop:
			return
		case <-ticker.C:
		}
		e.mu.Lock()
		var stale []*ccSession
		for _, s := range e.sessions {
			s.mu.Lock()
			if time.Since(s.lastUsed) > idle {
				stale = append(stale, s)
			}
			s.mu.Unlock()
		}
		e.mu.Unlock()
		for _, s := range stale {
			e.mu.Lock()
			l := e.userLocks[s.userID]
			e.mu.Unlock()
			if l == nil || !l.TryLock() {
				continue
			}
			e.dropSession(s.userID, s)
			l.Unlock()
		}
	}
}

// close implements turnEngine.
func (e *claudeCodeEngine) close() {
	close(e.stop)
	e.mu.Lock()
	all := make([]*ccSession, 0, len(e.sessions))
	for _, s := range e.sessions {
		all = append(all, s)
	}
	e.mu.Unlock()
	for _, s := range all {
		e.dropSession(s.userID, s)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = e.httpSrv.Shutdown(ctx)
}

func (s *ccSession) begin(t *ccTurn) {
	s.mu.Lock()
	s.active = t
	s.lastUsed = time.Now()
	s.mu.Unlock()
}

func (s *ccSession) end() {
	s.mu.Lock()
	s.active = nil
	s.lastUsed = time.Now()
	s.mu.Unlock()
}

func (s *ccSession) activeTurn() *ccTurn {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.active
}

// syncTools makes s.server list exactly tools: availableTools is the single
// source of truth for which tools a turn offers (the source-dependent
// speak_reply/stop_speech withholding, lazy load_tool_group expansion, ...),
// so the claude_code orchestrator mirrors it rather than re-deriving any of
// those rules. Only differences are applied; the SDK debounces them into a
// tools/list_changed notification to the CLI.
func (s *ccSession) syncTools(e *claudeCodeEngine, tools []llm.ToolDef) {
	changed := false
	defer func() {
		if changed {
			s.mu.Lock()
			if s.relisted == nil {
				s.relisted = make(chan struct{})
			}
			s.mu.Unlock()
		}
	}()
	want := make(map[string]bool, len(tools))
	for _, td := range tools {
		want[td.Name] = true
		fp := toolFingerprint(td)
		s.mu.Lock()
		same := s.registered[td.Name] == fp
		s.mu.Unlock()
		if same {
			continue
		}
		s.server.AddTool(&mcp.Tool{Name: td.Name, Description: td.Description, InputSchema: objectSchema(td.Parameters)}, e.toolHandler(s, td.Name))
		changed = true
		s.mu.Lock()
		s.registered[td.Name] = fp
		s.mu.Unlock()
	}
	s.mu.Lock()
	var gone []string
	for name := range s.registered {
		if !want[name] {
			gone = append(gone, name)
			delete(s.registered, name)
		}
	}
	s.mu.Unlock()
	if len(gone) > 0 {
		s.server.RemoveTools(gone...)
		changed = true
	}
}

// observeRelist is Miranda MCP's receiving middleware: a tools/list request
// is the CLI acting on a tools/list_changed notification, which releases
// awaitRelist.
func (s *ccSession) observeRelist(next mcp.MethodHandler) mcp.MethodHandler {
	return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
		res, err := next(ctx, method, req)
		if method == "tools/list" {
			s.mu.Lock()
			if s.relisted != nil {
				close(s.relisted)
				s.relisted = nil
			}
			s.mu.Unlock()
		}
		return res, err
	}
}

// awaitRelist waits (bounded by relistTimeout, plus relistGrace once the
// re-fetch is seen) for the CLI to pick up a tool list syncTools just
// changed. Without it the message races the notification: measured against the real CLI, a turn sent right after the
// change still offered the model the previous turn's tools (e.g.
// speak_reply on an ha_assist turn). Proceeding after the timeout is safe —
// Miranda MCP itself refuses any tool not registered for this turn — it only
// risks the model attempting a call that then fails.
func (s *ccSession) awaitRelist(ctx context.Context) {
	s.mu.Lock()
	ch := s.relisted
	s.mu.Unlock()
	if ch == nil {
		return
	}
	select {
	case <-ch:
		select {
		case <-time.After(relistGrace):
		case <-ctx.Done():
		}
	case <-time.After(relistTimeout):
		s.mu.Lock()
		if s.relisted == ch {
			s.relisted = nil
		}
		s.mu.Unlock()
	case <-ctx.Done():
	}
}

// toolHandler is Miranda MCP's handler for one tool: it runs the call
// through the same executeTool/record path runAgentLoop uses, under the
// active turn's own ctx and control.
func (e *claudeCodeEngine) toolHandler(s *ccSession, name string) mcp.ToolHandler {
	return func(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		turn := s.activeTurn()
		if turn == nil {
			return toolErrorResult("no Miranda turn is in progress"), nil
		}

		s.toolMu.Lock()
		defer s.toolMu.Unlock()
		// Checked under toolMu: turn.allowed is only ever replaced by a
		// load_tool_group call below, which holds it too.
		if !turn.allowed[name] {
			return toolErrorResult(fmt.Sprintf("tool %q is not available on this turn", name)), nil
		}

		args := "{}"
		if len(req.Params.Arguments) > 0 {
			args = string(req.Params.Arguments)
		}
		tc := llm.ToolCall{ID: toolUseID(req), Name: name, Arguments: args}
		// Recorded as an assistant tool-call message followed by its result,
		// exactly like runAgentLoop does — so the conversation replays the
		// same way under either orchestrator. The text the model wrote
		// alongside the call isn't attached (the stream and the MCP call
		// arrive on different channels); the final reply is recorded by
		// Handle as usual.
		e.o.recordAssistantToolCallMessage(turn.ctx, s.userID, s.convID, "", []llm.ToolCall{tc})
		result := e.o.executeTool(turn.ctx, s.userID, s.convID, turn.source, tc, turn.control)
		e.o.recordToolCall(turn.ctx, s.userID, s.convID, tc, result)

		// load_tool_group grew the tool set — publish the expanded list for
		// the rest of this turn, same as runAgentLoop's own recompute.
		if turn.control.groupsChanged {
			tools := e.o.availableTools(turn.ctx, s.userID, turn.control)
			turn.control.groupsChanged = false
			s.syncTools(e, tools)
			turn.allowed = toolNameSet(tools)
		}
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: result}}}, nil
	}
}

// toolUseID reuses the CLI's own tool_use id (sent in _meta) so history and
// llm.log refer to the same call by the same id; falls back to a fresh one.
func toolUseID(req *mcp.CallToolRequest) string {
	if id, ok := req.Params.Meta["claudecode/toolUseId"].(string); ok && id != "" {
		return id
	}
	return "call_" + uuid.NewString()
}

func toolErrorResult(msg string) *mcp.CallToolResult {
	return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: msg}}}
}

func toolNameSet(tools []llm.ToolDef) map[string]bool {
	m := make(map[string]bool, len(tools))
	for _, t := range tools {
		m[t.Name] = true
	}
	return m
}

func toolFingerprint(td llm.ToolDef) string {
	b, _ := json.Marshal(struct {
		D string
		P map[string]any
	}{td.Description, td.Parameters})
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// objectSchema returns params as an MCP input schema. The SDK panics on a
// schema whose type isn't "object"; a parameterless ToolDef has a nil map.
func objectSchema(params map[string]any) map[string]any {
	if params == nil || params["type"] != "object" {
		return map[string]any{"type": "object", "properties": map[string]any{}}
	}
	return params
}

func randomToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("orchestrator: claude_code: token: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// composeClaudeCodeMessage builds the text sent to the process for one turn.
// The volatile half of the system prompt (the current time) is prefixed to
// every message, since the process's system prompt is fixed at start. A
// fresh process for a conversation that already has turns (after a restart,
// a crash, a memory change, or switching orchestrators mid-conversation)
// first gets the conversation so far, rendered from history — Miranda's
// history, not the CLI's, is the source of truth (the CLI persists nothing).
func composeClaudeCodeMessage(t engineTurn, fresh bool) string {
	var b strings.Builder
	if fresh && len(t.prior) > 0 {
		b.WriteString("Ниже — предыдущая часть этого разговора, восстановленная из истории Miranda. Продолжай его с учётом этого контекста; не пересказывай его.\n<previous_conversation>\n")
		b.WriteString(renderTranscript(t.prior))
		b.WriteString("</previous_conversation>\n\n")
	}
	b.WriteString("[")
	b.WriteString(t.volatileSystem)
	b.WriteString("]\n\n")
	b.WriteString(t.userContent)
	return b.String()
}

// transcriptToolResultMax bounds one replayed tool result — enough for the
// model to know what came back, without re-sending a huge document.
const transcriptToolResultMax = 1000

func renderTranscript(msgs []llm.Message) string {
	var b strings.Builder
	for _, m := range msgs {
		switch m.Role {
		case llm.RoleUser:
			fmt.Fprintf(&b, "[user]: %s\n", m.Content)
		case llm.RoleAssistant:
			if m.Content != "" {
				fmt.Fprintf(&b, "[assistant]: %s\n", m.Content)
			}
			for _, tc := range m.ToolCalls {
				fmt.Fprintf(&b, "[tool call] %s(%s)\n", tc.Name, tc.Arguments)
			}
		case llm.RoleTool:
			content := m.Content
			if r := []rune(content); len(r) > transcriptToolResultMax {
				content = string(r[:transcriptToolResultMax]) + "…"
			}
			fmt.Fprintf(&b, "[tool result]: %s\n", content)
		}
	}
	return b.String()
}

// ccTraceBuilder renders a claude_code turn into llm.log blocks, one per
// model call inside the CLI (a call ends where the CLI feeds tool results
// back). Responses use the Anthropic Messages shape ({"content":[...]}), so
// miranda-llm's llmtrace/analyze — and with it the anomaly detector and the
// llm-trace CLI — read them unchanged.
type ccTraceBuilder struct {
	request  any
	response []json.RawMessage
	started  time.Time
}

func newCCTraceBuilder(system, userText string, tools []llm.ToolDef) *ccTraceBuilder {
	names := make([]map[string]string, 0, len(tools))
	for _, t := range tools {
		names = append(names, map[string]string{"name": t.Name})
	}
	return &ccTraceBuilder{
		request: map[string]any{
			"system":   []map[string]string{{"type": "text", "text": system}},
			"messages": []any{map[string]any{"role": "user", "content": []any{map[string]string{"type": "text", "text": userText}}}},
			"tools":    names,
		},
		started: time.Now(),
	}
}

func (tb *ccTraceBuilder) addResponse(b claudecode.ContentBlock) {
	// The CLI doesn't expose thinking text (the blocks arrive empty), so
	// they would only add noise to the trace.
	if b.Type == "thinking" || b.Type == "redacted_thinking" {
		return
	}
	tb.response = append(tb.response, unprefixToolUse(b))
}

// unprefixToolUse strips the CLI's "mcp__miranda__" namespace from a
// tool_use block's name, so the trace names tools exactly as the request's
// tool list (and history, and the miranda orchestrator's traces) do —
// otherwise the anomaly detector would flag every call as an unknown tool.
func unprefixToolUse(b claudecode.ContentBlock) json.RawMessage {
	name, ok := strings.CutPrefix(b.Name, "mcp__"+mirandaMCPServerName+"__")
	if b.Type != "tool_use" || !ok {
		return b.Raw
	}
	var m map[string]any
	if json.Unmarshal(b.Raw, &m) != nil {
		return b.Raw
	}
	m["name"] = name
	out, err := json.Marshal(m)
	if err != nil {
		return b.Raw
	}
	return out
}

func (tb *ccTraceBuilder) nextRequest(results []claudecode.ContentBlock) {
	raws := make([]json.RawMessage, 0, len(results))
	for _, r := range results {
		raws = append(raws, r.Raw)
	}
	tb.request = map[string]any{"messages": []any{map[string]any{"role": "user", "content": raws}}}
	tb.started = time.Now()
}

// flush writes the pending block (if there is anything to write) and
// resets the response.
func (tb *ccTraceBuilder) flush(ctx context.Context, tracer llm.Tracer, err error) {
	if tracer == nil || (len(tb.response) == 0 && err == nil) {
		return
	}
	req, _ := json.Marshal(tb.request)
	var resp []byte
	if err == nil {
		resp, _ = json.Marshal(map[string]any{
			"content":     tb.response,
			"duration_ms": time.Since(tb.started).Milliseconds(),
		})
	}
	tracer.Trace(ctx, claudeCodeProviderName, string(req), string(resp), err)
	tb.response = nil
}
