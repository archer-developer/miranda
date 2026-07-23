# Miranda — project notes for Claude Code

Miranda is a standalone Go Agent Service (the "brain" behind a home voice
assistant built around Home Assistant) — see `README.md` for architecture,
build/test commands, and the HA integration. `docs/PROJECT_PREREQUISITES.md`
has the original design rationale in Russian.

## Environment facts to account for when checking documentation

- **Home Assistant version: 2026.7.2.** Always check API/integration
  documentation and behavior against this version specifically — HA's
  conversation entity platform, MCP Server integration, and other APIs
  referenced here evolve quickly between versions.
- **Yandex Station integration**: we use
  [AlexxIT/YandexStation](https://github.com/AlexxIT/YandexStation) as the
  HA custom_component that exposes Yandex Stations as `media_player`
  entities. Miranda's TTS dispatch (`internal/tts`) targets those entities
  via `media_player.play_media` with `media_content_type: text` (never
  `dialog` — that reopens the station's own mic and conflicts with the
  voice pipeline). When touching TTS/media_player code or docs, check
  behavior against this specific integration, not HA's generic
  `media_player` semantics or other Yandex integrations (`yandex_smart_home`,
  `hass-yandex-music-browser`).

## Conventions

- Write explanatory comments (doc-comments on exported symbols, comments on
  non-obvious logic) — this project intentionally diverges from a
  terse/no-comments default; see repo history for why.
- No Docker, no cgo. Single static Go binary (`CGO_ENABLED=0`), pure-Go
  SQLite (`modernc.org/sqlite`). Keep new dependencies cgo-free.
- Config: every field has a Go-level default in `internal/config.Default()`;
  `config.yaml` only needs to override what differs.
