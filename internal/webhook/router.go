// Package webhook applies verified Stripe events to the local projection. It is
// the authoritative, near-real-time source of payment status. Events are
// de-duplicated by id so redelivery is safe.
package webhook

import (
	"context"
	"errors"
	"time"

	"github.com/barakahfund/payments/internal/domain"
	"github.com/barakahfund/payments/internal/email"
	"github.com/barakahfund/payments/internal/payment"
	"github.com/barakahfund/payments/internal/store"
	"github.com/barakahfund/payments/internal/telemetry"
)

// Router applies events to the store, sends failure emails, and forwards a
// notification to the caller's webhook URL (per-request, else the default).
type Router struct {
	store             store.Store
	notifier          email.Notifier
	now               func() time.Time
	forwarder         Forwarder
	defaultWebhookURL string
	metrics           *telemetry.Metrics
}

// Option configures a Router.
type Option func(*Router)

// WithForwarder sets the outbound webhook forwarder.
func WithForwarder(f Forwarder) Option { return func(r *Router) { r.forwarder = f } }

// WithMetrics sets the telemetry recorder.
func WithMetrics(m *telemetry.Metrics) Option { return func(r *Router) { r.metrics = m } }

// WithDefaultWebhookURL sets the fallback URL used when a request did not
// specify its own webhook_url.
func WithDefaultWebhookURL(u string) Option { return func(r *Router) { r.defaultWebhookURL = u } }

// New builds a Router. Forwarding is disabled unless WithForwarder is given.
func New(st store.Store, n email.Notifier, now func() time.Time, opts ...Option) *Router {
	if now == nil {
		now = time.Now
	}
	r := &Router{store: st, notifier: n, now: now}
	for _, o := range opts {
		o(r)
	}
	return r
}

// Handle applies one event. Unknown or duplicate events are no-ops.
func (r *Router) Handle(ctx context.Context, e payment.Event) error {
	first, err := r.store.MarkEventProcessed(ctx, e.ID)
	if err != nil {
		return err
	}
	if !first {
		return nil // already processed
	}
	r.metrics.RecordEvent(ctx, string(e.Type))
	switch e.Type {
	case payment.EventPaymentSucceeded:
		p, err := r.applyPaymentStatus(ctx, e, domain.PaymentSucceeded)
		if err != nil {
			return err
		}
		r.metrics.RecordDonation(ctx, p.TenantID, string(domain.PaymentSucceeded))
		r.metrics.RecordCaptured(ctx, p.TenantID, p.Amount.Currency, p.Amount.Amount)
		return r.forward(ctx, e, p)
	case payment.EventPaymentFailed:
		p, err := r.applyPaymentStatus(ctx, e, domain.PaymentFailed)
		if err != nil {
			return err
		}
		r.metrics.RecordDonation(ctx, p.TenantID, string(domain.PaymentFailed))
		if err := r.emailFailure(ctx, p); err != nil {
			return err
		}
		return r.forward(ctx, e, p)
	default:
		return nil
	}
}

// forward delivers a notification to the caller's webhook URL. The URL is taken
// from the request-supplied webhook_url metadata, falling back to the default.
func (r *Router) forward(ctx context.Context, e payment.Event, p domain.Payment) error {
	if r.forwarder == nil {
		return nil
	}
	url := ""
	if e.PaymentIntent != nil {
		url = e.PaymentIntent.Metadata["webhook_url"]
	}
	if url == "" {
		url = r.defaultWebhookURL
	}
	if url == "" {
		return nil // nothing to forward to
	}
	return r.forwarder.Notify(ctx, url, Notification{
		Event:           string(e.Type),
		PaymentIntentID: p.StripePaymentIntentID,
		TenantID:        p.TenantID,
		Status:          string(p.Status),
		Amount:          p.Amount.Amount,
		Currency:        p.Amount.Currency,
	})
}

func (r *Router) applyPaymentStatus(ctx context.Context, e payment.Event, status domain.PaymentStatus) (domain.Payment, error) {
	pi := e.PaymentIntent
	if pi == nil {
		return domain.Payment{}, errors.New("webhook: missing payment intent")
	}
	tenantID := pi.Metadata["tenant_id"]
	now := r.now()
	p, err := r.store.GetPayment(ctx, tenantID, pi.ID)
	if err != nil {
		// Backfill an unseen intent (webhook arrived before our write, or was missed).
		p = domain.Payment{
			TenantID: tenantID, StripePaymentIntentID: pi.ID, Amount: pi.Amount,
			ProductID: pi.Metadata["product_id"], Source: domain.SourceWebhook, CreatedAt: now,
		}
	}
	p.Status = status
	p.FailureReason = pi.FailureReason
	p.UpdatedAt = now
	if p.Source == "" {
		p.Source = domain.SourceWebhook
	}
	return p, r.store.UpsertPayment(ctx, p)
}

func (r *Router) emailFailure(ctx context.Context, p domain.Payment) error {
	if p.DonorID == "" {
		return nil
	}
	d, err := r.store.GetDonor(ctx, p.TenantID, p.DonorID)
	if err != nil {
		return nil // donor unknown; nothing to email
	}
	return r.notifier.PaymentFailed(ctx, d.Email, p)
}
