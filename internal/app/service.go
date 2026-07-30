// Package app holds the use-case services. They orchestrate the gateway port,
// the store, idempotency, money rules and application fees. They never process
// payments themselves — Stripe does. Services are stateless per request.
package app

import (
	"context"
	"fmt"
	"time"

	"github.com/barakahfund/payments/internal/domain"
	"github.com/barakahfund/payments/internal/email"
	"github.com/barakahfund/payments/internal/money"
	"github.com/barakahfund/payments/internal/payment"
	"github.com/barakahfund/payments/internal/store"
)

// Service implements the donation use-cases.
type Service struct {
	gw       payment.Gateway
	store    store.Store
	notifier email.Notifier
	now      func() time.Time
	feeBps   int64 // platform application fee, basis points (100 = 1%)
}

// Options configures a Service.
type Options struct {
	Now               func() time.Time
	ApplicationFeeBps int64
}

// New builds a Service.
func New(gw payment.Gateway, st store.Store, n email.Notifier, opt Options) *Service {
	if opt.Now == nil {
		opt.Now = time.Now
	}
	return &Service{gw: gw, store: st, notifier: n, now: opt.Now, feeBps: opt.ApplicationFeeBps}
}

// account resolves the Stripe connected account for a request. There is no
// account table lookup: the caller passes the Stripe account id directly, and
// it is used as-is for the Stripe-Account header. An empty id means
// platform-direct — charge on the platform account with no header. Stripe
// itself rejects accounts that aren't connected or can't charge, so no local
// chargeable gate is needed.
func (s *Service) account(_ context.Context, accountID string) (domain.Account, error) {
	return domain.Account{ID: accountID, StripeAccountID: accountID, ChargesEnabled: true}, nil
}

func (s *Service) fee(amount money.Money) money.Money {
	return money.New(amount.Amount*s.feeBps/10000, amount.Currency)
}

// attribution builds the Stripe metadata common to charges/subscriptions/links:
// account_id routes webhooks back to the local projection, tenant_id is the
// caller's own identifier carried through Stripe purely so it comes back in
// events and results.
func attribution(accountID, tenantID string) map[string]string {
	meta := map[string]string{"account_id": accountID}
	if tenantID != "" {
		meta["tenant_id"] = tenantID
	}
	return meta
}

// EnsureDonor returns the donor for an email, creating a Stripe customer and a
// donor record on first sight. This is the "client with a Stripe-like id".
func (s *Service) EnsureDonor(ctx context.Context, accountID, emailAddr, name string) (domain.Donor, error) {
	t, err := s.account(ctx, accountID)
	if err != nil {
		return domain.Donor{}, err
	}
	if d, err := s.store.FindDonorByEmail(ctx, accountID, emailAddr); err == nil {
		return d, nil
	}
	cus, err := s.gw.CreateCustomer(ctx, t.StripeAccountID, payment.CreateCustomerParams{Email: emailAddr, Name: name})
	if err != nil {
		return domain.Donor{}, fmt.Errorf("create customer: %w", err)
	}
	d := domain.Donor{ID: cus.ID, AccountID: accountID, Email: emailAddr, Name: name, StripeCustomerID: cus.ID}
	if err := s.store.SaveDonor(ctx, d); err != nil {
		return domain.Donor{}, err
	}
	return d, nil
}

// AddCard starts the add-card flow and returns a SetupIntent secret for the client.
func (s *Service) AddCard(ctx context.Context, accountID, donorID string) (payment.SetupIntent, error) {
	t, err := s.account(ctx, accountID)
	if err != nil {
		return payment.SetupIntent{}, err
	}
	d, err := s.store.GetDonor(ctx, accountID, donorID)
	if err != nil {
		return payment.SetupIntent{}, err
	}
	return s.gw.CreateSetupIntent(ctx, t.StripeAccountID, d.StripeCustomerID)
}

// ListCards returns the donor's saved cards.
func (s *Service) ListCards(ctx context.Context, accountID, donorID string) ([]payment.Card, error) {
	t, err := s.account(ctx, accountID)
	if err != nil {
		return nil, err
	}
	d, err := s.store.GetDonor(ctx, accountID, donorID)
	if err != nil {
		return nil, err
	}
	return s.gw.ListPaymentMethods(ctx, t.StripeAccountID, d.StripeCustomerID)
}

// RemoveCard detaches a saved card.
func (s *Service) RemoveCard(ctx context.Context, accountID, paymentMethodID string) error {
	t, err := s.account(ctx, accountID)
	if err != nil {
		return err
	}
	return s.gw.DetachPaymentMethod(ctx, t.StripeAccountID, paymentMethodID)
}

// StartDonationInput is the request for a one-off custom-amount donation.
type StartDonationInput struct {
	AccountID       string // Stripe connected account the charge runs on
	TenantID        string // optional caller identifier, echoed back via metadata
	DonorID         string
	ProductID       string
	Amount          money.Money
	PaymentMethodID string // set to confirm a saved card server-side
	IdempotencyKey  string
	WebhookURL      string // optional; caller's notification URL (else the default)
}

// StartDonation creates a PaymentIntent and returns the client secret for the
// client to finish. The attempt is persisted as `requested`.
func (s *Service) StartDonation(ctx context.Context, in StartDonationInput) (payment.PaymentIntent, error) {
	if err := in.Amount.Validate(); err != nil {
		return payment.PaymentIntent{}, payment.NewError(payment.CodeInvalid, err.Error())
	}
	t, err := s.account(ctx, in.AccountID)
	if err != nil {
		return payment.PaymentIntent{}, err
	}
	d, err := s.store.GetDonor(ctx, in.AccountID, in.DonorID)
	if err != nil {
		return payment.PaymentIntent{}, err
	}
	if in.IdempotencyKey != "" {
		if piID, ok := s.store.GetIdempotent(ctx, in.IdempotencyKey); ok {
			return s.gw.GetPaymentIntent(ctx, t.StripeAccountID, piID)
		}
	}
	meta := attribution(in.AccountID, in.TenantID)
	meta["product_id"] = in.ProductID
	meta["email"] = d.Email
	if in.WebhookURL != "" {
		meta["webhook_url"] = in.WebhookURL
	}
	pi, err := s.gw.CreatePaymentIntent(ctx, t.StripeAccountID, payment.CreatePaymentIntentParams{
		Amount:          in.Amount,
		CustomerID:      d.StripeCustomerID,
		PaymentMethodID: in.PaymentMethodID,
		ApplicationFee:  s.fee(in.Amount),
		ReceiptEmail:    d.Email,
		Confirm:         in.PaymentMethodID != "",
		Metadata:        meta,
	})
	if err != nil {
		return payment.PaymentIntent{}, fmt.Errorf("create payment intent: %w", err)
	}
	now := s.now()
	localMeta := map[string]string{"product_id": in.ProductID}
	if in.TenantID != "" {
		localMeta["tenant_id"] = in.TenantID
	}
	if err := s.store.UpsertPayment(ctx, domain.Payment{
		AccountID: in.AccountID, DonorID: in.DonorID, ProductID: in.ProductID,
		StripePaymentIntentID: pi.ID, Amount: in.Amount, Status: pi.Status,
		ApplicationFee: s.fee(in.Amount), Source: domain.SourceAPI,
		CreatedAt: now, UpdatedAt: now,
		Metadata: localMeta,
	}); err != nil {
		return payment.PaymentIntent{}, err
	}
	if in.IdempotencyKey != "" {
		_ = s.store.SaveIdempotent(ctx, in.IdempotencyKey, pi.ID)
	}
	return pi, nil
}

// RecurringInput is the request for a monthly custom-amount donation.
type RecurringInput struct {
	AccountID       string // Stripe connected account the subscription runs on
	TenantID        string // optional caller identifier, echoed back via metadata
	DonorID         string
	ProductName     string
	ProductID       string
	Amount          money.Money
	PaymentMethodID string
	WebhookURL      string // optional; caller's notification URL (else the default)
}

// CreateRecurringDonation mints a fixed monthly price at the donor's chosen
// amount (custom_unit_amount is one-time only) and starts a subscription.
func (s *Service) CreateRecurringDonation(ctx context.Context, in RecurringInput) (payment.Subscription, error) {
	if err := in.Amount.Validate(); err != nil {
		return payment.Subscription{}, payment.NewError(payment.CodeInvalid, err.Error())
	}
	t, err := s.account(ctx, in.AccountID)
	if err != nil {
		return payment.Subscription{}, err
	}
	d, err := s.store.GetDonor(ctx, in.AccountID, in.DonorID)
	if err != nil {
		return payment.Subscription{}, err
	}
	prod, err := s.gw.CreateProduct(ctx, t.StripeAccountID, in.ProductName, nil)
	if err != nil {
		return payment.Subscription{}, fmt.Errorf("create product: %w", err)
	}
	price, err := s.gw.CreatePrice(ctx, t.StripeAccountID, payment.CreatePriceParams{
		ProductID: prod.ID, Amount: in.Amount, Interval: "month",
	})
	if err != nil {
		return payment.Subscription{}, fmt.Errorf("create price: %w", err)
	}
	submeta := attribution(in.AccountID, in.TenantID)
	submeta["product_id"] = in.ProductID
	if in.WebhookURL != "" {
		submeta["webhook_url"] = in.WebhookURL
	}
	sub, err := s.gw.CreateSubscription(ctx, t.StripeAccountID, payment.CreateSubscriptionParams{
		CustomerID: d.StripeCustomerID, PriceID: price.ID, PaymentMethodID: in.PaymentMethodID,
		Metadata: submeta,
	})
	if err != nil {
		return payment.Subscription{}, fmt.Errorf("create subscription: %w", err)
	}
	if err := s.store.UpsertSubscription(ctx, domain.Subscription{
		AccountID: in.AccountID, DonorID: in.DonorID, ProductID: in.ProductID,
		StripeSubscriptionID: sub.ID, PriceID: price.ID, Status: sub.Status,
		CurrentPeriodEnd: sub.CurrentPeriodEnd,
	}); err != nil {
		return payment.Subscription{}, err
	}
	return sub, nil
}

// CancelSubscription cancels a recurring donation.
func (s *Service) CancelSubscription(ctx context.Context, accountID, subID string, atPeriodEnd bool) (payment.Subscription, error) {
	return s.mutateSub(ctx, accountID, func(acct string) (payment.Subscription, error) {
		return s.gw.CancelSubscription(ctx, acct, subID, atPeriodEnd)
	})
}

// SuspendSubscription pauses collection on a recurring donation.
func (s *Service) SuspendSubscription(ctx context.Context, accountID, subID string) (payment.Subscription, error) {
	return s.mutateSub(ctx, accountID, func(acct string) (payment.Subscription, error) {
		return s.gw.PauseSubscription(ctx, acct, subID)
	})
}

// ResumeSubscription resumes a paused recurring donation.
func (s *Service) ResumeSubscription(ctx context.Context, accountID, subID string) (payment.Subscription, error) {
	return s.mutateSub(ctx, accountID, func(acct string) (payment.Subscription, error) {
		return s.gw.ResumeSubscription(ctx, acct, subID)
	})
}

func (s *Service) mutateSub(ctx context.Context, accountID string, fn func(acct string) (payment.Subscription, error)) (payment.Subscription, error) {
	t, err := s.account(ctx, accountID)
	if err != nil {
		return payment.Subscription{}, err
	}
	return fn(t.StripeAccountID)
}

// LinkInput is the request for a hosted donation link.
type LinkInput struct {
	AccountID   string // Stripe connected account the link charges on
	TenantID    string // optional caller identifier, echoed back via metadata
	ProductName string
	ProductID   string      // our product/campaign id, stamped into metadata for attribution
	CampaignID  string      // optional, stamped into metadata
	Amount      money.Money // preset/min for custom; fixed for recurring
	Recurring   bool
	DonorID     string            // optional; when set, the link pre-fills the donor's email
	Email       string            // optional donor email; stamped into metadata and pre-fills the hosted page
	WebhookURL  string            // optional; caller's notification URL (else the default)
	Metadata    map[string]string // optional custom parameters; ride through to the charge and webhooks

	// AmountEditable (one-time only): when true, Amount is a pre-filled default
	// the donor can change on the hosted page; when false, Amount is locked.
	AmountEditable bool
}

// CreateDonationLink builds a Stripe-hosted payment link. Single-payment links
// use a custom-amount price (donor types the amount); recurring links use a
// fixed monthly price set at creation time. Attribution metadata is propagated
// onto the resulting charge/subscription so webhooks can identify account and
// product. When DonorID is set, the hosted page pre-fills the donor's email and
// carries a client_reference_id back in events.
func (s *Service) CreateDonationLink(ctx context.Context, in LinkInput) (payment.PaymentLink, error) {
	t, err := s.account(ctx, in.AccountID)
	if err != nil {
		return payment.PaymentLink{}, err
	}
	prod, err := s.gw.CreateProduct(ctx, t.StripeAccountID, in.ProductName, nil)
	if err != nil {
		return payment.PaymentLink{}, fmt.Errorf("create product: %w", err)
	}
	priceParams := payment.CreatePriceParams{ProductID: prod.ID, Amount: in.Amount}
	mode := "payment"
	switch {
	case in.Recurring:
		priceParams.Interval = "month" // fixed monthly amount
		mode = "subscription"
	case !in.Amount.IsPositive():
		// no amount supplied → pay-what-you-want (donor enters the amount)
		priceParams.CustomAmount = true
	case in.AmountEditable:
		// amount supplied but editable → pre-filled preset the donor can change
		priceParams.CustomAmount = true
	default:
		// one-time with a locked amount → charge exactly that amount (fixed price)
	}
	price, err := s.gw.CreatePrice(ctx, t.StripeAccountID, priceParams)
	if err != nil {
		return payment.PaymentLink{}, fmt.Errorf("create price: %w", err)
	}

	// Start with the caller's custom parameters, then set the reserved keys so
	// our attribution/routing always wins over any collision.
	meta := map[string]string{}
	for k, v := range in.Metadata {
		meta[k] = v
	}
	for k, v := range attribution(in.AccountID, in.TenantID) {
		meta[k] = v
	}
	if in.ProductID != "" {
		meta["product_id"] = in.ProductID
	}
	if in.CampaignID != "" {
		meta["campaign_id"] = in.CampaignID
	}
	if in.WebhookURL != "" {
		meta["webhook_url"] = in.WebhookURL
	}
	params := payment.CreatePaymentLinkParams{PriceID: price.ID, Mode: mode, Metadata: meta}
	if in.Email != "" {
		meta["email"] = in.Email
		params.PrefilledEmail = in.Email
	}
	if in.DonorID != "" {
		if d, err := s.store.GetDonor(ctx, in.AccountID, in.DonorID); err == nil {
			// the donor record is authoritative for prefill when both are given
			params.PrefilledEmail = d.Email
			params.ClientReferenceID = d.ID
		}
	}
	return s.gw.CreatePaymentLink(ctx, t.StripeAccountID, params)
}
