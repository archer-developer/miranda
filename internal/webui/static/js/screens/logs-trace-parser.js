// Reassembles internal/llmtrace's block-formatted log lines back into
// structured call records. Pure parsing logic, no DOM — kept apart from
// logs-trace-view.js (the rendering half) so the exact block-framing format
// (see internal/llmtrace/llmtrace.go's Logger.Trace) is understood in one
// place only.
//
// Every traced LLM call reaches the browser as a FLOOD of separate
// hub.Events, one per text line — internal/hub.Writer.Write splits whatever
// it's given on '\n' and publishes one event per line, so a multi-KB request/
// response block never arrives as a single message. Logger.Trace always
// writes a block in this exact shape (blank line included):
//
//   === <RFC3339 timestamp> provider=<name> [conversation=<id>] ===
//   --- request ---
//   <request JSON, pretty-printed, multi-line>
//   --- response ---
//   <response JSON, multi-line -- OR a single "error: <message>" line>
//   <blank line, terminating the block>
//
// Note there is no blank line between the request and response sections —
// only one, right at the very end.

const HEADER_RE = /^=== (\S+) provider=(\S+)(?: conversation=(\S+))? ===$/;
const REQUEST_MARKER = "--- request ---";
const RESPONSE_MARKER = "--- response ---";
const ERROR_PREFIX = "error: ";

/**
 * Attempts JSON.parse, returning null instead of throwing on failure. A
 * block's JSON body can in principle arrive truncated — e.g. the hub's
 * bounded replay buffer (config.WebUI.LogBufferSize) evicting the first
 * lines of a large block while its tail remains — so a parse failure is an
 * expected, handled outcome here, not a bug to surface as an error.
 */
function tryParseJson(text) {
  if (!text) return null;
  try {
    return JSON.parse(text);
  } catch {
    return null;
  }
}

/** Turns one fully-accumulated in-progress block into its final record. */
function finalizeBlock(open) {
  const requestRaw = open.requestLines.join("\n");
  const responseRaw = open.responseLines.join("\n");
  const isError = responseRaw.startsWith(ERROR_PREFIX);
  return {
    timestamp: open.timestamp,
    provider: open.provider,
    conversationId: open.conversationId,
    requestRaw,
    requestJson: tryParseJson(requestRaw),
    responseRaw,
    responseJson: isError ? null : tryParseJson(responseRaw),
    isError,
    errorText: isError ? responseRaw.slice(ERROR_PREFIX.length) : null,
  };
}

/**
 * Creates an incremental, stateful line parser. Call `feed(line)` once per
 * raw text line, in arrival order (a hub.Event's `message`) — it returns a
 * finished call record the instant a line completes one (the blank line
 * terminating a block), and `null` for every line in between.
 *
 * Deliberately tolerant of malformed/partial input rather than throwing:
 * content lines encountered with no block currently open (e.g. the tail of
 * an older block whose header line already scrolled out of the hub's bounded
 * replay buffer) are silently dropped instead of crashing the log viewer. A
 * dangling block that never reaches its terminating blank line (which
 * shouldn't happen — internal/hub.Writer publishes a whole block's lines
 * from one synchronous Write() call, so they always arrive as a contiguous
 * run) simply never completes; nothing is rendered for it until it does.
 */
export function createTraceParser() {
  let open = null; // block currently being accumulated, or null between blocks
  let section = null; // "request" | "response" | null (before the first marker)

  return {
    feed(line) {
      const header = HEADER_RE.exec(line);
      if (header) {
        // A header always starts a new block, even over one that never
        // closed (defensive only — see the doc comment above).
        open = {
          timestamp: header[1],
          provider: header[2],
          conversationId: header[3] || null,
          requestLines: [],
          responseLines: [],
        };
        section = null;
        return null;
      }

      if (!open) return null; // a stray line outside any recognized block

      if (line === REQUEST_MARKER) {
        section = "request";
        return null;
      }
      if (line === RESPONSE_MARKER) {
        section = "response";
        return null;
      }
      if (line === "") {
        const finished = finalizeBlock(open);
        open = null;
        section = null;
        return finished;
      }

      if (section === "request") open.requestLines.push(line);
      else if (section === "response") open.responseLines.push(line);
      return null;
    },
  };
}
