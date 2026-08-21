// Profile screen: read-only account info (config.yaml-sourced — same
// pattern as everywhere else in this project, hand-edit the file rather
// than an in-app editor) plus avatar upload and passkey management. The
// avatar is the one field this screen writes rather than just displays —
// see handleAvatarFile below and internal/webui/avatar.go's
// handlePostAvatar. The passkeys section only renders if both the server
// has WebAuthn enabled and this browser context actually supports it
// (window.PublicKeyCredential — false on any insecure origin), so there's
// never a control that would just fail if clicked.
import { t } from "../i18n.js";
import { icon } from "../icons.js";
import { showToast } from "../toast.js";
import * as webauthn from "../webauthn.js";
import { cropAvatar } from "../avatar-crop.js";

const user = window.MIRANDA_USER || {};

// Used for the profile screen's own (large) preview circle — #avatar-preview
// fills #avatar-dropzone (h-32 w-32), so h-full/w-full here resolves
// against that. The header's smaller circle uses headerAvatarMarkup below
// instead, deliberately not this: reusing h-full/w-full there would
// resolve against #header-avatar's own box, which is right in principle,
// but the two need different fallback icon sizes and the img needs the
// header's ring — easier to keep them as two small templates than one
// parameterized over both.
function avatarMarkup(url, iconSizeClass) {
  if (url) {
    return `<img src="${url}" alt="" class="h-full w-full rounded-full object-cover" />`;
  }
  return `<span class="flex h-full w-full items-center justify-center rounded-full bg-(--color-surface-2) text-(--color-text-faint)">
    <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" class="${iconSizeClass}" aria-hidden="true"><circle cx="12" cy="8" r="4"/><path d="M4 20c0-4 4-6 8-6s8 2 8 6"/></svg>
  </span>`;
}

// Mirrors index.html's #header-avatar markup exactly (ring included) so a
// live update after upload looks identical to what a fresh page load would
// render — see that template's comment on why #header-avatar itself (not
// this markup) is what's sized to 28px.
function headerAvatarMarkup(url) {
  if (url) {
    return `<img src="${url}" alt="" class="h-full w-full rounded-full object-cover ring-1 ring-(--color-border)" />`;
  }
  return `<span class="flex h-full w-full items-center justify-center rounded-full bg-(--color-surface-2) text-(--color-text-faint)">
    <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" class="h-4 w-4" aria-hidden="true"><circle cx="12" cy="8" r="4"/><path d="M4 20c0-4 4-6 8-6s8 2 8 6"/></svg>
  </span>`;
}

function updateHeaderAvatar(url) {
  const header = document.querySelector("#header-avatar");
  if (header) header.innerHTML = headerAvatarMarkup(url);
}

function field(label, value) {
  const row = document.createElement("div");
  row.className = "flex items-center justify-between gap-4 border-b border-(--color-border)/70 py-3 text-sm last:border-b-0";
  const l = document.createElement("span");
  l.className = "text-(--color-text-faint)";
  l.textContent = label;
  const v = document.createElement("span");
  v.className = "truncate font-medium text-(--color-text)";
  v.textContent = value;
  row.append(l, v);
  return row;
}

function passkeySkeleton() {
  const row = document.createElement("div");
  row.className = "flex items-center gap-3 rounded-lg border border-(--color-border) p-3";
  row.innerHTML = `<div class="skeleton animate-shimmer h-8 w-8 shrink-0 rounded-full"></div><div class="skeleton animate-shimmer h-4 w-32 rounded"></div>`;
  return row;
}

function passkeyRow(cred, onRemoved) {
  const row = document.createElement("div");
  row.className =
    "flex items-center justify-between gap-3 rounded-lg border border-(--color-border) p-3 transition-colors hover:border-(--color-border-strong)";

  const left = document.createElement("div");
  left.className = "flex min-w-0 items-center gap-3";
  left.innerHTML = `<span class="flex h-8 w-8 shrink-0 items-center justify-center rounded-full bg-(--color-surface-2) text-(--color-text-faint)">${icon("key", "h-4 w-4")}</span>`;
  const label = document.createElement("span");
  label.className = "truncate text-sm text-(--color-text)";
  label.textContent = cred.nickname || cred.id;
  left.appendChild(label);

  const removeBtn = document.createElement("button");
  removeBtn.type = "button";
  const idleLabel = `${t("profile_remove_button", "Remove")} ${cred.nickname || cred.id}`;
  const idleClass =
    "flex h-11 w-11 shrink-0 items-center justify-center rounded-lg text-(--color-text-faint) transition-colors hover:bg-(--color-danger-bg) hover:text-(--color-danger-icon) focus-visible:outline-none disabled:cursor-not-allowed disabled:opacity-50";
  const armedClass =
    "flex h-11 w-11 shrink-0 items-center justify-center rounded-lg bg-(--color-danger-bg) text-(--color-danger-icon) transition-colors focus-visible:outline-none disabled:cursor-not-allowed disabled:opacity-50";
  removeBtn.setAttribute("aria-label", idleLabel);
  removeBtn.className = idleClass;
  removeBtn.innerHTML = icon("trash", "h-4 w-4");

  // Deleting a passkey is destructive and irreversible (re-adding means
  // going through WebAuthn registration again from scratch), so it needs a
  // confirmation step — but a native confirm() would be a jarring, blocking
  // browser dialog out of step with every other interaction in this
  // dashboard (see the toast/accordion patterns elsewhere on this screen),
  // and there's no modal component in this codebase to reuse instead. A
  // click-to-arm/click-again-to-confirm toggle on the same button reuses
  // the existing icon-button, needs no new component, and auto-disarms
  // (timeout or focus loss) so it can't be confirmed by an unrelated later
  // click.
  let armed = false;
  let armTimer = null;

  function disarm() {
    armed = false;
    clearTimeout(armTimer);
    removeBtn.className = idleClass;
    removeBtn.setAttribute("aria-label", idleLabel);
    removeBtn.innerHTML = icon("trash", "h-4 w-4");
  }

  removeBtn.addEventListener("click", async () => {
    if (!armed) {
      armed = true;
      removeBtn.className = armedClass;
      removeBtn.setAttribute("aria-label", `${t("profile_remove_confirm_label", "Click again to confirm removing")} ${cred.nickname || cred.id}`);
      removeBtn.innerHTML = icon("check", "h-4 w-4");
      armTimer = setTimeout(disarm, 3000);
      return;
    }

    clearTimeout(armTimer);
    removeBtn.disabled = true;
    try {
      await webauthn.deleteCredential(cred.id);
      showToast(t("profile_remove_button", "Remove") + " — " + (cred.nickname || cred.id), "success");
      onRemoved();
    } catch (err) {
      showToast(`${t("request_failed", "Request failed:")} ${err}`, "error");
      removeBtn.disabled = false;
      disarm();
    }
  });
  removeBtn.addEventListener("focusout", () => {
    if (armed) disarm();
  });

  row.append(left, removeBtn);
  return row;
}

async function renderCredentials(listEl) {
  listEl.innerHTML = "";
  listEl.appendChild(passkeySkeleton());
  listEl.appendChild(passkeySkeleton());
  try {
    const creds = await webauthn.listCredentials();
    listEl.innerHTML = "";
    if (!creds || creds.length === 0) {
      // Clears login.js's remembered "passkey" method if this account just
      // lost its last one (e.g. removed below) — a stale "passkey" memory
      // would otherwise collapse this account's next login behind a button
      // that can't work. Doesn't touch a remembered "password" method,
      // which stays accurate either way.
      webauthn.forgetPasskeyLogin();
      listEl.innerHTML = `<p class="rounded-lg border border-dashed border-(--color-border) px-4 py-6 text-center text-sm text-(--color-text-faint)">${t("profile_no_passkeys", "No passkeys registered yet.")}</p>`;
      return;
    }
    for (const c of creds) {
      listEl.appendChild(passkeyRow(c, () => renderCredentials(listEl)));
    }
  } catch (err) {
    listEl.innerHTML = `<p class="text-sm text-(--color-danger-text)"></p>`;
    listEl.querySelector("p").textContent = `${t("failed_to_load", "Failed to load:")} ${err}`;
  }
}

function mountPasskeys(container) {
  const section = container.querySelector("#passkeys-section");
  section.classList.remove("hidden");

  const listEl = container.querySelector("#passkeys-list");
  const addBtn = container.querySelector("#add-passkey");
  const cancelBtn = container.querySelector("#passkey-cancel");
  const form = container.querySelector("#add-passkey-form");
  const nicknameInput = container.querySelector("#passkey-nickname");
  const confirmBtn = container.querySelector("#passkey-confirm");

  function openForm() {
    form.classList.remove("grid-rows-[0fr]");
    form.classList.add("grid-rows-[1fr]");
    nicknameInput.focus();
  }
  function closeForm() {
    form.classList.add("grid-rows-[0fr]");
    form.classList.remove("grid-rows-[1fr]");
    nicknameInput.value = "";
  }

  addBtn.addEventListener("click", openForm);
  cancelBtn.addEventListener("click", closeForm);

  confirmBtn.addEventListener("click", async () => {
    confirmBtn.disabled = true;
    const original = confirmBtn.textContent;
    confirmBtn.textContent = t("sending_ellipsis", "Sending…");
    try {
      await webauthn.registerPasskey(nicknameInput.value.trim() || "Passkey");
      closeForm();
      showToast(t("memory_saved", "Saved"), "success");
      renderCredentials(listEl);
    } catch (err) {
      showToast(`${t("request_failed", "Request failed:")} ${err}`, "error");
    } finally {
      confirmBtn.disabled = false;
      confirmBtn.textContent = original;
    }
  });

  renderCredentials(listEl);
}

// Wires the circular drop-zone rendered in mount()'s template: click or
// drag-and-drop a file in, crop it (avatar-crop.js), upload the result to
// internal/webui/avatar.go's handlePostAvatar, then reflect the new avatar
// both in this screen's own preview circle and in the header (see
// updateHeaderAvatar) — no full page reload needed either way.
function mountAvatarUpload(container) {
  const dropzone = container.querySelector("#avatar-dropzone");
  const fileInput = container.querySelector("#avatar-file-input");
  const preview = container.querySelector("#avatar-preview");

  const openPicker = () => fileInput.click();
  dropzone.addEventListener("click", openPicker);
  dropzone.addEventListener("keydown", (e) => {
    if (e.key === "Enter" || e.key === " ") {
      e.preventDefault();
      openPicker();
    }
  });
  dropzone.addEventListener("dragover", (e) => {
    e.preventDefault();
    dropzone.classList.add("border-(--color-accent-emphasis)");
  });
  dropzone.addEventListener("dragleave", () => {
    dropzone.classList.remove("border-(--color-accent-emphasis)");
  });
  dropzone.addEventListener("drop", (e) => {
    e.preventDefault();
    dropzone.classList.remove("border-(--color-accent-emphasis)");
    const file = e.dataTransfer?.files?.[0];
    if (file) handleAvatarFile(file, preview);
  });
  fileInput.addEventListener("change", () => {
    const file = fileInput.files?.[0];
    fileInput.value = ""; // otherwise re-selecting the same file fires no "change"
    if (file) handleAvatarFile(file, preview);
  });
}

async function handleAvatarFile(file, preview) {
  if (!file.type.startsWith("image/")) {
    showToast(t("profile_avatar_invalid_file", "Please choose an image file"), "error");
    return;
  }

  const blob = await cropAvatar(file);
  if (!blob) return; // cancelled, or the file couldn't be decoded as an image

  const previousHTML = preview.innerHTML;
  const objURL = URL.createObjectURL(blob);
  preview.innerHTML = `<img src="${objURL}" alt="" class="h-full w-full rounded-full object-cover" />`;

  try {
    const formData = new FormData();
    formData.append("avatar", blob, "avatar.jpg");
    const res = await fetch("/api/profile/avatar", { method: "POST", body: formData });
    if (!res.ok) throw new Error(String(res.status));
    const data = await res.json();
    user.avatar = data.avatar_url;
    preview.innerHTML = avatarMarkup(data.avatar_url, "h-10 w-10");
    updateHeaderAvatar(data.avatar_url);
  } catch {
    preview.innerHTML = previousHTML;
    showToast(t("profile_avatar_upload_error", "Couldn't upload avatar"), "error");
  } finally {
    URL.revokeObjectURL(objURL);
  }
}

export function mount(container) {
  container.innerHTML = `
    <div class="scrollbar-thin h-full overflow-y-auto">
      <div class="mx-auto max-w-xl px-4 py-6 sm:px-6 sm:py-8">
        <h1 class="mb-6 text-2xl font-semibold tracking-tight text-(--color-text)">${t("nav_profile", "Profile")}</h1>

        <div class="mb-6 flex flex-col items-center gap-3">
          <div id="avatar-dropzone" tabindex="0" role="button"
               aria-label="${t("profile_avatar_upload_label", "Upload avatar")}"
               class="group relative flex h-32 w-32 items-center justify-center overflow-hidden rounded-full border-2 border-dashed border-(--color-border-strong) bg-(--color-surface-2) transition-colors hover:border-(--color-accent-emphasis) focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-(--color-accent-emphasis)">
            <div id="avatar-preview" class="h-full w-full">${avatarMarkup(user.avatar, "h-10 w-10")}</div>
            <div class="pointer-events-none absolute inset-0 flex items-center justify-center bg-black/40 opacity-0 transition-opacity group-hover:opacity-100">
              <span class="text-xs font-medium text-white">${t("profile_avatar_change", "Change")}</span>
            </div>
          </div>
          <input id="avatar-file-input" type="file" accept="image/*" class="hidden" />
          <p class="text-xs text-(--color-text-faint)">${t("profile_avatar_hint", "Click or drop an image")}</p>
        </div>

        <section class="rounded-xl border border-(--color-border) bg-(--color-surface)/30 px-4">
          <div id="profile-fields"></div>
        </section>

        <section id="passkeys-section" class="mt-6 hidden">
          <div class="mb-1 flex items-center justify-between gap-3">
            <h2 class="text-base font-semibold text-(--color-text)">${t("profile_passkeys_title", "Passkeys")}</h2>
            <button id="add-passkey" type="button"
              class="flex items-center gap-1.5 rounded-lg border border-(--color-border-strong) px-3 py-1.5 text-xs font-medium text-(--color-text) transition-colors hover:border-(--color-text-faint) hover:bg-(--color-surface-2)/60 focus-visible:outline-none">
              ${icon("plus", "h-3.5 w-3.5")}${t("profile_add_passkey_button", "Add passkey on this device")}
            </button>
          </div>
          <p class="mb-4 text-sm text-(--color-text-faint)">${t("profile_passkeys_hint", "Sign in without a password using Face ID, Touch ID, or a security key.")}</p>

          <!-- Grid-rows accordion trick: 0fr/1fr row-track transition is the
               one well-established way to animate an inline element's
               height without an explicit pixel value, so it doesn't just
               snap open/closed like the old hidden/flex toggle did. The
               persona doc's "avoid animating height/layout" guidance is
               about gratuitous layout thrash; a small settings form
               expanding in place has no opacity/transform-only equivalent
               that also collapses its space to zero when closed. -->
          <div id="add-passkey-form" class="grid grid-rows-[0fr] transition-[grid-template-rows] duration-200 ease-out">
            <div class="overflow-hidden">
              <div class="mb-4 flex flex-col gap-2 pt-1">
                <input id="passkey-nickname" type="text" placeholder="${t("profile_passkey_nickname_placeholder", "e.g. iPhone")}"
                  class="w-full min-w-0 rounded-lg border border-(--color-border-strong) bg-(--color-bg) px-3 py-2 text-sm placeholder:text-(--color-text-faint) focus:border-(--color-accent-emphasis) focus:outline-none focus-visible:outline-none" />
                <div class="flex flex-wrap gap-2">
                  <button id="passkey-confirm" type="button"
                    class="shrink-0 rounded-lg bg-(--color-accent) px-3.5 py-2 text-sm font-medium text-white transition-colors hover:bg-(--color-accent-hover) focus-visible:outline-none disabled:cursor-not-allowed disabled:opacity-60">
                    ${t("profile_add_passkey_button", "Add passkey on this device")}
                  </button>
                  <button id="passkey-cancel" type="button"
                    class="shrink-0 rounded-lg px-3 py-2 text-sm font-medium text-(--color-text-faint) transition-colors hover:bg-(--color-surface-2) focus-visible:outline-none">
                    ${t("profile_add_passkey_cancel", "Cancel")}
                  </button>
                </div>
              </div>
            </div>
          </div>

          <div id="passkeys-list" class="space-y-2"></div>
        </section>

        <!-- An installed home-screen PWA has no browser reload button and
             can sit on an already-loaded shell document indefinitely (iOS
             tends to resume a suspended WKWebView instance rather than
             re-fetch "/" on every reopen) — this is the user-visible escape
             hatch for "I deployed a new build but my phone still shows the
             old one". location.reload() pairs with the server's own
             Cache-Control: no-store on "/" (see webui.go's handleIndex) so
             the fetch it triggers is always guaranteed fresh, not served
             from the HTTP cache either. -->
        <button id="force-refresh" type="button" class="mt-8 flex w-full items-center justify-center gap-2 rounded-lg border border-(--color-border) px-4 py-2.5 text-sm font-medium text-(--color-text-muted) transition-colors hover:border-(--color-border-strong) hover:bg-(--color-surface-2) focus-visible:outline-none">
          ${icon("refresh-cw", "h-4 w-4")}${t("profile_refresh_button", "Refresh app")}
        </button>

        <form method="POST" action="/logout" class="mt-3">
          <button type="submit" class="flex w-full items-center justify-center gap-2 rounded-lg border border-(--color-border) px-4 py-2.5 text-sm font-medium text-(--color-text-muted) transition-colors hover:border-(--color-danger-border) hover:bg-(--color-danger-bg) hover:text-(--color-danger-text) focus-visible:outline-none">
            ${icon("log-out", "h-4 w-4")}${t("logout_button", "Log out")}
          </button>
        </form>
      </div>
    </div>`;

  mountAvatarUpload(container);

  const fields = container.querySelector("#profile-fields");
  fields.appendChild(field(t("profile_username_label", "Username"), user.username || ""));
  if (user.displayName && user.displayName !== user.username) {
    fields.appendChild(field(t("profile_display_name_label", "Name"), user.displayName));
  }
  if (user.language) fields.appendChild(field(t("profile_language_label", "Language"), user.language));
  if (user.haUserId) fields.appendChild(field(t("profile_ha_user_id_label", "HA user id"), user.haUserId));

  if (window.MIRANDA_WEBAUTHN_ENABLED && webauthn.isSupported()) {
    mountPasskeys(container);
  }

  container.querySelector("#force-refresh").addEventListener("click", () => {
    location.reload();
  });
}

export function unmount() {}
