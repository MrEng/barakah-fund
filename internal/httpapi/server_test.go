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
	st.SaveAccount(ctx, domain.Account{ID: "t1", StripeAccountID: "acct_1", ChargesEnabled: true})
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
	body := fmt.Sprintf(`{"account_id":"t1","tenant_id":"org-7","donor_id":%q,"product_id":"prod_1","amount":5000,"currency":"USD",
		"metadata":{"type":"donation","item_type":"program","item_id":42,"selection":{"project_id":7,"plan_id":3},"frequency":"once","reference":"ref-uuid-1","mode":"test"}}`, d.ID)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/v1/donations", bytes.NewBufferString(body)))
	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
	var resp struct {
		PaymentIntentID string            `json:"payment_intent_id"`
		ClientSecret    string            `json:"client_secret"`
		Status          string            `json:"status"`
		AccountID       string            `json:"account_id"`
		TenantID        string            `json:"tenant_id"`
		Metadata        map[string]string `json:"metadata"`
	}
	json.Unmarshal(rr.Body.Bytes(), &resp)
	if resp.ClientSecret == "" || resp.Status != string(domain.PaymentRequested) {
		t.Fatalf("resp = %+v", resp)
	}
	if resp.AccountID != "t1" || resp.TenantID != "org-7" {
		t.Fatalf("resp attribution = %+v, want account t1 / tenant org-7", resp)
	}
	// custom metadata is echoed back; non-string values arrive JSON-encoded
	if resp.Metadata["type"] != "donation" || resp.Metadata["item_id"] != "42" ||
		resp.Metadata["selection"] != `{"plan_id":3,"project_id":7}` || resp.Metadata["frequency"] != "once" {
		t.Fatalf("resp metadata = %+v", resp.Metadata)
	}
	stored, _ := st.GetPayment(context.Background(), "t1", resp.PaymentIntentID)
	if stored.Metadata["reference"] != "ref-uuid-1" || stored.Metadata["tenant_id"] != "org-7" {
		t.Fatalf("stored metadata = %+v", stored.Metadata)
	}
	if _, err := st.GetPayment(context.Background(), "t1", resp.PaymentIntentID); err != nil {
		t.Fatalf("payment not stored: %v", err)
	}
}

func TestFlatTopLevelFieldsBecomeMetadata(t *testing.T) {
	srv, _, _ := newServer(t)
	// attribution fields sent flat at the top level, no "metadata" wrapper
	body := `{"account_id": "t1", "tenant_id": "019dd9ba-aabf-70f0-9029-3f3e04de720a",
		"product_name": "Water Wells Donation", "email": "test@gmail.com",
		"type": "donation", "item_type": "program", "item_id": 42,
		"selection": {"project_id": 7, "plan_id": 3}, "frequency": "once",
		"amount": 50, "currency": "cad", "reference": "my-unique-uuid-string", "mode": "test"}`
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/v1/payment-links", bytes.NewBufferString(body)))
	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
	var resp struct {
		URL      string            `json:"url"`
		TenantID string            `json:"tenant_id"`
		Metadata map[string]string `json:"metadata"`
	}
	json.Unmarshal(rr.Body.Bytes(), &resp)
	m := resp.Metadata
	if m["type"] != "donation" || m["item_type"] != "program" || m["item_id"] != "42" ||
		m["selection"] != `{"plan_id":3,"project_id":7}` || m["frequency"] != "once" ||
		m["reference"] != "my-unique-uuid-string" || m["mode"] != "test" {
		t.Fatalf("metadata = %+v", m)
	}
	// known request fields must not leak into metadata
	for _, k := range []string{"account_id", "tenant_id", "product_name", "email", "amount", "currency"} {
		if _, ok := m[k]; ok {
			t.Fatalf("known field %q leaked into metadata: %+v", k, m)
		}
	}
	if resp.TenantID != "019dd9ba-aabf-70f0-9029-3f3e04de720a" || resp.URL == "" {
		t.Fatalf("resp = %+v", resp)
	}
}

func TestModeSelectsStripeStack(t *testing.T) {
	ctx := context.Background()
	gwTest, gwLive := mock.New(), mock.New()
	st := store.NewMemory()
	rec := &email.Recorder{}
	srv := NewServer(Deps{
		Service:     app.New(gwTest, st, rec, app.Options{}),
		ServiceLive: app.New(gwLive, st, rec, app.Options{}),
		Router:      webhook.New(st, rec, nil), Engine: recon.New(gwTest, st, nil),
		Gateway: stripe.New("sk"), Store: st, WebhookSecret: webhookSecret, Currency: "USD",
	})
	_ = ctx

	body := `{"account_id":"t1","product_name":"Zakat","amount":1000,"currency":"USD","mode":"prod"}`
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/v1/payment-links", bytes.NewBufferString(body)))
	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
	if gwLive.LastLinkParams().PriceID == "" {
		t.Fatal("prod mode must use the live gateway")
	}
	if gwTest.LastLinkParams().PriceID != "" {
		t.Fatal("prod mode must not touch the test gateway")
	}

	rr = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/v1/payment-links",
		bytes.NewBufferString(`{"account_id":"t1","product_name":"Z","amount":100,"currency":"USD","mode":"staging"}`)))
	if rr.Code != http.StatusBadRequest || !bytes.Contains(rr.Body.Bytes(), []byte("unknown mode")) {
		t.Fatalf("unknown mode: status = %d body=%s", rr.Code, rr.Body.String())
	}
}

func TestLiveModeUnconfiguredRejected(t *testing.T) {
	srv, _, _ := newServer(t) // no ServiceLive wired
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/v1/payment-links",
		bytes.NewBufferString(`{"account_id":"t1","product_name":"Z","amount":100,"currency":"USD","mode":"live"}`)))
	if rr.Code != http.StatusBadRequest || !bytes.Contains(rr.Body.Bytes(), []byte("not configured")) {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
}

func TestAccountIDRequired(t *testing.T) {
	srv, _, d := newServer(t)
	for _, tc := range []struct{ name, path, body string }{
		{"donation", "/v1/donations", fmt.Sprintf(`{"donor_id":%q,"amount":5000,"currency":"USD"}`, d.ID)},
		{"payment-link", "/v1/payment-links", `{"product_name":"Zakat","amount":5000,"currency":"USD"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rr := httptest.NewRecorder()
			srv.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodPost, tc.path, bytes.NewBufferString(tc.body)))
			if rr.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400: %s", rr.Code, rr.Body.String())
			}
			if !bytes.Contains(rr.Body.Bytes(), []byte("account_id is required")) {
				t.Fatalf("body = %s, want account_id is required", rr.Body.String())
			}
		})
	}
}

func TestCreatePaymentLinkEndpoint(t *testing.T) {
	srv, _, d := newServer(t)
	for _, tc := range []struct {
		name      string
		recurring bool
		wantMode  string
	}{
		{"one-time", false, "payment"},
		{"subscription", true, "subscription"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := fmt.Sprintf(`{"account_id":"t1","customer_id":%q,"product_name":"Zakat","amount":5000,"currency":"USD","recurring":%v}`, d.ID, tc.recurring)
			rr := httptest.NewRecorder()
			srv.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/v1/payment-links", bytes.NewBufferString(body)))
			if rr.Code != http.StatusCreated {
				t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
			}
			var resp struct{ URL, Mode string }
			json.Unmarshal(rr.Body.Bytes(), &resp)
			if resp.URL == "" || resp.Mode != tc.wantMode {
				t.Fatalf("resp = %+v, want mode %s", resp, tc.wantMode)
			}
		})
	}
}

func TestWebhookEndpoint(t *testing.T) {
	srv, st, d := newServer(t)
	// Create a donation to get a real intent id.
	body := fmt.Sprintf(`{"account_id":"t1","donor_id":%q,"product_id":"prod_1","amount":5000,"currency":"USD"}`, d.ID)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/v1/donations", bytes.NewBufferString(body)))
	var created struct {
		PaymentIntentID string `json:"payment_intent_id"`
	}
	json.Unmarshal(rr.Body.Bytes(), &created)

	payload := fmt.Sprintf(
		`{"id":"evt_1","type":"payment_intent.succeeded","account":"acct_1","data":{"object":{"id":%q,"amount":5000,"currency":"usd","metadata":{"account_id":"t1"}}}}`,
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
	st.UpsertPayment(ctx, domain.Payment{AccountID: "t1", StripePaymentIntentID: "pi_a", DonorID: d.ID, Amount: money.New(1000, "USD"), Status: domain.PaymentSucceeded, CreatedAt: time.Now()})
	st.UpsertPayment(ctx, domain.Payment{AccountID: "t1", StripePaymentIntentID: "pi_b", DonorID: d.ID, Amount: money.New(2000, "USD"), Status: domain.PaymentSucceeded, CreatedAt: time.Now()})
	st.UpsertPayment(ctx, domain.Payment{AccountID: "t1", StripePaymentIntentID: "pi_c", DonorID: d.ID, Amount: money.New(500, "USD"), Status: domain.PaymentFailed, CreatedAt: time.Now()})

	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/v1/metrics/accounts/t1/summary", nil))
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
