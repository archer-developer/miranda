package agentloop

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"slices"
	"sort"
	"strings"
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"

	llm "github.com/archer-developer/miranda-llm"
	"github.com/archer-developer/miranda-llm/llmtest"
	"github.com/archer-developer/miranda-llm/llmtrace"
	"github.com/archer-developer/miranda-llm/llmtrace/analyze"
	"github.com/archer-developer/miranda/internal/claudecode"
	"github.com/archer-developer/miranda/internal/config"
	"github.com/archer-developer/miranda/internal/mcp"
	"github.com/archer-developer/miranda/internal/mcp/mcptest"
	"github.com/archer-developer/miranda/internal/users"
)

// fakeClaudeEnv switches this package's test binary into a fake `claude`
// CLI (see TestMain/runFakeClaude) — EnableClaudeCode is pointed at
// os.Args[0], so the claude_code orchestrator is exercised end to end
// (process, stream-json, Miranda MCP over real HTTP, executeTool, history)
// without the real CLI or any network.
const fakeClaudeEnv = "MIRANDA_FAKE_CLAUDE"

func TestMain(m *testing.M) {
	if os.Getenv(fakeClaudeEnv) == "1" {
		runFakeClaude()
		return
	}
	os.Exit(m.Run())
}

// runFakeClaude handles `auth status` and the stream-json session. On
// startup it connects to the MCP server named in --mcp-config, like the real
// CLI. Each message is answered with one result whose text reports what the
// fake saw: the tools listed to it, whether the message carried a replayed
// transcript, and — for a message ending in "CALL <tool> <json args>" — that
// tool's result as returned by Miranda MCP. A message ending in "crash" exits.
func runFakeClaude() {
	args := os.Args[1:]
	if slices.Equal(args, []string{"auth", "status"}) {
		fmt.Println(`{"loggedIn":true,"authMethod":"claude.ai","subscriptionType":"pro"}`)
		return
	}
	var mcpPath string
	if i := slices.Index(args, "--mcp-config"); i >= 0 {
		mcpPath = args[i+1]
	}
	raw, err := os.ReadFile(mcpPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "read mcp config:", err)
		os.Exit(2)
	}
	var cfg struct {
		MCPServers map[string]claudecode.MCPServer `json:"mcpServers"`
	}
	_ = json.Unmarshal(raw, &cfg)
	srv := cfg.MCPServers[mirandaMCPServerName]
	client, err := mcp.Connect(context.Background(), mirandaMCPServerName, srv.URL, strings.TrimPrefix(srv.Headers["Authorization"], "Bearer "))
	if err != nil {
		fmt.Fprintln(os.Stderr, "connect:", err)
		os.Exit(2)
	}

	in := bufio.NewScanner(os.Stdin)
	in.Buffer(make([]byte, 1<<20), 1<<20)
	out := json.NewEncoder(os.Stdout)
	for in.Scan() {
		var msg struct {
			Message struct {
				Content []map[string]any `json:"content"`
			} `json:"message"`
		}
		_ = json.Unmarshal(in.Bytes(), &msg)
		var text string
		for _, c := range msg.Message.Content {
			if c["type"] == "text" {
				text, _ = c["text"].(string)
			}
		}
		lastLine := text[strings.LastIndex(text, "\n")+1:]
		if lastLine == "crash" {
			fmt.Fprintln(os.Stderr, "boom")
			os.Exit(3)
		}

		tools, _ := client.ListTools(context.Background())
		names := make([]string, 0, len(tools))
		for _, t := range tools {
			names = append(names, t.Name)
		}
		sort.Strings(names)

		var toolResult string
		if rest, ok := strings.CutPrefix(lastLine, "CALL "); ok {
			name, callArgs, _ := strings.Cut(rest, " ")
			_ = out.Encode(map[string]any{"type": "assistant", "message": map[string]any{"id": "m1", "content": []any{
				map[string]any{"type": "tool_use", "id": "toolu_1", "name": "mcp__miranda__" + name, "input": json.RawMessage(callArgs)}}}})
			res, err := client.CallTool(context.Background(), name, callArgs)
			if err != nil {
				res = "ERR " + err.Error()
			}
			toolResult = res
			_ = out.Encode(map[string]any{"type": "user", "message": map[string]any{"content": []any{
				map[string]any{"type": "tool_result", "tool_use_id": "toolu_1", "content": res}}}})
		}
		reply := fmt.Sprintf("tools=%s prev=%v result=%s", strings.Join(names, ","), strings.Contains(text, "<previous_conversation>"), toolResult)
		_ = out.Encode(map[string]any{"type": "assistant", "message": map[string]any{"id": "m2", "content": []any{map[string]any{"type": "text", "text": reply}}}})
		_ = out.Encode(map[string]any{"type": "result", "subtype": "success", "result": reply, "num_turns": 2})
	}
}

// newClaudeCodeTestOrchestrator is newTestOrchestrator switched to the
// claude_code orchestrator backed by the fake CLI. The FakeProvider is never
// asked for a turn — it only satisfies the router, which summarization
// would still use.
func newClaudeCodeTestOrchestrator(t *testing.T) *Orchestrator {
	t.Helper()
	t.Setenv(fakeClaudeEnv, "1")
	o, _, _ := newTestOrchestrator(t, llmtest.New("local"))
	require.NoError(t, o.EnableClaudeCode(config.ClaudeCodeConfig{Binary: os.Args[0], MaxTurns: 5}, nil))
	t.Cleanup(o.Close)
	return o
}

func TestClaudeCode_ToolCallRunsThroughMirandaAndIsRecorded(t *testing.T) {
	o := newClaudeCodeTestOrchestrator(t)

	resp, err := o.Handle(context.Background(), InputRequest{Source: WebUISource, UserID: "alex",
		Text: `CALL remember_this {"fact":"allergic to cats"}`})
	require.NoError(t, err)
	require.Equal(t, claudeCodeProviderName, resp.ProviderUsed)
	require.Contains(t, resp.Reply, "result=remembered")
	require.Contains(t, listedTools(resp.Reply), rememberToolName, "Miranda's built-in tools must be listed over Miranda MCP")

	content, err := o.memory.Read("alex")
	require.NoError(t, err)
	require.Contains(t, content, "allergic to cats", "the tool must have run through executeTool")

	msgs, err := o.history.ConversationMessages(context.Background(), resp.ConversationID)
	require.NoError(t, err)
	roles := make([]string, 0, len(msgs))
	for _, m := range msgs {
		roles = append(roles, m.Role)
	}
	require.Equal(t, []string{"user", "assistant", "tool", "assistant"}, roles,
		"a tool call must be recorded like the miranda orchestrator records it")
	require.NotEmpty(t, msgs[1].ToolCalls[0].ID)
	require.Equal(t, msgs[1].ToolCalls[0].ID, msgs[2].ToolCallID, "the tool result must answer the recorded call")
	require.Equal(t, "remember_this", msgs[1].ToolCalls[0].Name)
}

func TestClaudeCode_ProcessIsReusedThenReplaysHistoryAfterRestart(t *testing.T) {
	o := newClaudeCodeTestOrchestrator(t)
	ctx := context.Background()

	first, err := o.Handle(ctx, InputRequest{Source: WebUISource, UserID: "alex", Text: "привет"})
	require.NoError(t, err)
	require.Contains(t, first.Reply, "prev=false", "a new conversation has nothing to replay")

	second, err := o.Handle(ctx, InputRequest{Source: WebUISource, UserID: "alex", Text: "как дела"})
	require.NoError(t, err)
	require.Equal(t, first.ConversationID, second.ConversationID)
	require.Contains(t, second.Reply, "prev=false", "a live process already holds the context — no replay")

	e := o.engine.(*claudeCodeEngine)
	e.mu.Lock()
	sess := e.sessions["alex"]
	e.mu.Unlock()
	e.dropSession("alex", sess) // e.g. reaped as idle, or Miranda restarted

	third, err := o.Handle(ctx, InputRequest{Source: WebUISource, UserID: "alex", Text: "продолжим"})
	require.NoError(t, err)
	require.Equal(t, first.ConversationID, third.ConversationID)
	require.Contains(t, third.Reply, "prev=true", "a fresh process must get the conversation replayed from history")
}

func TestClaudeCode_ToolListFollowsAvailableToolsPerTurn(t *testing.T) {
	t.Setenv(fakeClaudeEnv, "1")
	o, tts := newTestOrchestratorWithTTS(t, llmtest.New("local"))
	require.NoError(t, o.EnableClaudeCode(config.ClaudeCodeConfig{Binary: os.Args[0]}, nil))
	t.Cleanup(o.Close)
	ctx := context.Background()

	web, err := o.Handle(ctx, InputRequest{Source: WebUISource, UserID: "alex", Text: "hi"})
	require.NoError(t, err)
	require.Contains(t, listedTools(web.Reply), speakReplyToolName)

	// An ha_assist turn is voiced by the Assist pipeline: availableTools
	// withholds speak_reply there, and Miranda MCP must follow it.
	voice, err := o.Handle(ctx, InputRequest{Source: users.SourceHAAssist, UserID: "alex",
		Text: `CALL speak_reply {"text":"x"}`})
	require.NoError(t, err)
	require.NotContains(t, listedTools(voice.Reply), speakReplyToolName)
	require.Contains(t, voice.Reply, "unknown tool", "a withheld tool is not registered on Miranda MCP at all")
	requireNeverSpoken(t, tts)
}

// TestClaudeCode_LazyGroupsAreExpandedUpFront is the regression test for
// the first production turn: the CLI applies tools/list_changed only to its
// next message, so a lazy group loaded mid-turn via load_tool_group never
// became callable. Under claude_code every lazy group's real tools must be
// listed from the start, with no load_tool_group stub at all.
func TestClaudeCode_LazyGroupsAreExpandedUpFront(t *testing.T) {
	t.Setenv(fakeClaudeEnv, "1")
	diary := mcptest.New("diary", llm.ToolDef{Name: "add_entry", Parameters: map[string]any{"type": "object"}}).
		WithResult("add_entry", "saved")
	o, _, _ := newTestOrchestrator(t, llmtest.New("local"), diary)
	o.SetLazyMCPServers(map[string]string{"diary": "Personal diary."})
	require.NoError(t, o.EnableClaudeCode(config.ClaudeCodeConfig{Binary: os.Args[0]}, nil))
	t.Cleanup(o.Close)

	resp, err := o.Handle(context.Background(), InputRequest{Source: WebUISource, UserID: "alex",
		Text: `CALL diary_add_entry {"text":"подключила Claude Code"}`})
	require.NoError(t, err)
	require.Contains(t, listedTools(resp.Reply), "diary_add_entry")
	require.NotContains(t, listedTools(resp.Reply), loadToolGroupToolName)
	require.Contains(t, resp.Reply, "result=saved", "the lazy server's tool must be callable on the first turn")
}

func TestClaudeCode_FailedTurnFallsBackAndRecovers(t *testing.T) {
	o := newClaudeCodeTestOrchestrator(t)
	ctx := context.Background()

	resp, err := o.Handle(ctx, InputRequest{Source: WebUISource, UserID: "alex", Text: "crash"})
	require.NoError(t, err)
	require.Equal(t, o.turnFailureReply("alex"), resp.Reply)

	again, err := o.Handle(ctx, InputRequest{Source: WebUISource, UserID: "alex", Text: "ещё раз"})
	require.NoError(t, err)
	require.Contains(t, again.Reply, "prev=true", "the crashed process is replaced and the conversation replayed")
}

// TestClaudeCode_TraceBlocksParseLikeAnthropic guards llm.log: a claude_code
// turn must write blocks llmtrace/analyze reads — one per model call, with
// tool names unprefixed — so the anomaly detector and `miranda llm-trace`
// work for this orchestrator without any change in miranda-llm.
func TestClaudeCode_TraceBlocksParseLikeAnthropic(t *testing.T) {
	t.Setenv(fakeClaudeEnv, "1")
	o, _, _ := newTestOrchestrator(t, llmtest.New("local"))
	var buf bytes.Buffer
	require.NoError(t, o.EnableClaudeCode(config.ClaudeCodeConfig{Binary: os.Args[0]}, llmtrace.New(&buf)))
	t.Cleanup(o.Close)

	_, err := o.Handle(context.Background(), InputRequest{Source: WebUISource, UserID: "alex",
		Text: `CALL remember_this {"fact":"x"}`})
	require.NoError(t, err)

	blocks, err := analyze.ParseAll(&buf)
	require.NoError(t, err)
	require.Len(t, blocks, 2, "one block for the tool-calling model call, one for the final reply")
	require.Equal(t, claudeCodeProviderName, blocks[0].Provider)
	require.NotEmpty(t, blocks[0].Conversation)
	calls := analyze.ExtractToolCalls(blocks[0])
	require.Len(t, calls, 1)
	require.Equal(t, rememberToolName, calls[0].Name, "the mcp__miranda__ prefix must be stripped")
	require.Contains(t, blocks[1].Request, "tool_result")
	require.Empty(t, analyze.ExtractToolCalls(blocks[1]))
}

func TestClaudeCode_MirandaMCPRejectsUnknownToken(t *testing.T) {
	o := newClaudeCodeTestOrchestrator(t)
	e := o.engine.(*claudeCodeEngine)

	for _, auth := range []string{"", "Bearer nope"} {
		req, err := http.NewRequest(http.MethodPost, e.baseURL, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
		require.NoError(t, err)
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		_ = resp.Body.Close()
		require.Equal(t, http.StatusUnauthorized, resp.StatusCode, "auth %q", auth)
	}
}

func TestToolUseIDReusesTheCLIsID(t *testing.T) {
	req := &sdkmcp.CallToolRequest{Params: &sdkmcp.CallToolParamsRaw{Meta: sdkmcp.Meta{"claudecode/toolUseId": "toolu_9"}}}
	require.Equal(t, "toolu_9", toolUseID(req))
	require.True(t, strings.HasPrefix(toolUseID(&sdkmcp.CallToolRequest{Params: &sdkmcp.CallToolParamsRaw{}}), "call_"))
}

// listedTools extracts the fake CLI's "tools=a,b,c" field from its reply.
func listedTools(reply string) []string {
	field, _, _ := strings.Cut(strings.TrimPrefix(reply, "tools="), " ")
	return strings.Split(field, ",")
}

func TestComposeClaudeCodeMessage(t *testing.T) {
	turn := engineTurn{volatileSystem: "Текущее время пользователя: 10:00.", userContent: "привет"}
	require.Equal(t, "[Текущее время пользователя: 10:00.]\n\nпривет", composeClaudeCodeMessage(turn, true))
}
