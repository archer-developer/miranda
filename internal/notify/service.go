package notify

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"

	webpush "github.com/SherClockHolmes/webpush-go"
)

// WebPushConfig holds what Service needs to actually send a browser push —
// see config.WebPushConfig for the YAML-facing counterpart this is built
// from. A nil *WebPushConfig on Service (rather than a zero value) is what
// means "push delivery is off, feed-only" — see NewService.
type WebPushConfig struct {
	VAPIDPublicKey  string
	VAPIDPrivateKey string
	Subject         string
	// HTTPClient overrides the client used to actually POST to a push
	// service's endpoint — nil means webpush-go's own default (&http.Client{}).
	// Exists so tests can inject a stub instead of hitting a real push
	// service; production callers should leave this nil.
	HTTPClient webpush.HTTPClient
}

// payload is the JSON body a push message carries — read by sw.js's push
// event handler on the browser side.
type payload struct {
	Title string `json:"title"`
	Body  string `json:"body"`
}

// Service is the one entry point everything else in Miranda calls to
// deliver a notification — persisting it (always) and best-effort pushing
// it to every browser subscription the target user has registered (only
// when webpush is configured). See ../../docs/adr/native-notifications.md.
type Service struct {
	store   *Store
	webpush *WebPushConfig // nil = feed-only, no browser push leg
	logger  *slog.Logger
}

// NewService builds a Service. webpushCfg may be nil — the feed still
// works, notifications just never reach a browser as an OS push.
func NewService(store *Store, webpushCfg *WebPushConfig, logger *slog.Logger) *Service {
	if logger == nil {
		logger = slog.Default()
	}
	return &Service{store: store, webpush: webpushCfg, logger: logger}
}

// Notify persists a notification for userID and best-effort pushes it to
// every browser subscription that user has registered. Returns an error
// only if the persist step itself fails — the feed row is the record of
// truth; a missing or failed push leg (no subscription yet, a dead
// endpoint, a transient network error) never fails the call, the same
// "delivery-leg failure must not fail the whole send" reasoning
// deliverReminder's Telegram leg already uses.
func (s *Service) Notify(ctx context.Context, userID, title, body, source string) (Notification, error) {
	n, err := s.store.CreateNotification(ctx, userID, title, body, source)
	if err != nil {
		return Notification{}, err
	}

	if s.webpush != nil {
		s.pushToSubscriptions(ctx, userID, title, body)
	}

	return n, nil
}

// pushToSubscriptions sends the payload to every subscription userID has
// registered. Best-effort per subscription: one failing device never stops
// delivery to another, and a 404/410 (the push service's own "this
// endpoint is dead, stop sending to it" signal) prunes that row so it's
// never retried again — mirrors the reasoning in deliverReminder's doc
// comment about not retrying a deterministic failure forever.
func (s *Service) pushToSubscriptions(ctx context.Context, userID, title, body string) {
	subs, err := s.store.SubscriptionsForUser(ctx, userID)
	if err != nil {
		s.logger.Warn("notify: list subscriptions for push failed", "user_id", userID, "error", err)
		return
	}
	if len(subs) == 0 {
		return
	}

	message, err := json.Marshal(payload{Title: title, Body: body})
	if err != nil {
		s.logger.Warn("notify: marshal push payload failed", "user_id", userID, "error", err)
		return
	}

	opts := &webpush.Options{
		HTTPClient:      s.webpush.HTTPClient,
		Subscriber:      s.webpush.Subject,
		VAPIDPublicKey:  s.webpush.VAPIDPublicKey,
		VAPIDPrivateKey: s.webpush.VAPIDPrivateKey,
		TTL:             60,
	}

	for _, sub := range subs {
		wsub := &webpush.Subscription{
			Endpoint: sub.Endpoint,
			Keys:     webpush.Keys{Auth: sub.Auth, P256dh: sub.P256dh},
		}
		resp, err := webpush.SendNotificationWithContext(ctx, message, wsub, opts)
		if err != nil {
			s.logger.Warn("notify: push send failed", "user_id", userID, "endpoint", sub.Endpoint, "error", err)
			continue
		}
		_ = resp.Body.Close()
		if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone {
			if err := s.store.deleteSubscriptionByEndpoint(ctx, sub.Endpoint); err != nil {
				s.logger.Warn("notify: prune dead subscription failed", "endpoint", sub.Endpoint, "error", err)
			}
			continue
		}
		if resp.StatusCode >= 300 {
			s.logger.Warn("notify: push send rejected", "user_id", userID, "endpoint", sub.Endpoint, "status", resp.StatusCode)
		}
	}
}

// ListForUser, UnreadCount, MarkAllRead delegate straight to the store —
// Service exists as the one seam callers go through (rather than handing
// out *Store directly) so a future concern (e.g. rate-limiting reads)
// has one place to land without touching every caller.

func (s *Service) ListForUser(ctx context.Context, userID string, limit int) ([]Notification, error) {
	return s.store.ListForUser(ctx, userID, limit)
}

func (s *Service) UnreadCount(ctx context.Context, userID string) (int, error) {
	return s.store.UnreadCount(ctx, userID)
}

func (s *Service) MarkAllRead(ctx context.Context, userID string) error {
	return s.store.MarkAllRead(ctx, userID)
}

// Subscribe registers (or re-registers, on endpoint conflict) a browser's
// Web Push subscription for userID.
func (s *Service) Subscribe(ctx context.Context, sub Subscription) error {
	if sub.Endpoint == "" || sub.P256dh == "" || sub.Auth == "" {
		return fmt.Errorf("notify: subscription missing endpoint/keys")
	}
	return s.store.UpsertSubscription(ctx, sub)
}

// Unsubscribe removes userID's subscription for endpoint. See
// Store.DeleteSubscription for the ErrNotFound contract.
func (s *Service) Unsubscribe(ctx context.Context, endpoint, userID string) error {
	return s.store.DeleteSubscription(ctx, endpoint, userID)
}

// Close closes the underlying store's database connection — cmd/miranda
// defers this alongside its other SQLite-backed stores.
func (s *Service) Close() error {
	return s.store.Close()
}

// WebPushEnabled reports whether the browser-push delivery leg is
// configured — internal/webui uses this to decide whether to mount the
// /api/push/* routes at all.
func (s *Service) WebPushEnabled() bool {
	return s.webpush != nil
}

// VAPIDPublicKey returns the public key browsers need for
// pushManager.subscribe()'s applicationServerKey — empty when webpush isn't
// configured (callers must check WebPushEnabled first).
func (s *Service) VAPIDPublicKey() string {
	if s.webpush == nil {
		return ""
	}
	return s.webpush.VAPIDPublicKey
}
