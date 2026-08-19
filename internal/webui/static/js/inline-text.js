// Minimal inline-markdown rendering for message bubbles: handles only single
// backtick code spans (`` `text` ``), since that's the one construct the
// model reliably produces for file paths/commands/identifiers in a reply,
// and the one this app previously had no visual treatment for — a reply
// would show the literal backticks instead of a styled span. Deliberately
// not a full markdown parser: bold/italic/links/etc. are left as plain text.
const CODE_SPAN_RE = /`([^`\n]+)`/g;

// Default <code> span style — tuned for a neutral/light surface (the
// assistant bubble's bg-(--color-surface)/70, or history.js's plain page
// background). Callers rendering onto a colored surface (chat.js's blue
// bg-(--color-accent) user bubble) must pass their own codeClassName —
// this default's bg-(--color-surface-2) reads as a barely-visible dark
// smudge against that blue, not a code span.
const DEFAULT_CODE_CLASS = "rounded bg-(--color-surface-2) px-1 py-0.5 font-mono text-[0.85em] break-all";

/**
 * Append `text` to `el` as a mix of text nodes and <code> spans for any
 * `` `backtick-quoted` `` segments. Every piece of message content still
 * reaches the DOM only via textContent/createTextNode — never innerHTML —
 * so untrusted user/assistant text can never be interpreted as markup.
 * codeClassName overrides the <code> span's classes — see
 * DEFAULT_CODE_CLASS's doc comment for when a caller needs to.
 */
export function renderInlineText(el, text, codeClassName = DEFAULT_CODE_CLASS) {
  let lastIndex = 0;
  for (const match of text.matchAll(CODE_SPAN_RE)) {
    if (match.index > lastIndex) {
      el.appendChild(document.createTextNode(text.slice(lastIndex, match.index)));
    }
    const code = document.createElement("code");
    code.className = codeClassName;
    code.textContent = match[1];
    el.appendChild(code);
    lastIndex = match.index + match[0].length;
  }
  if (lastIndex < text.length) {
    el.appendChild(document.createTextNode(text.slice(lastIndex)));
  }
}
