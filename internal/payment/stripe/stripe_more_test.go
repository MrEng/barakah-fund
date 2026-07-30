package stripe

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/barakahfund/payments/internal/domain"
	"github.com/barakahfund/payments/internal/money"
	"github.com/barakahfund/payments/internal/payment"
)

// routed spins up a server that dispatches on method+path prefix and records
// the last form values seen.
func routed(t *testing.T, handler func(r *http.Request) string) (*Client, *recorder) {
	t.Helper()
	rec := &recorder{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		rec.method = r.Method
		rec.path = r.URL.Path
		rec.form = r.Form
		fmt.Fprint(w, handler(r))
	}))
	t.Cleanup(srv.Close)
	return New("sk", WithBaseURL(srv.URL)), rec
}

type recorder struct {
	method string
	path   string
	form   map[string][]string
}

func (r *recorder) get(k string) string {
	if v, ok := r.form[k]; ok && len(v) > 0 {
		return v[0]
	}
	return ""
}

func TestConfirmedIntentDisallowsRedirectMethods(t *testing.T) {
	ctx := context.Background()
	c, rec := routed(t, func(_ *http.Request) string {
		return `{"id":"pi_1","client_secret":"pi_1_secret","status":"succeeded","amount":1000,"currency":"usd"}`
	})
	_, err := c.CreatePaymentIntent(ctx, "acct_1", payment.CreatePaymentIntentParams{
		Amount: money.New(1000, "USD"), CustomerID: "cus_1", PaymentMethodID: "pm_1", Confirm: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	// off-session server confirms cannot follow a browser redirect
	if rec.get("automatic_payment_methods[enabled]") != "true" ||
		rec.get("automatic_payment_methods[allow_redirects]") != "never" {
		t.Fatalf("automatic_payment_methods = enabled %q redirects %q, want true/never",
			rec.get("automatic_payment_methods[enabled]"), rec.get("automatic_payment_methods[allow_redirects]"))
	}
}

func TestSetupIntentAndCards(t *testing.T) {
	ctx := context.Background()
	c, rec := routed(t, func(r *http.Request) string {
		switch {
		case strings.HasSuffix(r.URL.Path, "/setup_intents"):
			return `{"id":"seti_1","client_secret":"seti_1_secret","status":"requires_payment_method"}`
		case strings.HasSuffix(r.URL.Path, "/detach"):
			return `{"id":"pm_1"}`
		case strings.HasSuffix(r.URL.Path, "/payment_methods"):
			return `{"data":[{"id":"pm_1","card":{"brand":"visa","last4":"4242","exp_month":12,"exp_year":2030}}]}`
		}
		return `{}`
	})
	si, err := c.CreateSetupIntent(ctx, "acct_1", "cus_1")
	if err != nil || si.ClientSecret == "" {
		t.Fatalf("setup intent = %+v err %v", si, err)
	}
	if rec.get("usage") != "off_session" {
		t.Fatalf("usage = %q, want off_session", rec.get("usage"))
	}
	cards, err := c.ListPaymentMethods(ctx, "acct_1", "cus_1")
	if err != nil || len(cards) != 1 || cards[0].Last4 != "4242" {
		t.Fatalf("cards = %+v err %v", cards, err)
	}
	if err := c.DetachPaymentMethod(ctx, "acct_1", "pm_1"); err != nil {
		t.Fatal(err)
	}
}

func TestRefund(t *testing.T) {
	ctx := context.Background()
	c, rec := routed(t, func(r *http.Request) string {
		return `{"id":"re_1","payment_intent":"pi_1","amount":1000,"currency":"usd"}`
	})
	amt := money.New(1000, "USD")
	ref, err := c.CreateRefund(ctx, "acct_1", "pi_1", &amt)
	if err != nil || ref.ID != "re_1" || ref.Amount.Amount != 1000 {
		t.Fatalf("refund = %+v err %v", ref, err)
	}
	if rec.get("payment_intent") != "pi_1" || rec.get("amount") != "1000" {
		t.Fatalf("form = %v", rec.form)
	}
}

func TestProductAndPrices(t *testing.T) {
	ctx := context.Background()
	c, rec := routed(t, func(r *http.Request) string {
		switch {
		case strings.HasSuffix(r.URL.Path, "/products"):
			return `{"id":"prod_1","name":"Water"}`
		case strings.HasSuffix(r.URL.Path, "/prices"):
			return `{"id":"price_1","unit_amount":0,"currency":"usd"}`
		}
		return `{}`
	})
	prod, err := c.CreateProduct(ctx, "acct_1", "Water", nil)
	if err != nil || prod.ID != "prod_1" {
		t.Fatalf("product = %+v err %v", prod, err)
	}
	// custom amount (pay-what-you-want) one-time price with an editable preset
	_, err = c.CreatePrice(ctx, "acct_1", payment.CreatePriceParams{ProductID: "prod_1", Amount: money.New(700, "USD"), CustomAmount: true})
	if err != nil {
		t.Fatal(err)
	}
	if rec.get("custom_unit_amount[enabled]") != "true" {
		t.Fatalf("expected custom_unit_amount enabled, form=%v", rec.form)
	}
	if rec.get("custom_unit_amount[preset]") != "700" {
		t.Fatalf("expected editable preset 700, form=%v", rec.form)
	}
	// fixed recurring price
	_, err = c.CreatePrice(ctx, "acct_1", payment.CreatePriceParams{ProductID: "prod_1", Amount: money.New(1500, "USD"), Interval: "month"})
	if err != nil {
		t.Fatal(err)
	}
	if rec.get("unit_amount") != "1500" || rec.get("recurring[interval]") != "month" {
		t.Fatalf("recurring price form = %v", rec.form)
	}
}

func TestSubscriptionForwarders(t *testing.T) {
	ctx := context.Background()
	c, rec := routed(t, func(r *http.Request) string {
		return `{"id":"sub_1","status":"active","customer":"cus_1","current_period_end":1780000000}`
	})
	sub, err := c.CreateSubscription(ctx, "acct_1", payment.CreateSubscriptionParams{CustomerID: "cus_1", PriceID: "price_1"})
	if err != nil || sub.Status != domain.SubActive {
		t.Fatalf("sub = %+v err %v", sub, err)
	}
	if rec.get("items[0][price]") != "price_1" {
		t.Fatalf("items form = %v", rec.form)
	}
	if _, err := c.PauseSubscription(ctx, "acct_1", "sub_1"); err != nil {
		t.Fatal(err)
	}
	if rec.get("pause_collection[behavior]") != "void" {
		t.Fatalf("pause form = %v", rec.form)
	}
	if _, err := c.ResumeSubscription(ctx, "acct_1", "sub_1"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.CancelSubscription(ctx, "acct_1", "sub_1", true); err != nil {
		t.Fatal(err)
	}
	if rec.get("cancel_at_period_end") != "true" {
		t.Fatalf("cancel form = %v", rec.form)
	}
	if _, err := c.CancelSubscription(ctx, "acct_1", "sub_1", false); err != nil {
		t.Fatal(err)
	}
	if rec.method != http.MethodDelete {
		t.Fatalf("cancel-now method = %s, want DELETE", rec.method)
	}
}

func TestPaymentLinkAndListPaymentIntents(t *testing.T) {
	ctx := context.Background()
	c, rec := routed(t, func(r *http.Request) string {
		switch {
		case strings.HasSuffix(r.URL.Path, "/payment_links"):
			return `{"id":"plink_1","url":"https://donate.stripe.com/x","active":true}`
		case strings.HasSuffix(r.URL.Path, "/payment_intents"):
			return `{"data":[{"id":"pi_1","status":"succeeded","amount":100,"currency":"usd"}]}`
		}
		return `{}`
	})
	link, err := c.CreatePaymentLink(ctx, "acct_1", payment.CreatePaymentLinkParams{
		PriceID: "price_1", Mode: "payment",
		Metadata:       map[string]string{"tenant_id": "t1"},
		PrefilledEmail: "d@e.f", ClientReferenceID: "cus_1",
	})
	if err != nil || link.URL == "" || link.Mode != "payment" {
		t.Fatalf("link = %+v err %v", link, err)
	}
	if rec.get("line_items[0][price]") != "price_1" {
		t.Fatalf("link form = %v", rec.form)
	}
	// metadata is set on the link AND propagated to the resulting charge
	if rec.get("metadata[tenant_id]") != "t1" || rec.get("payment_intent_data[metadata][tenant_id]") != "t1" {
		t.Fatalf("metadata not propagated: form = %v", rec.form)
	}
	// donor-recognition params are appended to the hosted URL
	if !strings.Contains(link.URL, "prefilled_email=d%40e.f") || !strings.Contains(link.URL, "client_reference_id=cus_1") {
		t.Fatalf("link URL missing recognition params: %s", link.URL)
	}
	pis, err := c.ListPaymentIntents(ctx, "acct_1", time.Time{}, time.Now())
	if err != nil || len(pis) != 1 || pis[0].Status != domain.PaymentSucceeded {
		t.Fatalf("pis = %+v err %v", pis, err)
	}
}
