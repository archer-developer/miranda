# Redaction (`internal/redact`)

`internal/redact` masks sensitive values out of text **before it reaches
disk** — `"пин-код от телефона Ани 665533"` is stored as `"пин-код от
телефона Ани ******"`. On by default (`config.RedactConfig`, unlike most
gated features it needs no key or URL).

**The boundary is the disk, not the network.** The in-flight request still
carries the user's original words to the model, so the assistant can act on a
secret it was just told. The next turn replays the conversation from SQLite,
so it reads the masked text and no longer knows the value. This is deliberate;
masking is irreversible, and there is no vault, no reveal in the web UI, and
no un-masking CLI.

Detection is deterministic — no model call, no map iteration, and `Redact` is
idempotent so text copied sink-to-sink (e.g. `restore.go`) is not
progressively mangled. Two rule families:

- **Anchored**: a trigger word from the lexicon plus something value-shaped
  within `window_runes` after it. This is what catches a bare six-digit
  number, which no standalone pattern could flag without also flagging "мне 45
  лет". The lexicon is *configuration* (`config.Default()`), and a trigger
  preceded by a letter or digit is rejected as a mid-word match — that alone
  handles "промокод"; `trigger_exclusions` covers what it can't see
  (hyphenated compounds, phrases like "код ошибки").
- **Format**: self-identifying values needing no trigger — Luhn-checked card,
  JWT, prefixed API keys, PEM block, SNILS, IBAN. These are *code*, in this
  package's registry; config only names which are on, and an unknown name
  fails startup.

Wired at the **sinks**, never at the call sites, so no future write path can
forget:

| Sink | Where |
|---|---|
| SQLite messages, tool calls, system prompts, recaps (and the FTS index, via triggers) | `history.Store.SetRedactor` — applied inside every `Append*`/`Set*` method |
| `data/memory/*.md` | `memory.Store.SetRedactor` — applied in `writeFile`, the one function all four write paths funnel through |
| `schedule` DB `prompt` (both tables) | `schedule.Store.SetRedactor` |
| `logs/llm.log`, the web UI's live LLM-trace tab, **and** `logs/anomalies/` | `redact.Tracer` wrapping the **outer** `llmtrace.ContextTracer` in `cmd/miranda` — that one placement covers all three, because `ContextTracer` fans out below it (wrapping its `Default` instead would leave the ctx-attached `anomaly.Recorder` unmasked) |
| `logs/miranda.log`, stdout/**the systemd journal**, and the web UI's `app_log` tab | `httpapi.Server.SetRedactor` — `handleInput` logs the raw request body before auth and before parsing (`server.go`), which makes it the *earliest* appearance of user text in the process, ahead of every store |

That last row is easy to miss and was missed once: the store-level masking is
not sufficient on its own, because `handleInput` deliberately logs the body
verbatim so a misconfigured HA client is diagnosable. The body's *shape* is
what makes that line useful, so masking the values inside it costs nothing.
Note the journal is the worst of the log sinks to leak into — `config.Logging`
governs `miranda.log`'s rotation but has no say over `journalctl`'s retention.

`internal/backup` needs nothing: it copies databases that are already masked.
Out of scope by design: the hub publishes to `/ws/chat` and `/ws/logs`, which
are RAM plus the browser, not disk.

Residual, deliberately not wired: `internal/tools`' Tavily calls log the
model's search `query` and fetch `url` at **debug** level. A secret reaching
one would mean the model chose to web-search it, which is a different problem;
noted here so it is a decision rather than an oversight.

Every value span is guaranteed free of `"` and `\`, and the mask character
needs no escaping — that is what makes masking a marshalled request in
`llm.log` structurally unable to produce invalid JSON, rather than merely
unlikely to.

**The accepted limit**: a bare value with no trigger word near it and no
self-identifying format is *not* masked — `{"ok":"665533"}` stays as it is,
because that number is indistinguishable from "мне 45 лет" without a model
call, and masking every number a household assistant hears is worse. In
practice the surrounding text carries the trigger (a tool result echoes the
fact it just saved), which is why the end-to-end sweep passes on realistic
data. `internal/redact.TestUntriggeredValueIsNotMasked` pins this down so the
sweep isn't read as a stronger promise than it is.

`TestSecretNeverReachesDisk` (`internal/redact/disk_boundary_test.go`) is the
test that checks the actual promise: it drives every store with the shipped
lexicon and then blind-walks the data directory looking for the raw value. It
deliberately greps files rather than checking a list of columns, so a new
table or a new sink that forgets to redact fails it without anyone remembering
to extend it.
