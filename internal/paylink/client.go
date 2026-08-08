// Package paylink is a small HTTP client used for testing the deployed service.
// It calls the /v1/payment-links endpoint and returns the hosted donation URL,
// with one helper for one-time payments and one for monthly subscriptions.
package paylink

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Client talks to a running Barakah Fund API (e.g. the Cloud Run URL).
type Client struct {
	BaseURL string
	HTTP    *http.Client
}

// New builds a Client for the given base URL.
func New(baseURL string) *Client {
	return &Client{BaseURL: baseURL, HTTP: &http.Client{Timeout: 30 * time.Second}}
}

// Request is the input for a payment link.
type Request struct {
	AccountID      string            // Stripe connected account the link charges on
	Mode           string            // required: "test" or "live"/"prod", selects the Stripe key
	TenantID       string            // optional caller identifier, echoed back via metadata
	CustomerID     string            // donor / customer id (pre-fills the hosted page)
	Email          string            // optional donor email; stamped into metadata and pre-fills the page
	ProductName    string            // campaign/product name
	ProductID      string            // optional, for attribution metadata
	AmountMinor    int64             // minor units; one-time: preset/min, subscription: fixed monthly
	Currency       string            // ISO-4217, e.g. "USD"
	WebhookURL     string            // optional; caller's notification URL (else the server default)
	Metadata       map[string]string // optional custom key/value parameters attached to the payment
	AmountEditable bool              // one-time only: donor may edit the pre-filled amount
}

// OneTimePaymentLink creates a hosted one-time, custom-amount donation link.
func (c *Client) OneTimePaymentLink(ctx context.Context, req Request) (string, error) {
	return c.create(ctx, req, false)
}

// SubscriptionPaymentLink creates a hosted monthly donation link.
func (c *Client) SubscriptionPaymentLink(ctx context.Context, req Request) (string, error) {
	return c.create(ctx, req, true)
}

func (c *Client) create(ctx context.Context, req Request, recurring bool) (string, error) {
	body, _ := json.Marshal(map[string]any{
		"account_id":      req.AccountID,
		"mode":            req.Mode,
		"tenant_id":       req.TenantID,
		"customer_id":     req.CustomerID,
		"email":           req.Email,
		"product_name":    req.ProductName,
		"product_id":      req.ProductID,
		"amount":          req.AmountMinor,
		"currency":        req.Currency,
		"recurring":       recurring,
		"webhook_url":     req.WebhookURL,
		"metadata":        req.Metadata,
		"editable_amount": req.AmountEditable,
	})
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/v1/payment-links", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := c.HTTP.Do(httpReq)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return "", fmt.Errorf("payment-links: http %d: %s", resp.StatusCode, string(data))
	}
	var out struct {
		URL  string `json:"url"`
		Mode string `json:"mode"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return "", fmt.Errorf("decode response: %w", err)
	}
	return out.URL, nil
}
