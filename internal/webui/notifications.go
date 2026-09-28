package webui

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/archer-developer/miranda/internal/notify"
)

// defaultNotificationLimit caps how many rows GET /api/notifications
// returns by default — the bell's list screen shows recent history, not a
// full unbounded archive; a client can still pass ?limit= for more.
const defaultNotificationLimit = 50

// handleGetNotifications lists the logged-in user's own notifications,
// newest first — see notify.Store.ListForUser. Scoped the same way
// handleGetMemory/handleDialogs already are: currentUser(r), never a
// client-supplied user id.
func (h *Handler) handleGetNotifications(w http.ResponseWriter, r *http.Request) {
	user := currentUser(r)

	limit := defaultNotificationLimit
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}

	items, err := h.notify.ListForUser(r.Context(), user.Username, limit)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, notificationsView{Notifications: items})
}

// handlePostNotificationsRead marks every one of the logged-in user's
// currently-unread notifications as read — called when the bell's list
// screen mounts or reloads (see docs/adr/native-notifications.md: opening/
// re-rendering the list *is* the read fact).
func (h *Handler) handlePostNotificationsRead(w http.ResponseWriter, r *http.Request) {
	user := currentUser(r)
	if err := h.notify.MarkAllRead(r.Context(), user.Username); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleGetUnreadCount reports how many of the logged-in user's
// notifications are still unread — loaded once at app boot to paint the
// bell's dot before the notifications screen has ever been opened.
func (h *Handler) handleGetUnreadCount(w http.ResponseWriter, r *http.Request) {
	user := currentUser(r)
	count, err := h.notify.UnreadCount(r.Context(), user.Username)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, unreadCountView{Count: count})
}

type notificationsView struct {
	Notifications []notify.Notification `json:"notifications"`
}

type unreadCountView struct {
	Count int `json:"count"`
}

// handleGetVAPIDKey returns the raw public VAPID key a browser needs for
// pushManager.subscribe()'s applicationServerKey. Returned as plain text
// (not JSON) — the client converts it straight to a Uint8Array, the same
// base64url buffer conversion webauthn.js already has, so there's no
// object wrapper worth adding.
func (h *Handler) handleGetVAPIDKey(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte(h.notify.VAPIDPublicKey()))
}

// pushSubscribeRequest mirrors the browser's PushSubscription.toJSON()
// shape.
type pushSubscribeRequest struct {
	Endpoint string `json:"endpoint"`
	Keys     struct {
		P256dh string `json:"p256dh"`
		Auth   string `json:"auth"`
	} `json:"keys"`
}

// handlePostPushSubscribe registers (or re-registers) the logged-in user's
// browser Web Push subscription.
func (h *Handler) handlePostPushSubscribe(w http.ResponseWriter, r *http.Request) {
	user := currentUser(r)

	var body pushSubscribeRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	sub := notify.Subscription{
		Endpoint:  body.Endpoint,
		UserID:    user.Username,
		P256dh:    body.Keys.P256dh,
		Auth:      body.Keys.Auth,
		UserAgent: r.UserAgent(),
	}
	if err := h.notify.Subscribe(r.Context(), sub); err != nil {
		http.Error(w, "invalid subscription: "+err.Error(), http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// pushUnsubscribeRequest is DELETE /api/push/subscribe's body — just the
// endpoint to remove, ownership-checked server-side against currentUser(r)
// (see notify.Store.DeleteSubscription), never trusted from the client
// beyond identifying which row.
type pushUnsubscribeRequest struct {
	Endpoint string `json:"endpoint"`
}

// handleDeletePushSubscribe removes the logged-in user's own subscription
// for the given endpoint — see notify.Store.DeleteSubscription's
// ownership-scoped ErrNotFound contract.
func (h *Handler) handleDeletePushSubscribe(w http.ResponseWriter, r *http.Request) {
	user := currentUser(r)

	var body pushUnsubscribeRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	if err := h.notify.Unsubscribe(r.Context(), body.Endpoint, user.Username); err != nil {
		if errors.Is(err, notify.ErrNotFound) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
