package agentloop

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/archer-developer/miranda-llm/llmtest"
	"github.com/archer-developer/miranda/internal/config"
	"github.com/archer-developer/miranda/internal/users"
)

// TestClaudeCodeE2E drives the claude_code orchestrator against the REAL
// Claude Code CLI (logged in on this machine; uses the subscription and the
// shipped defaults — model sonnet, WebSearch/WebFetch on). Skipped unless MIRANDA_CLAUDE_E2E=1 — it needs the CLI, network
// and a login, and its assertions depend on model behavior. Run with:
//
//	MIRANDA_CLAUDE_E2E=1 go test ./internal/agent_loop/ -run ClaudeCodeE2E -v
func TestClaudeCodeE2E(t *testing.T) {
	if os.Getenv("MIRANDA_CLAUDE_E2E") != "1" {
		t.Skip("set MIRANDA_CLAUDE_E2E=1 to run against the real claude CLI")
	}
	o, tts := newTestOrchestratorWithTTS(t, llmtest.New("local"))
	// The shipped defaults: model sonnet, WebSearch/WebFetch built-ins on.
	cfg := config.Default().Agent.ClaudeCode
	require.NoError(t, o.EnableClaudeCode(cfg, nil))
	t.Cleanup(o.Close)
	ctx := context.Background()

	// 1. A real tool call through Miranda MCP.
	r1, err := o.Handle(ctx, InputRequest{Source: WebUISource, UserID: "alex",
		Text: "Запомни с помощью тула remember_this: я люблю зелёный чай. Ответь одним словом."})
	require.NoError(t, err)
	t.Logf("turn 1: %q", r1.Reply)
	mem, err := o.memory.Read("alex")
	require.NoError(t, err)
	require.Contains(t, strings.ToLower(mem), "чай", "remember_this must have run through executeTool")

	// 2. Context is held by the live process.
	r2, err := o.Handle(ctx, InputRequest{Source: WebUISource, UserID: "alex",
		Text: "Какой напиток я только что просил запомнить? Ответь одним словом, тулы не вызывай."})
	require.NoError(t, err)
	t.Logf("turn 2: %q", r2.Reply)
	require.Contains(t, strings.ToLower(r2.Reply), "чай")

	// 3. The tool list follows availableTools per turn: speak_reply exists
	// on a web turn and is withdrawn (tools/list_changed) on an ha_assist one.
	r3, err := o.Handle(ctx, InputRequest{Source: WebUISource, UserID: "alex",
		Text: "Перечисли через запятую точные имена всех доступных тебе тулов, без пояснений."})
	require.NoError(t, err)
	t.Logf("turn 3 (web): %q", r3.Reply)
	require.Contains(t, r3.Reply, speakReplyToolName)

	r4, err := o.Handle(ctx, InputRequest{Source: users.SourceHAAssist, UserID: "alex",
		Text: "Список тулов мог измениться. Перечисли через запятую точные имена всех доступных тебе СЕЙЧАС тулов, без пояснений."})
	require.NoError(t, err)
	t.Logf("turn 4 (ha_assist): %q", r4.Reply)
	// Asserted on what the CLI itself reported offering the model (its
	// system/init event), not on the reply — the model may simply repeat
	// turn 3's list from context.
	e := o.engine.(*claudeCodeEngine)
	e.mu.Lock()
	sess := e.sessions["alex"]
	e.mu.Unlock()
	sess.mu.Lock()
	offered := sess.initTools
	sess.mu.Unlock()
	t.Logf("turn 4 offered by CLI: %v", offered)
	require.NotContains(t, offered, "mcp__miranda__"+speakReplyToolName, "speak_reply must be withdrawn on an ha_assist turn")
	require.Contains(t, offered, "mcp__miranda__"+rememberToolName)
	require.Contains(t, offered, "WebSearch", "the configured built-in web tools must be offered")
	requireNeverSpoken(t, tts)

	// 4. The built-in web tools actually work under the subscription.
	r5, err := o.Handle(ctx, InputRequest{Source: WebUISource, UserID: "alex",
		Text: "Найди через WebSearch текущую стабильную версию Go на go.dev и ответь только номером версии."})
	require.NoError(t, err)
	t.Logf("turn 5 (web search): %q", r5.Reply)
	require.Equal(t, claudeCodeProviderName, r5.ProviderUsed)
	require.Regexp(t, `1\.\d+`, r5.Reply)
}
