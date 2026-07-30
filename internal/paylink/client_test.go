package paylink

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// fakeAPI returns a server that echoes the requested mode and records the body.
func fakeAPI(t *testing.T, seen *map[string]any) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		*seen = body
		mode := "payment"
		if body["recurring"] == true {
			mode = "subscription"
		}
		w.WriteHeader(http.StatusCreated)
		fmt.Fprintf(w, `{"id":"plink_1","url":"https://donate.stripe.test/plink_1?mode=%s","mode":%q}`, mode, mode)
	}))
}

func TestOneTimePaymentLink(t *testing.T) {
	var seen map[string]any
	srv := fakeAPI(t, &seen)
	defer srv.Close()

	url, err := New(srv.URL).OneTimePaymentLink(context.Background(), Request{
		AccountID: "acct_demo", TenantID: "demo-tenant", CustomerID: "cus_123", ProductName: "Zakat", AmountMinor: 5000, Currency: "USD",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(url, "mode=payment") {
		t.Fatalf("url = %s, want one-time payment link", url)
	}
	if seen["recurring"] != false || seen["customer_id"] != "cus_123" {
		t.Fatalf("request body = %v", seen)
	}
	if seen["account_id"] != "acct_demo" || seen["tenant_id"] != "demo-tenant" {
		t.Fatalf("request attribution = %v, want account acct_demo / tenant demo-tenant", seen)
	}
}

func TestSubscriptionPaymentLink(t *testing.T) {
	var seen map[string]any
	srv := fakeAPI(t, &seen)
	defer srv.Close()

	url, err := New(srv.URL).SubscriptionPaymentLink(context.Background(), Request{
		AccountID: "acct_demo", CustomerID: "cus_123", ProductName: "Monthly Sadaqah", AmountMinor: 2000, Currency: "USD",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(url, "mode=subscription") {
		t.Fatalf("url = %s, want subscription link", url)
	}
	if seen["recurring"] != true {
		t.Fatalf("request body = %v", seen)
	}
}

// TestLiveDeployedService hits the real deployed API when BARAKAH_BASE_URL is
// set (e.g. the Cloud Run URL). It is skipped in the normal offline suite.
//
//	BARAKAH_BASE_URL=https://...run.app go test ./internal/paylink/ -run Live -v
func TestLiveDeployedService(t *testing.T) {
	base := os.Getenv("BARAKAH_BASE_URL")
	if base == "" {
		t.Skip("set BARAKAH_BASE_URL to run against the deployed service")
	}
	c := New(base)
	ctx := context.Background()
	// Empty AccountID → the server charges platform-direct (your Stripe account).

	// One-time with an editable preset: $50 is pre-filled but the donor can change it.
	oneTime, err := c.OneTimePaymentLink(ctx, Request{
		ProductName: "Water Wells", AmountMinor: 5000, Currency: "USD", AmountEditable: true,
	})
	if err != nil {
		t.Fatalf("one-time: %v", err)
	}
	t.Logf("one-time link: %s", oneTime)

	// Subscription is a FIXED monthly price: 100 minor units = $1.00 / month.
	sub, err := c.SubscriptionPaymentLink(ctx, Request{
		ProductName: "Monthly Water", AmountMinor: 100, Currency: "USD",
	})
	if err != nil {
		t.Fatalf("subscription: %v", err)
	}
	t.Logf("subscription link: %s", sub)
}
