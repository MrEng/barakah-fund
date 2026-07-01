// Package mock is a stateful, in-memory implementation of payment.Gateway.
// It keeps payments and returns their status the way Stripe would, enabling
// integration tests to run with no network. It also exposes a small control
// surface (Succeed/Fail, SetNextOutcome, injectable clock) used only by tests.
package mock

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"sync"
	"time"

	"github.com/barakahfund/payments/internal/domain"
	"github.com/barakahfund/payments/internal/money"
	"github.com/barakahfund/payments/internal/payment"
)

// account holds all objects for one connected account.
type account struct {
	customers   map[string]payment.Customer
	cards       map[string][]payment.Card // customerID -> cards
	intents     map[string]*payment.PaymentIntent
	subs        map[string]*payment.Subscription
	products    map[string]payment.Product
	prices      map[string]payment.Price
	links       map[string]payment.PaymentLink
	balanceTxns []payment.BalanceTxn
}

func newAccount() *account {
	return &account{
		customers: map[string]payment.Customer{},
		cards:     map[string][]payment.Card{},
		intents:   map[string]*payment.PaymentIntent{},
		subs:      map[string]*payment.Subscription{},
		products:  map[string]payment.Product{},
		prices:    map[string]payment.Price{},
		links:     map[string]payment.PaymentLink{},
	}
}

// Mock is the in-memory gateway.
type Mock struct {
	mu       sync.Mutex
	accounts map[string]*account
	now      func() time.Time

	// nextOutcome, when set, forces the result of the next confirmed payment.
	nextOutcome *domain.PaymentStatus
	nextReason  string

	// lastLink records the params of the most recent CreatePaymentLink (tests).
	lastLink payment.CreatePaymentLinkParams
}

// LastLinkParams returns the params of the most recent CreatePaymentLink call.
func (m *Mock) LastLinkParams() payment.CreatePaymentLinkParams {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.lastLink
}

// New builds a Mock with a real clock.
func New() *Mock {
	return &Mock{accounts: map[string]*account{}, now: time.Now}
}

// SetClock injects a deterministic clock (tests).
func (m *Mock) SetClock(now func() time.Time) { m.now = now }

// SetNextOutcome forces the outcome of the next confirmed payment (tests).
func (m *Mock) SetNextOutcome(status domain.PaymentStatus, reason string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.nextOutcome = &status
	m.nextReason = reason
}

func (m *Mock) acct(id string) *account {
	a := m.accounts[id]
	if a == nil {
		a = newAccount()
		m.accounts[id] = a
	}
	return a
}

func genID(prefix string) string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return prefix + "_" + hex.EncodeToString(b)
}

// --- Gateway: customer ---

func (m *Mock) CreateCustomer(_ context.Context, account string, p payment.CreateCustomerParams) (payment.Customer, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c := payment.Customer{ID: genID("cus"), Email: p.Email, Name: p.Name}
	m.acct(account).customers[c.ID] = c
	return c, nil
}

// --- Gateway: cards ---

func (m *Mock) CreateSetupIntent(_ context.Context, account, customerID string) (payment.SetupIntent, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	id := genID("seti")
	return payment.SetupIntent{ID: id, ClientSecret: id + "_secret", Status: "requires_payment_method"}, nil
}

// AttachCard is a test helper simulating a completed SetupIntent.
func (m *Mock) AttachCard(account, customerID string, card payment.Card) payment.Card {
	m.mu.Lock()
	defer m.mu.Unlock()
	if card.ID == "" {
		card.ID = genID("pm")
	}
	a := m.acct(account)
	a.cards[customerID] = append(a.cards[customerID], card)
	return card
}

func (m *Mock) ListPaymentMethods(_ context.Context, account, customerID string) ([]payment.Card, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]payment.Card(nil), m.acct(account).cards[customerID]...), nil
}

func (m *Mock) DetachPaymentMethod(_ context.Context, account, paymentMethodID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	a := m.acct(account)
	for cust, cards := range a.cards {
		for i, c := range cards {
			if c.ID == paymentMethodID {
				a.cards[cust] = append(cards[:i], cards[i+1:]...)
				return nil
			}
		}
	}
	return payment.NewError(payment.CodeNotFound, "payment method not found")
}

// --- Gateway: payments ---

func (m *Mock) CreatePaymentIntent(_ context.Context, account string, p payment.CreatePaymentIntentParams) (payment.PaymentIntent, error) {
	if err := p.Amount.Validate(); err != nil {
		return payment.PaymentIntent{}, payment.NewError(payment.CodeInvalid, err.Error())
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	id := genID("pi")
	pi := &payment.PaymentIntent{
		ID:           id,
		ClientSecret: id + "_secret",
		Status:       domain.PaymentRequested,
		Amount:       p.Amount,
		CustomerID:   p.CustomerID,
		Metadata:     p.Metadata,
		Created:      m.now(),
	}
	a := m.acct(account)
	a.intents[id] = pi
	if p.Confirm {
		m.applyOutcomeLocked(a, pi)
	}
	return *pi, nil
}

// applyOutcomeLocked resolves a confirmed intent, honouring a forced outcome.
func (m *Mock) applyOutcomeLocked(a *account, pi *payment.PaymentIntent) {
	status := domain.PaymentSucceeded
	reason := ""
	if m.nextOutcome != nil {
		status, reason = *m.nextOutcome, m.nextReason
		m.nextOutcome, m.nextReason = nil, ""
	}
	pi.Status = status
	pi.FailureReason = reason
	if status == domain.PaymentSucceeded {
		m.recordChargeLocked(a, pi)
	}
}

func (m *Mock) recordChargeLocked(a *account, pi *payment.PaymentIntent) {
	fee := money.Zero(pi.Amount.Currency)
	a.balanceTxns = append(a.balanceTxns, payment.BalanceTxn{
		ID:       genID("txn"),
		Type:     "charge",
		Amount:   pi.Amount,
		Fee:      fee,
		SourceID: pi.ID,
		Created:  m.now(),
	})
}

func (m *Mock) GetPaymentIntent(_ context.Context, account, id string) (payment.PaymentIntent, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	pi := m.acct(account).intents[id]
	if pi == nil {
		return payment.PaymentIntent{}, payment.NewError(payment.CodeNotFound, "payment intent not found")
	}
	return *pi, nil
}

func (m *Mock) CreateRefund(_ context.Context, account, paymentIntentID string, amount *money.Money) (payment.Refund, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	a := m.acct(account)
	pi := a.intents[paymentIntentID]
	if pi == nil {
		return payment.Refund{}, payment.NewError(payment.CodeNotFound, "payment intent not found")
	}
	amt := pi.Amount
	if amount != nil {
		amt = *amount
	}
	a.balanceTxns = append(a.balanceTxns, payment.BalanceTxn{
		ID: genID("txn"), Type: "refund", Amount: money.New(-amt.Amount, amt.Currency),
		SourceID: paymentIntentID, Created: m.now(),
	})
	return payment.Refund{ID: genID("re"), PaymentIntentID: paymentIntentID, Amount: amt}, nil
}

// --- Gateway: catalog ---

func (m *Mock) CreateProduct(_ context.Context, account, name string, _ map[string]string) (payment.Product, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	pr := payment.Product{ID: genID("prod"), Name: name}
	m.acct(account).products[pr.ID] = pr
	return pr, nil
}

func (m *Mock) CreatePrice(_ context.Context, account string, p payment.CreatePriceParams) (payment.Price, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	pr := payment.Price{ID: genID("price"), Amount: p.Amount, Interval: p.Interval, CustomAmount: p.CustomAmount}
	m.acct(account).prices[pr.ID] = pr
	return pr, nil
}

// --- Gateway: subscriptions ---

func (m *Mock) CreateSubscription(_ context.Context, account string, p payment.CreateSubscriptionParams) (payment.Subscription, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	id := genID("sub")
	s := &payment.Subscription{
		ID: id, Status: domain.SubActive, CustomerID: p.CustomerID, PriceID: p.PriceID,
		CurrentPeriodEnd:          m.now().AddDate(0, 1, 0),
		LatestInvoiceClientSecret: genID("pi") + "_secret",
	}
	m.acct(account).subs[id] = s
	return *s, nil
}

func (m *Mock) setSubStatus(account, id string, status domain.SubscriptionStatus) (payment.Subscription, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.acct(account).subs[id]
	if s == nil {
		return payment.Subscription{}, payment.NewError(payment.CodeNotFound, "subscription not found")
	}
	s.Status = status
	return *s, nil
}

func (m *Mock) CancelSubscription(_ context.Context, account, id string, _ bool) (payment.Subscription, error) {
	return m.setSubStatus(account, id, domain.SubCanceled)
}
func (m *Mock) PauseSubscription(_ context.Context, account, id string) (payment.Subscription, error) {
	return m.setSubStatus(account, id, domain.SubPaused)
}
func (m *Mock) ResumeSubscription(_ context.Context, account, id string) (payment.Subscription, error) {
	return m.setSubStatus(account, id, domain.SubActive)
}

// --- Gateway: payment links ---

func (m *Mock) CreatePaymentLink(_ context.Context, account string, p payment.CreatePaymentLinkParams) (payment.PaymentLink, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	id := genID("plink")
	link := "https://donate.stripe.test/" + id
	if p.PrefilledEmail != "" {
		link += "?prefilled_email=" + p.PrefilledEmail
	}
	l := payment.PaymentLink{ID: id, URL: link, Mode: p.Mode, Active: true}
	m.acct(account).links[id] = l
	m.lastLink = p
	return l, nil
}

// --- Gateway: reconciliation reads ---

func (m *Mock) ListBalanceTransactions(_ context.Context, account string, from, to time.Time) ([]payment.BalanceTxn, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []payment.BalanceTxn
	for _, t := range m.acct(account).balanceTxns {
		if inWindow(t.Created, from, to) {
			out = append(out, t)
		}
	}
	return out, nil
}

func (m *Mock) ListPaymentIntents(_ context.Context, account string, from, to time.Time) ([]payment.PaymentIntent, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []payment.PaymentIntent
	for _, pi := range m.acct(account).intents {
		if inWindow(pi.Created, from, to) {
			out = append(out, *pi)
		}
	}
	return out, nil
}

func inWindow(t, from, to time.Time) bool {
	return !t.Before(from) && !t.After(to)
}

// --- Test-only control helpers: simulate Stripe webhooks ---

// Succeed marks an intent paid and returns the webhook event Stripe would send.
func (m *Mock) Succeed(account, piID string) payment.Event {
	m.mu.Lock()
	defer m.mu.Unlock()
	a := m.acct(account)
	pi := a.intents[piID]
	if pi != nil && pi.Status != domain.PaymentSucceeded {
		pi.Status = domain.PaymentSucceeded
		m.recordChargeLocked(a, pi)
	}
	cp := *pi
	return payment.Event{ID: genID("evt"), Type: payment.EventPaymentSucceeded, Account: account, PaymentIntent: &cp}
}

// Fail marks an intent failed and returns the webhook event.
func (m *Mock) Fail(account, piID, reason string) payment.Event {
	m.mu.Lock()
	defer m.mu.Unlock()
	pi := m.acct(account).intents[piID]
	if pi != nil {
		pi.Status = domain.PaymentFailed
		pi.FailureReason = reason
	}
	cp := *pi
	return payment.Event{ID: genID("evt"), Type: payment.EventPaymentFailed, Account: account, PaymentIntent: &cp}
}

// VerifyWebhookSignature is not used by the mock path (events are built via
// Succeed/Fail), so it rejects to make accidental use obvious in tests.
func (m *Mock) VerifyWebhookSignature(_ []byte, _, _ string) (payment.Event, error) {
	return payment.Event{}, payment.NewError(payment.CodeInvalid, "mock does not verify signatures; use Succeed/Fail")
}

// compile-time check that Mock implements the Gateway port.
var _ payment.Gateway = (*Mock)(nil)
