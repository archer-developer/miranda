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

func TestEncryptionKeyAllowedServers_GrantsOnlyPermittedServers(t *testing.T) {
	servers := []config.MCPServer{
		{Name: "diary", URL: "https://diary.example.com/mcp", EncryptionKeyAllowed: true},
		{Name: "ha", URL: "http://homeassistant.local:8123/api/mcp", EncryptionKeyAllowed: false},
		{Name: "sandbox", URL: "http://127.0.0.1:8788/mcp", EncryptionKeyAllowed: false},
	}

	allowed := encryptionKeyAllowedServers(servers, discardLogger())

	require.Equal(t, "encryption_key", allowed["diary"])
	require.NotContains(t, allowed, "ha")
	require.NotContains(t, allowed, "sandbox")
}

func TestEncryptionKeyAllowedServers_UsesConfiguredArgName(t *testing.T) {
	servers := []config.MCPServer{
		{Name: "diary", URL: "https://diary.example.com/mcp", EncryptionKeyAllowed: true, EncryptionKeyArgName: "record_encryption_key"},
	}

	allowed := encryptionKeyAllowedServers(servers, discardLogger())

	require.Equal(t, "record_encryption_key", allowed["diary"])
}

// TestEncryptionKeyAllowedServers_NonHTTPSNeverGranted is defense-in-depth
// coverage: config.validateEncryptionKeyServers should already reject this
// combination at config-load time, but encryptionKeyAllowedServers must
// never grant it even if that validation is ever bypassed or drifts.
func TestEncryptionKeyAllowedServers_NonHTTPSNeverGranted(t *testing.T) {
	servers := []config.MCPServer{
		{Name: "diary", URL: "http://diary.example.com/mcp", EncryptionKeyAllowed: true},
	}

	allowed := encryptionKeyAllowedServers(servers, discardLogger())

	require.NotContains(t, allowed, "diary")
}
