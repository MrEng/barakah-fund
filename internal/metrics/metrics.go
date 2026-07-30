// Package metrics aggregates the payments projection into per-account and
// per-product business metrics for the dashboard API. It reads the store (kept
// correct by webhooks + reconciliation), never counters, so results stay
// accurate after reconciliation.
package metrics

import (
	"context"

	"github.com/barakahfund/payments/internal/domain"
	"github.com/barakahfund/payments/internal/money"
	"github.com/barakahfund/payments/internal/store"
)

// Summary holds aggregate donation metrics over a set of payments.
type Summary struct {
	Currency     string
	SuccessCount int
	FailureCount int
	PendingCount int
	AttemptCount int
	Requested    money.Money // sum of all attempt amounts
	Captured     money.Money // sum of succeeded amounts
	SuccessRate  float64     // succeeded / (succeeded + failed)
}

// Aggregator computes summaries from the store.
type Aggregator struct {
	store    store.Store
	currency string
}

// New builds an Aggregator. currency is the account's reporting currency.
func New(st store.Store, currency string) *Aggregator {
	return &Aggregator{store: st, currency: currency}
}

// AccountSummary aggregates all of an account's payments in a window.
func (a *Aggregator) AccountSummary(ctx context.Context, f store.PaymentFilter) (Summary, error) {
	payments, err := a.store.ListPayments(ctx, f)
	if err != nil {
		return Summary{}, err
	}
	return Summarize(payments, a.currency), nil
}

// ByProduct groups an account's payments by product id.
func (a *Aggregator) ByProduct(ctx context.Context, f store.PaymentFilter) (map[string]Summary, error) {
	payments, err := a.store.ListPayments(ctx, f)
	if err != nil {
		return nil, err
	}
	groups := map[string][]domain.Payment{}
	for _, p := range payments {
		groups[p.ProductID] = append(groups[p.ProductID], p)
	}
	out := make(map[string]Summary, len(groups))
	for pid, ps := range groups {
		out[pid] = Summarize(ps, a.currency)
	}
	return out, nil
}

// Summarize computes a Summary from payments, counting only the given currency.
func Summarize(payments []domain.Payment, currency string) Summary {
	s := Summary{Currency: currency, Requested: money.Zero(currency), Captured: money.Zero(currency)}
	for _, p := range payments {
		if p.Amount.Currency != currency {
			continue
		}
		s.AttemptCount++
		s.Requested.Amount += p.Amount.Amount
		switch p.Status {
		case domain.PaymentSucceeded:
			s.SuccessCount++
			s.Captured.Amount += p.Amount.Amount
		case domain.PaymentFailed:
			s.FailureCount++
		default:
			s.PendingCount++
		}
	}
	if denom := s.SuccessCount + s.FailureCount; denom > 0 {
		s.SuccessRate = float64(s.SuccessCount) / float64(denom)
	}
	return s
}
