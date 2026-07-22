// Package openaicompat implements llm.Provider on top of the official
// github.com/openai/openai-go SDK, pointed at any OpenAI Chat Completions
// compatible backend (OpenAI itself, or a local/self-hosted server such as
// Ollama, vLLM, LM Studio, or a hosted router like OpenRouter) via
// option.WithBaseURL. This is the single client used for every provider that
// isn't native Anthropic.
package openaicompat

import (
	"context"
	"fmt"

	"github.com/openai/openai-go/v2"
	"github.com/openai/openai-go/v2/option"
	"github.com/openai/openai-go/v2/packages/ssestream"
	"github.com/openai/openai-go/v2/shared"

	"github.com/archer-developer/miranda/internal/llm"
)

// Provider is an llm.Provider backed by any OpenAI-compatible Chat
// Completions endpoint.
type Provider struct {
	name   string
	model  string
	client openai.Client
}

// New builds a Provider named name, targeting model on the endpoint at
// baseURL (empty baseURL means the real OpenAI API). apiKey may be empty for
// local backends that don't require authentication.
func New(name, baseURL, model, apiKey string) *Provider {
	opts := []option.RequestOption{}
	if baseURL != "" {
		opts = append(opts, option.WithBaseURL(baseURL))
	}
	if apiKey != "" {
		opts = append(opts, option.WithAPIKey(apiKey))
	}
	return &Provider{name: name, model: model, client: openai.NewClient(opts...)}
}

func (p *Provider) Name() string { return p.name }

// Chat implements llm.Provider by streaming a Chat Completions response and
// re-assembling incrementally-streamed tool-call fragments (the API sends a
// function name/arguments split across many chunks, keyed by tool_call
// index) into whole llm.ToolCall values before they're handed to the caller.
func (p *Provider) Chat(ctx context.Context, req llm.ChatRequest) (<-chan llm.StreamChunk, error) {
	params := openai.ChatCompletionNewParams{
		Model:    shared.ChatModel(p.model),
		Messages: toOpenAIMessages(req.Messages),
		Tools:    toOpenAITools(req.Tools),
	}

	stream := p.client.Chat.Completions.NewStreaming(ctx, params)

	out := make(chan llm.StreamChunk)
	go pump(stream, out)
	return out, nil
}

// pendingToolCall accumulates a tool call's fragments as they stream in.
type pendingToolCall struct {
	id, name, arguments string
}

func pump(stream *ssestream.Stream[openai.ChatCompletionChunk], out chan<- llm.StreamChunk) {
	defer close(out)

	// Tool calls stream as fragments identified by index; accumulate until
	// the stream ends, then emit each as one complete llm.ToolCall.
	pending := map[int64]*pendingToolCall{}
	var order []int64

	for stream.Next() {
		chunk := stream.Current()
		if len(chunk.Choices) == 0 {
			continue
		}
		delta := chunk.Choices[0].Delta

		if delta.Content != "" {
			out <- llm.StreamChunk{TextDelta: delta.Content}
		}

		for _, tc := range delta.ToolCalls {
			pc, ok := pending[tc.Index]
			if !ok {
				pc = &pendingToolCall{}
				pending[tc.Index] = pc
				order = append(order, tc.Index)
			}
			if tc.ID != "" {
				pc.id = tc.ID
			}
			if tc.Function.Name != "" {
				pc.name += tc.Function.Name
			}
			pc.arguments += tc.Function.Arguments
		}
	}

	if err := stream.Err(); err != nil {
		out <- llm.StreamChunk{Err: fmt.Errorf("openaicompat: stream: %w", err)}
		return
	}

	for _, idx := range order {
		pc := pending[idx]
		out <- llm.StreamChunk{ToolCall: &llm.ToolCall{ID: pc.id, Name: pc.name, Arguments: pc.arguments}}
	}

	out <- llm.StreamChunk{Done: true}
}

func toOpenAIMessages(msgs []llm.Message) []openai.ChatCompletionMessageParamUnion {
	out := make([]openai.ChatCompletionMessageParamUnion, 0, len(msgs))
	for _, m := range msgs {
		switch m.Role {
		case llm.RoleSystem:
			out = append(out, openai.SystemMessage(m.Content))
		case llm.RoleUser:
			out = append(out, openai.UserMessage(m.Content))
		case llm.RoleTool:
			out = append(out, openai.ToolMessage(m.Content, m.ToolCallID))
		case llm.RoleAssistant:
			msg := openai.AssistantMessage(m.Content)
			if len(m.ToolCalls) > 0 {
				calls := make([]openai.ChatCompletionMessageToolCallUnionParam, 0, len(m.ToolCalls))
				for _, tc := range m.ToolCalls {
					calls = append(calls, openai.ChatCompletionMessageToolCallUnionParam{
						OfFunction: &openai.ChatCompletionMessageFunctionToolCallParam{
							ID: tc.ID,
							Function: openai.ChatCompletionMessageFunctionToolCallFunctionParam{
								Name:      tc.Name,
								Arguments: tc.Arguments,
							},
						},
					})
				}
				msg.OfAssistant.ToolCalls = calls
			}
			out = append(out, msg)
		}
	}
	return out
}

func toOpenAITools(tools []llm.ToolDef) []openai.ChatCompletionToolUnionParam {
	if len(tools) == 0 {
		return nil
	}
	out := make([]openai.ChatCompletionToolUnionParam, 0, len(tools))
	for _, t := range tools {
		out = append(out, openai.ChatCompletionFunctionTool(shared.FunctionDefinitionParam{
			Name:        t.Name,
			Description: openai.String(t.Description),
			Parameters:  shared.FunctionParameters(t.Parameters),
		}))
	}
	return out
}
