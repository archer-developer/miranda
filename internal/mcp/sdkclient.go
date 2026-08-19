package mcp

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	llm "github.com/archer-developer/miranda-llm"
)

const clientVersion = "0.1.0"

// SDKClient is a Client backed by the official MCP Go SDK, talking to a
// remote MCP server over the Streamable HTTP transport (what Home Assistant
// and most other MCP servers expose).
type SDKClient struct {
	name    string
	session *sdkmcp.ClientSession
}

// Connect opens an MCP session named name against the server at url. token,
// if non-empty, is sent as a Bearer Authorization header on every request
// (e.g. a Home Assistant Long-Lived Access Token).
func Connect(ctx context.Context, name, url, token string) (*SDKClient, error) {
	httpClient := http.DefaultClient
	if token != "" {
		httpClient = &http.Client{Transport: bearerTransport{token: token, base: http.DefaultTransport}}
	}

	transport := &sdkmcp.StreamableClientTransport{Endpoint: url, HTTPClient: httpClient}
	client := sdkmcp.NewClient(&sdkmcp.Implementation{Name: "miranda", Version: clientVersion}, nil)

	session, err := client.Connect(ctx, transport, nil)
	if err != nil {
		return nil, fmt.Errorf("mcp: connect to %s (%s): %w", name, url, err)
	}

	return &SDKClient{name: name, session: session}, nil
}

// Name implements Client.
func (c *SDKClient) Name() string { return c.name }

// Close implements Client, releasing this session's background keepalive
// goroutine and its underlying HTTP/SSE connection. Safe to call more than
// once (ClientSession.Close is idempotent).
func (c *SDKClient) Close() error {
	return c.session.Close()
}

// classifyErr wraps err with ErrDisconnected when it means the
// session/transport itself is gone, and leaves it unwrapped when it's an
// application-level RPC error (a *jsonrpc.Error response) — the server
// answered, so the session is still alive, even though this one call failed.
// The MCP Go SDK surfaces both cases through the same call signature, so
// this is the one place that has to tell them apart.
func classifyErr(err error) error {
	if err == nil {
		return nil
	}
	var rpcErr *jsonrpc.Error
	if errors.As(err, &rpcErr) {
		return err
	}
	return fmt.Errorf("%w: %v", ErrDisconnected, err)
}

// ListTools implements Client.
func (c *SDKClient) ListTools(ctx context.Context) ([]llm.ToolDef, error) {
	result, err := c.session.ListTools(ctx, nil)
	if err != nil {
		return nil, classifyErr(fmt.Errorf("mcp: list tools on %s: %w", c.name, err))
	}

	out := make([]llm.ToolDef, 0, len(result.Tools))
	for _, t := range result.Tools {
		// The SDK documents that, on the client side, InputSchema is always
		// the server's schema unmarshaled as a plain map[string]any.
		params, _ := t.InputSchema.(map[string]any)
		out = append(out, llm.ToolDef{
			Name:        t.Name,
			Description: t.Description,
			Parameters:  params,
		})
	}
	return out, nil
}

// CallTool implements Client.
func (c *SDKClient) CallTool(ctx context.Context, name string, argumentsJSON string) (string, error) {
	result, err := c.session.CallTool(ctx, &sdkmcp.CallToolParams{
		Name:      name,
		Arguments: rawJSON(argumentsJSON),
	})
	if err != nil {
		return "", classifyErr(fmt.Errorf("mcp: call %s on %s: %w", name, c.name, err))
	}

	var text string
	for _, content := range result.Content {
		if tc, ok := content.(*sdkmcp.TextContent); ok {
			text += tc.Text
		}
	}
	if result.IsError {
		return text, fmt.Errorf("mcp: tool %s on %s reported an error: %s", name, c.name, text)
	}
	return text, nil
}

// rawJSON lets an already-serialized JSON argument string pass through the
// SDK's `Arguments any` field unchanged instead of being re-encoded as a
// JSON string literal.
type rawJSON string

func (r rawJSON) MarshalJSON() ([]byte, error) {
	if r == "" {
		return []byte("{}"), nil
	}
	return []byte(r), nil
}

// bearerTransport injects a Bearer Authorization header into every request,
// used for MCP servers (like Home Assistant) that require a static token.
type bearerTransport struct {
	token string
	base  http.RoundTripper
}

func (t bearerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	req.Header.Set("Authorization", "Bearer "+t.token)
	return t.base.RoundTrip(req)
}
