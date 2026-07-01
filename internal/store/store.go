// Package store defines the persistence port and an in-memory implementation.
// The real deployment backs this with Cloud SQL (PostgreSQL); the in-memory
// store is used by unit and integration tests. All access is tenant-scoped.
package store

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/barakahfund/payments/internal/domain"
)

// ErrNotFound is returned when a row does not exist.
var ErrNotFound = errors.New("store: not found")

// PaymentFilter narrows a payments query for metrics and reporting.
type PaymentFilter struct {
	TenantID  string
	ProductID string // optional
	From      time.Time
	To        time.Time
}

// Store is the persistence port used by services, webhooks, recon and metrics.
type Store interface {
	// Tenants
	SaveTenant(ctx context.Context, t domain.Tenant) error
	GetTenant(ctx context.Context, id string) (domain.Tenant, error)
	ListTenants(ctx context.Context) ([]domain.Tenant, error)

	// Donors
	SaveDonor(ctx context.Context, d domain.Donor) error
	GetDonor(ctx context.Context, tenantID, donorID string) (domain.Donor, error)
	FindDonorByEmail(ctx context.Context, tenantID, email string) (domain.Donor, error)

	// Payments (upsert keyed by StripePaymentIntentID)
	UpsertPayment(ctx context.Context, p domain.Payment) error
	GetPayment(ctx context.Context, tenantID, paymentIntentID string) (domain.Payment, error)
	ListPayments(ctx context.Context, f PaymentFilter) ([]domain.Payment, error)

	// Subscriptions (upsert keyed by StripeSubscriptionID)
	UpsertSubscription(ctx context.Context, s domain.Subscription) error

	// Ledger (idempotent by StripeBalanceTxnID); returns true if newly inserted
	UpsertLedgerEntry(ctx context.Context, e domain.LedgerEntry) (bool, error)

	// Webhook dedup; returns true the first time an event id is seen
	MarkEventProcessed(ctx context.Context, eventID string) (bool, error)

	// Idempotency for start-payment; returns stored intent id if present
	GetIdempotent(ctx context.Context, key string) (string, bool)
	SaveIdempotent(ctx context.Context, key, paymentIntentID string) error
}

// Memory is a concurrency-safe in-memory Store.
type Memory struct {
	mu          sync.RWMutex
	tenants     map[string]domain.Tenant
	donors      map[string]domain.Donor   // tenantID|donorID
	payments    map[string]domain.Payment // tenantID|piID
	subs        map[string]domain.Subscription
	ledger      map[string]domain.LedgerEntry // balanceTxnID
	events      map[string]bool
	idempotency map[string]string
}

// NewMemory builds an empty in-memory store.
func NewMemory() *Memory {
	return &Memory{
		tenants:     map[string]domain.Tenant{},
		donors:      map[string]domain.Donor{},
		payments:    map[string]domain.Payment{},
		subs:        map[string]domain.Subscription{},
		ledger:      map[string]domain.LedgerEntry{},
		events:      map[string]bool{},
		idempotency: map[string]string{},
	}
}

func key(parts ...string) string {
	s := ""
	for i, p := range parts {
		if i > 0 {
			s += "|"
		}
		s += p
	}
	return s
}

func (m *Memory) SaveTenant(_ context.Context, t domain.Tenant) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.tenants[t.ID] = t
	return nil
}

func (m *Memory) GetTenant(_ context.Context, id string) (domain.Tenant, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	t, ok := m.tenants[id]
	if !ok {
		return domain.Tenant{}, ErrNotFound
	}
	return t, nil
}

func (m *Memory) ListTenants(_ context.Context) ([]domain.Tenant, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]domain.Tenant, 0, len(m.tenants))
	for _, t := range m.tenants {
		out = append(out, t)
	}
	return out, nil
}

func (m *Memory) SaveDonor(_ context.Context, d domain.Donor) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.donors[key(d.TenantID, d.ID)] = d
	return nil
}

func (m *Memory) GetDonor(_ context.Context, tenantID, donorID string) (domain.Donor, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	d, ok := m.donors[key(tenantID, donorID)]
	if !ok {
		return domain.Donor{}, ErrNotFound
	}
	return d, nil
}

func (m *Memory) FindDonorByEmail(_ context.Context, tenantID, email string) (domain.Donor, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, d := range m.donors {
		if d.TenantID == tenantID && d.Email == email {
			return d, nil
		}
	}
	return domain.Donor{}, ErrNotFound
}

func (m *Memory) UpsertPayment(_ context.Context, p domain.Payment) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.payments[key(p.TenantID, p.StripePaymentIntentID)] = p
	return nil
}

func (m *Memory) GetPayment(_ context.Context, tenantID, piID string) (domain.Payment, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	p, ok := m.payments[key(tenantID, piID)]
	if !ok {
		return domain.Payment{}, ErrNotFound
	}
	return p, nil
}

func (m *Memory) ListPayments(_ context.Context, f PaymentFilter) ([]domain.Payment, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []domain.Payment
	for _, p := range m.payments {
		if p.TenantID != f.TenantID {
			continue
		}
		if f.ProductID != "" && p.ProductID != f.ProductID {
			continue
		}
		if !f.From.IsZero() && p.CreatedAt.Before(f.From) {
			continue
		}
		if !f.To.IsZero() && p.CreatedAt.After(f.To) {
			continue
		}
		out = append(out, p)
	}
	return out, nil
}

func (m *Memory) UpsertSubscription(_ context.Context, s domain.Subscription) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.subs[key(s.TenantID, s.StripeSubscriptionID)] = s
	return nil
}

func (m *Memory) UpsertLedgerEntry(_ context.Context, e domain.LedgerEntry) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.ledger[e.StripeBalanceTxnID]; ok {
		return false, nil
	}
	m.ledger[e.StripeBalanceTxnID] = e
	return true, nil
}

func (m *Memory) MarkEventProcessed(_ context.Context, eventID string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.events[eventID] {
		return false, nil
	}
	m.events[eventID] = true
	return true, nil
}

func (m *Memory) GetIdempotent(_ context.Context, k string) (string, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	v, ok := m.idempotency[k]
	return v, ok
}

func (m *Memory) SaveIdempotent(_ context.Context, k, piID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.idempotency[k] = piID
	return nil
}
