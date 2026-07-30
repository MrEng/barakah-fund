// Package payment defines the PaymentGateway port: the single seam between the
// application and Stripe. Both the real Stripe adapter and the in-memory mock
// implement Gateway, so services depend only on this interface.
package payment

import (
	"context"
	"time"

	"github.com/barakahfund/payments/internal/domain"
	"github.com/barakahfund/payments/internal/money"
)

// Card is re-exported for gateway callers.
type Card = domain.Card

// Customer is a Stripe customer (a donor's id on a connected account).
type Customer struct {
	ID    string
	Email string
	Name  string
}

// SetupIntent backs the "add card" flow; the client finishes it with the secret.
type SetupIntent struct {
	ID           string
	ClientSecret string
	Status       string
}

// PaymentIntent is a single charge attempt.
type PaymentIntent struct {
	ID            string
	ClientSecret  string
	Status        domain.PaymentStatus
	Amount        money.Money
	CustomerID    string
	FailureReason string
	Metadata      map[string]string
	Created       time.Time
}

// Subscription is a recurring charge on the gateway side.
type Subscription struct {
	ID                        string
	Status                    domain.SubscriptionStatus
	CustomerID                string
	PriceID                   string
	CurrentPeriodEnd          time.Time
	LatestInvoiceClientSecret string
}

// Product and Price mirror Stripe catalog objects.
type Product struct {
	ID   string
	Name string
}

type Price struct {
	ID           string
	Amount       money.Money
	Interval     string
	CustomAmount bool
}

// PaymentLink is a hosted, shareable donation URL.
type PaymentLink struct {
	ID     string
	URL    string
	Mode   string
	Active bool
}

// Refund reverses a charge, fully or partially.
type Refund struct {
	ID              string
	PaymentIntentID string
	Amount          money.Money
}

// BalanceTxn is an entry in Stripe's authoritative money ledger.
type BalanceTxn struct {
	ID       string
	Type     string // charge, refund, ...
	Amount   money.Money
	Fee      money.Money
	SourceID string // originating pi_/ch_ id
	Created  time.Time
}

// EventType enumerates the webhook events we handle.
type EventType string

const (
	EventPaymentSucceeded EventType = "payment_intent.succeeded"
	EventPaymentFailed    EventType = "payment_intent.payment_failed"
	EventInvoicePaid      EventType = "invoice.paid"
	EventInvoiceFailed    EventType = "invoice.payment_failed"
	EventChargeRefunded   EventType = "charge.refunded"
	EventSubUpdated       EventType = "customer.subscription.updated"
	EventSubDeleted       EventType = "customer.subscription.deleted"
)

// Event is a verified webhook event with the relevant object resolved.
type Event struct {
	ID            string
	Type          EventType
	Account       string // connected account (acct_...)
	PaymentIntent *PaymentIntent
	Subscription  *Subscription
	Refund        *Refund
}

// Parameter structs -------------------------------------------------------

type CreateCustomerParams struct {
	Email    string
	Name     string
	Metadata map[string]string
}

type CreatePaymentIntentParams struct {
	Amount          money.Money
	CustomerID      string
	PaymentMethodID string // optional; set for server-side confirm of a saved card
	ApplicationFee  money.Money
	ReceiptEmail    string
	Metadata        map[string]string
	Confirm         bool // true = confirm now (saved card); false = client finishes with secret
}

type CreatePriceParams struct {
	ProductID    string
	Amount       money.Money // used for fixed prices
	Interval     string      // "" one-time, "month" recurring
	CustomAmount bool        // pay-what-you-want (one-time only)
	Metadata     map[string]string
}

type CreateSubscriptionParams struct {
	CustomerID      string
	PriceID         string
	PaymentMethodID string
	Metadata        map[string]string
}

type CreatePaymentLinkParams struct {
	PriceID  string
	Mode     string            // "payment" or "subscription"
	Metadata map[string]string // propagated to the link AND the resulting charge/subscription

	// PrefilledEmail pre-fills the email field on the hosted page (URL param).
	PrefilledEmail string
	// ClientReferenceID is echoed back in webhooks/events to identify the donor.
	ClientReferenceID string
}

// Gateway is the port implemented by the Stripe adapter and the mock.
// Every method also takes the Stripe connected-account id (account).
type Gateway interface {
	// Customer
	CreateCustomer(ctx context.Context, account string, p CreateCustomerParams) (Customer, error)

	// Cards
	CreateSetupIntent(ctx context.Context, account, customerID string) (SetupIntent, error)
	ListPaymentMethods(ctx context.Context, account, customerID string) ([]Card, error)
	DetachPaymentMethod(ctx context.Context, account, paymentMethodID string) error

	// Payments
	CreatePaymentIntent(ctx context.Context, account string, p CreatePaymentIntentParams) (PaymentIntent, error)
	GetPaymentIntent(ctx context.Context, account, id string) (PaymentIntent, error)
	CreateRefund(ctx context.Context, account, paymentIntentID string, amount *money.Money) (Refund, error)

	// Catalog
	CreateProduct(ctx context.Context, account, name string, metadata map[string]string) (Product, error)
	CreatePrice(ctx context.Context, account string, p CreatePriceParams) (Price, error)

	// Subscriptions
	CreateSubscription(ctx context.Context, account string, p CreateSubscriptionParams) (Subscription, error)
	CancelSubscription(ctx context.Context, account, id string, atPeriodEnd bool) (Subscription, error)
	PauseSubscription(ctx context.Context, account, id string) (Subscription, error)
	ResumeSubscription(ctx context.Context, account, id string) (Subscription, error)

	// Payment links
	CreatePaymentLink(ctx context.Context, account string, p CreatePaymentLinkParams) (PaymentLink, error)

	// Reconciliation reads
	ListBalanceTransactions(ctx context.Context, account string, from, to time.Time) ([]BalanceTxn, error)
	ListPaymentIntents(ctx context.Context, account string, from, to time.Time) ([]PaymentIntent, error)

	// Webhooks
	VerifyWebhookSignature(payload []byte, sigHeader, secret string) (Event, error)
}
