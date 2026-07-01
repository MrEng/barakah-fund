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
)

// Router applies events to the store and sends failure emails.
type Router struct {
	store    store.Store
	notifier email.Notifier
	now      func() time.Time
}

// New builds a Router.
func New(st store.Store, n email.Notifier, now func() time.Time) *Router {
	if now == nil {
		now = time.Now
	}
	return &Router{store: st, notifier: n, now: now}
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
	switch e.Type {
	case payment.EventPaymentSucceeded:
		_, err := r.applyPaymentStatus(ctx, e, domain.PaymentSucceeded)
		return err
	case payment.EventPaymentFailed:
		p, err := r.applyPaymentStatus(ctx, e, domain.PaymentFailed)
		if err != nil {
			return err
		}
		return r.emailFailure(ctx, p)
	default:
		return nil
	}
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
