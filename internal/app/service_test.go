package app

import (
	"context"
	"testing"
	"time"

	"github.com/barakahfund/payments/internal/domain"
	"github.com/barakahfund/payments/internal/email"
	"github.com/barakahfund/payments/internal/money"
	"github.com/barakahfund/payments/internal/payment"
	"github.com/barakahfund/payments/internal/payment/mock"
	"github.com/barakahfund/payments/internal/store"
)

type fixture struct {
	svc    *Service
	gw     *mock.Mock
	store  *store.Memory
	mail   *email.Recorder
	tenant domain.Tenant
	donor  domain.Donor
}

func setup(t *testing.T) fixture {
	t.Helper()
	ctx := context.Background()
	gw := mock.New()
	st := store.NewMemory()
	rec := &email.Recorder{}
	svc := New(gw, st, rec, Options{ApplicationFeeBps: 200}) // 2% fee

	tenant := domain.Tenant{ID: "t1", Name: "Barakah Water", StripeAccountID: "acct_1", ChargesEnabled: true}
	if err := st.SaveTenant(ctx, tenant); err != nil {
		t.Fatal(err)
	}
	donor, err := svc.EnsureDonor(ctx, tenant.ID, "donor@example.com", "Aisha")
	if err != nil {
		t.Fatal(err)
	}
	return fixture{svc: svc, gw: gw, store: st, mail: rec, tenant: tenant, donor: donor}
}

func TestEnsureDonorIsIdempotent(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	again, err := f.svc.EnsureDonor(ctx, f.tenant.ID, "donor@example.com", "Aisha")
	if err != nil {
		t.Fatal(err)
	}
	if again.ID != f.donor.ID {
		t.Fatalf("donor id changed: %s vs %s", again.ID, f.donor.ID)
	}
}

func TestStartDonationPersistsRequested(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	pi, err := f.svc.StartDonation(ctx, StartDonationInput{
		TenantID: f.tenant.ID, DonorID: f.donor.ID, ProductID: "prod_water",
		Amount: money.New(5000, "USD"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if pi.Status != domain.PaymentRequested || pi.ClientSecret == "" {
		t.Fatalf("pi = %+v", pi)
	}
	stored, err := f.store.GetPayment(ctx, f.tenant.ID, pi.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != domain.PaymentRequested || stored.Source != domain.SourceAPI {
		t.Fatalf("stored = %+v", stored)
	}
	if stored.ApplicationFee.Amount != 100 { // 2% of 5000
		t.Fatalf("fee = %d, want 100", stored.ApplicationFee.Amount)
	}
}

func TestStartDonationWithSavedCardConfirms(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	card := f.gw.AttachCard(f.tenant.StripeAccountID, f.donor.StripeCustomerID, payment.Card{Brand: "visa", Last4: "4242"})
	pi, err := f.svc.StartDonation(ctx, StartDonationInput{
		TenantID: f.tenant.ID, DonorID: f.donor.ID, ProductID: "prod_water",
		Amount: money.New(2500, "USD"), PaymentMethodID: card.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if pi.Status != domain.PaymentSucceeded {
		t.Fatalf("status = %s, want succeeded", pi.Status)
	}
}

func TestStartDonationIdempotencyKey(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	in := StartDonationInput{
		TenantID: f.tenant.ID, DonorID: f.donor.ID, ProductID: "prod_water",
		Amount: money.New(1000, "USD"), IdempotencyKey: "key-123",
	}
	first, _ := f.svc.StartDonation(ctx, in)
	second, _ := f.svc.StartDonation(ctx, in)
	if first.ID != second.ID {
		t.Fatalf("idempotency broken: %s vs %s", first.ID, second.ID)
	}
}

func TestStartDonationRejectsBadAmount(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	_, err := f.svc.StartDonation(ctx, StartDonationInput{
		TenantID: f.tenant.ID, DonorID: f.donor.ID, Amount: money.New(0, "USD"),
	})
	if err == nil {
		t.Fatal("expected error for zero amount")
	}
}

func TestChargesDisabledTenantRejected(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	f.store.SaveTenant(ctx, domain.Tenant{ID: "t2", StripeAccountID: "acct_2", ChargesEnabled: false})
	_, err := f.svc.StartDonation(ctx, StartDonationInput{TenantID: "t2", DonorID: "x", Amount: money.New(100, "USD")})
	if err == nil {
		t.Fatal("expected not-chargeable error")
	}
}

func TestAddListRemoveCard(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	si, err := f.svc.AddCard(ctx, f.tenant.ID, f.donor.ID)
	if err != nil || si.ClientSecret == "" {
		t.Fatalf("add card setup intent = %+v err %v", si, err)
	}
	card := f.gw.AttachCard(f.tenant.StripeAccountID, f.donor.StripeCustomerID, payment.Card{Brand: "visa", Last4: "1111"})
	cards, _ := f.svc.ListCards(ctx, f.tenant.ID, f.donor.ID)
	if len(cards) != 1 {
		t.Fatalf("cards = %d, want 1", len(cards))
	}
	if err := f.svc.RemoveCard(ctx, f.tenant.ID, card.ID); err != nil {
		t.Fatal(err)
	}
	cards, _ = f.svc.ListCards(ctx, f.tenant.ID, f.donor.ID)
	if len(cards) != 0 {
		t.Fatalf("cards after remove = %d, want 0", len(cards))
	}
}

func TestRecurringDonationLifecycle(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	card := f.gw.AttachCard(f.tenant.StripeAccountID, f.donor.StripeCustomerID, payment.Card{Brand: "visa", Last4: "4242"})
	sub, err := f.svc.CreateRecurringDonation(ctx, RecurringInput{
		TenantID: f.tenant.ID, DonorID: f.donor.ID, ProductName: "Monthly Water", ProductID: "prod_water",
		Amount: money.New(1500, "USD"), PaymentMethodID: card.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if sub.Status != domain.SubActive {
		t.Fatalf("sub status = %s", sub.Status)
	}
	suspended, _ := f.svc.SuspendSubscription(ctx, f.tenant.ID, sub.ID)
	if suspended.Status != domain.SubPaused {
		t.Fatalf("suspended = %s", suspended.Status)
	}
	resumed, _ := f.svc.ResumeSubscription(ctx, f.tenant.ID, sub.ID)
	if resumed.Status != domain.SubActive {
		t.Fatalf("resumed = %s", resumed.Status)
	}
	canceled, _ := f.svc.CancelSubscription(ctx, f.tenant.ID, sub.ID, false)
	if canceled.Status != domain.SubCanceled {
		t.Fatalf("canceled = %s", canceled.Status)
	}
}

func TestCreateDonationLinkSingleAndRecurring(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	single, err := f.svc.CreateDonationLink(ctx, LinkInput{
		TenantID: f.tenant.ID, ProductName: "Zakat", ProductID: "prod_zakat",
		Amount: money.New(1000, "USD"), DonorID: f.donor.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if single.Mode != "payment" || single.URL == "" || !single.Active {
		t.Fatalf("single link = %+v", single)
	}
	// attribution metadata and donor prefill propagate to the gateway call
	lp := f.gw.LastLinkParams()
	if lp.Metadata["tenant_id"] != f.tenant.ID || lp.Metadata["product_id"] != "prod_zakat" {
		t.Fatalf("link metadata = %+v", lp.Metadata)
	}
	if lp.PrefilledEmail != f.donor.Email || lp.ClientReferenceID != f.donor.ID {
		t.Fatalf("donor prefill not propagated: %+v", lp)
	}
	recur, err := f.svc.CreateDonationLink(ctx, LinkInput{TenantID: f.tenant.ID, ProductName: "Sadaqah", Amount: money.New(2000, "USD"), Recurring: true})
	if err != nil {
		t.Fatal(err)
	}
	if recur.Mode != "subscription" {
		t.Fatalf("recurring link mode = %s", recur.Mode)
	}
}

func TestServiceUsesInjectedClock(t *testing.T) {
	ctx := context.Background()
	gw := mock.New()
	st := store.NewMemory()
	fixed := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	svc := New(gw, st, &email.Recorder{}, Options{Now: func() time.Time { return fixed }})
	st.SaveTenant(ctx, domain.Tenant{ID: "t1", StripeAccountID: "acct_1", ChargesEnabled: true})
	d, _ := svc.EnsureDonor(ctx, "t1", "x@y.z", "X")
	pi, _ := svc.StartDonation(ctx, StartDonationInput{TenantID: "t1", DonorID: d.ID, Amount: money.New(100, "USD")})
	stored, _ := st.GetPayment(ctx, "t1", pi.ID)
	if !stored.CreatedAt.Equal(fixed) {
		t.Fatalf("created at = %v, want %v", stored.CreatedAt, fixed)
	}
}
