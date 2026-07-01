package webhook

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"
)

// Notification is the payload forwarded to a caller's webhook URL when a
// payment reaches a terminal state.
type Notification struct {
	Event           string `json:"event"`
	PaymentIntentID string `json:"payment_intent_id"`
	TenantID        string `json:"tenant_id"`
	Status          string `json:"status"`
	Amount          int64  `json:"amount"`
	Currency        string `json:"currency"`
}

// Forwarder delivers a Notification to a caller-supplied URL.
type Forwarder interface {
	Notify(ctx context.Context, url string, n Notification) error
}

// HTTPForwarder POSTs the notification as JSON.
type HTTPForwarder struct{ HTTP *http.Client }

// NewHTTPForwarder builds an HTTP forwarder with a sane timeout.
func NewHTTPForwarder() *HTTPForwarder {
	return &HTTPForwarder{HTTP: &http.Client{Timeout: 10 * time.Second}}
}

func (f *HTTPForwarder) Notify(ctx context.Context, url string, n Notification) error {
	body, _ := json.Marshal(n)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := f.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("webhook forward: http %d", resp.StatusCode)
	}
	return nil
}

// ForwardCall records one forwarded notification (test spy).
type ForwardCall struct {
	URL          string
	Notification Notification
}

// ForwardRecorder captures forwarded notifications for assertions in tests.
type ForwardRecorder struct {
	mu    sync.Mutex
	Calls []ForwardCall
}

func (r *ForwardRecorder) Notify(_ context.Context, url string, n Notification) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.Calls = append(r.Calls, ForwardCall{URL: url, Notification: n})
	return nil
}
