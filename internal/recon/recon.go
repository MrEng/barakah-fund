// Package recon reconciles the local projection against Stripe. It guarantees
// the DB has a row for every real transaction even if a webhook was missed. It
// is the backstop layer, run on a schedule and via an admin from/to API.
package recon

import (
	"context"
	"time"

	"github.com/barakahfund/payments/internal/domain"
	"github.com/barakahfund/payments/internal/money"
	"github.com/barakahfund/payments/internal/payment"
	"github.com/barakahfund/payments/internal/store"
)

// Report summarises one reconciliation pass.
type Report struct {
	TenantID   string
	Scanned    int
	Backfilled int
	Updated    int
	Flagged    int
}

// Engine reconciles a tenant over a time window.
type Engine struct {
	gw    payment.Gateway
	store store.Store
	now   func() time.Time
}

// New builds an Engine.
func New(gw payment.Gateway, st store.Store, now func() time.Time) *Engine {
	if now == nil {
		now = time.Now
	}
	return &Engine{gw: gw, store: st, now: now}
}

// Reconcile pulls Stripe's authoritative data for [from,to] and converges the
// local projection. Idempotent: safe to run concurrently or repeatedly.
func (e *Engine) Reconcile(ctx context.Context, t domain.Tenant, from, to time.Time) (Report, error) {
	rep := Report{TenantID: t.ID}

	// 1. Balance transactions are Stripe's money truth.
	txns, err := e.gw.ListBalanceTransactions(ctx, t.StripeAccountID, from, to)
	if err != nil {
		return rep, err
	}
	for _, tx := range txns {
		rep.Scanned++
		inserted, err := e.store.UpsertLedgerEntry(ctx, domain.LedgerEntry{
			TenantID: t.ID, StripeBalanceTxnID: tx.ID, Type: tx.Type,
			Amount: tx.Amount, Fee: tx.Fee, CreatedAt: tx.Created,
		})
		if err != nil {
			return rep, err
		}
		if inserted {
			rep.Backfilled++
		}
		if tx.Type == "charge" {
			backfilled, err := e.ensureSucceededPayment(ctx, t.ID, tx)
			if err != nil {
				return rep, err
			}
			if backfilled {
				rep.Backfilled++
			}
		}
	}

	// 2. Failed PaymentIntents never hit balance transactions; catch them too.
	pis, err := e.gw.ListPaymentIntents(ctx, t.StripeAccountID, from, to)
	if err != nil {
		return rep, err
	}
	for _, pi := range pis {
		rep.Scanned++
		if pi.Status != domain.PaymentFailed {
			continue
		}
		existing, err := e.store.GetPayment(ctx, t.ID, pi.ID)
		if err != nil {
			if err := e.backfillPayment(ctx, t.ID, pi.ID, pi.Amount, domain.PaymentFailed, pi.FailureReason, pi.Created); err != nil {
				return rep, err
			}
			rep.Backfilled++
			continue
		}
		if existing.Status != domain.PaymentFailed {
			existing.Status = domain.PaymentFailed
			existing.FailureReason = pi.FailureReason
			existing.UpdatedAt = e.now()
			if err := e.store.UpsertPayment(ctx, existing); err != nil {
				return rep, err
			}
			rep.Updated++
		}
	}
	return rep, nil
}

// ensureSucceededPayment guarantees a succeeded payment row exists for a charge.
// Returns true if a missing row was backfilled.
func (e *Engine) ensureSucceededPayment(ctx context.Context, tenantID string, tx payment.BalanceTxn) (bool, error) {
	p, err := e.store.GetPayment(ctx, tenantID, tx.SourceID)
	if err != nil {
		return true, e.backfillPayment(ctx, tenantID, tx.SourceID, tx.Amount, domain.PaymentSucceeded, "", tx.Created)
	}
	if p.Status != domain.PaymentSucceeded {
		p.Status = domain.PaymentSucceeded
		p.UpdatedAt = e.now()
		return false, e.store.UpsertPayment(ctx, p)
	}
	return false, nil
}

func (e *Engine) backfillPayment(ctx context.Context, tenantID, piID string, amount money.Money, status domain.PaymentStatus, reason string, created time.Time) error {
	return e.store.UpsertPayment(ctx, domain.Payment{
		TenantID: tenantID, StripePaymentIntentID: piID, Amount: amount, Status: status,
		Source: domain.SourceReconciliation, FailureReason: reason,
		CreatedAt: created, UpdatedAt: e.now(),
	})
}

// ReconcileAll fans out over every tenant. In production this is one Pub/Sub
// message per tenant; this in-process helper is used by the scheduler and tests.
func (e *Engine) ReconcileAll(ctx context.Context, from, to time.Time) ([]Report, error) {
	tenants, err := e.store.ListTenants(ctx)
	if err != nil {
		return nil, err
	}
	reports := make([]Report, 0, len(tenants))
	for _, t := range tenants {
		r, err := e.Reconcile(ctx, t, from, to)
		if err != nil {
			return reports, err
		}
		reports = append(reports, r)
	}
	return reports, nil
}
