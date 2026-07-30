package recon

import (
	"context"
	"testing"
	"time"

	"github.com/barakahfund/payments/internal/domain"
	"github.com/barakahfund/payments/internal/money"
	"github.com/barakahfund/payments/internal/payment"
	"github.com/barakahfund/payments/internal/payment/mock"
	"github.com/barakahfund/payments/internal/store"
)

func fixedClock() func() time.Time {
	now := time.Date(2026, 6, 15, 10, 0, 0, 0, time.UTC)
	return func() time.Time { return now }
}

func newEngine(t *testing.T) (*Engine, *mock.Mock, *store.Memory, domain.Account) {
	t.Helper()
	clock := fixedClock()
	gw := mock.New()
	gw.SetClock(clock)
	st := store.NewMemory()
	account := domain.Account{ID: "t1", StripeAccountID: "acct_1", ChargesEnabled: true}
	st.SaveAccount(context.Background(), account)
	return New(gw, st, clock), gw, st, account
}

func window() (time.Time, time.Time) {
	base := time.Date(2026, 6, 15, 10, 0, 0, 0, time.UTC)
	return base.Add(-24 * time.Hour), base.Add(time.Hour)
}

func TestReconcileBackfillsMissingSucceededPayment(t *testing.T) {
	eng, gw, st, account := newEngine(t)
	ctx := context.Background()
	// A confirmed charge exists at Stripe but was never persisted locally.
	pi, _ := gw.CreatePaymentIntent(ctx, "acct_1", payment.CreatePaymentIntentParams{Amount: money.New(3000, "USD"), Confirm: true})

	from, to := window()
	rep, err := eng.Reconcile(ctx, account, from, to)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Backfilled == 0 {
		t.Fatalf("expected backfills, got report %+v", rep)
	}
	got, err := st.GetPayment(ctx, "t1", pi.ID)
	if err != nil {
		t.Fatalf("payment not backfilled: %v", err)
	}
	if got.Status != domain.PaymentSucceeded || got.Source != domain.SourceReconciliation {
		t.Fatalf("backfilled payment = %+v", got)
	}
}

func TestReconcileBackfillsFailedPayment(t *testing.T) {
	eng, gw, st, account := newEngine(t)
	ctx := context.Background()
	pi, _ := gw.CreatePaymentIntent(ctx, "acct_1", payment.CreatePaymentIntentParams{Amount: money.New(800, "USD")})
	gw.Fail("acct_1", pi.ID, "declined") // no balance txn for failures

	from, to := window()
	if _, err := eng.Reconcile(ctx, account, from, to); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetPayment(ctx, "t1", pi.ID)
	if err != nil {
		t.Fatalf("failed payment not backfilled: %v", err)
	}
	if got.Status != domain.PaymentFailed {
		t.Fatalf("status = %s, want failed", got.Status)
	}
}

func TestReconcileIsIdempotent(t *testing.T) {
	eng, gw, _, account := newEngine(t)
	ctx := context.Background()
	gw.CreatePaymentIntent(ctx, "acct_1", payment.CreatePaymentIntentParams{Amount: money.New(1200, "USD"), Confirm: true})

	from, to := window()
	first, _ := eng.Reconcile(ctx, account, from, to)
	second, _ := eng.Reconcile(ctx, account, from, to)
	if first.Backfilled == 0 {
		t.Fatalf("first run should backfill, got %+v", first)
	}
	if second.Backfilled != 0 {
		t.Fatalf("second run should backfill nothing, got %+v", second)
	}
}

func TestReconcileAllIteratesAccounts(t *testing.T) {
	eng, gw, st, _ := newEngine(t)
	ctx := context.Background()
	st.SaveAccount(ctx, domain.Account{ID: "t2", StripeAccountID: "acct_2", ChargesEnabled: true})
	gw.CreatePaymentIntent(ctx, "acct_1", payment.CreatePaymentIntentParams{Amount: money.New(100, "USD"), Confirm: true})
	gw.CreatePaymentIntent(ctx, "acct_2", payment.CreatePaymentIntentParams{Amount: money.New(200, "USD"), Confirm: true})

	from, to := window()
	reports, err := eng.ReconcileAll(ctx, from, to)
	if err != nil {
		t.Fatal(err)
	}
	if len(reports) != 2 {
		t.Fatalf("reports = %d, want 2", len(reports))
	}
}
