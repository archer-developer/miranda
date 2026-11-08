// Package claudecode drives one long-lived headless Claude Code CLI process
// over its stream-json protocol (`claude -p --input-format stream-json
// --output-format stream-json`): write one user message to stdin, read
// NDJSON events from stdout until that message's "result" event, repeat.
// The process keeps its own conversation context between turns, so a caller
// only ever sends the new message.
//
// This package knows nothing about Miranda — no history, memory or tools.
// It only owns the process lifecycle and the wire protocol; the
// claude_code orchestrator in internal/agent_loop layers Miranda's
// semantics on top. See docs/adr/claude-code-orchestrator.md.
package claudecode

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// MCPServer is one --mcp-config entry, in the CLI's own JSON shape. Only
// the HTTP transport is modeled — the one the orchestrator's loopback
// Miranda MCP endpoint uses.
type MCPServer struct {
	Type    string            `json:"type"` // "http"
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers,omitempty"`
}

// Options configures Start.
type Options struct {
	// Binary is the CLI executable ("claude" when empty).
	Binary string
	// Model is passed as --model; "" keeps the CLI default.
	Model string
	// SystemPrompt fully replaces Claude Code's own default system prompt
	// (--system-prompt-file) — the process acts as Miranda, not as a coding
	// agent. Fixed for the process's lifetime: a caller that needs a
	// different prompt starts a new process.
	SystemPrompt string
	// MCPServers are the only MCP servers the process sees
	// (--strict-mcp-config ignores the user's own ~/.claude config).
	MCPServers map[string]MCPServer
	// BuiltinTools are the Claude Code built-in tools enabled for the
	// process (--tools), e.g. "WebSearch", "WebFetch". Empty disables every
	// built-in tool. Each one is also auto-approved.
	BuiltinTools []string
	// AllowedTools are auto-approved without a permission prompt (in -p
	// mode an unapproved tool is simply denied), e.g. "mcp__miranda__*".
	AllowedTools []string
	// MaxTurns bounds one Turn's internal agent loop (--max-turns); 0
	// leaves the CLI default.
	MaxTurns int
	// AllowAPIKeyEnv keeps ANTHROPIC_API_KEY/ANTHROPIC_AUTH_TOKEN in the
	// child's environment — see config.ClaudeCodeConfig.AllowAPIKeyEnv.
	AllowAPIKeyEnv bool
	// ExtraArgs are appended verbatim after every flag Start sets itself.
	ExtraArgs []string
}

// Image is one base64-encoded image attached to a user message.
type Image struct {
	MIMEType string
	Base64   string
}

// UserMessage is what one Turn sends.
type UserMessage struct {
	Text   string
	Images []Image
}

// ContentBlock is one Anthropic Messages API content block as it appears in
// stream-json "assistant"/"user" events — text, tool_use, tool_result,
// thinking. Only the fields the orchestrator reads are modeled; Raw keeps
// the block verbatim for tracing.
type ContentBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	IsError   bool            `json:"is_error,omitempty"`
	Raw       json.RawMessage `json:"-"`
}

// Handler receives a Turn's events as they stream in. Every field is
// optional. Callbacks run on the goroutine that called Turn.
type Handler struct {
	// OnInit receives the tool names the CLI offers the model for this turn
	// (its per-turn "system/init" event) — after any tools/list_changed
	// refresh, so it shows what the model can actually call.
	OnInit func(tools []string)
	// OnTextDelta receives assistant text as it is generated (from
	// --include-partial-messages), for live display.
	OnTextDelta func(delta string)
	// OnAssistant receives each completed assistant content block — the CLI
	// emits one "assistant" event per block, so a response with text plus
	// two tool calls arrives as three calls sharing messageID.
	OnAssistant func(messageID string, block ContentBlock)
	// OnToolResults receives the tool results the CLI fed back to the model
	// after executing its tool calls — the boundary between two model calls
	// inside one Turn.
	OnToolResults func(blocks []ContentBlock)
}

// Result is a Turn's terminal "result" event.
type Result struct {
	// Subtype is "success", or an error kind such as "error_max_turns" /
	// "error_during_execution".
	Subtype string
	IsError bool
	// Text is the final assistant text (the "result" field).
	Text       string
	NumTurns   int
	DurationMS int64
	// CostUSD is the CLI's API-price estimate; under a subscription login
	// nothing is actually billed per token, but it still tracks usage.
	CostUSD   float64
	SessionID string
	Usage     json.RawMessage
}

// ErrProcessExited is returned by Turn when the process ended before
// producing that turn's result. The Process is unusable afterwards.
var ErrProcessExited = errors.New("claudecode: process exited")

// Process is one running CLI process. Turns are serialized: Turn holds an
// internal lock for its whole duration.
type Process struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	events chan json.RawMessage
	// exited is closed once the stdout reader sees EOF (the process is gone
	// or going); waitErr is valid after that.
	exited  chan struct{}
	waitErr error
	stderr  *tailBuffer
	dir     string

	turnMu    sync.Mutex
	closeOnce sync.Once
}

// Start launches the CLI. The process runs in a private temporary working
// directory holding its system prompt and MCP config (both may contain
// sensitive values — memory, the MCP bearer token — so the directory is
// 0700 and removed by Close). No conversation transcript is persisted
// (--no-session-persistence), none of the invoking user's own Claude Code
// settings, hooks, skills or MCP servers are loaded, and only
// opts.BuiltinTools of the built-in tools are enabled — beyond those, the
// process can only act through opts.MCPServers.
func Start(opts Options) (*Process, error) {
	binary := opts.Binary
	if binary == "" {
		binary = "claude"
	}
	dir, err := os.MkdirTemp("", "miranda-claude-")
	if err != nil {
		return nil, fmt.Errorf("claudecode: create work dir: %w", err)
	}
	cleanup := func() { _ = os.RemoveAll(dir) }

	promptPath := filepath.Join(dir, "system-prompt.md")
	if err := os.WriteFile(promptPath, []byte(opts.SystemPrompt), 0o600); err != nil {
		cleanup()
		return nil, fmt.Errorf("claudecode: write system prompt: %w", err)
	}
	mcpJSON, err := json.Marshal(map[string]any{"mcpServers": opts.MCPServers})
	if err != nil {
		cleanup()
		return nil, fmt.Errorf("claudecode: encode mcp config: %w", err)
	}
	mcpPath := filepath.Join(dir, "mcp.json")
	if err := os.WriteFile(mcpPath, mcpJSON, 0o600); err != nil {
		cleanup()
		return nil, fmt.Errorf("claudecode: write mcp config: %w", err)
	}

	cmd := exec.Command(binary, buildArgs(opts, promptPath, mcpPath)...)
	cmd.Dir = dir
	cmd.Env = childEnv(os.Environ(), opts.AllowAPIKeyEnv)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		cleanup()
		return nil, fmt.Errorf("claudecode: stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cleanup()
		return nil, fmt.Errorf("claudecode: stdout pipe: %w", err)
	}
	tail := &tailBuffer{max: 4096}
	cmd.Stderr = tail
	if err := cmd.Start(); err != nil {
		cleanup()
		return nil, fmt.Errorf("claudecode: start %s: %w", binary, err)
	}

	p := &Process{
		cmd:    cmd,
		stdin:  stdin,
		events: make(chan json.RawMessage, 64),
		exited: make(chan struct{}),
		stderr: tail,
		dir:    dir,
	}
	go p.readLoop(stdout)
	return p, nil
}

// buildArgs assembles the CLI command line — split out of Start so the
// exact flag set is unit-testable without spawning anything.
func buildArgs(opts Options, promptPath, mcpPath string) []string {
	args := []string{
		"-p",
		"--input-format", "stream-json",
		"--output-format", "stream-json",
		"--verbose", // required by -p together with stream-json output
		"--include-partial-messages",
		"--no-session-persistence",
		// Only the listed built-in tools exist for the model; an empty value
		// disables all of them. MCP tools are unaffected.
		"--tools", strings.Join(opts.BuiltinTools, ","),
		"--strict-mcp-config",
		"--mcp-config", mcpPath,
		// Ignore the invoking OS user's own user/project/local settings
		// (their hooks, permissions, plugins). --bare would be stricter, but
		// it never reads the OAuth login, i.e. the subscription.
		"--setting-sources", "",
		"--disable-slash-commands",
		"--system-prompt-file", promptPath,
	}
	if allowed := append(append([]string{}, opts.BuiltinTools...), opts.AllowedTools...); len(allowed) > 0 {
		args = append(args, "--allowedTools", strings.Join(allowed, ","))
	}
	if opts.Model != "" {
		args = append(args, "--model", opts.Model)
	}
	if opts.MaxTurns > 0 {
		args = append(args, "--max-turns", strconv.Itoa(opts.MaxTurns))
	}
	return append(args, opts.ExtraArgs...)
}

// AuthStatus is the subset of `claude auth status` the orchestrator logs.
type AuthStatus struct {
	LoggedIn         bool   `json:"loggedIn"`
	AuthMethod       string `json:"authMethod"`
	SubscriptionType string `json:"subscriptionType"`
}

// CheckAuth runs `claude auth status` with exactly the environment Start
// gives a turn (childEnv) — otherwise an ANTHROPIC_API_KEY in Miranda's own
// environment makes the CLI report "api_key" even though every turn, run
// with that key stripped, actually uses the subscription login.
func CheckAuth(ctx context.Context, binary string, allowAPIKeyEnv bool) (AuthStatus, error) {
	cmd := exec.CommandContext(ctx, binary, "auth", "status")
	cmd.Env = childEnv(os.Environ(), allowAPIKeyEnv)
	out, err := cmd.Output()
	if err != nil {
		return AuthStatus{}, fmt.Errorf("claudecode: auth status: %w", err)
	}
	var st AuthStatus
	if err := json.Unmarshal(out, &st); err != nil {
		return AuthStatus{}, fmt.Errorf("claudecode: parse auth status: %w", err)
	}
	return st, nil
}

// childEnv is the process environment minus the API-key variables (unless
// allowAPIKey), so the CLI authenticates with its own subscription login.
// CLAUDECODE is dropped too: it marks "running inside Claude Code", which
// is only ever true by accident here (a developer launching Miranda from a
// Claude Code session) and changes the CLI's behavior.
func childEnv(env []string, allowAPIKey bool) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		name, _, _ := strings.Cut(kv, "=")
		switch name {
		case "ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN":
			if !allowAPIKey {
				continue
			}
		case "CLAUDECODE":
			continue
		}
		out = append(out, kv)
	}
	return out
}

// readLoop forwards stdout's NDJSON lines to p.events until EOF, then reaps
// the process. A bufio.Reader (not Scanner) since a single event carrying a
// large tool result or image has no useful upper bound.
func (p *Process) readLoop(stdout io.Reader) {
	r := bufio.NewReader(stdout)
	for {
		line, err := r.ReadBytes('\n')
		if line = bytes.TrimSpace(line); len(line) > 0 && json.Valid(line) {
			p.events <- json.RawMessage(line)
		}
		if err != nil {
			break
		}
	}
	p.waitErr = p.cmd.Wait()
	close(p.exited)
	close(p.events)
}

// Alive reports whether the process is still running.
func (p *Process) Alive() bool {
	select {
	case <-p.exited:
		return false
	default:
		return true
	}
}

// Turn sends msg and blocks until its "result" event, streaming
// intermediate events to h. If ctx ends first the process is killed — its
// conversation state is then unknown, so a half-finished turn can't leak
// into the next one — and ctx.Err() is returned; the caller must Start a
// new Process.
func (p *Process) Turn(ctx context.Context, msg UserMessage, h Handler) (Result, error) {
	p.turnMu.Lock()
	defer p.turnMu.Unlock()

	if !p.Alive() {
		return Result{}, p.exitError()
	}
	line, err := encodeUserMessage(msg)
	if err != nil {
		return Result{}, err
	}
	if _, err := p.stdin.Write(line); err != nil {
		return Result{}, fmt.Errorf("claudecode: write message: %w", err)
	}

	for {
		select {
		case <-ctx.Done():
			p.kill()
			return Result{}, ctx.Err()
		case raw, ok := <-p.events:
			if !ok {
				return Result{}, p.exitError()
			}
			if res, done := dispatch(raw, h); done {
				return res, nil
			}
		}
	}
}

// encodeUserMessage renders msg as one stream-json input line.
func encodeUserMessage(msg UserMessage) ([]byte, error) {
	content := make([]map[string]any, 0, 1+len(msg.Images))
	for _, img := range msg.Images {
		content = append(content, map[string]any{
			"type":   "image",
			"source": map[string]any{"type": "base64", "media_type": img.MIMEType, "data": img.Base64},
		})
	}
	content = append(content, map[string]any{"type": "text", "text": msg.Text})
	line, err := json.Marshal(map[string]any{
		"type":    "user",
		"message": map[string]any{"role": "user", "content": content},
	})
	if err != nil {
		return nil, fmt.Errorf("claudecode: encode message: %w", err)
	}
	return append(line, '\n'), nil
}

// event is the union of the stream-json output event fields dispatch reads.
type event struct {
	Type    string `json:"type"`
	Subtype string `json:"subtype"`
	Message struct {
		ID      string            `json:"id"`
		Content []json.RawMessage `json:"content"`
	} `json:"message"`
	Event struct {
		Type  string `json:"type"`
		Delta struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"delta"`
	} `json:"event"`
	Tools        []string        `json:"tools"`
	IsError      bool            `json:"is_error"`
	Result       string          `json:"result"`
	NumTurns     int             `json:"num_turns"`
	DurationMS   int64           `json:"duration_ms"`
	TotalCostUSD float64         `json:"total_cost_usd"`
	SessionID    string          `json:"session_id"`
	Usage        json.RawMessage `json:"usage"`
}

// dispatch routes one output event to h, reporting done with the decoded
// Result once it sees the turn's "result" event. Events it doesn't model
// (system/init, rate_limit_event, hook events, ...) are ignored.
func dispatch(raw json.RawMessage, h Handler) (Result, bool) {
	var ev event
	if err := json.Unmarshal(raw, &ev); err != nil {
		return Result{}, false
	}
	switch ev.Type {
	case "system":
		if ev.Subtype == "init" && h.OnInit != nil {
			h.OnInit(ev.Tools)
		}
	case "stream_event":
		if ev.Event.Type == "content_block_delta" && ev.Event.Delta.Type == "text_delta" && h.OnTextDelta != nil {
			h.OnTextDelta(ev.Event.Delta.Text)
		}
	case "assistant":
		if h.OnAssistant != nil {
			for _, b := range decodeBlocks(ev.Message.Content) {
				h.OnAssistant(ev.Message.ID, b)
			}
		}
	case "user":
		// Only tool results are interesting: the CLI also echoes plain user
		// text in some modes (--replay-user-messages), which isn't news.
		var results []ContentBlock
		for _, b := range decodeBlocks(ev.Message.Content) {
			if b.Type == "tool_result" {
				results = append(results, b)
			}
		}
		if len(results) > 0 && h.OnToolResults != nil {
			h.OnToolResults(results)
		}
	case "result":
		return Result{
			Subtype:    ev.Subtype,
			IsError:    ev.IsError,
			Text:       ev.Result,
			NumTurns:   ev.NumTurns,
			DurationMS: ev.DurationMS,
			CostUSD:    ev.TotalCostUSD,
			SessionID:  ev.SessionID,
			Usage:      ev.Usage,
		}, true
	}
	return Result{}, false
}

func decodeBlocks(raws []json.RawMessage) []ContentBlock {
	out := make([]ContentBlock, 0, len(raws))
	for _, r := range raws {
		var b ContentBlock
		if json.Unmarshal(r, &b) != nil {
			continue
		}
		b.Raw = r
		out = append(out, b)
	}
	return out
}

func (p *Process) exitError() error {
	<-p.exited
	if tail := strings.TrimSpace(p.stderr.String()); tail != "" {
		return fmt.Errorf("%w (%v): %s", ErrProcessExited, p.waitErr, tail)
	}
	return fmt.Errorf("%w (%v)", ErrProcessExited, p.waitErr)
}

func (p *Process) kill() {
	if p.cmd.Process != nil {
		_ = p.cmd.Process.Kill()
	}
}

// Close ends the process — gracefully first (closing stdin makes the CLI
// exit after any in-flight turn), killing it if it hasn't exited within a
// few seconds — and removes its working directory. Safe to call more than
// once and concurrently with a Turn (which then fails with
// ErrProcessExited).
func (p *Process) Close() error {
	p.closeOnce.Do(func() {
		// Drain whatever is still buffered or in flight: readLoop blocks on
		// a full p.events with nobody reading, and would then never reach
		// the Wait that closes p.exited.
		go func() {
			for range p.events {
			}
		}()
		_ = p.stdin.Close()
		select {
		case <-p.exited:
		case <-time.After(5 * time.Second):
			p.kill()
			<-p.exited
		}
		_ = os.RemoveAll(p.dir)
	})
	return nil
}

// tailBuffer keeps the last max bytes written to it — the process's stderr,
// surfaced in ErrProcessExited so a crash (or "not logged in") explains
// itself without an unbounded buffer.
type tailBuffer struct {
	mu  sync.Mutex
	max int
	buf []byte
}

func (t *tailBuffer) Write(b []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, b...)
	if over := len(t.buf) - t.max; over > 0 {
		t.buf = t.buf[over:]
	}
	return len(b), nil
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.buf)
}
