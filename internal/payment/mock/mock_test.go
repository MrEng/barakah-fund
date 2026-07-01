package mock

import (
	"context"
	"testing"
	"time"

	"github.com/barakahfund/payments/internal/domain"
	"github.com/barakahfund/payments/internal/money"
	"github.com/barakahfund/payments/internal/payment"
)

const acct = "acct_test"

func TestCreatePaymentIntentRequiresPositiveAmount(t *testing.T) {
	m := New()
	_, err := m.CreatePaymentIntent(context.Background(), acct, payment.CreatePaymentIntentParams{Amount: money.New(0, "USD")})
	if err == nil {
		t.Fatal("expected error for zero amount")
	}
}

func TestPaymentIntentKeptAndStatusReturned(t *testing.T) {
	m := New()
	ctx := context.Background()
	pi, err := m.CreatePaymentIntent(ctx, acct, payment.CreatePaymentIntentParams{Amount: money.New(1500, "USD")})
	if err != nil {
		t.Fatal(err)
	}
	if pi.Status != domain.PaymentRequested {
		t.Fatalf("status = %s, want requested", pi.Status)
	}
	got, err := m.GetPaymentIntent(ctx, acct, pi.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != pi.ID || got.Status != domain.PaymentRequested {
		t.Fatalf("get returned %+v", got)
	}
}

func TestConfirmSucceedsAndRecordsBalanceTxn(t *testing.T) {
	m := New()
	ctx := context.Background()
	pi, err := m.CreatePaymentIntent(ctx, acct, payment.CreatePaymentIntentParams{Amount: money.New(2000, "USD"), Confirm: true})
	if err != nil {
		t.Fatal(err)
	}
	if pi.Status != domain.PaymentSucceeded {
		t.Fatalf("status = %s, want succeeded", pi.Status)
	}
	txns, _ := m.ListBalanceTransactions(ctx, acct, time.Time{}, time.Now().Add(time.Hour))
	if len(txns) != 1 || txns[0].SourceID != pi.ID {
		t.Fatalf("balance txns = %+v", txns)
	}
}

func TestSetNextOutcomeForcesFailure(t *testing.T) {
	m := New()
	ctx := context.Background()
	m.SetNextOutcome(domain.PaymentFailed, "card_declined")
	pi, _ := m.CreatePaymentIntent(ctx, acct, payment.CreatePaymentIntentParams{Amount: money.New(500, "USD"), Confirm: true})
	if pi.Status != domain.PaymentFailed || pi.FailureReason != "card_declined" {
		t.Fatalf("pi = %+v", pi)
	}
}

func TestCardsLifecycle(t *testing.T) {
	m := New()
	ctx := context.Background()
	cus, _ := m.CreateCustomer(ctx, acct, payment.CreateCustomerParams{Email: "a@b.c"})
	card := m.AttachCard(acct, cus.ID, payment.Card{Brand: "visa", Last4: "4242"})
	cards, _ := m.ListPaymentMethods(ctx, acct, cus.ID)
	if len(cards) != 1 {
		t.Fatalf("want 1 card, got %d", len(cards))
	}
	if err := m.DetachPaymentMethod(ctx, acct, card.ID); err != nil {
		t.Fatal(err)
	}
	cards, _ = m.ListPaymentMethods(ctx, acct, cus.ID)
	if len(cards) != 0 {
		t.Fatalf("want 0 cards after detach, got %d", len(cards))
	}
}

func TestSubscriptionStateMachine(t *testing.T) {
	m := New()
	ctx := context.Background()
	sub, _ := m.CreateSubscription(ctx, acct, payment.CreateSubscriptionParams{CustomerID: "cus_x", PriceID: "price_x"})
	if sub.Status != domain.SubActive {
		t.Fatalf("new sub status = %s", sub.Status)
	}
	paused, _ := m.PauseSubscription(ctx, acct, sub.ID)
	if paused.Status != domain.SubPaused {
		t.Fatalf("paused status = %s", paused.Status)
	}
	resumed, _ := m.ResumeSubscription(ctx, acct, sub.ID)
	if resumed.Status != domain.SubActive {
		t.Fatalf("resumed status = %s", resumed.Status)
	}
	canceled, _ := m.CancelSubscription(ctx, acct, sub.ID, false)
	if canceled.Status != domain.SubCanceled {
		t.Fatalf("canceled status = %s", canceled.Status)
	}
}

func TestSucceedAndFailBuildEvents(t *testing.T) {
	m := New()
	ctx := context.Background()
	pi, _ := m.CreatePaymentIntent(ctx, acct, payment.CreatePaymentIntentParams{Amount: money.New(100, "USD")})
	ev := m.Succeed(acct, pi.ID)
	if ev.Type != payment.EventPaymentSucceeded || ev.PaymentIntent.Status != domain.PaymentSucceeded {
		t.Fatalf("succeed event = %+v", ev)
	}
	pi2, _ := m.CreatePaymentIntent(ctx, acct, payment.CreatePaymentIntentParams{Amount: money.New(100, "USD")})
	ev2 := m.Fail(acct, pi2.ID, "declined")
	if ev2.Type != payment.EventPaymentFailed || ev2.PaymentIntent.Status != domain.PaymentFailed {
		t.Fatalf("fail event = %+v", ev2)
	}
}

func TestListBalanceTransactionsWindow(t *testing.T) {
	m := New()
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	m.SetClock(func() time.Time { return base })
	ctx := context.Background()
	m.CreatePaymentIntent(ctx, acct, payment.CreatePaymentIntentParams{Amount: money.New(100, "USD"), Confirm: true})
	// window before the txn returns nothing
	got, _ := m.ListBalanceTransactions(ctx, acct, base.Add(-2*time.Hour), base.Add(-time.Hour))
	if len(got) != 0 {
		t.Fatalf("expected no txns out of window, got %d", len(got))
	}
	got, _ = m.ListBalanceTransactions(ctx, acct, base.Add(-time.Hour), base.Add(time.Hour))
	if len(got) != 1 {
		t.Fatalf("expected 1 txn in window, got %d", len(got))
	}
}
