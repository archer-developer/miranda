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

	llm "github.com/archer-developer/miranda-llm"
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

// removeClient closes c and drops it from name's slot, so a later
// HasClient(name) tells the reconnect loop to bring it back — but only if c
// is still the client actually registered there. A caller only ever learns
// a client is dead after an RPC on it (ListTools/CallTool) returns, and
// that call can be in flight for as long as its own timeout while a
// concurrent KeepConnected tick independently detects the same death,
// evicts, and successfully reconnects — installing a brand new, healthy
// client under the same name before the first caller's stale error comes
// back. Deleting unconditionally by name (as this used to) would then let
// that stale caller tear down the fresh reconnect instead of the client it
// actually observed fail. Comparing identity first (mirroring SetClient's
// own old != c guard) makes a name-only match a no-op instead.
func (m *Manager) removeClient(name string, c Client) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if cur, ok := m.clients[name]; ok && cur == c {
		_ = cur.Close()
		delete(m.clients, name)
	}
}

// client returns name's currently connected client, if any, under a read
// lock — the single-client counterpart to snapshot() for callers (currently
// just checkHealth) that don't need a full copy of the client set.
func (m *Manager) client(name string) (Client, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	c, ok := m.clients[name]
	return c, ok
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
				m.removeClient(name, c)
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
		m.removeClient(name, c)
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

// ServerAndTool returns which server owns prefixedName (as produced by
// Tools) and that tool's own, unprefixed name — the extended form of
// ServerForTool needed by callers (e.g. httpapi.Orchestrator.executeTool's
// session-id injection, see docs/medical-card-session-injection.md) that
// must know the bare tool name, not just its owning server, without
// duplicating the "<server>_<tool>" convention prefixedToolName already owns
// as its single source of truth. ServerForTool and serverForTool below both
// delegate to the same serverAndTool so there is exactly one place that
// walks order looking for a prefix match.
func (m *Manager) ServerAndTool(prefixedName string) (server, tool string, ok bool) {
	return serverAndTool(m.orderSnapshot(), prefixedName)
}

// KNOWN ISSUE: this HasPrefix scan is ambiguous, not just in theory — two
// enabled servers where one name is a "_"-delimited prefix of the other
// (e.g. "medical" and "medical_card") can resolve a call to the wrong
// server, and since tool names can themselves contain underscores, the
// ambiguity isn't even limited to that case (server "a" + tool "b_c" and
// server "a_b" + tool "c" both mint the identical prefixed name "a_b_c").
// validateMCPServerNames only rejects exact duplicate server names, not
// this. Every caller (ServerForTool, ServerAndTool, Call's own dispatch,
// and everything httpapi.Orchestrator.executeTool gates on their result —
// see CLAUDE.md's "Tools available to the model" section) inherits the
// misattribution risk. Not yet fixed; the correct fix is to stop
// re-deriving (server, tool) from the string here and instead have Tools()
// record the exact prefixedName -> (server, tool) pairing at the moment it
// mints each prefixed name (it already knows this unambiguously there),
// with this function becoming a lookup into that map instead of a scan.
func serverAndTool(order []string, prefixedName string) (server, tool string, ok bool) {
	for _, name := range order {
		prefix := prefixedToolName(name, "")
		if strings.HasPrefix(prefixedName, prefix) {
			return name, strings.TrimPrefix(prefixedName, prefix), true
		}
	}
	return "", "", false
}

func serverForTool(order []string, prefixedName string) (string, bool) {
	server, _, ok := serverAndTool(order, prefixedName)
	return server, ok
}

// checkHealth proactively verifies name's currently-connected client is
// still alive by issuing a lightweight ListTools call, evicting it on
// ErrDisconnected exactly like Manager.Tools/Call do. Without this,
// KeepConnected's reconnect attempt below only ever fires after
// HasClient(name) goes false — and the only thing that used to flip it false
// was a real ListTools/CallTool made from a live agent turn (see Tools/Call
// above). A server that restarts with nobody talking to Miranda at that
// moment would otherwise sit marked "connected" indefinitely: the eviction
// that wakes the reconnect logic never fires until some later turn happens
// to hit the dead session. This runs once per already-connected tick (i.e.
// every baseInterval — see the backoff comment below) so a dead session gets
// noticed on its own.
//
// A ListTools call that merely runs past timeout is deliberately NOT
// treated as disconnection: SDKClient.classifyErr wraps any non-RPC error
// — including this call's own context.DeadlineExceeded — as ErrDisconnected
// indiscriminately, since it can't otherwise tell "the transport died" from
// "the server hasn't answered yet" (e.g. Home Assistant enumerating a large
// number of entities). Evicting on a bare timeout would turn ordinary
// slowness into a needless reconnect cycle every tick until the server
// happens to answer within timeout. checkCtx.Err() lets this function tell
// the two apart even though the wrapped error can't: only a failure that
// arrives before checkCtx's own deadline is a genuine transport-level
// signal worth evicting on.
func (m *Manager) checkHealth(ctx context.Context, name string, timeout time.Duration) {
	c, ok := m.client(name)
	if !ok {
		return
	}
	checkCtx, cancel := context.WithTimeout(ctx, timeout)
	_, err := c.ListTools(checkCtx)
	timedOut := errors.Is(checkCtx.Err(), context.DeadlineExceeded)
	cancel()
	if err == nil {
		return
	}
	if timedOut {
		m.logger.Debug("mcp: health check timed out, leaving client in place", "server", name, "timeout", timeout)
		return
	}
	if errors.Is(err, ErrDisconnected) {
		m.logger.Warn("mcp: health check found server disconnected, dropping until it reconnects", "server", name, "error", err)
		m.removeClient(name, c)
	}
}

// KeepConnected keeps a client named name attached to m for as long as ctx is
// alive: whenever HasClient(name) is false (never connected yet, evicted
// after a disconnection discovered by Tools/Call, or just evicted by
// checkHealth below), it calls connect — bounded by attemptTimeout — and
// SetClients the result on success. A failed attempt retries after
// baseInterval, doubling up to maxInterval on each consecutive failure so an
// extended outage doesn't cost an indefinite connection attempt every
// baseInterval; a success resets the interval back to baseInterval. While
// already connected, each tick also runs checkHealth so a session that dies
// with no live agent turn to notice it still gets reconnected. Callers
// should launch this in its own goroutine — it only returns once ctx is
// cancelled.
func (m *Manager) KeepConnected(ctx context.Context, name string, baseInterval, maxInterval, attemptTimeout time.Duration, connect func(ctx context.Context) (Client, error)) {
	wait := baseInterval
	for {
		if m.HasClient(name) {
			m.checkHealth(ctx, name, attemptTimeout)
		}
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
