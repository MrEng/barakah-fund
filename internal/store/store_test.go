package store

import (
	"context"
	"testing"
	"time"

	"github.com/barakahfund/payments/internal/domain"
	"github.com/barakahfund/payments/internal/money"
)

func TestPaymentUpsertAndGet(t *testing.T) {
	ctx := context.Background()
	m := NewMemory()
	p := domain.Payment{TenantID: "t1", StripePaymentIntentID: "pi_1", Amount: money.New(100, "USD"), Status: domain.PaymentRequested}
	if err := m.UpsertPayment(ctx, p); err != nil {
		t.Fatal(err)
	}
	got, err := m.GetPayment(ctx, "t1", "pi_1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != domain.PaymentRequested {
		t.Fatalf("status = %s", got.Status)
	}
	// upsert overwrites
	p.Status = domain.PaymentSucceeded
	m.UpsertPayment(ctx, p)
	got, _ = m.GetPayment(ctx, "t1", "pi_1")
	if got.Status != domain.PaymentSucceeded {
		t.Fatalf("status after upsert = %s", got.Status)
	}
}

func TestLedgerIdempotent(t *testing.T) {
	ctx := context.Background()
	m := NewMemory()
	e := domain.LedgerEntry{TenantID: "t1", StripeBalanceTxnID: "txn_1", Amount: money.New(100, "USD")}
	ins, _ := m.UpsertLedgerEntry(ctx, e)
	if !ins {
		t.Fatal("first insert should be new")
	}
	ins, _ = m.UpsertLedgerEntry(ctx, e)
	if ins {
		t.Fatal("second insert should not be new")
	}
}

func TestMarkEventProcessedDedup(t *testing.T) {
	ctx := context.Background()
	m := NewMemory()
	first, _ := m.MarkEventProcessed(ctx, "evt_1")
	if !first {
		t.Fatal("first should be true")
	}
	again, _ := m.MarkEventProcessed(ctx, "evt_1")
	if again {
		t.Fatal("second should be false")
	}
}

func TestListPaymentsFilter(t *testing.T) {
	ctx := context.Background()
	m := NewMemory()
	base := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	m.UpsertPayment(ctx, domain.Payment{TenantID: "t1", StripePaymentIntentID: "pi_a", ProductID: "prod_1", Amount: money.New(100, "USD"), CreatedAt: base})
	m.UpsertPayment(ctx, domain.Payment{TenantID: "t1", StripePaymentIntentID: "pi_b", ProductID: "prod_2", Amount: money.New(200, "USD"), CreatedAt: base.Add(48 * time.Hour)})
	m.UpsertPayment(ctx, domain.Payment{TenantID: "t2", StripePaymentIntentID: "pi_c", ProductID: "prod_1", Amount: money.New(300, "USD"), CreatedAt: base})

	all, _ := m.ListPayments(ctx, PaymentFilter{TenantID: "t1"})
	if len(all) != 2 {
		t.Fatalf("t1 payments = %d, want 2", len(all))
	}
	byProduct, _ := m.ListPayments(ctx, PaymentFilter{TenantID: "t1", ProductID: "prod_1"})
	if len(byProduct) != 1 {
		t.Fatalf("t1 prod_1 = %d, want 1", len(byProduct))
	}
	windowed, _ := m.ListPayments(ctx, PaymentFilter{TenantID: "t1", From: base.Add(-time.Hour), To: base.Add(time.Hour)})
	if len(windowed) != 1 {
		t.Fatalf("windowed = %d, want 1", len(windowed))
	}
}

func TestFindDonorByEmail(t *testing.T) {
	ctx := context.Background()
	m := NewMemory()
	m.SaveDonor(ctx, domain.Donor{ID: "cus_1", TenantID: "t1", Email: "d@e.f"})
	d, err := m.FindDonorByEmail(ctx, "t1", "d@e.f")
	if err != nil || d.ID != "cus_1" {
		t.Fatalf("find donor = %+v err %v", d, err)
	}
	if _, err := m.FindDonorByEmail(ctx, "t1", "missing@e.f"); err == nil {
		t.Fatal("expected not found")
	}
}
