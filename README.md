# Miranda

Miranda is the "brain" behind a custom home voice assistant built around
Home Assistant — a standalone Go **Agent Service**, not a custom_component
living inside HA. It compiles to a single self-contained binary (no Docker,
no cgo), routes conversations across multiple LLM providers, keeps dialog
history in embedded SQLite and long-term memory as per-user markdown files,
calls tools over MCP (Home Assistant and others), and speaks replies through
Yandex Station. See `docs/PROJECT_PREREQUISITES.md` for the full design
rationale.

```
HA (voice input, MCP tools, TTS output) <---> Miranda Agent Service <---> LLM providers
```

---

## Building

Requires Go 1.25+ (the module pins `go 1.25.0`; with `GOTOOLCHAIN=auto` —
the default — `go build` fetches a matching toolchain automatically if your
installed `go` is older).

```bash
go build -o miranda ./cmd/miranda
# or
make build
```

The binary is fully static (`CGO_ENABLED=0`, pure-Go SQLite driver via
`modernc.org/sqlite`), so cross-compiling for another host is just:

```bash
GOOS=linux GOARCH=arm64 go build -o miranda-linux-arm64 ./cmd/miranda
```

Run it with `make run` (builds then runs), or directly:

```bash
MIRANDA_CONFIG=./config/config.yaml ./miranda
```

## Testing

```bash
make test        # go test ./... -race — unit tests + the black-box agent-loop
                  # integration test in test/integration (fake LLM + fake MCP,
                  # real SQLite/history/memory, real HTTP server)
make lint         # golangci-lint run ./...
make fmt          # gofmt + goimports
make check        # fmt + lint + test — run this before committing
```

`make lint`/`make check` need `golangci-lint` and `goimports` on `PATH` —
`make tools` installs both (plus the Tailwind CLI, see below).

## Configuration

Copy `config/config.example.yaml` to `config/config.yaml` and edit it — every
field has a built-in default (see `internal/config/config.go`), so you only
need to override what differs. Secrets (API keys, tokens) are never put in
the file directly: each provider/server entry names an environment variable
(`api_key_env`, `token_env`) to read at startup instead.

```bash
cp config/config.example.yaml config/config.yaml
```

For local development, those environment variables don't need to be
exported by hand every session: copy `.env.example` to `.env` and fill it
in — Miranda loads it at startup (`internal/envfile`). A variable already
set in the real environment always wins over `.env`, so this has no effect
in production setups (systemd, etc.) that already set secrets some other
way.

```bash
cp .env.example .env
```

Key sections: `llm.providers` (the fallback chain — one entry per model
backend, `type: openai_compat` for any OpenAI Chat Completions compatible
server — Ollama, vLLM, LM Studio, OpenRouter — or `type: anthropic` for
Claude), `mcp.servers` (tool sources, see below), `tts` (Yandex Station
routing), `storage` (SQLite + memory file paths), `users` (web UI login
accounts — see **Web UI** below).

## Logging

Everything printed to the terminal is also mirrored to
`logging.dir/miranda.log` (`./logs/miranda.log` by default), rotated by size
so it never grows unbounded (`logging.max_size_mb` / `max_backups` /
`max_age_days`).

Separately, `logging.dir/llm.log` traces every single LLM request and
response: the exact system prompt, message history, and tools sent, plus
the model's reply text/tool calls (or the error, if the call failed) —
this is the tool for figuring out *why* a prompt didn't produce the tool
call or answer you expected. It's written by `internal/llm/router`
regardless of which provider handled the turn (including both legs of an
escalation handoff), and each block is tagged with `conversation=<id>` so
you can grep one dialog's turns out of the file.

## Web UI

Miranda serves a monitoring dashboard at its configured `server.http_addr`
(default `http://localhost:8787`) — live log tail over WebSocket, dialog
history browser, and a debug text input that hits the same unified command
interface HA uses. It's server-rendered Go (`html/template` + vanilla JS,
`internal/webui`), styled with Tailwind CSS v4.

Tailwind is compiled ahead of time by its **standalone CLI** (no Node/npm
needed, not even at build time for the Go binary — the generated
`internal/webui/static/css/styles.css` is committed and embedded via
`go:embed`). Only regenerate it after changing markup in
`internal/webui/templates` or `internal/webui/static`:

```bash
./scripts/download-tailwindcli.sh   # once, installs bin/tailwindcss
make css                             # regenerates styles.css
```

### Login

The web UI requires signing in — there's no anonymous access, and no
opt-out (unlike `server.auth_token`'s bearer-token auth for HA/curl, which
stays open when unset for LAN-only dev use). With no `users` configured, the
dashboard is unreachable by design; that's the fail-closed default, not a
bug to work around.

Add accounts under `users` in `config.yaml`:

```yaml
users:
  - username: alex
    password_hash: "$2a$10$..." # go run ./cmd/hashpw <password>
    full_name: "Alex"
    avatar: "" # https URL, or a filename under storage.avatars_dir
    ha_user_id: "" # see below
    telegram_name: ""
    language: "ru" # ru | be | en, this user's default after login
```

Generate a password hash (plaintext passwords are never stored or logged):

```bash
go run ./cmd/hashpw 'your-password'
```

**Username is the canonical identity for memory/history** — the same
`data/memory/<username>.md` and SQLite rows are used whether a person talks
to Miranda by logging into the web UI or by speaking through HA. The two
channels are reconciled via `ha_user_id`: HA's speaker-recognition system
sends its own (HA-internal) user id with every voice turn, and if it matches
a configured user's `ha_user_id`, Miranda maps it to that user's `username`
before touching history/memory. Find the right value by checking what HA
sends — Miranda logs the raw incoming `user_id` for unmatched HA requests —
or from the HA person/user's own id. A web UI login session always uses its
own username directly; the debug form no longer asks for `user_id` or a
bearer token — your identity comes entirely from being logged in, and the
same-origin session cookie is sent automatically.

### Language

The dashboard and login page are available in Russian (default), Belarusian,
and English — switch with the header's RU/BE/EN links (stored in a cookie,
no reload-losing-state JS needed) or set a per-user default via `language`
in their `users` entry. This only affects UI chrome; it has nothing to do
with what language you can talk to Miranda in, which is unconstrained.

---

## Home Assistant integration

There are **two independent integration points** between Miranda and Home
Assistant, covered in the two sections below. They use separate tokens and
separate HA integrations — don't mix them up.

1. **Home Assistant as an MCP server** — gives Miranda's agent loop the
   ability to call HA services and read entity state as tools (turn on
   lights, read sensors, etc).
2. **Miranda as a Home Assistant conversation agent** — a thin
   custom_component (`ha-integration/miranda`) that forwards Assist
   pipeline text to Miranda and speaks its reply back. No LLM logic lives in
   HA; everything (routing, memory, tool calls, TTS chunking) happens in the
   Agent Service.

### Connecting Home Assistant as an MCP server

1. **Settings → Devices & Services → Add Integration**, search for
   **"Model Context Protocol Server"**, and add it. This exposes an MCP
   endpoint at `http://<ha-host>:8123/api/mcp` (Streamable HTTP transport).
2. **Choose what Miranda can control**: the MCP server only exposes entities
   that are **exposed to Assist** — go to **Settings → Voice Assistants →
   Expose** and toggle on whatever you want Miranda to query/control.
   Anything not exposed there is invisible to Miranda's tool calls.
3. **Create a Long-Lived Access Token**: click your profile (bottom-left) →
   **Security** tab → **Long-lived access tokens** → **Create token**. Copy
   it immediately — HA only shows it once. Treat it like a password;
   consider a dedicated, limited HA user for Miranda rather than an admin
   account's token.
4. **Wire the token into Miranda** via an environment variable (never in
   `config.yaml` directly):
   ```bash
   export HA_MCP_TOKEN="<the long-lived access token>"
   ```
   (or, for local development, add `HA_MCP_TOKEN=<token>` to `.env` instead —
   see Configuration above.)
   ```yaml
   # config.yaml
   mcp:
     servers:
       - name: ha
         url: "http://<ha-host>:8123/api/mcp"
         token_env: "HA_MCP_TOKEN"
         enabled: true
   ```
   Restart Miranda. A server that fails to connect is logged and skipped
   rather than blocking startup — check Miranda's logs or the web UI's live
   log tail if HA's tools aren't showing up.
5. **(Optional) TTS needs its own HA credentials.** Yandex Station dispatch
   talks to HA's REST API directly (`media_player.play_media`) and needs its
   own token via the `HA_BASE_URL` / `HA_TOKEN` environment variables. You
   can reuse the token from step 3, or create a separate one to revoke
   independently.

### Connecting Miranda to Home Assistant (thin conversation client)

The custom_component lives in `ha-integration/miranda`.

**Prerequisites**: Home Assistant 2024.6+ (needed for the config-entry-based
Conversation entity platform), and a running Miranda instance reachable from
your HA host, e.g. `http://192.168.1.50:8787`.

1. Copy the `ha-integration/miranda` folder into your HA config's
   `custom_components` directory, so you end up with
   `<config>/custom_components/miranda/...` (on Container/Core installs
   that's typically `/config/custom_components/miranda/`).
2. Restart Home Assistant.
3. **Settings → Devices & Services → Add Integration**, search for
   **"Miranda"**. Fill in:
   - **Base URL**: where Miranda is reachable (`server.http_addr` from
     `config.yaml`, with a real host instead of just a port).
   - **Bearer token**: only if `server.auth_token` is set in Miranda's
     `config.yaml`; leave empty for LAN-only dev setups.

   The form validates connectivity against Miranda's `/healthz` endpoint
   before saving.
4. **Settings → Voice Assistants**, open (or create) a pipeline, and set
   **Conversation agent** to **Miranda**. Voice input transcribed by that
   pipeline's STT step is now forwarded to Miranda (`source: ha_assist`);
   its reply is spoken back through that pipeline's TTS step *and*
   dispatched to Miranda's own Yandex Station channel — `ha_assist` is the
   only source that gets the direct TTS dispatch automatically, since it's
   the only channel with a physical speaker to answer through. Every other
   channel (the web UI, a future Telegram bot/mobile app) only gets its
   reply back over its own connection (the HTTP response) unless the user
   explicitly asks to hear it, via the model's `speak_reply` tool.
5. The integration ships with English, Russian, and Belarusian translations
   for its config flow; Home Assistant picks the one matching your user
   profile's language automatically.

Reopening the integration's entry (**Configure**) lets you adjust the
per-request timeout (default 30s — the agent loop can call an LLM and
several tools in one turn).

### Testing the full loop

- **No voice hardware needed**: **Settings → Voice Assistants**, open your
  pipeline, use the **"Try"** text box.
- **Bypass HA entirely** (isolates thin-client vs. Miranda issues):
  ```bash
  curl -X POST http://<miranda-host>:8787/api/v1/input \
    -H "Content-Type: application/json" \
    -d '{"source":"cli","user_id":"debug","text":"привет"}'
  ```
- Watch Miranda's web UI for the live log tail while testing either path.

### Troubleshooting

| Symptom | Likely cause |
|---|---|
| Config flow shows "cannot_connect" | Wrong base URL, Miranda not running, or a network path issue between HA and Miranda's host. Test with `curl http://<host>:8787/healthz` from the HA host itself. |
| Assist replies with "Не удалось связаться с Miranda" | Same as above, or Miranda returned a non-200 (check Miranda's logs — a 401 specifically means the bearer token in the integration doesn't match `server.auth_token`). |
| Miranda's tool list from HA is missing an entity | That entity isn't toggled on under **Settings → Voice Assistants → Expose**. |
| Miranda logs "mcp: failed to connect, skipping this server" for `ha` | Check the MCP Server integration is added in HA, the URL/port in `config.yaml` is correct, and `HA_MCP_TOKEN` is set and valid. |

---

## Project layout

```
cmd/miranda/            entrypoint — wires config, storage, LLM router, MCP, TTS, HTTP server
internal/config/        YAML config + defaults
internal/httpapi/       unified command interface (POST /api/v1/input), the agent loop, /ws/logs
internal/hub/           in-process log/event broadcast for the web UI
internal/llm/           provider-agnostic chat interface
  openaicompat/           client on the official openai-go SDK (any OpenAI-compatible backend)
  anthropic/              client on the official anthropic-sdk-go SDK
  router/                 fallback chain + escalate_to_claude handoff
internal/mcp/            MCP Client/Manager abstraction, multi-server tool-name prefixing
internal/history/        SQLite (pure-Go, no cgo) dialog log with FTS5 search
internal/memory/         per-user markdown long-term memory
internal/tts/            sentence-boundary chunking + Yandex Station dispatch
internal/ha/             minimal Home Assistant REST client (for TTS dispatch)
internal/webui/          Tailwind v4 dashboard, embedded via go:embed
test/integration/        black-box agent-loop test (fake LLM + fake MCP, real everything else)
ha-integration/miranda/  the HA thin conversation client custom_component
docs/                    design docs
```
