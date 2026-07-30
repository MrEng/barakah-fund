package stripe

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/barakahfund/payments/internal/domain"
	"github.com/barakahfund/payments/internal/money"
	"github.com/barakahfund/payments/internal/payment"
)

func TestCreateCustomerSendsAccountHeaderAndForm(t *testing.T) {
	var gotAccount, gotEmail, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		gotAccount = r.Header.Get("Stripe-Account")
		gotAuth = r.Header.Get("Authorization")
		gotEmail = r.Form.Get("email")
		fmt.Fprint(w, `{"id":"cus_123","email":"d@e.f","name":"D"}`)
	}))
	defer srv.Close()

	c := New("sk_test_x", WithBaseURL(srv.URL))
	cus, err := c.CreateCustomer(context.Background(), "acct_9", payment.CreateCustomerParams{Email: "d@e.f", Name: "D"})
	if err != nil {
		t.Fatal(err)
	}
	if cus.ID != "cus_123" {
		t.Fatalf("id = %s", cus.ID)
	}
	if gotAccount != "acct_9" {
		t.Fatalf("Stripe-Account = %q, want acct_9", gotAccount)
	}
	if gotAuth != "Bearer sk_test_x" {
		t.Fatalf("Authorization = %q", gotAuth)
	}
	if gotEmail != "d@e.f" {
		t.Fatalf("email form = %q", gotEmail)
	}
}

func TestCreatePaymentIntentMapsResponse(t *testing.T) {
	var gotAmount, gotCurrency, gotMeta, gotAPM, gotRedirects string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		gotAmount = r.Form.Get("amount")
		gotCurrency = r.Form.Get("currency")
		gotMeta = r.Form.Get("metadata[tenant_id]")
		gotAPM = r.Form.Get("automatic_payment_methods[enabled]")
		gotRedirects = r.Form.Get("automatic_payment_methods[allow_redirects]")
		fmt.Fprint(w, `{"id":"pi_1","client_secret":"pi_1_secret","status":"requires_confirmation","amount":5000,"currency":"usd","customer":"cus_1"}`)
	}))
	defer srv.Close()

	c := New("sk", WithBaseURL(srv.URL))
	pi, err := c.CreatePaymentIntent(context.Background(), "acct_1", payment.CreatePaymentIntentParams{
		Amount: money.New(5000, "USD"), CustomerID: "cus_1", Metadata: map[string]string{"tenant_id": "t1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if gotAmount != "5000" || gotCurrency != "usd" || gotMeta != "t1" {
		t.Fatalf("form amount=%s currency=%s meta=%s", gotAmount, gotCurrency, gotMeta)
	}
	// client-completed intents accept every dashboard-enabled method
	if gotAPM != "true" || gotRedirects != "always" {
		t.Fatalf("automatic_payment_methods enabled=%q allow_redirects=%q, want true/always", gotAPM, gotRedirects)
	}
	if pi.Status != domain.PaymentRequested || pi.ClientSecret != "pi_1_secret" {
		t.Fatalf("pi = %+v", pi)
	}
	if pi.Amount.Amount != 5000 || pi.Amount.Currency != "USD" {
		t.Fatalf("amount = %+v", pi.Amount)
	}
}

func TestPaymentIntentFailureMapping(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"id":"pi_2","status":"requires_payment_method","amount":100,"currency":"usd","last_payment_error":{"message":"Your card was declined."}}`)
	}))
	defer srv.Close()
	c := New("sk", WithBaseURL(srv.URL))
	pi, err := c.GetPaymentIntent(context.Background(), "acct_1", "pi_2")
	if err != nil {
		t.Fatal(err)
	}
	if pi.Status != domain.PaymentFailed || pi.FailureReason == "" {
		t.Fatalf("pi = %+v", pi)
	}
}

func TestListBalanceTransactions(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"data":[{"id":"txn_1","type":"charge","amount":5000,"fee":175,"currency":"usd","source":"ch_1","created":1750000000}]}`)
	}))
	defer srv.Close()
	c := New("sk", WithBaseURL(srv.URL))
	txns, err := c.ListBalanceTransactions(context.Background(), "acct_1", time.Time{}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(txns) != 1 || txns[0].Type != "charge" || txns[0].Amount.Amount != 5000 || txns[0].SourceID != "ch_1" {
		t.Fatalf("txns = %+v", txns)
	}
}

func TestErrorMapping(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		fmt.Fprint(w, `{"error":{"type":"api_error","message":"rate limited"}}`)
	}))
	defer srv.Close()
	c := New("sk", WithBaseURL(srv.URL))
	_, err := c.CreateCustomer(context.Background(), "acct_1", payment.CreateCustomerParams{})
	var perr *payment.Error
	if !errors.As(err, &perr) {
		t.Fatalf("err type = %T", err)
	}
	if perr.Code != payment.CodeRateLimit || !perr.Retryable {
		t.Fatalf("mapped error = %+v", perr)
	}
}

func TestVerifyWebhookSignature(t *testing.T) {
	secret := "whsec_test"
	payload := []byte(`{"id":"evt_1","type":"payment_intent.succeeded","account":"acct_1","data":{"object":{"id":"pi_1","amount":2000,"currency":"usd","metadata":{"tenant_id":"t1"}}}}`)
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(ts + "."))
	mac.Write(payload)
	sig := hex.EncodeToString(mac.Sum(nil))
	header := "t=" + ts + ",v1=" + sig

	c := New("sk")
	ev, err := c.VerifyWebhookSignature(payload, header, secret)
	if err != nil {
		t.Fatal(err)
	}
	if ev.Type != payment.EventPaymentSucceeded || ev.PaymentIntent == nil {
		t.Fatalf("event = %+v", ev)
	}
	if ev.PaymentIntent.Status != domain.PaymentSucceeded || ev.PaymentIntent.Metadata["tenant_id"] != "t1" {
		t.Fatalf("event pi = %+v", ev.PaymentIntent)
	}
}

func TestVerifyWebhookSignatureRejectsBadSig(t *testing.T) {
	c := New("sk")
	_, err := c.VerifyWebhookSignature([]byte(`{}`), "t=1,v1=deadbeef", "whsec")
	if err == nil {
		t.Fatal("expected signature mismatch error")
	}
}
