# Native notification channel (`internal/notify`)

The default proactive-notification channel — a persisted, per-user feed
(the web UI's bell icon) with an optional Web Push delivery leg layered on
top. Replaced Telegram's old always-on auto-CC role for reminders; see
`docs/adr/native-notifications.md` for the full rationale.

On by default (`config.NotifyConfig.Enabled`, unlike WebAuthn/Telegram —
the feed itself needs no deployment secret, same posture as
`ScheduleConfig`). The nested `WebPush` leg (`config.WebPushConfig`) is a
separate opt-in (`Enabled` defaults false) — VAPID keys are
deployment-specific, generated once via `go run ./cmd/miranda vapid-keys`.

## Structure

- `Store` (`store.go`) — one SQLite file (`Storage.NotifySQLitePath`,
  isolated the same way `WebAuthnSQLitePath`/`ScheduleSQLitePath` are)
  holding two tables: `notifications` (the feed) and
  `webpush_subscriptions` (one row per browser, keyed by `endpoint` —
  globally unique per the Push API spec, so a re-subscribe is a plain
  upsert).
- `Service` (`service.go`) — the one entry point everything else calls.
  `Notify(ctx, userID, title, body, source)` always persists a row, then
  best-effort pushes to every registered subscription if `WebPush` is
  configured (via `github.com/SherClockHolmes/webpush-go`); a `404`/`410`
  response prunes that subscription so it's never retried again — same
  "don't retry a deterministic failure forever" reasoning
  `deliverReminder`'s Telegram leg already used.
- This package knows nothing about `internal/hub` or `internal/agent_loop`
  — same separation `internal/telegram` keeps. It cannot publish a live
  "new notification" event itself; see `Orchestrator.notifyUser` below.

## Orchestrator wiring

`Orchestrator.SetNotify(svc, cfg)` wires the feed in (nil = `send_notification`
never offered, `deliverReminder`'s notify legs are skipped).
`Orchestrator.notifyUser(ctx, userID, title, body, source)`
(`internal/agent_loop/orchestrator.go`) is the one path both
`send_notification`'s dispatch and `deliverReminder`'s legs go through
instead of calling `notify.Service.Notify` directly — it calls `Notify`,
then publishes `hub.Event{Source: "chat", UserID: userID, Data:
ChatEvent{Type: "notification", Notification: &n}}` on the same per-user
chat WS channel `GET /ws/chat/{username}` already streams (no separate
live-update connection needed).

## `deliverReminder`'s channel change

See `docs/adr/native-notifications.md` §2.3 for the exact diff, but in
short: the old "always-additionally Telegram, unless origin was already
Telegram" leg is gone. A web/unknown-origin reminder now notifies through
this package as its origin-channel delivery (replacing the old silent
`appendReminderToHistory`); voice/Telegram-origin reminders still get an
additional best-effort Notify leg so every reminder ends up in the feed
exactly once, but Telegram is no longer auto-CC'd on every firing —
Telegram now only fires when the reminder's origin literally was
Telegram, or a user explicitly asks for Telegram by name via
`send_telegram` (whose tool description was rewritten specifically to
require that explicit ask — see `tool_catalog.go`).

## `send_notification` tool

Mirrors `send_telegram`'s exact shape (`{text, recipient}`,
`Orchestrator.resolveRecipient` — the shared recipient-resolution helper
both dispatch handlers use). Enabled whenever `o.notify != nil &&
NotifyConfig.SendNotificationTool`. Its description explicitly frames it
as the default channel — `send_telegram`'s own description was rewritten
in tandem to say "ONLY when the user explicitly names Telegram" so the
two tools' zones of responsibility don't overlap.

## Web UI

`internal/webui`'s `Notify` interface (`webui.go`) is the subset of
`*notify.Service` the dashboard needs — a nil `Notify` disables every
`/api/notifications*`/`/api/push/*` route (mirrors `WebAuthnService`'s
nil-disables convention). Watch the classic Go footgun here:
`cmd/miranda`'s `notifyForWebUI` must stay a true nil interface, not a
nil `*notify.Service` wrapped in one — see that variable's doc comment in
`main.go`, same reasoning `webauthnSvc`'s already documents.

Routes (`notifications.go`), all scoped to `currentUser(r)`, never a
client-supplied id:

| Route | What it does |
|---|---|
| `GET /api/notifications` | List, newest first, capped at `?limit=` (default 50). |
| `POST /api/notifications/read` | `MarkAllRead` — called when the bell's list screen mounts or live-reloads. |
| `GET /api/notifications/unread-count` | Paints the bell's dot at app boot. |
| `GET /api/push/vapid-key` | Only mounted when `WebPushEnabled()` — raw public key text for `pushManager.subscribe()`. |
| `POST` / `DELETE /api/push/subscribe` | Register/remove this browser's Web Push subscription. |

Frontend: `static/js/notify-badge.js` (the bell's dot — see its doc
comment for why the icon lives in a separate `#notify-bell-icon` child
span rather than being painted via `bell.innerHTML`, which would wipe out
the dot sibling), `static/js/push.js` (subscribe/unsubscribe, mirrors
`webauthn.js`'s shape), `static/js/screens/notifications.js` (the list
screen — `refreshAndMarkRead()` is exported and shared between its own
`mount()` and `app.js`'s live chat-ws.js "notification" event listener,
so "opened the list" and "list already open, new one arrived" go through
identical code), `static/js/sw.js` (`push`/`notificationclick` handlers).

**JSON tags matter**: `notify.Notification`/`notify.Subscription`
(`store.go`) are marshaled straight out over both the REST endpoints and
the live `ChatEvent` payload — missing/wrong `json:"..."` tags here
silently break the frontend (found once already: an untagged struct sent
`Body`/`CreatedAt` instead of `body`/`created_at`, and
`renderInlineText`/`formatDate` failed on `undefined` with no server-side
error at all).

**iOS/Safari-only 403 "BadJwtToken"**: `service.go`'s
`normalizeVAPIDSubject` strips a leading `mailto:` off
`WebPushConfig.Subject` before handing it to webpush-go as
`Options.Subscriber` — that library's own `getVAPIDAuthorizationHeader`
(vapid.go) re-prepends `mailto:` to anything not already starting with
`https:`, so passing our config's RFC 8292-idiomatic `"mailto:foo@bar"`
subject straight through used to double it into the JWT's `sub` claim as
`"mailto:mailto:foo@bar"`. Google's FCM (every Android/desktop-Chrome
subscription) silently tolerates that malformed value; Apple's push
service (`web.push.apple.com` — i.e. every iOS/Safari subscription,
including Chrome-on-iOS since it's a WebKit wrapper there) validates it
strictly and rejects the whole send with `403`/`"BadJwtToken"` — logged as
`notify: push send rejected ... status=403`, easy to miss since Notify's
per-subscription send loop never fails the caller for it (see
pushToSubscriptions' own best-effort doc comment). This is
`SherClockHolmes/webpush-go#81`, open and unfixed upstream as of this
writing — `TestNotify_VAPIDSubjectNotDoubled` (service_test.go) is the
regression test, decoding the real JWT webpush-go produces rather than
just unit-testing `normalizeVAPIDSubject` in isolation, specifically so a
future webpush-go upgrade that changes this behavior gets caught here
instead of silently reintroducing the bug.
