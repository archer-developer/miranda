# Miranda — thin Home Assistant conversation client

This is a **thin client only**. It has no LLM logic, memory, or tool-calling
of its own — it forwards Assist pipeline text (plus the speaker-recognized
`user_id` and `conversation_id`) to a running Miranda Agent Service (the Go binary built from the root of
this repo — see `cmd/miranda`) over HTTP and relays the reply back into the
pipeline. Everything else (model routing, memory, MCP tool calls, TTS
chunking) happens in the Agent Service, not here.

There are **two independent integration points** between Miranda and Home
Assistant, and this document covers both:

1. **HA → Miranda** (this custom component): so Assist can send voice/text
   input to Miranda and speak its reply.
2. **Miranda → HA** (the built-in MCP Server integration): so Miranda can
   call HA services/entities as tools (turn on lights, read sensors, etc).

They use separate tokens and separate HA integrations — don't mix them up.

---

## 1. Prerequisites

- Home Assistant 2024.6 or newer (needed for the config-entry-based
  Conversation entity platform this component uses).
- A running Miranda Agent Service reachable from your HA instance on the
  network (build with `go build ./cmd/miranda` from the repo root, or see
  `Makefile`'s `build`/`run` targets). Note its address, e.g.
  `http://192.168.1.50:8787`.

## 2. Installing the thin client

**Manual install** (no HACS repo published yet):

1. Copy the `miranda` folder from this directory into your Home Assistant
   config's `custom_components` directory, so you end up with:
   ```
   <config>/custom_components/miranda/
     __init__.py
     conversation.py
     config_flow.py
     const.py
     manifest.json
     strings.json
     translations/en.json
   ```
   On a Container/Core install this is typically `/config/custom_components/miranda/`.
2. Restart Home Assistant.
3. Go to **Settings → Devices & Services → Add Integration**, search for
   **"Miranda"**, and select it.
4. Fill in the form:
   - **Base URL**: where Miranda is reachable, e.g. `http://192.168.1.50:8787`
     (this is `server.http_addr` from Miranda's `config.yaml`, with a host
     instead of just a port).
   - **Bearer token**: only if you set `server.auth_token` in Miranda's
     `config.yaml`. Leave empty for LAN-only dev setups with no token.

   The form validates connectivity against Miranda's `/healthz` endpoint
   before saving — if it fails, double-check the URL, that Miranda is
   running, and that HA can reach that host/port.

5. Go to **Settings → Voice Assistants**, open (or create) a pipeline, and
   set **Conversation agent** to **Miranda**. Voice input transcribed by
   that pipeline's STT step will now be forwarded to Miranda, and its reply
   spoken back through that pipeline's TTS step.

   Note: this is *separate* from Miranda's own Yandex Station TTS channel
   (configured under `tts:` in Miranda's `config.yaml`) — the Assist
   pipeline's TTS step and Miranda's direct-to-speaker channel are two
   different output paths that can both be active at once.

### Options

Reopening the integration's entry (**Settings → Devices & Services → Miranda
→ Configure**) lets you adjust the per-request timeout (default 30s — the
agent loop can call an LLM and several tools in one turn, so keep this
generous).

---

## 3. Connecting Home Assistant to Miranda as an MCP server

This is the reverse direction: giving Miranda's agent loop the ability to
call HA services and read entity state as tools. It uses HA's built-in
**Model Context Protocol Server** integration plus a **Long-Lived Access
Token** — unrelated to the bearer token from step 2.

### 3.1 Enable the MCP Server integration

1. **Settings → Devices & Services → Add Integration**, search for
   **"Model Context Protocol Server"**, and add it.
2. This exposes an MCP endpoint at `http://<ha-host>:8123/api/mcp` (Streamable
   HTTP transport).

### 3.2 Choose what Miranda is allowed to control

The MCP server only exposes entities that are **exposed to Assist** — the
same visibility list used by voice control. Go to **Settings → Voice
Assistants → Expose** and toggle on whatever you want Miranda to be able to
query/control (lights, climate, media players, etc). Anything not exposed
here is invisible to Miranda's tool calls, regardless of MCP being enabled.

### 3.3 Create a Long-Lived Access Token

1. Click your **profile** (bottom-left, your name/avatar).
2. Go to the **Security** tab.
3. Under **Long-lived access tokens**, click **Create token**, name it
   something like `miranda-mcp`, and **copy it immediately** — HA only shows
   it once.

Treat this token like a password: it grants API access scoped to whatever
that HA user account can do. Consider creating a dedicated, limited HA user
for Miranda rather than using an admin account's token.

### 3.4 Wire the token into Miranda

Miranda reads MCP tokens from environment variables, not from `config.yaml`
directly (so the token itself never has to touch the config file or git).
Set the environment variable named in `token_env` before starting Miranda,
e.g.:

```bash
export HA_MCP_TOKEN="<the long-lived access token from 3.3>"
```

And in Miranda's `config.yaml`:

```yaml
mcp:
  servers:
    - name: ha
      url: "http://<ha-host>:8123/api/mcp"
      token_env: "HA_MCP_TOKEN"
      enabled: true
```

Restart Miranda. On startup it logs a warning (and skips the server, rather
than failing to start) if it can't connect — check Miranda's logs or the web
UI's live log tail if tools from HA aren't showing up.

### 3.5 (Optional) TTS also needs its own HA credentials

If you're using Miranda's Yandex Station / HA TTS dispatch (separate from
both integrations above — see `tts:` in Miranda's `config.yaml`), it talks
to HA's REST API directly and needs its own token via the `HA_BASE_URL` /
`HA_TOKEN` environment variables. You can reuse the same Long-Lived Access
Token from 3.3 for this, or create a separate one if you want to be able to
revoke them independently.

---

## 4. Testing the full loop

- **Text only, no voice hardware needed**: in HA, go to **Settings → Voice
  Assistants**, open your pipeline, and use the **"Try"** text box at the
  bottom — this exercises the thin client end-to-end without needing a wake
  word or microphone.
- **Direct to Miranda, bypassing HA entirely**: useful for isolating whether
  a problem is in the thin client or in Miranda itself.
  ```bash
  curl -X POST http://<miranda-host>:8787/api/v1/input \
    -H "Content-Type: application/json" \
    -d '{"source":"cli","user_id":"debug","text":"привет"}'
  ```
- Watch Miranda's web UI (`http://<miranda-host>:8787`) for the live log
  tail while testing either path — it shows the request coming in, which
  provider answered, any tool calls, and TTS dispatch.

## Troubleshooting

| Symptom | Likely cause |
|---|---|
| Config flow shows "cannot_connect" | Wrong base URL, Miranda not running, or a firewall/network path issue between HA and Miranda's host. Test with `curl http://<host>:8787/healthz` from the HA host itself. |
| Assist replies with "Не удалось связаться с Miranda" | Same as above, or Miranda is up but returned a non-200 (check Miranda's logs for the actual error — a 401 here specifically means the bearer token configured in this integration doesn't match Miranda's `server.auth_token`). |
| Miranda's tool list from HA is empty / missing an entity | That entity isn't toggled on under **Settings → Voice Assistants → Expose** — the MCP server only sees what's exposed to Assist. |
| Miranda logs "mcp: failed to connect, skipping this server" for `ha` | Check the MCP Server integration is added in HA, the URL/port in `config.yaml` is correct, and `HA_MCP_TOKEN` is set and valid (tokens don't expire by default, but can be manually revoked from the same Security tab). |
