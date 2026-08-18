package agentloop

import (
	"context"
	"time"
)

// WebUISource is the InputRequest.Source value forced onto any request
// authenticated via a web UI session cookie, regardless of what the client
// sent — a browser can't be allowed to claim source: "ha_assist" and ride
// the HA user-id-mapping path, or spoof another user_id.
const WebUISource = "web_ui"

// TurnTimeout bounds one full agent turn: LLM generation and tool calls,
// which for a turn that calls several tools (or escalates to a slower
// provider) can legitimately take well past what a browser tab, VPN link,
// or reverse proxy will hold a connection open for. TTS dispatch itself no
// longer contributes to this: Dispatcher.Speak only enqueues text onto a
// background Player and returns immediately (see internal/tts/player.go) —
// synthesis and the physical speaker's actual playback duration happen
// entirely off this turn's own goroutine. DetachedTurnContext below is what
// makes TurnTimeout the thing that bounds a turn instead of the caller's
// connection: once a reply may already have been enqueued for TTS or sent
// to Telegram, it must still get recorded to history even if the original
// HTTP/webhook connection drops first.
const TurnTimeout = 5 * time.Minute

// DetachedTurnContext derives a context for one Orchestrator.Handle call
// that keeps parent values (deadlines aside) but is immune to the parent
// being cancelled by the inbound connection closing — see TurnTimeout.
// Without this, a dropped client connection cancels ctx mid-turn, and
// everything downstream that still needs to run (persisting the
// assistant's reply to history so the next turn sees it) fails with
// context.Canceled.
func DetachedTurnContext(parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(parent), TurnTimeout)
}
