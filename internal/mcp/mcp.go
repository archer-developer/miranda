// Package mcp abstracts MCP (Model Context Protocol) servers as tool
// sources for the agent. Client is implemented for real by SDKClient
// (wrapping the official MCP Go SDK) and by mcptest.Fake in tests. Manager
// aggregates several Clients (e.g. Home Assistant plus other tool servers)
// into one flat, collision-free tool namespace for the LLM.
package mcp

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/archer-developer/miranda/internal/llm"
)

// ErrDisconnected marks a ListTools/CallTool error as meaning the
// server's session/transport itself is gone (as opposed to a one-off
// application-level RPC error on an otherwise-healthy session). Client
// implementations should wrap their error with this (errors.Is-checkable)
// only when the session is genuinely dead — SDKClient does this by
// distinguishing transport failures from *jsonrpc.Error responses (see
// sdkclient.go's classifyErr). Manager uses it to decide whether a failure
// warrants evicting the client (and triggering a reconnect) or is just a
// transient error to retry next turn on the same, still-live session.
var ErrDisconnected = errors.New("mcp: server disconnected")

// prefixedToolName builds the tool name Manager.Tools advertises for a
// given server/tool pair — the single source of truth for that
// "<serverName>_<toolName>" convention, used by Tools/Call/serverForTool
// below so the format can't drift between where it's built and where it's
// parsed back apart.
func prefixedToolName(serverName, toolName string) string {
	return serverName + "_" + toolName
}

// Client is one MCP server's tool source: it can list its tools and invoke
// them by name.
type Client interface {
	// Name is this server's short identifier, used as the tool name prefix.
	Name() string
	// ListTools and CallTool should wrap a returned error with ErrDisconnected
	// (via fmt.Errorf("...: %w", ErrDisconnected)) when it means the
	// underlying session/transport has died, and leave any other error
	// unwrapped — see ErrDisconnected's doc comment.
	ListTools(ctx context.Context) ([]llm.ToolDef, error)
	// CallTool invokes tool name (unprefixed) with argumentsJSON and returns
	// the tool's textual result.
	CallTool(ctx context.Context, name string, argumentsJSON string) (string, error)
	// Close releases the underlying session/connection. It must be safe to
	// call on a Client that's about to be discarded (evicted or replaced) so
	// Manager never leaks a session's background goroutine/connection.
	Close() error
}

// Manager aggregates tools from multiple MCP Clients into one namespace,
// prefixing each tool name with "<server>_" so identically named tools from
// different servers can't collide.
//
// Clients can be added or dropped after construction (SetClient, and the
// automatic eviction in Tools below) — a server that's unreachable at
// startup, or whose session dies mid-run, isn't fatal to the whole Manager.
// cmd/miranda's keepMCPConnected retries such a server in the background and
// calls SetClient once it's reachable again, with no Miranda restart needed.
type Manager struct {
	logger *slog.Logger

	mu      sync.RWMutex
	clients map[string]Client
	order   []string // insertion order, stable across reconnects
}

// NewManager builds a Manager over the given Clients, if any — more can be
// added later via SetClient.
func NewManager(logger *slog.Logger, clients ...Client) *Manager {
	if logger == nil {
		logger = slog.Default()
	}
	m := &Manager{logger: logger, clients: make(map[string]Client, len(clients))}
	for _, c := range clients {
		m.clients[c.Name()] = c
		m.order = append(m.order, c.Name())
	}
	return m
}

// SetClient adds c as (or replaces) the client serving its server name,
// keeping that name's original position in iteration order if it already
// had one — so a reconnected server's tools reappear in the same place.
// Membership is checked against order, not clients: a name evicted by
// removeClient stays in order (just without a live client) precisely so a
// later SetClient here restores it to that same slot instead of appending a
// duplicate. If c replaces a still-live client for the same name (e.g. two
// misconfigured servers sharing a name both connect), the one being replaced
// is closed rather than silently dropped.
func (m *Manager) SetClient(c Client) {
	m.mu.Lock()
	defer m.mu.Unlock()
	name := c.Name()
	if old, ok := m.clients[name]; ok && old != c {
		_ = old.Close()
	}
	m.clients[name] = c
	for _, n := range m.order {
		if n == name {
			return
		}
	}
	m.order = append(m.order, name)
}

// HasClient reports whether name currently has a connected client. The
// background reconnect loop uses this to skip servers that are already up.
func (m *Manager) HasClient(name string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	_, ok := m.clients[name]
	return ok
}

// removeClient closes name's client (if any) and drops it, so a later
// HasClient(name) tells the reconnect loop to bring it back.
func (m *Manager) removeClient(name string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if c, ok := m.clients[name]; ok {
		_ = c.Close()
	}
	delete(m.clients, name)
}

// snapshot copies the current client set and iteration order under a read
// lock, so Tools/Call can walk them without holding the lock across
// potentially slow network calls.
func (m *Manager) snapshot() ([]string, map[string]Client) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	order := append([]string(nil), m.order...)
	clients := make(map[string]Client, len(m.clients))
	for name, c := range m.clients {
		clients[name] = c
	}
	return order, clients
}

// Tools lists every tool across all configured servers, with names prefixed
// by their owning server so Call can route back to the right one. A server
// whose ListTools call fails is logged and left out of this call's result
// rather than failing the whole call — one bad MCP server must never take
// every other tool source down with it. Only a failure wrapped with
// ErrDisconnected (the session/transport itself is gone) evicts the server
// from the Manager so the background reconnect loop picks it back up; a
// plain application-level error leaves the client in place to retry next
// turn, since tearing down a healthy session over one transient error would
// cost up to a full reconnect cycle for no reason.
func (m *Manager) Tools(ctx context.Context) []llm.ToolDef {
	order, clients := m.snapshot()

	var out []llm.ToolDef
	for _, name := range order {
		c, ok := clients[name]
		if !ok {
			continue
		}
		tools, err := c.ListTools(ctx)
		if err != nil {
			if errors.Is(err, ErrDisconnected) {
				m.logger.Warn("mcp: server disconnected, dropping it until it reconnects", "server", name, "error", err)
				m.removeClient(name)
			} else {
				m.logger.Warn("mcp: failed to list tools, will retry next turn", "server", name, "error", err)
			}
			continue
		}
		for _, t := range tools {
			t.Name = prefixedToolName(name, t.Name)
			out = append(out, t)
		}
	}
	return out
}

// Call routes a prefixed tool name (as produced by Tools) to the server that
// owns it and invokes it there. Like Tools, a CallTool failure that means the
// session died (ErrDisconnected) evicts the client immediately instead of
// waiting for the next Tools() call to notice, so the background reconnect
// loop can start working on it right away.
func (m *Manager) Call(ctx context.Context, prefixedName, argumentsJSON string) (string, error) {
	order, clients := m.snapshot()

	name, ok := serverForTool(order, prefixedName)
	if !ok {
		return "", fmt.Errorf("mcp: no configured server matches tool %q", prefixedName)
	}
	c, ok := clients[name]
	if !ok {
		return "", fmt.Errorf("mcp: server %q for tool %q is currently disconnected", name, prefixedName)
	}
	result, err := c.CallTool(ctx, strings.TrimPrefix(prefixedName, prefixedToolName(name, "")), argumentsJSON)
	if err != nil && errors.Is(err, ErrDisconnected) {
		m.logger.Warn("mcp: server disconnected, dropping it until it reconnects", "server", name, "error", err)
		m.removeClient(name)
	}
	return result, err
}

// ServerForTool returns which server owns prefixedName (as produced by
// Tools), without invoking anything — used by callers (e.g.
// internal/httpapi.executeTool's encryption-key injection) that need to
// know a tool call's owning server before deciding what to do, separately
// from Call actually dispatching to it. Only reads order (under the read
// lock, not the full client-set copy snapshot() also builds — this is
// called on every tool call once a keyring is configured, so it's worth
// not paying for a map copy it doesn't need).
func (m *Manager) ServerForTool(prefixedName string) (string, bool) {
	return serverForTool(m.orderSnapshot(), prefixedName)
}

// orderSnapshot copies just the current iteration order under a read lock —
// the lighter-weight half of snapshot(), for callers that only need to
// resolve a tool name to its owning server without a client to call.
func (m *Manager) orderSnapshot() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return append([]string(nil), m.order...)
}

func serverForTool(order []string, prefixedName string) (string, bool) {
	for _, name := range order {
		if strings.HasPrefix(prefixedName, prefixedToolName(name, "")) {
			return name, true
		}
	}
	return "", false
}

// KeepConnected keeps a client named name attached to m for as long as ctx is
// alive: whenever HasClient(name) is false (never connected yet, or evicted
// after a disconnection), it calls connect — bounded by attemptTimeout — and
// SetClients the result on success. A failed attempt retries after
// baseInterval, doubling up to maxInterval on each consecutive failure so an
// extended outage doesn't cost an indefinite connection attempt every
// baseInterval; a success resets the interval back to baseInterval. Callers
// should launch this in its own goroutine — it only returns once ctx is
// cancelled.
func (m *Manager) KeepConnected(ctx context.Context, name string, baseInterval, maxInterval, attemptTimeout time.Duration, connect func(ctx context.Context) (Client, error)) {
	wait := baseInterval
	for {
		if !m.HasClient(name) {
			attemptCtx, cancel := context.WithTimeout(ctx, attemptTimeout)
			client, err := connect(attemptCtx)
			cancel()
			if err != nil {
				m.logger.Warn("mcp: failed to connect, will retry", "server", name, "error", err, "retry_in", wait)
			} else {
				m.logger.Info("mcp: connected", "server", name)
				m.SetClient(client)
				wait = baseInterval
			}
		}

		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}

		// Grow the wait for the next iteration only while still down, so a
		// sustained outage backs off instead of retrying at baseInterval
		// forever; a healthy (or newly reconnected) server stays polled at
		// baseInterval so eviction is noticed promptly, not after a stale
		// multi-minute backoff left over from a previous outage.
		if m.HasClient(name) {
			wait = baseInterval
		} else if next := wait * 2; next <= maxInterval {
			wait = next
		} else {
			wait = maxInterval
		}
	}
}
