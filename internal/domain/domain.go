// Package domain holds the persisted entities and value types of the payment
// service. These are the projection kept in sync with Stripe by webhooks and
// reconciliation; they are distinct from the gateway DTOs in package payment.
package domain

import (
	"time"

	"github.com/barakahfund/payments/internal/money"
)

// PaymentStatus is the lifecycle state of a single donation attempt.
type PaymentStatus string

const (
	PaymentRequested PaymentStatus = "requested"
	PaymentSucceeded PaymentStatus = "succeeded"
	PaymentFailed    PaymentStatus = "failed"
	PaymentCanceled  PaymentStatus = "canceled"
)

// SubscriptionStatus is the lifecycle state of a recurring donation.
type SubscriptionStatus string

const (
	SubActive   SubscriptionStatus = "active"
	SubPaused   SubscriptionStatus = "paused"
	SubCanceled SubscriptionStatus = "canceled"
)

// Source records how a projection row was learned.
type Source string

const (
	SourceAPI            Source = "api"
	SourceWebhook        Source = "webhook"
	SourceReconciliation Source = "reconciliation"
)

// Account is a donation-collecting organization backed by a Stripe connected
// account. The caller's own tenant identifier is not modelled here; it rides
// through Stripe metadata (`tenant_id`) purely for attribution.
type Account struct {
	ID              string
	Name            string
	StripeAccountID string
	ChargesEnabled  bool
	PayoutsEnabled  bool
}

// Donor is a giver, mapped to a Stripe customer scoped to the connected account.
type Donor struct {
	ID               string
	AccountID        string
	Email            string
	Name             string
	StripeCustomerID string
}

// Card is a stored payment method (never raw PAN data).
type Card struct {
	ID        string // Stripe payment-method id (pm_...)
	Brand     string
	Last4     string
	ExpMonth  int
	ExpYear   int
	IsDefault bool
}

// Product is a campaign or cause donations are directed to.
type Product struct {
	ID              string
	AccountID       string
	StripeProductID string
	Name            string
	CampaignID      string
}

// Price is a Stripe price under a product (one-time custom-amount or recurring fixed).
type Price struct {
	ID             string
	StripePriceID  string
	Amount         money.Money
	Interval       string // "" one-time, "month" recurring
	IsCustomAmount bool
}

// Payment is one donation attempt (every attempt is stored, including failures).
type Payment struct {
	AccountID             string
	DonorID               string
	ProductID             string
	StripePaymentIntentID string
	Amount                money.Money
	Status                PaymentStatus
	ApplicationFee        money.Money
	Source                Source
	FailureReason         string
	CreatedAt             time.Time
	UpdatedAt             time.Time
	Metadata              map[string]string
}

// Subscription is a recurring donation projection.
type Subscription struct {
	AccountID            string
	DonorID              string
	ProductID            string
	StripeSubscriptionID string
	PriceID              string
	Status               SubscriptionStatus
	CurrentPeriodEnd     time.Time
}

// LedgerEntry mirrors a Stripe balance transaction (authoritative money movement).
type LedgerEntry struct {
	AccountID          string
	StripeBalanceTxnID string
	Type               string // charge, refund, adjustment, ...
	Amount             money.Money
	Fee                money.Money
	CreatedAt          time.Time
}
