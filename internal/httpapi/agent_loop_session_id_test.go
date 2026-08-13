package httpapi

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"

	llm "github.com/archer-developer/miranda-llm"
	"github.com/archer-developer/miranda-llm/llmtest"
	"github.com/archer-developer/miranda/internal/mcp/mcptest"
)

func TestExecuteTool_InjectsSessionIDForAllowedTool(t *testing.T) {
	medical := mcptest.New("medical_card", llm.ToolDef{Name: "medical.ask"}).WithResult("medical.ask", "some answer")
	provider := llmtest.New("local",
		llmtest.Response{ToolCall: &llm.ToolCall{ID: "call-1", Name: "medical_card_medical.ask", Arguments: `{"question":"hi"}`}},
		llmtest.Response{Text: "Done."},
	)
	o, _, _ := newTestOrchestrator(t, provider, medical)
	o.SetMCPServerExtensions(map[string]MCPServerExtension{
		"medical_card": {SessionIDArg: "sessionId", SessionIDTools: map[string]bool{"medical.ask": true}},
	}, 0)

	resp, err := o.Handle(context.Background(), InputRequest{Source: "cli", UserID: "alex", Text: "ask the medical card"})
	require.NoError(t, err)
	require.Equal(t, "Done.", resp.Reply)

	require.Len(t, medical.Calls, 1)
	var wireArgs map[string]any
	require.NoError(t, json.Unmarshal([]byte(medical.Calls[0].ArgumentsJSON), &wireArgs))
	require.Equal(t, resp.ConversationID, wireArgs["sessionId"], "the injected sessionId must equal this turn's resolved conversation id")
	require.Equal(t, "hi", wireArgs["question"])
}

// TestExecuteTool_InjectsSessionIDUnderServerConfiguredArgName guards
// per-server argument-name overrides working the same way EncryptionKeyArg
// does — see TestExecuteTool_InjectsUnderServerConfiguredArgName.
func TestExecuteTool_InjectsSessionIDUnderServerConfiguredArgName(t *testing.T) {
	medical := mcptest.New("medical_card", llm.ToolDef{Name: "medical.ask"}).WithResult("medical.ask", "some answer")
	provider := llmtest.New("local",
		llmtest.Response{ToolCall: &llm.ToolCall{ID: "call-1", Name: "medical_card_medical.ask", Arguments: `{"question":"hi"}`}},
		llmtest.Response{Text: "Done."},
	)
	o, _, _ := newTestOrchestrator(t, provider, medical)
	o.SetMCPServerExtensions(map[string]MCPServerExtension{
		"medical_card": {SessionIDArg: "conversation_session", SessionIDTools: map[string]bool{"medical.ask": true}},
	}, 0)

	resp, err := o.Handle(context.Background(), InputRequest{Source: "cli", UserID: "alex", Text: "ask the medical card"})
	require.NoError(t, err)
	require.Equal(t, "Done.", resp.Reply)

	require.Len(t, medical.Calls, 1)
	var wireArgs map[string]any
	require.NoError(t, json.Unmarshal([]byte(medical.Calls[0].ArgumentsJSON), &wireArgs))
	require.Contains(t, wireArgs, "conversation_session", "must use this server's configured arg name")
	require.NotContains(t, wireArgs, "sessionId", "must not also inject under the default name")
}

// TestExecuteTool_NeverInjectsSessionIDForNonAllowedTool guards the per-tool
// (not per-server) scoping this mechanism needs: a server allowed for one
// tool must not have the id injected into a different tool's call, even
// though both tools live on the same server — see
// docs/medical-card-session-injection.md §3.
func TestExecuteTool_NeverInjectsSessionIDForNonAllowedTool(t *testing.T) {
	medical := mcptest.New("medical_card", llm.ToolDef{Name: "medical.profile"}).WithResult("medical.profile", "profile data")
	provider := llmtest.New("local",
		llmtest.Response{ToolCall: &llm.ToolCall{ID: "call-1", Name: "medical_card_medical.profile", Arguments: `{}`}},
		llmtest.Response{Text: "Done."},
	)
	o, _, _ := newTestOrchestrator(t, provider, medical)
	// "medical_card" is allowed, but only for medical.ask — not medical.profile.
	o.SetMCPServerExtensions(map[string]MCPServerExtension{
		"medical_card": {SessionIDArg: "sessionId", SessionIDTools: map[string]bool{"medical.ask": true}},
	}, 0)

	resp, err := o.Handle(context.Background(), InputRequest{Source: "cli", UserID: "alex", Text: "get my profile"})
	require.NoError(t, err)
	require.Equal(t, "Done.", resp.Reply)

	require.Len(t, medical.Calls, 1)
	var wireArgs map[string]any
	require.NoError(t, json.Unmarshal([]byte(medical.Calls[0].ArgumentsJSON), &wireArgs))
	require.NotContains(t, wireArgs, "sessionId")
}

func TestExecuteTool_NeverInjectsSessionIDForNonAllowedServer(t *testing.T) {
	ha := mcptest.New("ha", llm.ToolDef{Name: "get_state"}).WithResult("get_state", "on")
	provider := llmtest.New("local",
		llmtest.Response{ToolCall: &llm.ToolCall{ID: "call-1", Name: "ha_get_state", Arguments: `{"entity_id":"light.living_room"}`}},
		llmtest.Response{Text: "Done."},
	)
	o, _, _ := newTestOrchestrator(t, provider, ha)
	o.SetMCPServerExtensions(map[string]MCPServerExtension{
		"medical_card": {SessionIDArg: "sessionId", SessionIDTools: map[string]bool{"medical.ask": true}},
	}, 0) // "ha" is not in this set

	resp, err := o.Handle(context.Background(), InputRequest{Source: "cli", UserID: "alex", Text: "is the light on?"})
	require.NoError(t, err)
	require.Equal(t, "Done.", resp.Reply)

	require.Len(t, ha.Calls, 1)
	var wireArgs map[string]any
	require.NoError(t, json.Unmarshal([]byte(ha.Calls[0].ArgumentsJSON), &wireArgs))
	require.NotContains(t, wireArgs, "sessionId")
}

// TestExecuteTool_ModelSuppliedSessionIDIsAlwaysOverwritten guards the
// "never trust the model" property from
// docs/medical-card-session-injection.md §1 — a model that hallucinates its
// own sessionId value must never see that value reach the wire.
func TestExecuteTool_ModelSuppliedSessionIDIsAlwaysOverwritten(t *testing.T) {
	medical := mcptest.New("medical_card", llm.ToolDef{Name: "medical.ask"}).WithResult("medical.ask", "some answer")
	provider := llmtest.New("local",
		llmtest.Response{ToolCall: &llm.ToolCall{ID: "call-1", Name: "medical_card_medical.ask", Arguments: `{"question":"hi","sessionId":"model-invented-value"}`}},
		llmtest.Response{Text: "Done."},
	)
	o, _, _ := newTestOrchestrator(t, provider, medical)
	o.SetMCPServerExtensions(map[string]MCPServerExtension{
		"medical_card": {SessionIDArg: "sessionId", SessionIDTools: map[string]bool{"medical.ask": true}},
	}, 0)

	resp, err := o.Handle(context.Background(), InputRequest{Source: "cli", UserID: "alex", Text: "ask the medical card"})
	require.NoError(t, err)

	require.Len(t, medical.Calls, 1)
	var wireArgs map[string]any
	require.NoError(t, json.Unmarshal([]byte(medical.Calls[0].ArgumentsJSON), &wireArgs))
	require.Equal(t, resp.ConversationID, wireArgs["sessionId"])
	require.NotEqual(t, "model-invented-value", wireArgs["sessionId"])
}

func TestSetSessionIDArg_Injects(t *testing.T) {
	result, ok := setSessionIDArg(`{"question":"hi"}`, "sessionId", "conv-123")
	require.True(t, ok)
	var m map[string]any
	require.NoError(t, json.Unmarshal([]byte(result), &m))
	require.Equal(t, "hi", m["question"])
	require.Equal(t, "conv-123", m["sessionId"])
}

func TestSetSessionIDArg_OverwritesModelSuppliedValue(t *testing.T) {
	result, ok := setSessionIDArg(`{"question":"hi","sessionId":"attacker"}`, "sessionId", "conv-123")
	require.True(t, ok)
	var m map[string]any
	require.NoError(t, json.Unmarshal([]byte(result), &m))
	require.Equal(t, "conv-123", m["sessionId"])
}

func TestSetSessionIDArg_ReturnsNotOKOnInvalidJSON(t *testing.T) {
	result, ok := setSessionIDArg("not json", "sessionId", "conv-123")
	require.False(t, ok)
	require.Equal(t, "not json", result)
}
