package webhook

import (
	"context"
	"testing"

	"github.com/barakahfund/payments/internal/app"
	"github.com/barakahfund/payments/internal/domain"
	"github.com/barakahfund/payments/internal/email"
	"github.com/barakahfund/payments/internal/money"
	"github.com/barakahfund/payments/internal/payment"
	"github.com/barakahfund/payments/internal/payment/mock"
	"github.com/barakahfund/payments/internal/store"
)

type harness struct {
	gw     *mock.Mock
	store  *store.Memory
	mail   *email.Recorder
	svc    *app.Service
	router *Router
	donor  domain.Donor
}

func newHarness(t *testing.T) harness {
	t.Helper()
	ctx := context.Background()
	gw := mock.New()
	st := store.NewMemory()
	rec := &email.Recorder{}
	svc := app.New(gw, st, rec, app.Options{})
	st.SaveTenant(ctx, domain.Tenant{ID: "t1", StripeAccountID: "acct_1", ChargesEnabled: true})
	d, err := svc.EnsureDonor(ctx, "t1", "donor@example.com", "Aisha")
	if err != nil {
		t.Fatal(err)
	}
	return harness{gw: gw, store: st, mail: rec, svc: svc, router: New(st, rec, nil), donor: d}
}

func (h harness) start(t *testing.T) payment.PaymentIntent {
	t.Helper()
	pi, err := h.svc.StartDonation(context.Background(), app.StartDonationInput{
		TenantID: "t1", DonorID: h.donor.ID, ProductID: "prod_1", Amount: money.New(4000, "USD"),
	})
	if err != nil {
		t.Fatal(err)
	}
	return pi
}

func TestWebhookMarksSucceeded(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	pi := h.start(t)
	if err := h.router.Handle(ctx, h.gw.Succeed("acct_1", pi.ID)); err != nil {
		t.Fatal(err)
	}
	got, _ := h.store.GetPayment(ctx, "t1", pi.ID)
	if got.Status != domain.PaymentSucceeded {
		t.Fatalf("status = %s, want succeeded", got.Status)
	}
}

func TestWebhookFailureSendsEmail(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	pi := h.start(t)
	if err := h.router.Handle(ctx, h.gw.Fail("acct_1", pi.ID, "card_declined")); err != nil {
		t.Fatal(err)
	}
	got, _ := h.store.GetPayment(ctx, "t1", pi.ID)
	if got.Status != domain.PaymentFailed || got.FailureReason != "card_declined" {
		t.Fatalf("payment = %+v", got)
	}
	if h.mail.Count("failed") != 1 {
		t.Fatalf("failure emails = %d, want 1", h.mail.Count("failed"))
	}
}

func TestWebhookDedup(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	pi := h.start(t)
	ev := h.gw.Fail("acct_1", pi.ID, "declined")
	_ = h.router.Handle(ctx, ev)
	_ = h.router.Handle(ctx, ev) // duplicate delivery
	if h.mail.Count("failed") != 1 {
		t.Fatalf("failure emails = %d, want 1 (deduped)", h.mail.Count("failed"))
	}
}

func TestForwardsToPerRequestWebhookURL(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	fwd := &ForwardRecorder{}
	router := New(h.store, h.mail, nil, WithForwarder(fwd), WithDefaultWebhookURL("https://default.example/hook"))

	pi, err := h.svc.StartDonation(ctx, app.StartDonationInput{
		TenantID: "t1", DonorID: h.donor.ID, ProductID: "prod_1", Amount: money.New(4000, "USD"),
		WebhookURL: "https://caller.example/notify",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := router.Handle(ctx, h.gw.Succeed("acct_1", pi.ID)); err != nil {
		t.Fatal(err)
	}
	if len(fwd.Calls) != 1 {
		t.Fatalf("forward calls = %d, want 1", len(fwd.Calls))
	}
	c := fwd.Calls[0]
	if c.URL != "https://caller.example/notify" {
		t.Fatalf("forwarded to %s, want caller URL", c.URL)
	}
	if c.Notification.Status != string(domain.PaymentSucceeded) || c.Notification.PaymentIntentID != pi.ID {
		t.Fatalf("notification = %+v", c.Notification)
	}
}

func TestForwardsToDefaultWhenUnspecified(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	fwd := &ForwardRecorder{}
	router := New(h.store, h.mail, nil, WithForwarder(fwd), WithDefaultWebhookURL("https://default.example/hook"))

	pi := h.start(t) // no WebhookURL
	if err := router.Handle(ctx, h.gw.Succeed("acct_1", pi.ID)); err != nil {
		t.Fatal(err)
	}
	if len(fwd.Calls) != 1 || fwd.Calls[0].URL != "https://default.example/hook" {
		t.Fatalf("expected forward to default URL, got %+v", fwd.Calls)
	}
}

func TestNoForwarderIsNoOp(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	pi := h.start(t)
	// Router built with no forwarder (h.router) must not error on terminal events.
	if err := h.router.Handle(ctx, h.gw.Succeed("acct_1", pi.ID)); err != nil {
		t.Fatal(err)
	}
}

func TestWebhookBackfillsUnseenIntent(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	// Intent created directly at the gateway (as if our write was missed).
	pi, _ := h.gw.CreatePaymentIntent(ctx, "acct_1", payment.CreatePaymentIntentParams{
		Amount: money.New(700, "USD"), Metadata: map[string]string{"tenant_id": "t1", "product_id": "prod_9"},
	})
	if _, err := h.store.GetPayment(ctx, "t1", pi.ID); err == nil {
		t.Fatal("payment should not exist yet")
	}
	if err := h.router.Handle(ctx, h.gw.Succeed("acct_1", pi.ID)); err != nil {
		t.Fatal(err)
	}
	got, err := h.store.GetPayment(ctx, "t1", pi.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != domain.PaymentSucceeded || got.Source != domain.SourceWebhook {
		t.Fatalf("backfilled payment = %+v", got)
	}
}
