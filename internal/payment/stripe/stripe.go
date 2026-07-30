// Package stripe is the production implementation of payment.Gateway. It calls
// Stripe's REST API directly over net/http (no SDK dependency), form-encoding
// requests and mapping JSON responses to the port's DTOs. Every method is a
// thin forwarder: build params, one call, map result. The connected account
// is passed via the Stripe-Account header.
package stripe

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/barakahfund/payments/internal/domain"
	"github.com/barakahfund/payments/internal/money"
	"github.com/barakahfund/payments/internal/payment"
)

const defaultBaseURL = "https://api.stripe.com"

// Client is the Stripe gateway adapter.
type Client struct {
	secretKey string
	baseURL   string
	http      *http.Client
}

// Option configures a Client.
type Option func(*Client)

// WithBaseURL overrides the API base (used by tests to point at httptest).
func WithBaseURL(u string) Option { return func(c *Client) { c.baseURL = u } }

// WithHTTPClient overrides the HTTP client.
func WithHTTPClient(h *http.Client) Option { return func(c *Client) { c.http = h } }

// New builds a Stripe client.
func New(secretKey string, opts ...Option) *Client {
	c := &Client{secretKey: secretKey, baseURL: defaultBaseURL, http: &http.Client{Timeout: 30 * time.Second}}
	for _, o := range opts {
		o(c)
	}
	return c
}

// do executes one API call, form-encoding params and mapping errors.
func (c *Client) do(ctx context.Context, method, path, account string, form url.Values, out any) error {
	target := c.baseURL + path
	var body io.Reader
	if method == http.MethodGet {
		if len(form) > 0 {
			target += "?" + form.Encode()
		}
	} else if len(form) > 0 {
		body = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return payment.NewError(payment.CodeAPI, err.Error())
	}
	req.Header.Set("Authorization", "Bearer "+c.secretKey)
	if account != "" {
		req.Header.Set("Stripe-Account", account)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return payment.NewError(payment.CodeAPI, err.Error())
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return parseError(resp.StatusCode, data)
	}
	if out != nil {
		if err := json.Unmarshal(data, out); err != nil {
			return payment.NewError(payment.CodeAPI, "decode: "+err.Error())
		}
	}
	return nil
}

func parseError(status int, data []byte) error {
	var env struct {
		Error struct {
			Type    string `json:"type"`
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	_ = json.Unmarshal(data, &env)
	code := payment.CodeAPI
	switch {
	case status == http.StatusTooManyRequests:
		code = payment.CodeRateLimit
	case status == http.StatusNotFound:
		code = payment.CodeNotFound
	case env.Error.Type == "card_error":
		code = payment.CodeCard
	case status == http.StatusBadRequest:
		code = payment.CodeInvalid
	}
	msg := env.Error.Message
	if msg == "" {
		msg = fmt.Sprintf("stripe http %d", status)
	}
	return payment.NewError(code, msg)
}

// --- JSON shapes (subset of Stripe objects) ---

type piJSON struct {
	ID               string            `json:"id"`
	ClientSecret     string            `json:"client_secret"`
	Status           string            `json:"status"`
	Amount           int64             `json:"amount"`
	Currency         string            `json:"currency"`
	Customer         string            `json:"customer"`
	Created          int64             `json:"created"`
	Metadata         map[string]string `json:"metadata"`
	LastPaymentError *struct {
		Message string `json:"message"`
	} `json:"last_payment_error"`
}

func (o piJSON) toDTO() payment.PaymentIntent {
	status := mapPIStatus(o.Status)
	reason := ""
	if o.LastPaymentError != nil {
		reason = o.LastPaymentError.Message
		if o.Status == "requires_payment_method" {
			status = domain.PaymentFailed
		}
	}
	return payment.PaymentIntent{
		ID: o.ID, ClientSecret: o.ClientSecret, Status: status,
		Amount: money.New(o.Amount, o.Currency), CustomerID: o.Customer,
		FailureReason: reason, Metadata: o.Metadata, Created: time.Unix(o.Created, 0).UTC(),
	}
}

func mapPIStatus(s string) domain.PaymentStatus {
	switch s {
	case "succeeded":
		return domain.PaymentSucceeded
	case "canceled":
		return domain.PaymentCanceled
	default:
		return domain.PaymentRequested
	}
}

func mapSubStatus(s string) domain.SubscriptionStatus {
	switch s {
	case "canceled":
		return domain.SubCanceled
	case "paused":
		return domain.SubPaused
	default:
		return domain.SubActive
	}
}

type subJSON struct {
	ID               string `json:"id"`
	Status           string `json:"status"`
	Customer         string `json:"customer"`
	CurrentPeriodEnd int64  `json:"current_period_end"`
	PauseCollection  any    `json:"pause_collection"`
}

func (o subJSON) toDTO(priceID string) payment.Subscription {
	status := mapSubStatus(o.Status)
	if o.PauseCollection != nil && status == domain.SubActive {
		status = domain.SubPaused
	}
	return payment.Subscription{
		ID: o.ID, Status: status, CustomerID: o.Customer, PriceID: priceID,
		CurrentPeriodEnd: time.Unix(o.CurrentPeriodEnd, 0).UTC(),
	}
}

// --- Gateway: customer ---

func (c *Client) CreateCustomer(ctx context.Context, account string, p payment.CreateCustomerParams) (payment.Customer, error) {
	form := url.Values{}
	setNonEmpty(form, "email", p.Email)
	setNonEmpty(form, "name", p.Name)
	addMetadata(form, p.Metadata)
	var out struct {
		ID    string `json:"id"`
		Email string `json:"email"`
		Name  string `json:"name"`
	}
	if err := c.do(ctx, http.MethodPost, "/v1/customers", account, form, &out); err != nil {
		return payment.Customer{}, err
	}
	return payment.Customer{ID: out.ID, Email: out.Email, Name: out.Name}, nil
}

// --- Gateway: cards ---

func (c *Client) CreateSetupIntent(ctx context.Context, account, customerID string) (payment.SetupIntent, error) {
	form := url.Values{}
	setNonEmpty(form, "customer", customerID)
	form.Set("usage", "off_session")
	var out struct {
		ID           string `json:"id"`
		ClientSecret string `json:"client_secret"`
		Status       string `json:"status"`
	}
	if err := c.do(ctx, http.MethodPost, "/v1/setup_intents", account, form, &out); err != nil {
		return payment.SetupIntent{}, err
	}
	return payment.SetupIntent{ID: out.ID, ClientSecret: out.ClientSecret, Status: out.Status}, nil
}

func (c *Client) ListPaymentMethods(ctx context.Context, account, customerID string) ([]payment.Card, error) {
	form := url.Values{}
	form.Set("customer", customerID)
	form.Set("type", "card")
	var out struct {
		Data []struct {
			ID   string `json:"id"`
			Card struct {
				Brand    string `json:"brand"`
				Last4    string `json:"last4"`
				ExpMonth int    `json:"exp_month"`
				ExpYear  int    `json:"exp_year"`
			} `json:"card"`
		} `json:"data"`
	}
	if err := c.do(ctx, http.MethodGet, "/v1/payment_methods", account, form, &out); err != nil {
		return nil, err
	}
	cards := make([]payment.Card, 0, len(out.Data))
	for _, d := range out.Data {
		cards = append(cards, payment.Card{
			ID: d.ID, Brand: d.Card.Brand, Last4: d.Card.Last4,
			ExpMonth: d.Card.ExpMonth, ExpYear: d.Card.ExpYear,
		})
	}
	return cards, nil
}

func (c *Client) DetachPaymentMethod(ctx context.Context, account, paymentMethodID string) error {
	return c.do(ctx, http.MethodPost, "/v1/payment_methods/"+paymentMethodID+"/detach", account, url.Values{}, nil)
}

// --- Gateway: payments ---

func (c *Client) CreatePaymentIntent(ctx context.Context, account string, p payment.CreatePaymentIntentParams) (payment.PaymentIntent, error) {
	form := url.Values{}
	form.Set("amount", strconv.FormatInt(p.Amount.Amount, 10))
	form.Set("currency", strings.ToLower(p.Amount.Currency))
	setNonEmpty(form, "customer", p.CustomerID)
	setNonEmpty(form, "payment_method", p.PaymentMethodID)
	setNonEmpty(form, "receipt_email", p.ReceiptEmail)
	if p.ApplicationFee.Amount > 0 {
		form.Set("application_fee_amount", strconv.FormatInt(p.ApplicationFee.Amount, 10))
	}
	if p.Confirm {
		form.Set("confirm", "true")
		form.Set("off_session", "true")
	}
	// Accept every payment method enabled in the account's Dashboard, pinned
	// explicitly rather than relying on the API-version default. Server-side
	// confirms of a saved card cannot follow a browser redirect, so those
	// restrict to non-redirect methods (the saved card qualifies).
	form.Set("automatic_payment_methods[enabled]", "true")
	if p.Confirm {
		form.Set("automatic_payment_methods[allow_redirects]", "never")
	} else {
		form.Set("automatic_payment_methods[allow_redirects]", "always")
	}
	addMetadata(form, p.Metadata)
	var out piJSON
	if err := c.do(ctx, http.MethodPost, "/v1/payment_intents", account, form, &out); err != nil {
		return payment.PaymentIntent{}, err
	}
	return out.toDTO(), nil
}

func (c *Client) GetPaymentIntent(ctx context.Context, account, id string) (payment.PaymentIntent, error) {
	var out piJSON
	if err := c.do(ctx, http.MethodGet, "/v1/payment_intents/"+id, account, url.Values{}, &out); err != nil {
		return payment.PaymentIntent{}, err
	}
	return out.toDTO(), nil
}

func (c *Client) CreateRefund(ctx context.Context, account, paymentIntentID string, amount *money.Money) (payment.Refund, error) {
	form := url.Values{}
	form.Set("payment_intent", paymentIntentID)
	if amount != nil {
		form.Set("amount", strconv.FormatInt(amount.Amount, 10))
	}
	var out struct {
		ID            string `json:"id"`
		PaymentIntent string `json:"payment_intent"`
		Amount        int64  `json:"amount"`
		Currency      string `json:"currency"`
	}
	if err := c.do(ctx, http.MethodPost, "/v1/refunds", account, form, &out); err != nil {
		return payment.Refund{}, err
	}
	return payment.Refund{ID: out.ID, PaymentIntentID: out.PaymentIntent, Amount: money.New(out.Amount, out.Currency)}, nil
}

// --- Gateway: catalog ---

func (c *Client) CreateProduct(ctx context.Context, account, name string, metadata map[string]string) (payment.Product, error) {
	form := url.Values{}
	form.Set("name", name)
	addMetadata(form, metadata)
	var out struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	if err := c.do(ctx, http.MethodPost, "/v1/products", account, form, &out); err != nil {
		return payment.Product{}, err
	}
	return payment.Product{ID: out.ID, Name: out.Name}, nil
}

func (c *Client) CreatePrice(ctx context.Context, account string, p payment.CreatePriceParams) (payment.Price, error) {
	form := url.Values{}
	form.Set("product", p.ProductID)
	form.Set("currency", strings.ToLower(p.Amount.Currency))
	if p.CustomAmount {
		form.Set("custom_unit_amount[enabled]", "true")
		if p.Amount.Amount > 0 {
			// pre-fill the editable amount box with the provided amount
			form.Set("custom_unit_amount[preset]", strconv.FormatInt(p.Amount.Amount, 10))
		}
	} else {
		form.Set("unit_amount", strconv.FormatInt(p.Amount.Amount, 10))
	}
	if p.Interval != "" {
		form.Set("recurring[interval]", p.Interval)
	}
	addMetadata(form, p.Metadata)
	var out struct {
		ID         string `json:"id"`
		UnitAmount int64  `json:"unit_amount"`
		Currency   string `json:"currency"`
		Recurring  *struct {
			Interval string `json:"interval"`
		} `json:"recurring"`
	}
	if err := c.do(ctx, http.MethodPost, "/v1/prices", account, form, &out); err != nil {
		return payment.Price{}, err
	}
	interval := ""
	if out.Recurring != nil {
		interval = out.Recurring.Interval
	}
	return payment.Price{ID: out.ID, Amount: money.New(out.UnitAmount, out.Currency), Interval: interval, CustomAmount: p.CustomAmount}, nil
}

// --- Gateway: subscriptions ---

func (c *Client) CreateSubscription(ctx context.Context, account string, p payment.CreateSubscriptionParams) (payment.Subscription, error) {
	form := url.Values{}
	form.Set("customer", p.CustomerID)
	form.Set("items[0][price]", p.PriceID)
	setNonEmpty(form, "default_payment_method", p.PaymentMethodID)
	addMetadata(form, p.Metadata)
	var out subJSON
	if err := c.do(ctx, http.MethodPost, "/v1/subscriptions", account, form, &out); err != nil {
		return payment.Subscription{}, err
	}
	return out.toDTO(p.PriceID), nil
}

func (c *Client) CancelSubscription(ctx context.Context, account, id string, atPeriodEnd bool) (payment.Subscription, error) {
	var out subJSON
	if atPeriodEnd {
		form := url.Values{}
		form.Set("cancel_at_period_end", "true")
		if err := c.do(ctx, http.MethodPost, "/v1/subscriptions/"+id, account, form, &out); err != nil {
			return payment.Subscription{}, err
		}
		return out.toDTO(""), nil
	}
	if err := c.do(ctx, http.MethodDelete, "/v1/subscriptions/"+id, account, url.Values{}, &out); err != nil {
		return payment.Subscription{}, err
	}
	return out.toDTO(""), nil
}

func (c *Client) PauseSubscription(ctx context.Context, account, id string) (payment.Subscription, error) {
	form := url.Values{}
	form.Set("pause_collection[behavior]", "void")
	var out subJSON
	if err := c.do(ctx, http.MethodPost, "/v1/subscriptions/"+id, account, form, &out); err != nil {
		return payment.Subscription{}, err
	}
	return out.toDTO(""), nil
}

func (c *Client) ResumeSubscription(ctx context.Context, account, id string) (payment.Subscription, error) {
	form := url.Values{}
	form.Set("pause_collection", "") // empty clears the pause
	var out subJSON
	if err := c.do(ctx, http.MethodPost, "/v1/subscriptions/"+id, account, form, &out); err != nil {
		return payment.Subscription{}, err
	}
	return out.toDTO(""), nil
}

// --- Gateway: payment links ---

func (c *Client) CreatePaymentLink(ctx context.Context, account string, p payment.CreatePaymentLinkParams) (payment.PaymentLink, error) {
	form := url.Values{}
	form.Set("line_items[0][price]", p.PriceID)
	form.Set("line_items[0][quantity]", "1")
	// Metadata on the link object itself.
	addMetadata(form, p.Metadata)
	// Propagate the same metadata onto the object the link creates, so webhooks
	// on the charge/subscription can attribute it (tenant, product, campaign).
	objPrefix := "payment_intent_data[metadata]"
	if p.Mode == "subscription" {
		objPrefix = "subscription_data[metadata]"
	}
	for k, v := range p.Metadata {
		form.Set(objPrefix+"["+k+"]", v)
	}
	var out struct {
		ID     string `json:"id"`
		URL    string `json:"url"`
		Active bool   `json:"active"`
	}
	if err := c.do(ctx, http.MethodPost, "/v1/payment_links", account, form, &out); err != nil {
		return payment.PaymentLink{}, err
	}
	return payment.PaymentLink{ID: out.ID, URL: withLinkQuery(out.URL, p), Mode: p.Mode, Active: out.Active}, nil
}

// withLinkQuery appends the donor-recognition query params Stripe reads on the
// hosted page (prefilled_email, client_reference_id).
func withLinkQuery(raw string, p payment.CreatePaymentLinkParams) string {
	if p.PrefilledEmail == "" && p.ClientReferenceID == "" {
		return raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	q := u.Query()
	if p.PrefilledEmail != "" {
		q.Set("prefilled_email", p.PrefilledEmail)
	}
	if p.ClientReferenceID != "" {
		q.Set("client_reference_id", p.ClientReferenceID)
	}
	u.RawQuery = q.Encode()
	return u.String()
}

// --- Gateway: reconciliation reads ---

func (c *Client) ListBalanceTransactions(ctx context.Context, account string, from, to time.Time) ([]payment.BalanceTxn, error) {
	form := windowForm(from, to)
	var txns []payment.BalanceTxn
	for {
		var out struct {
			HasMore bool `json:"has_more"`
			Data    []struct {
				ID       string `json:"id"`
				Type     string `json:"type"`
				Amount   int64  `json:"amount"`
				Fee      int64  `json:"fee"`
				Currency string `json:"currency"`
				Source   string `json:"source"`
				Created  int64  `json:"created"`
			} `json:"data"`
		}
		if err := c.do(ctx, http.MethodGet, "/v1/balance_transactions", account, form, &out); err != nil {
			return nil, err
		}
		for _, d := range out.Data {
			txns = append(txns, payment.BalanceTxn{
				ID: d.ID, Type: d.Type, Amount: money.New(d.Amount, d.Currency),
				Fee: money.New(d.Fee, d.Currency), SourceID: d.Source, Created: time.Unix(d.Created, 0).UTC(),
			})
		}
		if !out.HasMore || len(out.Data) == 0 {
			return txns, nil
		}
		form.Set("starting_after", out.Data[len(out.Data)-1].ID)
	}
}

func (c *Client) ListPaymentIntents(ctx context.Context, account string, from, to time.Time) ([]payment.PaymentIntent, error) {
	form := windowForm(from, to)
	var pis []payment.PaymentIntent
	for {
		var out struct {
			HasMore bool     `json:"has_more"`
			Data    []piJSON `json:"data"`
		}
		if err := c.do(ctx, http.MethodGet, "/v1/payment_intents", account, form, &out); err != nil {
			return nil, err
		}
		for _, d := range out.Data {
			pis = append(pis, d.toDTO())
		}
		if !out.HasMore || len(out.Data) == 0 {
			return pis, nil
		}
		form.Set("starting_after", out.Data[len(out.Data)-1].ID)
	}
}

// --- Gateway: webhook signature verification ---

// VerifyWebhookSignature validates Stripe's `Stripe-Signature` header and maps
// the event payload to a payment.Event.
func (c *Client) VerifyWebhookSignature(payload []byte, sigHeader, secret string) (payment.Event, error) {
	ts, sigs := parseSigHeader(sigHeader)
	if ts == "" || len(sigs) == 0 {
		return payment.Event{}, payment.NewError(payment.CodeInvalid, "malformed signature header")
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(ts + "."))
	mac.Write(payload)
	expected := hex.EncodeToString(mac.Sum(nil))
	ok := false
	for _, s := range sigs {
		if hmac.Equal([]byte(s), []byte(expected)) {
			ok = true
			break
		}
	}
	if !ok {
		return payment.Event{}, payment.NewError(payment.CodeInvalid, "signature mismatch")
	}
	return decodeEvent(payload)
}

func parseSigHeader(h string) (ts string, sigs []string) {
	for _, part := range strings.Split(h, ",") {
		kv := strings.SplitN(strings.TrimSpace(part), "=", 2)
		if len(kv) != 2 {
			continue
		}
		switch kv[0] {
		case "t":
			ts = kv[1]
		case "v1":
			sigs = append(sigs, kv[1])
		}
	}
	return ts, sigs
}

func decodeEvent(payload []byte) (payment.Event, error) {
	var env struct {
		ID      string `json:"id"`
		Type    string `json:"type"`
		Account string `json:"account"`
		Data    struct {
			Object json.RawMessage `json:"object"`
		} `json:"data"`
	}
	if err := json.Unmarshal(payload, &env); err != nil {
		return payment.Event{}, payment.NewError(payment.CodeInvalid, "decode event: "+err.Error())
	}
	e := payment.Event{ID: env.ID, Type: payment.EventType(env.Type), Account: env.Account}
	switch e.Type {
	case payment.EventPaymentSucceeded, payment.EventPaymentFailed:
		var o piJSON
		_ = json.Unmarshal(env.Data.Object, &o)
		pi := o.toDTO()
		if e.Type == payment.EventPaymentSucceeded {
			pi.Status = domain.PaymentSucceeded
		} else {
			pi.Status = domain.PaymentFailed
		}
		e.PaymentIntent = &pi
	case payment.EventSubUpdated, payment.EventSubDeleted:
		var o subJSON
		_ = json.Unmarshal(env.Data.Object, &o)
		sub := o.toDTO("")
		e.Subscription = &sub
	}
	return e, nil
}

// --- helpers ---

func setNonEmpty(f url.Values, k, v string) {
	if v != "" {
		f.Set(k, v)
	}
}

func addMetadata(f url.Values, m map[string]string) {
	for k, v := range m {
		f.Set("metadata["+k+"]", v)
	}
}

func windowForm(from, to time.Time) url.Values {
	f := url.Values{}
	f.Set("limit", "100")
	if !from.IsZero() {
		f.Set("created[gte]", strconv.FormatInt(from.Unix(), 10))
	}
	if !to.IsZero() {
		f.Set("created[lte]", strconv.FormatInt(to.Unix(), 10))
	}
	return f
}

// compile-time check that Client implements the Gateway port.
var _ payment.Gateway = (*Client)(nil)
