# Telegram channel (`internal/telegram`, `internal/httpapi/telegram.go`)

Optional (`config.TelegramConfig.Enabled`, default false — no safe
default, requires `bot_token` and `public_base_url`).

## Inbound path

Telegram POSTs an update to `webhook_path` →
`handleTelegramWebhook` checks the `X-Telegram-Bot-Api-Secret-Token`
header against a secret generated fresh at every process startup
(`telegram.RandomSecret`, then re-registered via `setWebhook` — nothing to
rotate by hand for the one process meant to own this bot's webhook) → the
sender's `@username` is resolved to a configured `Username` via
`users.Registry.ResolveByTelegramName`. **An unmatched account is logged
as a warning and dropped before `Orchestrator.Handle` is ever called** —
there's no history/memory identity for an unrecognized Telegram account.
A match's chat id is saved into `telegram.ChatStore` (a small JSON file,
`Storage.TelegramChatsPath`) on every message, since the Bot API gives no
way to learn a user's chat id except from a message they sent. The reply
is delivered via `telegram.Client.SendMessage` (the Bot API), not this
handler's HTTP response body.

## RegisterWebhook escape hatch

`TelegramConfig.RegisterWebhook` (default true). A second,
non-production instance sharing a real deployment's
`TELEGRAM_BOT_TOKEN`/`PublicBaseURL` (e.g. `go run ./cmd/miranda` locally
via `MIRANDA_CONFIG_DIR`) can still get a working
`telegram.Client`/`ChatStore` with this set to `false` — it just never
calls `setWebhook`, so it can't steal webhook ownership from the deployed
instance. **Background:** a stray local run once re-registered the real
bot's secret and broke inbound delivery until the real instance restarted;
`RegisterWebhook: false` is the fix.

## Outbound path: `send_telegram` tool

`Orchestrator.SetTelegram` wires this in; `config.TelegramConfig.SendMessageTool`
enables it. The model can push a message to any household member's
Telegram — the current user by default, or another one resolved by
`users.Registry.ResolveByDisplayName` (matching e.g. "Аня" against
`FullName`/`Username`). Fails with a clear error if the target has never
messaged the bot, since that's the only way `ChatStore` ever learns a
chat id.
