package main

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/archer-developer/miranda/internal/config"
	"github.com/archer-developer/miranda/internal/tools"
)

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
