// Lightweight circular avatar cropper. This project's static JS is plain
// ES modules with no bundler (see icons.js's doc comment), so a full
// npm-distributed cropper library isn't a good fit here — this reimplements
// the one interaction avatars need directly on <canvas>: pan by drag, zoom
// by wheel/slider, export a square JPEG blob. Modeled on chat.js's
// thumbnailDataURL for the load-image-into-canvas boilerplate.
import { t } from "./i18n.js";

// On-screen (and exported) size of the crop canvas. The server
// (internal/webui/avatar.go's handlePostAvatar) re-crops/resizes to
// avatarSize=128 regardless of what's sent, so this only needs to be large
// enough for a crisp preview and a decent-quality downscale, not exactly
// 128.
const CROP_PX = 480;

/**
 * Opens a modal circular cropper for `file`. Resolves with a square JPEG
 * Blob once the user clicks Save, or null if they cancel or the file can't
 * be decoded as an image.
 */
export function cropAvatar(file) {
  return new Promise((resolve) => {
    const img = new Image();
    const objURL = URL.createObjectURL(file);
    img.onerror = () => {
      URL.revokeObjectURL(objURL);
      resolve(null);
    };
    img.onload = () => {
      URL.revokeObjectURL(objURL);
      resolve(runCropper(img));
    };
    img.src = objURL;
  });
}

function runCropper(img) {
  return new Promise((resolve) => {
    const overlay = document.createElement("div");
    overlay.className = "fixed inset-0 z-50 flex items-center justify-center bg-black/60 p-4";

    const panel = document.createElement("div");
    panel.className =
      "flex w-full max-w-xs flex-col items-center gap-4 rounded-xl border border-(--color-border) bg-(--color-bg) p-5 shadow-xl";

    const canvas = document.createElement("canvas");
    canvas.width = CROP_PX;
    canvas.height = CROP_PX;
    canvas.className = "w-full max-w-[280px] touch-none cursor-grab rounded-full active:cursor-grabbing";

    const hint = document.createElement("p");
    hint.className = "text-center text-xs text-(--color-text-faint)";
    hint.textContent = t("avatar_crop_hint", "Drag to move, scroll to zoom");

    const zoom = document.createElement("input");
    zoom.type = "range";
    zoom.min = "1";
    zoom.max = "3";
    zoom.step = "0.01";
    zoom.value = "1";
    zoom.className = "w-full max-w-[280px] accent-(--color-accent)";
    zoom.setAttribute("aria-label", t("avatar_crop_hint", "Drag to move, scroll to zoom"));

    const actions = document.createElement("div");
    actions.className = "flex w-full max-w-[280px] gap-2";
    const cancelBtn = document.createElement("button");
    cancelBtn.type = "button";
    cancelBtn.textContent = t("avatar_crop_cancel", "Cancel");
    cancelBtn.className =
      "flex-1 rounded-lg border border-(--color-border) px-4 py-2 text-sm font-medium text-(--color-text-muted) transition-colors hover:bg-(--color-surface-2) focus-visible:outline-none";
    const saveBtn = document.createElement("button");
    saveBtn.type = "button";
    saveBtn.textContent = t("avatar_crop_save", "Save");
    saveBtn.className =
      "flex-1 rounded-lg bg-(--color-accent) px-4 py-2 text-sm font-medium text-white transition-colors hover:bg-(--color-accent-hover) focus-visible:outline-none";
    actions.append(cancelBtn, saveBtn);

    panel.append(canvas, zoom, hint, actions);
    overlay.appendChild(panel);
    document.body.appendChild(overlay);

    // baseScale fits the image's shorter side to the crop circle; `scale`
    // (1..3, driven by the slider/wheel) zooms in from there. offsetX/Y pan
    // in canvas pixels, clamped so the image can never be dragged past the
    // circle's edge.
    const baseScale = Math.max(CROP_PX / img.naturalWidth, CROP_PX / img.naturalHeight);
    let scale = 1;
    let offsetX = 0;
    let offsetY = 0;

    function clampOffsets() {
      const w = img.naturalWidth * baseScale * scale;
      const h = img.naturalHeight * baseScale * scale;
      const maxX = Math.max(0, (w - CROP_PX) / 2);
      const maxY = Math.max(0, (h - CROP_PX) / 2);
      offsetX = Math.min(maxX, Math.max(-maxX, offsetX));
      offsetY = Math.min(maxY, Math.max(-maxY, offsetY));
    }

    function draw() {
      const ctx = canvas.getContext("2d");
      ctx.clearRect(0, 0, CROP_PX, CROP_PX);
      ctx.save();
      ctx.beginPath();
      ctx.arc(CROP_PX / 2, CROP_PX / 2, CROP_PX / 2, 0, Math.PI * 2);
      ctx.clip();
      const w = img.naturalWidth * baseScale * scale;
      const h = img.naturalHeight * baseScale * scale;
      ctx.drawImage(img, (CROP_PX - w) / 2 - offsetX, (CROP_PX - h) / 2 - offsetY, w, h);
      ctx.restore();
      ctx.beginPath();
      ctx.arc(CROP_PX / 2, CROP_PX / 2, CROP_PX / 2 - 1, 0, Math.PI * 2);
      ctx.strokeStyle = "rgba(255,255,255,0.6)";
      ctx.lineWidth = 2;
      ctx.stroke();
    }
    draw();

    let dragging = false;
    let lastX = 0;
    let lastY = 0;
    // Canvas CSS size can be smaller than its CROP_PX pixel buffer (see the
    // max-w-[280px] class above), so pointer deltas need scaling back up to
    // canvas-pixel space or panning would drift slower than the cursor.
    function toCanvasScale() {
      return CROP_PX / canvas.getBoundingClientRect().width;
    }
    canvas.addEventListener("pointerdown", (e) => {
      dragging = true;
      lastX = e.clientX;
      lastY = e.clientY;
      canvas.setPointerCapture(e.pointerId);
    });
    canvas.addEventListener("pointermove", (e) => {
      if (!dragging) return;
      const s = toCanvasScale();
      offsetX -= (e.clientX - lastX) * s;
      offsetY -= (e.clientY - lastY) * s;
      lastX = e.clientX;
      lastY = e.clientY;
      clampOffsets();
      draw();
    });
    const endDrag = (e) => {
      dragging = false;
      try {
        canvas.releasePointerCapture(e.pointerId);
      } catch {
        /* already released (e.g. pointercancel) */
      }
    };
    canvas.addEventListener("pointerup", endDrag);
    canvas.addEventListener("pointercancel", endDrag);

    zoom.addEventListener("input", () => {
      scale = parseFloat(zoom.value);
      clampOffsets();
      draw();
    });
    canvas.addEventListener(
      "wheel",
      (e) => {
        e.preventDefault();
        scale = Math.min(3, Math.max(1, scale - e.deltaY * 0.002));
        zoom.value = String(scale);
        clampOffsets();
        draw();
      },
      { passive: false },
    );

    function close(result) {
      document.body.removeChild(overlay);
      resolve(result);
    }
    cancelBtn.addEventListener("click", () => close(null));
    overlay.addEventListener("click", (e) => {
      if (e.target === overlay) close(null);
    });
    saveBtn.addEventListener("click", () => {
      canvas.toBlob((blob) => close(blob), "image/jpeg", 0.92);
    });
  });
}
