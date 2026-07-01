package httpapi

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/barakahfund/payments/internal/app"
	"github.com/barakahfund/payments/internal/domain"
	"github.com/barakahfund/payments/internal/email"
	"github.com/barakahfund/payments/internal/money"
	"github.com/barakahfund/payments/internal/payment/mock"
	"github.com/barakahfund/payments/internal/payment/stripe"
	"github.com/barakahfund/payments/internal/recon"
	"github.com/barakahfund/payments/internal/store"
	"github.com/barakahfund/payments/internal/webhook"
)

const webhookSecret = "whsec_test"

func newServer(t *testing.T) (*Server, *store.Memory, domain.Donor) {
	t.Helper()
	ctx := context.Background()
	gw := mock.New()
	st := store.NewMemory()
	rec := &email.Recorder{}
	svc := app.New(gw, st, rec, app.Options{})
	st.SaveTenant(ctx, domain.Tenant{ID: "t1", StripeAccountID: "acct_1", ChargesEnabled: true})
	d, err := svc.EnsureDonor(ctx, "t1", "donor@example.com", "Aisha")
	if err != nil {
		t.Fatal(err)
	}
	srv := NewServer(Deps{
		Service: svc, Router: webhook.New(st, rec, nil), Engine: recon.New(gw, st, nil),
		Gateway: stripe.New("sk"), // used only for local webhook signature verification
		Store:   st, WebhookSecret: webhookSecret, Currency: "USD",
	})
	return srv, st, d
}

func TestHealth(t *testing.T) {
	srv, _, _ := newServer(t)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/health", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
}

func TestStartDonationEndpoint(t *testing.T) {
	srv, st, d := newServer(t)
	body := fmt.Sprintf(`{"tenant_id":"t1","donor_id":%q,"product_id":"prod_1","amount":5000,"currency":"USD"}`, d.ID)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/v1/donations", bytes.NewBufferString(body)))
	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
	var resp struct {
		PaymentIntentID string `json:"payment_intent_id"`
		ClientSecret    string `json:"client_secret"`
		Status          string `json:"status"`
	}
	json.Unmarshal(rr.Body.Bytes(), &resp)
	if resp.ClientSecret == "" || resp.Status != string(domain.PaymentRequested) {
		t.Fatalf("resp = %+v", resp)
	}
	if _, err := st.GetPayment(context.Background(), "t1", resp.PaymentIntentID); err != nil {
		t.Fatalf("payment not stored: %v", err)
	}
}

func TestWebhookEndpoint(t *testing.T) {
	srv, st, d := newServer(t)
	// Create a donation to get a real intent id.
	body := fmt.Sprintf(`{"tenant_id":"t1","donor_id":%q,"product_id":"prod_1","amount":5000,"currency":"USD"}`, d.ID)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/v1/donations", bytes.NewBufferString(body)))
	var created struct {
		PaymentIntentID string `json:"payment_intent_id"`
	}
	json.Unmarshal(rr.Body.Bytes(), &created)

	payload := fmt.Sprintf(
		`{"id":"evt_1","type":"payment_intent.succeeded","account":"acct_1","data":{"object":{"id":%q,"amount":5000,"currency":"usd","metadata":{"tenant_id":"t1"}}}}`,
		created.PaymentIntentID)
	req := httptest.NewRequest(http.MethodPost, "/v1/webhooks/stripe", bytes.NewBufferString(payload))
	req.Header.Set("Stripe-Signature", sign(payload, webhookSecret))

	rr2 := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr2, req)
	if rr2.Code != http.StatusOK {
		t.Fatalf("webhook status = %d body=%s", rr2.Code, rr2.Body.String())
	}
	got, _ := st.GetPayment(context.Background(), "t1", created.PaymentIntentID)
	if got.Status != domain.PaymentSucceeded {
		t.Fatalf("payment status = %s, want succeeded", got.Status)
	}
}

func TestWebhookRejectsBadSignature(t *testing.T) {
	srv, _, _ := newServer(t)
	req := httptest.NewRequest(http.MethodPost, "/v1/webhooks/stripe", bytes.NewBufferString(`{}`))
	req.Header.Set("Stripe-Signature", "t=1,v1=bad")
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rr.Code)
	}
}

func TestMetricsSummaryEndpoint(t *testing.T) {
	srv, st, d := newServer(t)
	ctx := context.Background()
	// two succeeded, one failed
	st.UpsertPayment(ctx, domain.Payment{TenantID: "t1", StripePaymentIntentID: "pi_a", DonorID: d.ID, Amount: money.New(1000, "USD"), Status: domain.PaymentSucceeded, CreatedAt: time.Now()})
	st.UpsertPayment(ctx, domain.Payment{TenantID: "t1", StripePaymentIntentID: "pi_b", DonorID: d.ID, Amount: money.New(2000, "USD"), Status: domain.PaymentSucceeded, CreatedAt: time.Now()})
	st.UpsertPayment(ctx, domain.Payment{TenantID: "t1", StripePaymentIntentID: "pi_c", DonorID: d.ID, Amount: money.New(500, "USD"), Status: domain.PaymentFailed, CreatedAt: time.Now()})

	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/v1/metrics/tenants/t1/summary", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	var sum struct {
		SuccessCount int `json:"SuccessCount"`
		FailureCount int `json:"FailureCount"`
		Captured     struct {
			Amount int64 `json:"Amount"`
		} `json:"Captured"`
	}
	json.Unmarshal(rr.Body.Bytes(), &sum)
	if sum.SuccessCount != 2 || sum.FailureCount != 1 || sum.Captured.Amount != 3000 {
		t.Fatalf("summary = %+v", sum)
	}
}

func TestReconcileEndpoint(t *testing.T) {
	srv, _, _ := newServer(t)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/admin/reconcile", bytes.NewBufferString(`{}`)))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
}

// helpers

func sign(payload, secret string) string {
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(ts + "."))
	mac.Write([]byte(payload))
	return "t=" + ts + ",v1=" + hex.EncodeToString(mac.Sum(nil))
}
