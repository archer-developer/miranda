// Shared helpers for rendering <download>{json}</download> markers that
// appendDownloadMarkers (internal/httpapi/agent_loop.go) deterministically
// appends to an assistant reply whenever the model retrieved a file from a
// sandbox session via download_file — injected server-side regardless of
// what the model's own text says, so any screen displaying message content
// always shows a working link. Used by both screens/chat.js (the live
// conversation) and screens/history.js (the read-only dialog browser) so a
// past conversation containing a download renders a chip there too, instead
// of the raw marker text.
import { icon } from "./icons.js";

/** Format a byte count as a short human-readable string ("1.2 MB", "340 KB", "12 B"). */
export function formatFileSize(bytes) {
  if (bytes < 1024) return `${bytes} B`;
  if (bytes < 1024 * 1024) return `${Math.round(bytes / 1024)} KB`;
  return `${(bytes / (1024 * 1024)).toFixed(1)} MB`;
}

/**
 * Strip <download>{json}</download> blocks from text and return a list of
 * download descriptors to render as chips instead. Each block's payload is
 * one line of JSON: {file_id, filename, size_bytes, mime_type}.
 */
export function extractDownloadBlocks(text) {
  const downloads = [];
  const clean = text.replace(/\n\n<download>([\s\S]*?)<\/download>/g, (_, json) => {
    try {
      const data = JSON.parse(json);
      downloads.push({
        fileId: data.file_id,
        filename: data.filename,
        sizeBytes: data.size_bytes,
        mimeType: data.mime_type,
      });
    } catch {
      // Malformed marker (shouldn't happen — server-generated): drop it
      // rather than showing raw JSON in the bubble.
    }
    return "";
  });
  return { displayText: clean.trim(), downloads };
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
