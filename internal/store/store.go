// Package store defines the persistence port and an in-memory implementation.
// The real deployment backs this with Cloud SQL (PostgreSQL); the in-memory
// store is used by unit and integration tests. All access is account-scoped.
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
	AccountID string
	ProductID string // optional
	From      time.Time
	To        time.Time
}

// Store is the persistence port used by services, webhooks, recon and metrics.
type Store interface {
	// Accounts
	SaveAccount(ctx context.Context, a domain.Account) error
	GetAccount(ctx context.Context, id string) (domain.Account, error)
	ListAccounts(ctx context.Context) ([]domain.Account, error)

	// Donors
	SaveDonor(ctx context.Context, d domain.Donor) error
	GetDonor(ctx context.Context, accountID, donorID string) (domain.Donor, error)
	FindDonorByEmail(ctx context.Context, accountID, email string) (domain.Donor, error)

	// Payments (upsert keyed by StripePaymentIntentID)
	UpsertPayment(ctx context.Context, p domain.Payment) error
	GetPayment(ctx context.Context, accountID, paymentIntentID string) (domain.Payment, error)
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
	accounts    map[string]domain.Account
	donors      map[string]domain.Donor   // accountID|donorID
	payments    map[string]domain.Payment // accountID|piID
	subs        map[string]domain.Subscription
	ledger      map[string]domain.LedgerEntry // balanceTxnID
	events      map[string]bool
	idempotency map[string]string
}

// NewMemory builds an empty in-memory store.
func NewMemory() *Memory {
	return &Memory{
		accounts:    map[string]domain.Account{},
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

func (m *Memory) SaveAccount(_ context.Context, a domain.Account) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.accounts[a.ID] = a
	return nil
}

func (m *Memory) GetAccount(_ context.Context, id string) (domain.Account, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	a, ok := m.accounts[id]
	if !ok {
		return domain.Account{}, ErrNotFound
	}
	return a, nil
}

func (m *Memory) ListAccounts(_ context.Context) ([]domain.Account, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]domain.Account, 0, len(m.accounts))
	for _, a := range m.accounts {
		out = append(out, a)
	}
	return out, nil
}

func (m *Memory) SaveDonor(_ context.Context, d domain.Donor) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.donors[key(d.AccountID, d.ID)] = d
	return nil
}

func (m *Memory) GetDonor(_ context.Context, accountID, donorID string) (domain.Donor, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	d, ok := m.donors[key(accountID, donorID)]
	if !ok {
		return domain.Donor{}, ErrNotFound
	}
	return d, nil
}

func (m *Memory) FindDonorByEmail(_ context.Context, accountID, email string) (domain.Donor, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, d := range m.donors {
		if d.AccountID == accountID && d.Email == email {
			return d, nil
		}
	}
	return domain.Donor{}, ErrNotFound
}

func (m *Memory) UpsertPayment(_ context.Context, p domain.Payment) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.payments[key(p.AccountID, p.StripePaymentIntentID)] = p
	return nil
}

func (m *Memory) GetPayment(_ context.Context, accountID, piID string) (domain.Payment, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	p, ok := m.payments[key(accountID, piID)]
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
		if p.AccountID != f.AccountID {
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
	m.subs[key(s.AccountID, s.StripeSubscriptionID)] = s
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
