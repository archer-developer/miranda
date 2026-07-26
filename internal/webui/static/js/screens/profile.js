// Profile screen: read-only account info (config.yaml-sourced — same
// pattern as everywhere else in this project, hand-edit the file rather
// than an in-app editor) plus passkey management. The passkeys section only
// renders if both the server has WebAuthn enabled and this browser context
// actually supports it (window.PublicKeyCredential — false on any insecure
// origin), so there's never a control that would just fail if clicked.
import { t } from "../i18n.js";
import { icon } from "../icons.js";
import { showToast } from "../toast.js";
import * as webauthn from "../webauthn.js";

const user = window.MIRANDA_USER || {};

function field(label, value) {
  const row = document.createElement("div");
  row.className = "flex items-center justify-between gap-4 border-b border-slate-800/70 py-3 text-sm last:border-b-0";
  const l = document.createElement("span");
  l.className = "text-slate-500";
  l.textContent = label;
  const v = document.createElement("span");
  v.className = "truncate font-medium text-slate-200";
  v.textContent = value;
  row.append(l, v);
  return row;
}

function passkeySkeleton() {
  const row = document.createElement("div");
  row.className = "flex items-center gap-3 rounded-lg border border-slate-800 p-3";
  row.innerHTML = `<div class="skeleton animate-shimmer h-8 w-8 shrink-0 rounded-full"></div><div class="skeleton animate-shimmer h-4 w-32 rounded"></div>`;
  return row;
}

function passkeyRow(cred, onRemoved) {
  const row = document.createElement("div");
  row.className = "flex items-center justify-between gap-3 rounded-lg border border-slate-800 p-3 transition-colors hover:border-slate-700";

  const left = document.createElement("div");
  left.className = "flex min-w-0 items-center gap-3";
  left.innerHTML = `<span class="flex h-8 w-8 shrink-0 items-center justify-center rounded-full bg-slate-800 text-slate-400">${icon("key", "h-4 w-4")}</span>`;
  const label = document.createElement("span");
  label.className = "truncate text-sm text-slate-200";
  label.textContent = cred.nickname || cred.id;
  left.appendChild(label);

  const removeBtn = document.createElement("button");
  removeBtn.type = "button";
  removeBtn.setAttribute("aria-label", `${t("profile_remove_button", "Remove")} ${cred.nickname || cred.id}`);
  removeBtn.className =
    "flex h-11 w-11 shrink-0 items-center justify-center rounded-lg text-slate-500 transition-colors hover:bg-red-950/40 hover:text-red-400 focus-visible:outline-none disabled:cursor-not-allowed disabled:opacity-50";
  removeBtn.innerHTML = icon("trash", "h-4 w-4");
  removeBtn.addEventListener("click", async () => {
    removeBtn.disabled = true;
    try {
      await webauthn.deleteCredential(cred.id);
      showToast(t("profile_remove_button", "Remove") + " — " + (cred.nickname || cred.id), "success");
      onRemoved();
    } catch (err) {
      showToast(`${t("request_failed", "Request failed:")} ${err}`, "error");
      removeBtn.disabled = false;
    }
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
      listEl.innerHTML = `<p class="rounded-lg border border-dashed border-slate-800 px-4 py-6 text-center text-sm text-slate-500">${t("profile_no_passkeys", "No passkeys registered yet.")}</p>`;
      return;
    }
    for (const c of creds) {
      listEl.appendChild(passkeyRow(c, () => renderCredentials(listEl)));
    }
  } catch (err) {
    listEl.innerHTML = `<p class="text-sm text-red-400"></p>`;
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

export function mount(container) {
  container.innerHTML = `
    <div class="scrollbar-thin h-full overflow-y-auto">
      <div class="mx-auto max-w-xl px-4 py-6 sm:px-6 sm:py-8">
        <h1 class="mb-6 text-2xl font-semibold tracking-tight text-white">${t("nav_profile", "Profile")}</h1>

        <section class="rounded-xl border border-slate-800 bg-slate-900/30 px-4">
          <div id="profile-fields"></div>
        </section>

        <section id="passkeys-section" class="mt-6 hidden">
          <div class="mb-1 flex items-center justify-between gap-3">
            <h2 class="text-base font-semibold text-slate-100">${t("profile_passkeys_title", "Passkeys")}</h2>
            <button id="add-passkey" type="button"
              class="flex items-center gap-1.5 rounded-lg border border-slate-700 px-3 py-1.5 text-xs font-medium text-slate-200 transition-colors hover:border-slate-600 hover:bg-slate-800/60 focus-visible:outline-none">
              ${icon("plus", "h-3.5 w-3.5")}${t("profile_add_passkey_button", "Add passkey on this device")}
            </button>
          </div>
          <p class="mb-4 text-sm text-slate-500">${t("profile_passkeys_hint", "Sign in without a password using Face ID, Touch ID, or a security key.")}</p>

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
              <div class="mb-4 flex flex-wrap gap-2 pt-1">
                <input id="passkey-nickname" type="text" placeholder="${t("profile_passkey_nickname_placeholder", "e.g. iPhone")}"
                  class="min-w-0 flex-1 rounded-lg border border-slate-700 bg-slate-950 px-3 py-2 text-sm placeholder:text-slate-600 focus:border-indigo-400 focus:outline-none focus-visible:outline-none" />
                <button id="passkey-confirm" type="button"
                  class="shrink-0 rounded-lg bg-indigo-500 px-3.5 py-2 text-sm font-medium text-white transition-colors hover:bg-indigo-400 focus-visible:outline-none disabled:cursor-not-allowed disabled:opacity-60">
                  ${t("profile_add_passkey_button", "Add passkey on this device")}
                </button>
                <button id="passkey-cancel" type="button"
                  class="shrink-0 rounded-lg px-3 py-2 text-sm font-medium text-slate-400 transition-colors hover:bg-slate-800 focus-visible:outline-none">
                  ${t("profile_add_passkey_cancel", "Cancel")}
                </button>
              </div>
            </div>
          </div>

          <div id="passkeys-list" class="space-y-2"></div>
        </section>

        <form method="POST" action="/logout" class="mt-8">
          <button type="submit" class="flex w-full items-center justify-center gap-2 rounded-lg border border-slate-800 px-4 py-2.5 text-sm font-medium text-slate-300 transition-colors hover:border-red-900/60 hover:bg-red-950/30 hover:text-red-300 focus-visible:outline-none">
            ${icon("log-out", "h-4 w-4")}${t("logout_button", "Log out")}
          </button>
        </form>
      </div>
    </div>`;

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
}

export function unmount() {}
