// Profile screen: read-only account info (config.yaml-sourced — same
// pattern as everywhere else in this project, hand-edit the file rather
// than an in-app editor) plus passkey management. The passkeys section only
// renders if both the server has WebAuthn enabled and this browser context
// actually supports it (window.PublicKeyCredential — false on any insecure
// origin), so there's never a control that would just fail if clicked.
import { t } from "../i18n.js";
import * as webauthn from "../webauthn.js";

const user = window.MIRANDA_USER || {};

function field(label, value) {
  const row = document.createElement("div");
  row.className = "flex justify-between border-b border-slate-800 py-2 text-sm";
  const l = document.createElement("span");
  l.className = "text-slate-500";
  l.textContent = label;
  const v = document.createElement("span");
  v.className = "text-slate-200";
  v.textContent = value;
  row.append(l, v);
  return row;
}

async function renderCredentials(listEl, statusEl) {
  listEl.textContent = t("loading_ellipsis", "Loading…");
  try {
    const creds = await webauthn.listCredentials();
    listEl.innerHTML = "";
    if (!creds || creds.length === 0) {
      listEl.textContent = t("profile_no_passkeys", "No passkeys registered yet.");
      return;
    }
    for (const c of creds) {
      const row = document.createElement("div");
      row.className = "flex items-center justify-between rounded-md border border-slate-800 p-2 text-sm";

      const label = document.createElement("span");
      label.textContent = c.nickname || c.id;

      const removeBtn = document.createElement("button");
      removeBtn.className = "text-xs text-red-400 hover:text-red-300";
      removeBtn.textContent = t("profile_remove_button", "Remove");
      removeBtn.addEventListener("click", async () => {
        removeBtn.disabled = true;
        try {
          await webauthn.deleteCredential(c.id);
          renderCredentials(listEl, statusEl);
        } catch (err) {
          statusEl.textContent = `${t("request_failed", "Request failed:")} ${err}`;
          removeBtn.disabled = false;
        }
      });

      row.append(label, removeBtn);
      listEl.appendChild(row);
    }
  } catch (err) {
    listEl.textContent = `${t("failed_to_load", "Failed to load:")} ${err}`;
  }
}

function mountPasskeys(container) {
  const section = container.querySelector("#passkeys-section");
  section.classList.remove("hidden");

  const listEl = container.querySelector("#passkeys-list");
  const statusEl = container.querySelector("#passkey-status");
  const addBtn = container.querySelector("#add-passkey");
  const form = container.querySelector("#add-passkey-form");
  const nicknameInput = container.querySelector("#passkey-nickname");

  addBtn.addEventListener("click", () => {
    form.classList.toggle("hidden");
    form.classList.toggle("flex");
    if (!form.classList.contains("hidden")) nicknameInput.focus();
  });

  container.querySelector("#passkey-confirm").addEventListener("click", async () => {
    statusEl.textContent = t("sending_ellipsis", "Sending…");
    try {
      await webauthn.registerPasskey(nicknameInput.value.trim() || "Passkey");
      nicknameInput.value = "";
      form.classList.add("hidden");
      form.classList.remove("flex");
      statusEl.textContent = "";
      renderCredentials(listEl, statusEl);
    } catch (err) {
      statusEl.textContent = `${t("request_failed", "Request failed:")} ${err}`;
    }
  });

  renderCredentials(listEl, statusEl);
}

export function mount(container) {
  container.innerHTML = `
    <div class="mx-auto flex h-full max-w-lg flex-col gap-6 overflow-y-auto p-4 sm:p-6">
      <h1 class="text-lg font-semibold">${t("nav_profile", "Profile")}</h1>
      <div id="profile-fields"></div>

      <div id="passkeys-section" class="hidden">
        <div class="mb-2 flex items-center justify-between">
          <h2 class="text-sm font-medium text-slate-300">${t("profile_passkeys_title", "Passkeys")}</h2>
          <button id="add-passkey" class="rounded-md border border-slate-700 px-3 py-1.5 text-xs text-slate-200 hover:bg-slate-800">
            ${t("profile_add_passkey_button", "Add passkey on this device")}
          </button>
        </div>
        <div id="add-passkey-form" class="mb-2 hidden gap-2">
          <input id="passkey-nickname" type="text" placeholder="${t("profile_passkey_nickname_placeholder", "e.g. iPhone")}"
            class="flex-1 rounded-md border border-slate-700 bg-slate-950 px-2 py-1 text-xs placeholder:text-slate-500 focus:border-slate-500 focus:outline-none" />
          <button id="passkey-confirm" class="rounded-md bg-indigo-500 px-3 py-1 text-xs font-medium text-white hover:bg-indigo-400">
            ${t("profile_add_passkey_button", "Add passkey on this device")}
          </button>
        </div>
        <div id="passkeys-list" class="space-y-2"></div>
        <p id="passkey-status" class="mt-2 text-xs text-slate-500"></p>
      </div>

      <form method="POST" action="/logout">
        <button type="submit" class="w-full rounded-md border border-slate-700 px-3 py-2 text-sm text-slate-300 hover:bg-slate-800">
          ${t("logout_button", "Log out")}
        </button>
      </form>
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
