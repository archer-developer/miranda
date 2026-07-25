// Package anthropic implements llm.Provider on top of the official
// github.com/anthropics/anthropic-sdk-go SDK, used exclusively for Claude —
// it gets full native support for tool use, streaming, and prompt caching,
// unlike routing Claude through an OpenAI-compatibility shim.
package anthropic

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/anthropics/anthropic-sdk-go/packages/ssestream"

	"github.com/archer-developer/miranda/internal/llm"
)

const defaultMaxTokens = 4096

// Provider is an llm.Provider backed by the native Anthropic Messages API.
type Provider struct {
	name      string
	model     string
	maxTokens int64
	client    anthropic.Client
}

// New builds a Provider named name for the given Claude model.
func New(name, model, apiKey string) *Provider {
	opts := []option.RequestOption{}
	if apiKey != "" {
		opts = append(opts, option.WithAPIKey(apiKey))
	}
	return &Provider{
		name:      name,
		model:     model,
		maxTokens: defaultMaxTokens,
		client:    anthropic.NewClient(opts...),
	}
}

func (p *Provider) Name() string { return p.name }

// Chat implements llm.Provider by streaming a Messages API response.
func (p *Provider) Chat(ctx context.Context, req llm.ChatRequest) (<-chan llm.StreamChunk, error) {
	system, messages := toAnthropicMessages(req.Messages)

	params := anthropic.MessageNewParams{
		Model:     anthropic.Model(p.model),
		MaxTokens: p.maxTokens,
		System:    system,
		Messages:  messages,
		Tools:     toAnthropicTools(req.Tools),
	}

	stream := p.client.Messages.NewStreaming(ctx, params)

	out := make(chan llm.StreamChunk)
	go pump(stream, out)
	return out, nil
}

// pump forwards text deltas as they arrive and uses the SDK's built-in
// Message.Accumulate to reconstruct the full response, so tool_use blocks
// (which the API streams as fragmented partial-JSON deltas) only need to be
// read once, fully assembled, at the end of the stream.
func pump(stream *ssestream.Stream[anthropic.MessageStreamEventUnion], out chan<- llm.StreamChunk) {
	defer close(out)

	var message anthropic.Message
	for stream.Next() {
		event := stream.Current()
		if err := message.Accumulate(event); err != nil {
			out <- llm.StreamChunk{Err: fmt.Errorf("anthropic: accumulate: %w", err)}
			return
		}
		if event.Type == "content_block_delta" && event.Delta.Text != "" {
			out <- llm.StreamChunk{TextDelta: event.Delta.Text}
		}
	}
	if err := stream.Err(); err != nil {
		out <- llm.StreamChunk{Err: fmt.Errorf("anthropic: stream: %w", err)}
		return
	}

	for _, block := range message.Content {
		if block.Type == "tool_use" {
			out <- llm.StreamChunk{ToolCall: &llm.ToolCall{ID: block.ID, Name: block.Name, Arguments: string(block.Input)}}
		}
	}

	out <- llm.StreamChunk{Done: true}
}

// toAnthropicMessages splits llm.Message history into Anthropic's separate
// top-level system prompt and turn-by-turn message list. Tool results are
// represented as user-role messages containing a tool_result block, per the
// Anthropic Messages API convention (there is no dedicated "tool" role).
//
// A cache_control breakpoint is placed on the last system block so that the
// system prompt (persona + per-user memory) is reused from Anthropic's prompt
// cache on subsequent turns of the same conversation, reducing both latency
// and input-token cost.
func toAnthropicMessages(msgs []llm.Message) ([]anthropic.TextBlockParam, []anthropic.MessageParam) {
	var system []anthropic.TextBlockParam
	var out []anthropic.MessageParam

	for _, m := range msgs {
		switch m.Role {
		case llm.RoleSystem:
			system = append(system, anthropic.TextBlockParam{Text: m.Content})

		case llm.RoleUser:
			out = append(out, anthropic.NewUserMessage(anthropic.NewTextBlock(m.Content)))

		case llm.RoleTool:
			out = append(out, anthropic.NewUserMessage(anthropic.NewToolResultBlock(m.ToolCallID, m.Content, false)))

		case llm.RoleAssistant:
			var blocks []anthropic.ContentBlockParamUnion
			if m.Content != "" {
				blocks = append(blocks, anthropic.NewTextBlock(m.Content))
			}
			for _, tc := range m.ToolCalls {
				var input any
				if tc.Arguments != "" {
					// Best-effort: malformed arguments just become a nil input
					// rather than failing the whole request.
					_ = json.Unmarshal([]byte(tc.Arguments), &input)
				}
				blocks = append(blocks, anthropic.NewToolUseBlock(tc.ID, input, tc.Name))
			}
			out = append(out, anthropic.NewAssistantMessage(blocks...))
		}
	}

	// Mark the last system block as a cache breakpoint. Anthropic renders the
	// prompt in the order tools → system → messages, so this checkpoint covers
	// the entire stable prefix (system prompt + per-user memory) and prevents
	// it from being re-priced on every subsequent turn.
	if len(system) > 0 {
		system[len(system)-1].CacheControl = anthropic.NewCacheControlEphemeralParam()
	}

	return system, out
}

func toAnthropicTools(tools []llm.ToolDef) []anthropic.ToolUnionParam {
	if len(tools) == 0 {
		return nil
	}
	out := make([]anthropic.ToolUnionParam, 0, len(tools))
	for _, t := range tools {
		out = append(out, anthropic.ToolUnionParam{
			OfTool: &anthropic.ToolParam{
				Name:        t.Name,
				Description: anthropic.String(t.Description),
				InputSchema: anthropic.ToolInputSchemaParam{
					Properties: t.Parameters["properties"],
					Required:   requiredFields(t.Parameters),
				},
			},
		})
	}

	// Anthropic's render order is tools → system → messages. A cache breakpoint
	// on the last tool caches the entire tool list as a shared prefix, so
	// repeated turns (which always send the same tool definitions) don't pay
	// input-token cost for them again.
	out[len(out)-1].OfTool.CacheControl = anthropic.NewCacheControlEphemeralParam()

	return out
}

func requiredFields(parameters map[string]any) []string {
	raw, ok := parameters["required"].([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(raw))
	for _, r := range raw {
		if s, ok := r.(string); ok {
			out = append(out, s)
		}
	}
	return out
}
