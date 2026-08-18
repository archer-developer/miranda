# File staging and download proxy (`internal/attachments`)

Two opposite directions — uploading files *to* external MCP services, and
proxying files *from* them back to the browser — both managed through
`internal/attachments.Store`.

## File staging (browser → Miranda → external MCP service)

Optional (`config.FileUploadConfig.Enabled`, default false). The backend,
not the model, moves file bytes — Miranda hosts the file and hands the
model a URI; whichever MCP tool needs the bytes fetches them itself.

**Why pull, not push:** an earlier version pushed bytes directly to
whichever server the model referenced. It failed when the model called a
*different* tool on the same server that accepted raw bytes directly
(`miranda-medical-card`'s `medical.upload_file`, `data` argument),
reproducing the exact hallucination bug this exists to prevent. Nothing
short of removing every raw-bytes-accepting tool closes that gap —
which is what the current design does. See
**`docs/adr/file-staging-refactor.md`** for the full incident writeup.

**How it works:**
- `POST /api/upload` (`internal/httpapi/upload.go`) stages the file into
  `attachments.Store`, generating an id via `attachments.NewFileID()`
  (24 random bytes, hex — same pattern as `telegram.RandomSecret`).
- `processAttachments` builds `fileURI = PublicBaseURL + "/files/" + fileID`
  and includes it in the prompt for every attachment (even inlined images
  get a URI in case a tool needs the file itself).
- `GET /files/{id}` (`handleFilesServe`) is **unauthenticated** — the id's
  randomness plus the store's TTL is the security boundary, same design
  as `GET /tts-audio/{filename}` for TTS audio.
- `cmd/miranda` refuses to start with file upload enabled and no
  `public_base_url` set.

**Model instruction:** never construct or embed file bytes in a tool call;
always pass the `fileUri` verbatim.

**UI rendering:** `processAttachments` appends one
`<attachment>{json}</attachment>` marker per file (fields: `filename`,
`mime_type`, `size_bytes`, `uri`, `note`). `downloads.js`'s
`extractAttachmentBlocks` (shared by `screens/chat.js` and
`screens/history.js`) strips this and renders a chip. **The marker is a
structured tag, not prose** — an earlier version matched an exact Russian
sentence, and it silently broke (chip disappeared, raw text leaked) the
moment that sentence's wording changed in the same diff.

## File download proxy (external MCP service → browser)

Some MCP servers return a link to a file they host rather than accepting
one (e.g. `miranda-medical-card`'s `medical.get_document` returning a
`fileUri` on `https://127.0.0.1:8791/...` — an internal address requiring
a bearer token, useless to a browser).

`GET /api/files/{file_id}` (`handleDownload`) is the one **authenticated**
download route: it proxies to whichever remote file an
`attachments.Record` names via `RemoteURL` (fetched with `RemoteToken` as
bearer auth), rather than a single hardcoded target.

**One mechanism, all servers:** a server opts in via
`config.MCPServer.ExposeFiles` (`expose_files: true`). Its
`FilesEndpoint()` lands in `Orchestrator`'s `mcpExtensions` map
(`SetMCPServerExtensions`). `executeTool` scans every tool call result
from an opted-in server via `detectRemoteFileLinks`
(`internal/agent_loop/downloads.go`) for a URL matching that prefix:

- **JSON results** — sibling fields (`title`/`filename`/`name`/
  `size_bytes`/`mime_type`) are read via `findSiblingObject` +
  `stringSiblingField`/`int64SiblingField`.
- **Plain "key: value" text results** (the sandbox's `download_file`,
  which reports `file_id: ...\nfile_uri: ...\nfilename: ...\n...`) — the
  same metadata is read from matching lines via
  `keyValueStringField`/`keyValueInt64Field`.

Only URLs matching an explicitly opted-in server's `FilesEndpoint()` are
ever proxied — untrusted input must never make Miranda's backend proxy,
with that server's bearer token, to an arbitrary address. Missing metadata
degrades cleanly: the download chip omits size/name rather than guessing.

**Why `download_file` needs a `file_uri` field:** the sandbox's tool must
return a full `file_uri`, not just a bare `file_id`, for the URL-matching
detection above to fire. See `docs/adr/file-staging-refactor.md` §6 for
the incident and the exact contract each file-exposing server must follow.

**Config warning vs. hard failure:** `cmd/miranda` fails startup on a
malformed opted-in server URL, but only *logs a warning* when
`file_upload.enabled` is true and the map is empty — a deployment that
only wants the upload direction is legitimate, but a forgotten
`expose_files: true` should still be visible somewhere.
