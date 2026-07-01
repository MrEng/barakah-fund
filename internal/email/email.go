// Package email defines the donor-notification port. Success receipts are sent
// by Stripe (enabled per connected account); one-off payment failures have no
// native Stripe email, so the application sends those via this Notifier.
package email

import (
	"context"
	"log/slog"
	"sync"

	"github.com/barakahfund/payments/internal/domain"
)

// Notifier sends donor-facing emails the platform is responsible for.
type Notifier interface {
	// PaymentFailed notifies the donor that a one-off donation failed.
	PaymentFailed(ctx context.Context, to string, p domain.Payment) error
	// PaymentSucceeded is optional; Stripe normally sends the receipt.
	PaymentSucceeded(ctx context.Context, to string, p domain.Payment) error
}

// LogNotifier writes notifications to a structured logger (default prod stub).
type LogNotifier struct{ Logger *slog.Logger }

func (n LogNotifier) PaymentFailed(_ context.Context, to string, p domain.Payment) error {
	n.log("payment_failed", to, p)
	return nil
}
func (n LogNotifier) PaymentSucceeded(_ context.Context, to string, p domain.Payment) error {
	n.log("payment_succeeded", to, p)
	return nil
}
func (n LogNotifier) log(kind, to string, p domain.Payment) {
	l := n.Logger
	if l == nil {
		l = slog.Default()
	}
	l.Info("donor_email", "kind", kind, "to", to, "amount", p.Amount.String(), "pi", p.StripePaymentIntentID)
}

// Sent records a single captured email (test spy).
type Sent struct {
	Kind    string
	To      string
	Payment domain.Payment
}

// Recorder captures notifications for assertions in tests.
type Recorder struct {
	mu   sync.Mutex
	Sent []Sent
}

func (r *Recorder) PaymentFailed(_ context.Context, to string, p domain.Payment) error {
	return r.record("failed", to, p)
}
func (r *Recorder) PaymentSucceeded(_ context.Context, to string, p domain.Payment) error {
	return r.record("succeeded", to, p)
}
func (r *Recorder) record(kind, to string, p domain.Payment) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.Sent = append(r.Sent, Sent{Kind: kind, To: to, Payment: p})
	return nil
}

// Count returns how many emails of a kind were sent.
func (r *Recorder) Count(kind string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, s := range r.Sent {
		if s.Kind == kind {
			n++
		}
	}
	return n
}
