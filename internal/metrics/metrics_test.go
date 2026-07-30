package metrics

import (
	"context"
	"testing"

	"github.com/barakahfund/payments/internal/domain"
	"github.com/barakahfund/payments/internal/money"
	"github.com/barakahfund/payments/internal/store"
)

func payments() []domain.Payment {
	return []domain.Payment{
		{ProductID: "p1", Amount: money.New(1000, "USD"), Status: domain.PaymentSucceeded},
		{ProductID: "p1", Amount: money.New(2000, "USD"), Status: domain.PaymentSucceeded},
		{ProductID: "p1", Amount: money.New(500, "USD"), Status: domain.PaymentFailed},
		{ProductID: "p2", Amount: money.New(300, "USD"), Status: domain.PaymentRequested},
		{ProductID: "p2", Amount: money.New(999, "EUR"), Status: domain.PaymentSucceeded}, // other currency ignored
	}
}

func TestSummarize(t *testing.T) {
	s := Summarize(payments(), "USD")
	if s.SuccessCount != 2 {
		t.Errorf("success = %d, want 2", s.SuccessCount)
	}
	if s.FailureCount != 1 {
		t.Errorf("failure = %d, want 1", s.FailureCount)
	}
	if s.PendingCount != 1 {
		t.Errorf("pending = %d, want 1", s.PendingCount)
	}
	if s.Requested.Amount != 3800 { // 1000+2000+500+300
		t.Errorf("requested = %d, want 3800", s.Requested.Amount)
	}
	if s.Captured.Amount != 3000 {
		t.Errorf("captured = %d, want 3000", s.Captured.Amount)
	}
	wantRate := 2.0 / 3.0
	if s.SuccessRate < wantRate-1e-9 || s.SuccessRate > wantRate+1e-9 {
		t.Errorf("success rate = %v, want %v", s.SuccessRate, wantRate)
	}
}

func TestAggregatorByProduct(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	for i, p := range payments() {
		p.AccountID = "t1"
		p.StripePaymentIntentID = "pi_" + string(rune('a'+i))
		st.UpsertPayment(ctx, p)
	}
	agg := New(st, "USD")
	groups, err := agg.ByProduct(ctx, store.PaymentFilter{AccountID: "t1"})
	if err != nil {
		t.Fatal(err)
	}
	if groups["p1"].SuccessCount != 2 {
		t.Errorf("p1 success = %d, want 2", groups["p1"].SuccessCount)
	}
	if groups["p1"].Captured.Amount != 3000 {
		t.Errorf("p1 captured = %d, want 3000", groups["p1"].Captured.Amount)
	}
}
