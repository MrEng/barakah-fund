// Package httpapi exposes the service over HTTP with thin handlers. It suits
// Cloud Run: the constructor wires dependencies and Handler() returns a
// net/http handler the caller serves on $PORT.
package httpapi

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/barakahfund/payments/internal/app"
	"github.com/barakahfund/payments/internal/domain"
	"github.com/barakahfund/payments/internal/metrics"
	"github.com/barakahfund/payments/internal/money"
	"github.com/barakahfund/payments/internal/payment"
	"github.com/barakahfund/payments/internal/recon"
	"github.com/barakahfund/payments/internal/store"
	"github.com/barakahfund/payments/internal/telemetry"
	"github.com/barakahfund/payments/internal/webhook"
)

// Deps are the collaborators an HTTP server needs.
type Deps struct {
	Service          *app.Service
	Router           *webhook.Router
	Engine           *recon.Engine
	Gateway          payment.Gateway
	Store            store.Store
	WebhookSecret    string
	Currency         string // reporting currency for the metrics dashboard
	DefaultAccountID string // used when a request omits account_id
	Metrics          *telemetry.Metrics
	Logger           *slog.Logger
}

// accountOr returns the request account id, or the configured default when empty.
func (s *Server) accountOr(id string) string {
	if id == "" {
		return s.deps.DefaultAccountID
	}
	return id
}

// Server holds dependencies and a route mux.
type Server struct {
	deps Deps
	mux  *http.ServeMux
}

// NewServer wires routes.
func NewServer(d Deps) *Server {
	if d.Logger == nil {
		d.Logger = slog.Default()
	}
	if d.Currency == "" {
		d.Currency = "USD"
	}
	s := &Server{deps: d, mux: http.NewServeMux()}
	s.mux.HandleFunc("GET /health", s.health)
	s.mux.HandleFunc("GET /{$}", s.health) // root also serves health
	s.mux.HandleFunc("POST /v1/donations", s.startDonation)
	s.mux.HandleFunc("POST /v1/payment-links", s.createPaymentLink)
	s.mux.HandleFunc("POST /v1/webhooks/stripe", s.webhook)
	s.mux.HandleFunc("GET /v1/metrics/accounts/{accountID}/summary", s.accountSummary)
	s.mux.HandleFunc("POST /admin/reconcile", s.reconcile)
	return s
}

// Handler returns the HTTP handler to serve.
func (s *Server) Handler() http.Handler { return s.mux }

func (s *Server) health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

type startDonationReq struct {
	AccountID       string `json:"account_id"` // Stripe connected account (falls back to the default)
	TenantID        string `json:"tenant_id"`  // optional caller identifier, echoed back via metadata
	DonorID         string `json:"donor_id"`
	ProductID       string `json:"product_id"`
	Amount          int64  `json:"amount"`
	Currency        string `json:"currency"`
	PaymentMethodID string `json:"payment_method_id"`
	IdempotencyKey  string `json:"idempotency_key"`
	WebhookURL      string `json:"webhook_url"` // optional
}

func (s *Server) startDonation(w http.ResponseWriter, r *http.Request) {
	var req startDonationReq
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	accountID := s.accountOr(req.AccountID)
	pi, err := s.deps.Service.StartDonation(r.Context(), app.StartDonationInput{
		AccountID: accountID, TenantID: req.TenantID, DonorID: req.DonorID, ProductID: req.ProductID,
		Amount: money.New(req.Amount, req.Currency), PaymentMethodID: req.PaymentMethodID,
		IdempotencyKey: req.IdempotencyKey, WebhookURL: req.WebhookURL,
	})
	if err != nil {
		writeError(w, statusForError(err), err)
		return
	}
	resp := map[string]any{
		"payment_intent_id": pi.ID,
		"client_secret":     pi.ClientSecret,
		"status":            pi.Status,
		"account_id":        accountID,
	}
	if req.TenantID != "" {
		resp["tenant_id"] = req.TenantID
	}
	writeJSON(w, http.StatusCreated, resp)
}

type createLinkReq struct {
	AccountID      string            `json:"account_id"` // Stripe connected account (falls back to the default)
	TenantID       string            `json:"tenant_id"`  // optional caller identifier, echoed back via metadata
	ProductName    string            `json:"product_name"`
	ProductID      string            `json:"product_id"`
	CustomerID     string            `json:"customer_id"` // donor id; optional, used to pre-fill the hosted page
	Email          string            `json:"email"`       // optional donor email; stamped into metadata + pre-fills the page
	Amount         int64             `json:"amount"`      // one-time: preset/min; subscription: fixed monthly
	Currency       string            `json:"currency"`
	Recurring      bool              `json:"recurring"`       // false = one-time custom amount, true = monthly
	WebhookURL     string            `json:"webhook_url"`     // optional
	Metadata       map[string]string `json:"metadata"`        // optional custom key/value parameters
	EditableAmount bool              `json:"editable_amount"` // one-time: donor may edit the amount
}

func (s *Server) createPaymentLink(w http.ResponseWriter, r *http.Request) {
	var req createLinkReq
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	cur := req.Currency
	if cur == "" {
		cur = s.deps.Currency
	}
	accountID := s.accountOr(req.AccountID)
	link, err := s.deps.Service.CreateDonationLink(r.Context(), app.LinkInput{
		AccountID: accountID, TenantID: req.TenantID, ProductName: req.ProductName, ProductID: req.ProductID,
		Amount: money.New(req.Amount, cur), Recurring: req.Recurring, DonorID: req.CustomerID, Email: req.Email,
		WebhookURL: req.WebhookURL, Metadata: req.Metadata, AmountEditable: req.EditableAmount,
	})
	if err != nil {
		writeError(w, statusForError(err), err)
		return
	}
	s.deps.Metrics.RecordPaymentLink(r.Context(), accountID, link.Mode)
	resp := map[string]any{
		"id":         link.ID,
		"url":        link.URL,
		"mode":       link.Mode,
		"account_id": accountID,
	}
	if req.TenantID != "" {
		resp["tenant_id"] = req.TenantID
	}
	writeJSON(w, http.StatusCreated, resp)
}

func (s *Server) webhook(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	ev, err := s.deps.Gateway.VerifyWebhookSignature(body, r.Header.Get("Stripe-Signature"), s.deps.WebhookSecret)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := s.deps.Router.Handle(r.Context(), ev); err != nil {
		// Ack anyway on internal error would lose the event; return 500 so Stripe retries.
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"received": "true"})
}

func (s *Server) accountSummary(w http.ResponseWriter, r *http.Request) {
	accountID := r.PathValue("accountID")
	from := parseTime(r.URL.Query().Get("from"))
	to := parseTime(r.URL.Query().Get("to"))
	agg := metrics.New(s.deps.Store, s.deps.Currency)
	sum, err := agg.AccountSummary(r.Context(), store.PaymentFilter{AccountID: accountID, From: from, To: to})
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, sum)
}

type reconcileReq struct {
	AccountID string `json:"account_id"`
	From      string `json:"from"`
	To        string `json:"to"`
}

func (s *Server) reconcile(w http.ResponseWriter, r *http.Request) {
	var req reconcileReq
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	from, to := parseTime(req.From), parseTime(req.To)
	if to.IsZero() {
		to = time.Now()
	}
	if from.IsZero() {
		from = to.Add(-24 * time.Hour)
	}
	ctx := r.Context()
	if req.AccountID == "" {
		reports, err := s.deps.Engine.ReconcileAll(ctx, from, to)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"reports": reports})
		return
	}
	// The account id is passed directly; no account table lookup.
	account := domain.Account{ID: req.AccountID, StripeAccountID: req.AccountID, ChargesEnabled: true}
	rep, err := s.deps.Engine.Reconcile(ctx, account, from, to)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, rep)
}

// helpers -----------------------------------------------------------------

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, map[string]string{"error": err.Error()})
}

func readJSON(r *http.Request, v any) error {
	return json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(v)
}

func parseTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

func statusForError(err error) int {
	var perr *payment.Error
	if errors.As(err, &perr) {
		switch perr.Code {
		case payment.CodeInvalid, payment.CodeCard:
			return http.StatusBadRequest
		case payment.CodeNotFound:
			return http.StatusNotFound
		case payment.CodeRateLimit:
			return http.StatusTooManyRequests
		}
	}
	return http.StatusInternalServerError
}
