// Maps a reply's `blocks` array (internal/replyformat's Block/Segment JSON —
// see webui's messageView.Blocks, InputResponse.Blocks, ChatEvent.Blocks)
// into a lit-html template. This is what replaces inline-text.js's
// backtick-only fallback for a message once the server has already parsed
// its markdown-lite text into structured blocks — see chat.js's bubble().
//
// Block shape (never both `segments` and `items` set on the same block —
// `segments` for type "paragraph", `items` for type "list"):
//   { type: "paragraph"|"list", segments?: Segment[], items?: Segment[][] }
//   Segment: { type: "text"|"bold"|"italic"|"code"|"link", text: string, url?: string }
// Go's `omitempty` means an absent field is simply missing from the JSON,
// not `null` — every access below guards with `?? []` rather than assuming
// presence.
//
// This only ever renders inside an *assistant* bubble (webui.go's
// messageView only computes Blocks for role "assistant"; user messages
// keep using inline-text.js's renderInlineText, unchanged) — so unlike
// chat.js's bubble() as a whole, nothing here needs to account for the
// white-on-accent user-bubble color case.
import { html } from "./vendor/lit-html/lit-html.mjs";

/**
 * True if url is safe to place in a real, clickable `href` — i.e. it
 * resolves to http(s) or mailto, never `javascript:`/`data:`/`vbscript:`/
 * etc. Segment.url is arbitrary model output, and unlike Segment.text
 * (safe by construction: lit-html auto-escapes it into inert text) a
 * scheme like `javascript:` placed directly in an anchor's href *is*
 * live, clickable code in this authenticated dashboard's own origin — not
 * a hypothetical: a page fetched via the web_fetch/MCP tools could contain
 * attacker-controlled markdown-link syntax that ends up echoed back into
 * the model's own reply text (indirect prompt injection), which
 * replyformat.Parse would then faithfully turn into exactly this link
 * segment. Resolved against location.origin so a relative/protocol-less
 * URL (which can only ever end up http/https here) is allowed without
 * special-casing it.
 */
function isSafeLinkURL(url) {
  try {
    const parsed = new URL(url, window.location.origin);
    return parsed.protocol === "http:" || parsed.protocol === "https:" || parsed.protocol === "mailto:";
  } catch {
    return false;
  }
}

/**
 * One inline segment. Every `.text` value is interpolated as a plain
 * lit-html child expression, so it's auto-escaped exactly like the
 * `textContent`-only discipline inline-text.js relied on — this is what
 * lets Segment.text (arbitrary model output) be trusted here without any
 * manual escaping. `url` (link segments only) needs its own check —
 * see isSafeLinkURL.
 */
function segmentTemplate(seg) {
  switch (seg.type) {
    case "bold":
      return html`<strong class="font-semibold">${seg.text}</strong>`;
    case "italic":
      return html`<em class="italic">${seg.text}</em>`;
    case "code":
      // Same look as inline-text.js's backtick-only code span, so a
      // server-parsed code segment renders identically to the old
      // client-side fallback.
      return html`<code
        class="rounded bg-(--color-surface-2) px-1 py-0.5 font-mono text-[0.85em] break-all"
        >${seg.text}</code
      >`;
    case "link":
      // A url with an unsafe scheme (javascript:, data:, ...) degrades to
      // its bare label text, never a clickable anchor — see
      // isSafeLinkURL's doc comment.
      if (!seg.url || !isSafeLinkURL(seg.url)) return seg.text;
      // target=_blank + rel=noopener,noreferrer: an assistant reply can
      // link anywhere (web_search/web_fetch results, calendar links,
      // etc.) — never let a linked page get a `window.opener` handle back
      // into the dashboard, and never leak the referrer.
      return html`<a
        href=${seg.url}
        target="_blank"
        rel="noopener noreferrer"
        class="break-words text-(--color-accent) underline decoration-(--color-border-strong) underline-offset-2 transition-colors hover:text-(--color-accent-hover)"
        >${seg.text}</a
      >`;
    case "text":
    default:
      // Unrecognized future segment types degrade to plain text rather
      // than being dropped — same "never fail on unexpected input"
      // principle replyformat.Parse itself follows server-side.
      return seg.text;
  }
}

/** One paragraph's or list item's run of segments. */
function segmentsTemplate(segs) {
  return html`${(segs ?? []).map(segmentTemplate)}`;
}

/** One Block — a paragraph or a list. */
function blockTemplate(block) {
  if (block.type === "list") {
    // The `>` immediately before/after the interpolations (no whitespace
    // in between, same trick segmentTemplate's <code>/<a> cases use)
    // matters here: the container this renders into has whitespace-pre-wrap
    // (see chat.js's bubble()), so a literal newline+indentation left as a
    // text node — as a "nicely formatted" multi-line template would — shows
    // up as a real blank line/large gap around the list, not just
    // source-level formatting.
    return html`<ul
      class="list-disc space-y-1 pl-5"
      >${(block.items ?? []).map((item) => html`<li>${segmentsTemplate(item)}</li>`)}</ul
    >`;
  }
  return html`<p>${segmentsTemplate(block.segments)}</p>`;
}

/**
 * Returns a lit-html template rendering `blocks` (see the Block shape
 * above). Callers render into a container that already carries the shared
 * text styling (font size, line height, `whitespace-pre-wrap`, and a
 * `space-y-*` gap between block-level children) — see chat.js's bubble().
 */
export function blocksTemplate(blocks) {
  return html`${(blocks ?? []).map(blockTemplate)}`;
}
