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
	"github.com/barakahfund/payments/internal/metrics"
	"github.com/barakahfund/payments/internal/money"
	"github.com/barakahfund/payments/internal/payment"
	"github.com/barakahfund/payments/internal/recon"
	"github.com/barakahfund/payments/internal/store"
	"github.com/barakahfund/payments/internal/webhook"
)

// Deps are the collaborators an HTTP server needs.
type Deps struct {
	Service       *app.Service
	Router        *webhook.Router
	Engine        *recon.Engine
	Gateway       payment.Gateway
	Store         store.Store
	WebhookSecret string
	Currency      string // reporting currency for the metrics dashboard
	Logger        *slog.Logger
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
	s.mux.HandleFunc("POST /v1/webhooks/stripe", s.webhook)
	s.mux.HandleFunc("GET /v1/metrics/tenants/{tenantID}/summary", s.tenantSummary)
	s.mux.HandleFunc("POST /admin/reconcile", s.reconcile)
	return s
}

// Handler returns the HTTP handler to serve.
func (s *Server) Handler() http.Handler { return s.mux }

func (s *Server) health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

type startDonationReq struct {
	TenantID        string `json:"tenant_id"`
	DonorID         string `json:"donor_id"`
	ProductID       string `json:"product_id"`
	Amount          int64  `json:"amount"`
	Currency        string `json:"currency"`
	PaymentMethodID string `json:"payment_method_id"`
	IdempotencyKey  string `json:"idempotency_key"`
}

func (s *Server) startDonation(w http.ResponseWriter, r *http.Request) {
	var req startDonationReq
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	pi, err := s.deps.Service.StartDonation(r.Context(), app.StartDonationInput{
		TenantID: req.TenantID, DonorID: req.DonorID, ProductID: req.ProductID,
		Amount: money.New(req.Amount, req.Currency), PaymentMethodID: req.PaymentMethodID,
		IdempotencyKey: req.IdempotencyKey,
	})
	if err != nil {
		writeError(w, statusForError(err), err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"payment_intent_id": pi.ID,
		"client_secret":     pi.ClientSecret,
		"status":            pi.Status,
	})
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

func (s *Server) tenantSummary(w http.ResponseWriter, r *http.Request) {
	tenantID := r.PathValue("tenantID")
	from := parseTime(r.URL.Query().Get("from"))
	to := parseTime(r.URL.Query().Get("to"))
	agg := metrics.New(s.deps.Store, s.deps.Currency)
	sum, err := agg.TenantSummary(r.Context(), store.PaymentFilter{TenantID: tenantID, From: from, To: to})
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, sum)
}

type reconcileReq struct {
	TenantID string `json:"tenant_id"`
	From     string `json:"from"`
	To       string `json:"to"`
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
	if req.TenantID == "" {
		reports, err := s.deps.Engine.ReconcileAll(ctx, from, to)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"reports": reports})
		return
	}
	tenant, err := s.deps.Store.GetTenant(ctx, req.TenantID)
	if err != nil {
		writeError(w, http.StatusNotFound, err)
		return
	}
	rep, err := s.deps.Engine.Reconcile(ctx, tenant, from, to)
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
