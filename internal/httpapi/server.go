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
	"reflect"
	"strconv"
	"strings"
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
	Service           *app.Service
	ServiceLive       *app.Service // optional; serves requests with "mode":"live"/"prod"
	Router            *webhook.Router
	Engine            *recon.Engine
	Gateway           payment.Gateway
	Store             store.Store
	WebhookSecret     string
	WebhookSecretLive string // optional; tried when the primary secret fails
	Currency          string // reporting currency for the metrics dashboard
	DefaultAccountID  string // used when a request omits account_id
	Metrics           *telemetry.Metrics
	Logger            *slog.Logger
}

// accountOr returns the request account id, or the configured default when empty.
func (s *Server) accountOr(id string) string {
	if id == "" {
		return s.deps.DefaultAccountID
	}
	return id
}

// errAccountRequired is returned when neither the request nor the server
// configuration provides a Stripe account id.
var errAccountRequired = errors.New("account_id is required")

// serviceFor picks the Stripe stack for a request's mode: "test" (or empty)
// uses the default key, "live"/"prod" the live key. Anything else is rejected.
func (s *Server) serviceFor(mode string) (*app.Service, error) {
	switch strings.ToLower(mode) {
	case "", "test":
		return s.deps.Service, nil
	case "live", "prod":
		if s.deps.ServiceLive == nil {
			return nil, errors.New("live mode is not configured on this server")
		}
		return s.deps.ServiceLive, nil
	default:
		return nil, errors.New("unknown mode " + strconv.Quote(mode) + `: use "test", "live" or "prod"`)
	}
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
	AccountID       string         `json:"account_id"` // Stripe connected account (required; DEFAULT_ACCOUNT_ID fills it when configured)
	TenantID        string         `json:"tenant_id"`  // optional caller identifier, echoed back via metadata
	DonorID         string         `json:"donor_id"`
	ProductID       string         `json:"product_id"`
	Amount          int64          `json:"amount"`
	Currency        string         `json:"currency"`
	PaymentMethodID string         `json:"payment_method_id"`
	IdempotencyKey  string         `json:"idempotency_key"`
	WebhookURL      string         `json:"webhook_url"` // optional
	Mode            string         `json:"mode"`        // "test" (default) or "live"/"prod": selects the Stripe key
	Metadata        map[string]any `json:"metadata"`    // optional custom parameters; non-strings are JSON-encoded
}

// stringifyMetadata converts caller metadata to Stripe's string-to-string
// format: strings pass through, everything else (numbers, booleans, nested
// objects, arrays) is JSON-encoded.
func stringifyMetadata(in map[string]any) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		if s, ok := v.(string); ok {
			out[k] = s
			continue
		}
		b, err := json.Marshal(v)
		if err != nil {
			continue
		}
		out[k] = string(b)
	}
	return out
}

// jsonKeys lists a request struct's json field names, so any other top-level
// key in a request body can be recognised as caller metadata.
func jsonKeys(v any) map[string]bool {
	keys := map[string]bool{}
	t := reflect.TypeOf(v)
	for i := 0; i < t.NumField(); i++ {
		if name, _, _ := strings.Cut(t.Field(i).Tag.Get("json"), ","); name != "" && name != "-" {
			keys[name] = true
		}
	}
	return keys
}

// decodeWithExtras decodes the body into req and returns every top-level field
// that is not a known request field, stringified. Callers may send attribution
// fields flat ("type": "donation", "reference": ...) instead of nesting them
// under "metadata".
func decodeWithExtras(r *http.Request, req any, known map[string]bool) (map[string]string, error) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(body, req); err != nil {
		return nil, err
	}
	var raw map[string]any
	_ = json.Unmarshal(body, &raw)
	for k := range known {
		delete(raw, k)
	}
	return stringifyMetadata(raw), nil
}

// mergeMeta overlays b onto a (b wins), allocating only when needed.
func mergeMeta(a, b map[string]string) map[string]string {
	if len(b) == 0 {
		return a
	}
	if a == nil {
		a = make(map[string]string, len(b))
	}
	for k, v := range b {
		a[k] = v
	}
	return a
}

var donationKnownKeys = jsonKeys(startDonationReq{})

func (s *Server) startDonation(w http.ResponseWriter, r *http.Request) {
	var req startDonationReq
	extras, err := decodeWithExtras(r, &req, donationKnownKeys)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	svc, err := s.serviceFor(req.Mode)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	// flat unknown fields plus the explicit metadata object (which wins)
	meta := mergeMeta(extras, stringifyMetadata(req.Metadata))
	if req.Mode != "" {
		meta = mergeMeta(meta, map[string]string{"mode": req.Mode})
	}
	accountID := s.accountOr(req.AccountID)
	if accountID == "" {
		writeError(w, http.StatusBadRequest, errAccountRequired)
		return
	}
	pi, err := svc.StartDonation(r.Context(), app.StartDonationInput{
		AccountID: accountID, TenantID: req.TenantID, DonorID: req.DonorID, ProductID: req.ProductID,
		Amount: money.New(req.Amount, req.Currency), PaymentMethodID: req.PaymentMethodID,
		IdempotencyKey: req.IdempotencyKey, WebhookURL: req.WebhookURL, Metadata: meta,
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
	if len(meta) > 0 {
		resp["metadata"] = meta
	}
	writeJSON(w, http.StatusCreated, resp)
}

type createLinkReq struct {
	AccountID      string         `json:"account_id"` // Stripe connected account (required; DEFAULT_ACCOUNT_ID fills it when configured)
	TenantID       string         `json:"tenant_id"`  // optional caller identifier, echoed back via metadata
	ProductName    string         `json:"product_name"`
	ProductID      string         `json:"product_id"`
	CustomerID     string         `json:"customer_id"` // donor id; optional, used to pre-fill the hosted page
	Email          string         `json:"email"`       // optional donor email; stamped into metadata + pre-fills the page
	Amount         int64          `json:"amount"`      // one-time: preset/min; subscription: fixed monthly
	Currency       string         `json:"currency"`
	Recurring      bool           `json:"recurring"`       // false = one-time custom amount, true = monthly
	WebhookURL     string         `json:"webhook_url"`     // optional
	Mode           string         `json:"mode"`            // "test" (default) or "live"/"prod": selects the Stripe key
	Metadata       map[string]any `json:"metadata"`        // optional custom parameters; non-strings are JSON-encoded
	EditableAmount bool           `json:"editable_amount"` // one-time: donor may edit the amount
}

var linkKnownKeys = jsonKeys(createLinkReq{})

func (s *Server) createPaymentLink(w http.ResponseWriter, r *http.Request) {
	var req createLinkReq
	extras, err := decodeWithExtras(r, &req, linkKnownKeys)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	svc, err := s.serviceFor(req.Mode)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	// flat unknown fields plus the explicit metadata object (which wins)
	meta := mergeMeta(extras, stringifyMetadata(req.Metadata))
	if req.Mode != "" {
		meta = mergeMeta(meta, map[string]string{"mode": req.Mode})
	}
	cur := req.Currency
	if cur == "" {
		cur = s.deps.Currency
	}
	accountID := s.accountOr(req.AccountID)
	if accountID == "" {
		writeError(w, http.StatusBadRequest, errAccountRequired)
		return
	}
	link, err := svc.CreateDonationLink(r.Context(), app.LinkInput{
		AccountID: accountID, TenantID: req.TenantID, ProductName: req.ProductName, ProductID: req.ProductID,
		Amount: money.New(req.Amount, cur), Recurring: req.Recurring, DonorID: req.CustomerID, Email: req.Email,
		WebhookURL: req.WebhookURL, Metadata: meta, AmountEditable: req.EditableAmount,
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
	if len(meta) > 0 {
		resp["metadata"] = meta
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
	if err != nil && s.deps.WebhookSecretLive != "" {
		// Live-mode events are signed with the live endpoint's secret.
		ev, err = s.deps.Gateway.VerifyWebhookSignature(body, r.Header.Get("Stripe-Signature"), s.deps.WebhookSecretLive)
	}
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
