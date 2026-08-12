package main

import (
	"io"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/archer-developer/miranda/internal/config"
	"github.com/archer-developer/miranda/internal/tools"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestValidateEscalationToolNames_CollisionWithReservedNameErrors(t *testing.T) {
	providers := []config.LLMProvider{
		{
			Name: "gemini-lite",
			Escalation: config.EscalationConfig{
				Enabled:  true,
				ToolName: tools.WebSearchToolName,
			},
		},
	}
	require.Error(t, validateEscalationToolNames(providers))
}

func TestValidateEscalationToolNames_NonCollidingNameIsFine(t *testing.T) {
	providers := []config.LLMProvider{
		{
			Name: "gemini-lite",
			Escalation: config.EscalationConfig{
				Enabled:  true,
				ToolName: "escalate_to_gemini_strong",
			},
		},
	}
	require.NoError(t, validateEscalationToolNames(providers))
}

func TestValidateEscalationToolNames_DisabledEscalationIgnoresName(t *testing.T) {
	providers := []config.LLMProvider{
		{
			Name: "gemini-lite",
			Escalation: config.EscalationConfig{
				Enabled:  false,
				ToolName: "web_fetch",
			},
		},
	}
	require.NoError(t, validateEscalationToolNames(providers))
}

func TestMCPServerExtensions_GrantsEncryptionKeyOnlyToPermittedServers(t *testing.T) {
	servers := []config.MCPServer{
		{Name: "diary", URL: "https://diary.example.com/mcp", EncryptionKeyAllowed: true},
		{Name: "ha", URL: "http://homeassistant.local:8123/api/mcp", EncryptionKeyAllowed: false},
		{Name: "sandbox", URL: "http://127.0.0.1:8788/mcp", EncryptionKeyAllowed: false},
	}

	extensions := mcpServerExtensions(servers, discardLogger())

	require.Equal(t, "encryption_key", extensions["diary"].EncryptionKeyArg)
	require.Empty(t, extensions["ha"].EncryptionKeyArg)
	require.Empty(t, extensions["sandbox"].EncryptionKeyArg)
}

func TestMCPServerExtensions_UsesConfiguredEncryptionKeyArgName(t *testing.T) {
	servers := []config.MCPServer{
		{Name: "diary", URL: "https://diary.example.com/mcp", EncryptionKeyAllowed: true, EncryptionKeyArgName: "record_encryption_key"},
	}

	extensions := mcpServerExtensions(servers, discardLogger())

	require.Equal(t, "record_encryption_key", extensions["diary"].EncryptionKeyArg)
}

// TestMCPServerExtensions_NonHTTPSEncryptionKeyNeverGranted is
// defense-in-depth coverage: config.validateEncryptionKeyServers should
// already reject this combination at config-load time, but
// mcpServerExtensions must never grant it even if that validation is ever
// bypassed or drifts.
func TestMCPServerExtensions_NonHTTPSEncryptionKeyNeverGranted(t *testing.T) {
	servers := []config.MCPServer{
		{Name: "diary", URL: "http://diary.example.com/mcp", EncryptionKeyAllowed: true},
	}

	extensions := mcpServerExtensions(servers, discardLogger())

	require.Empty(t, extensions["diary"].EncryptionKeyArg)
}

func TestMCPServerExtensions_GrantsSessionIDOnlyToServersWithNonEmptyTools(t *testing.T) {
	servers := []config.MCPServer{
		{Name: "medical_card", URL: "https://medical.example.com/mcp", SessionIDTools: []string{"medical.ask"}},
		{Name: "ha", URL: "http://homeassistant.local:8123/api/mcp"},
	}

	extensions := mcpServerExtensions(servers, discardLogger())

	require.Equal(t, "sessionId", extensions["medical_card"].SessionIDArg)
	require.Equal(t, map[string]bool{"medical.ask": true}, extensions["medical_card"].SessionIDTools)
	require.Empty(t, extensions["ha"].SessionIDArg)
}

func TestMCPServerExtensions_UsesConfiguredSessionIDArgName(t *testing.T) {
	servers := []config.MCPServer{
		{Name: "medical_card", URL: "https://medical.example.com/mcp", SessionIDArgName: "conversation_session", SessionIDTools: []string{"medical.ask"}},
	}

	extensions := mcpServerExtensions(servers, discardLogger())

	require.Equal(t, "conversation_session", extensions["medical_card"].SessionIDArg)
}

func TestMCPServerExtensions_MultipleSessionIDToolsAllScopedToTheirOwnServer(t *testing.T) {
	servers := []config.MCPServer{
		{Name: "medical_card", URL: "https://medical.example.com/mcp", SessionIDTools: []string{"medical.ask", "medical.followup"}},
	}

	extensions := mcpServerExtensions(servers, discardLogger())

	require.Equal(t, map[string]bool{"medical.ask": true, "medical.followup": true}, extensions["medical_card"].SessionIDTools)
	require.False(t, extensions["medical_card"].SessionIDTools["medical.profile"], "a tool not listed must not be allowed")
}
