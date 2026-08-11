// Shared helpers for rendering server-provided file chips.
//
// A downloaded file (internal/httpapi/agent_loop.go, e.g. the sandbox's
// download_file) is carried as structured data — history.Message.Downloads /
// InputResponse.Downloads / ChatEvent.Message.Downloads, an array of
// {file_id, filename, size_bytes, mime_type} — never as any kind of tag
// inside the message's own text. An earlier version appended an in-band
// "<download>{json}</download>" marker to the reply text instead; that
// meant a stored assistant reply's text, replayed back to the model as
// conversation history on every later turn, carried that exact tag shape —
// and a model was observed pattern-matching it back into a *new* reply of
// its own, with the wrong id, producing a broken chip. Keeping this
// structurally out of band closes that off entirely: nothing in any
// message's text ever looks like a file reference for a model to copy.
// downloadChip below just renders whatever the caller already has.
//
// An uploaded attachment (internal/httpapi/attachments.go's
// appendAttachmentMarker, appended to a *user* message) is a different
// case, still handled the older, in-text way (extractAttachmentBlocks) —
// unlike a download, the marker's fileURI is real information the model
// needs in order to later call a tool against that same file, not a pure
// UI hint, so folding it into content is what actually gets it into the
// model's context in the first place.
import { icon } from "./icons.js";

/** Format a byte count as a short human-readable string ("1.2 MB", "340 KB", "12 B"). */
export function formatFileSize(bytes) {
  if (bytes < 1024) return `${bytes} B`;
  if (bytes < 1024 * 1024) return `${Math.round(bytes / 1024)} KB`;
  return `${(bytes / (1024 * 1024)).toFixed(1)} MB`;
}

/** A clickable chip linking to a file the model retrieved from a sandbox
 * session via download_file. Clicking it streams the bytes through
 * GET /api/files/{file_id} (Server.handleDownload), authenticated by the
 * same session cookie the rest of the web UI uses. */
export function downloadChip(fileId, filename, size) {
  const el = document.createElement("a");
  el.href = `/api/files/${encodeURIComponent(fileId)}`;
  el.download = filename || "";
  el.className =
    "mt-1 flex w-fit items-center gap-1.5 rounded-lg border border-(--color-border) bg-(--color-surface-2) px-2.5 py-1.5 text-xs text-(--color-text) transition-colors hover:border-(--color-border-strong)";
  // icon() returns our own trusted SVG — safe to set via innerHTML.
  el.innerHTML = icon("download", "h-3.5 w-3.5 shrink-0 opacity-70");
  const nameSpan = document.createElement("span");
  nameSpan.className = "max-w-[200px] truncate";
  nameSpan.textContent = filename;
  el.appendChild(nameSpan);
  if (size != null) {
    const sizeSpan = document.createElement("span");
    sizeSpan.className = "shrink-0 opacity-60";
    sizeSpan.textContent = `· ${formatFileSize(size)}`;
    el.appendChild(sizeSpan);
  }
  return el;
}

/**
 * Strip everything processAttachments (internal/httpapi/attachments.go)
 * appends for an uploaded attachment — the <attachment>{json}</attachment>
 * marker itself (one line of JSON: {filename, mime_type, size_bytes, uri,
 * note}), plus the LLM-facing content blocks that accompany it
 * (<file:name>...</file> for inlined text, "[Изображение: ...]" for
 * images, or the "[Файл ... не найден ...]" notice when the referenced
 * file_id was invalid/expired) — none of these are meant for a human to
 * read verbatim in the chat UI. Returns the cleaned text plus one
 * descriptor per <attachment> marker to render as a chip instead.
 */
export function extractAttachmentBlocks(text) {
  const attachments = [];
  let clean = text;

  clean = clean.replace(/\n\n<attachment>([\s\S]*?)<\/attachment>/g, (_, json) => {
    try {
      const data = JSON.parse(json);
      attachments.push({
        filename: data.filename,
        mimeType: data.mime_type,
        sizeBytes: data.size_bytes,
        uri: data.uri,
      });
    } catch {
      // Malformed marker (shouldn't happen — server-generated): drop it
      // rather than showing raw JSON in the bubble.
    }
    return "";
  });

  clean = clean.replace(/\n\n<file:[^>]+>[\s\S]*?<\/file>/g, "");
  clean = clean.replace(/\n\n\[Изображение: "[^"]+" \([^)]+\)\]/g, "");
  clean = clean.replace(/\n\n\[Файл "[^"]+" не найден[^\]]*\]/g, "");

  return { displayText: clean.trim(), attachments };
}

/** A file attachment shown inside a user bubble in place of the raw
 * content it was uploaded from — see extractAttachmentBlocks. Images
 * render as a thumbnail when previewDataURL is available (only ever true
 * for the client's own just-sent attachment, from its local blob — a
 * reloaded/historical message has no local pixels to show and always
 * renders the compact chip instead), other files as a compact chip. */
export function attachmentChip(filename, size, previewDataURL) {
  const el = document.createElement("div");
  if (previewDataURL) {
    el.className = "mb-1 flex flex-col gap-0.5";
    const img = document.createElement("img");
    img.src = previewDataURL;
    img.alt = filename;
    img.className = "h-[120px] w-[120px] rounded-xl object-cover";
    el.appendChild(img);
    const caption = document.createElement("span");
    caption.className = "max-w-[200px] truncate text-xs text-white/60";
    caption.textContent = filename;
    el.appendChild(caption);
  } else {
    el.className =
      "mb-1 flex items-center gap-1.5 rounded-lg border border-white/20 bg-white/10 px-2.5 py-1 text-xs text-white/75";
    // icon() returns our own trusted SVG — safe to set via innerHTML.
    el.innerHTML = icon("paperclip", "h-3 w-3 shrink-0 opacity-60");
    const nameSpan = document.createElement("span");
    nameSpan.className = "max-w-[200px] truncate";
    nameSpan.textContent = filename;
    el.appendChild(nameSpan);
    if (size != null) {
      const sizeSpan = document.createElement("span");
      sizeSpan.className = "shrink-0 opacity-60";
      sizeSpan.textContent = `· ${formatFileSize(size)}`;
      el.appendChild(sizeSpan);
    }
  }
  return el;
}
