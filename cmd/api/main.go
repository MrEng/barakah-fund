// Command api is the Cloud Run entrypoint. It wires the gateway (real Stripe
// when STRIPE_SECRET_KEY is set, otherwise the in-memory mock for local demos),
// builds the HTTP server, and serves on $PORT with graceful shutdown.
package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/barakahfund/payments/internal/app"
	"github.com/barakahfund/payments/internal/email"
	"github.com/barakahfund/payments/internal/httpapi"
	"github.com/barakahfund/payments/internal/payment"
	"github.com/barakahfund/payments/internal/payment/mock"
	"github.com/barakahfund/payments/internal/payment/stripe"
	"github.com/barakahfund/payments/internal/recon"
	"github.com/barakahfund/payments/internal/store"
	"github.com/barakahfund/payments/internal/telemetry"
	"github.com/barakahfund/payments/internal/webhook"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	gw := buildGateway(logger)

	// Storage: Cloud SQL (Postgres) when DATABASE_URL is set, else in-memory.
	var st store.Store
	if dsn := os.Getenv("DATABASE_URL"); dsn != "" {
		pg, err := store.NewPostgres(context.Background(), dsn)
		if err != nil {
			logger.Error("cloud sql connect failed", "err", err)
			os.Exit(1)
		}
		if err := pg.Migrate(context.Background()); err != nil {
			logger.Error("cloud sql migrate failed", "err", err)
			os.Exit(1)
		}
		defer pg.Close()
		st = pg
		logger.Info("using cloud sql (postgres) store")
	} else {
		st = store.NewMemory()
		logger.Warn("DATABASE_URL unset; using in-memory store")
	}

	notifier := email.LogNotifier{Logger: logger}

	// Callers pass the Stripe connected-account id directly as account_id; there
	// is no account table to look up. When a request omits account_id it falls
	// back to DEFAULT_ACCOUNT_ID (legacy: DEFAULT_TENANT_ID), which defaults to
	// empty — meaning platform-direct: no Stripe-Account header, charge on the
	// platform (your) account.
	defaultAccountID := envStr("DEFAULT_ACCOUNT_ID", envStr("DEFAULT_TENANT_ID", ""))
	logger.Info("account resolution: account id passed directly", "default_account_id", defaultAccountID)

	// Telemetry: export metrics to Cloud Monitoring (no-op if it can't initialise,
	// e.g. running locally without credentials).
	tel, shutdownTel, err := telemetry.Setup(context.Background(), os.Getenv("GOOGLE_CLOUD_PROJECT"))
	if err != nil {
		logger.Warn("metrics export disabled", "err", err)
		tel = telemetry.NewNoop()
	} else {
		logger.Info("metrics export enabled (cloud monitoring)")
		defer func() { _ = shutdownTel(context.Background()) }()
	}

	svc := app.New(gw, st, notifier, app.Options{ApplicationFeeBps: envInt("APPLICATION_FEE_BPS", 0)})
	router := webhook.New(st, notifier, time.Now,
		webhook.WithForwarder(webhook.NewHTTPForwarder()),
		webhook.WithDefaultWebhookURL(os.Getenv("DEFAULT_WEBHOOK_URL")),
		webhook.WithMetrics(tel),
	)
	engine := recon.New(gw, st, time.Now, recon.WithMetrics(tel))

	srv := httpapi.NewServer(httpapi.Deps{
		Service: svc, Router: router, Engine: engine, Gateway: gw, Store: st,
		WebhookSecret:    os.Getenv("STRIPE_WEBHOOK_SECRET"),
		Currency:         envStr("REPORTING_CURRENCY", "USD"),
		DefaultAccountID: defaultAccountID,
		Metrics:          tel,
		Logger:           logger,
	})

	addr := ":" + envStr("PORT", "8080") // Cloud Run provides $PORT
	httpSrv := &http.Server{Addr: addr, Handler: srv.Handler(), ReadHeaderTimeout: 10 * time.Second}

	go func() {
		logger.Info("listening", "addr", addr)
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("server failed", "err", err)
			os.Exit(1)
		}
	}()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Background reconciliation loop (in-process). On Cloud Run this needs CPU
	// always allocated (--no-cpu-throttling) and a warm instance
	// (--min-instances>=1) to run reliably between requests.
	go runReconLoop(ctx, engine, envDuration("RECON_INTERVAL", 6*time.Hour), logger)

	<-ctx.Done()

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		logger.Error("graceful shutdown failed", "err", err)
	}
	logger.Info("stopped")
}

// runReconLoop runs reconciliation across all accounts on an interval until ctx
// is cancelled. It does an immediate pass on startup, then ticks.
func runReconLoop(ctx context.Context, engine *recon.Engine, interval time.Duration, logger *slog.Logger) {
	if interval <= 0 {
		logger.Info("reconciliation loop disabled")
		return
	}
	logger.Info("reconciliation loop started", "interval", interval.String())
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	reconcile := func() {
		to := time.Now()
		from := to.Add(-24 * time.Hour)
		reports, err := engine.ReconcileAll(ctx, from, to)
		if err != nil {
			logger.Error("reconciliation failed", "err", err)
			return
		}
		backfilled := 0
		for _, r := range reports {
			backfilled += r.Backfilled
		}
		logger.Info("reconciliation completed", "accounts", len(reports), "backfilled", backfilled)
	}

	reconcile() // initial pass on startup
	for {
		select {
		case <-ctx.Done():
			logger.Info("reconciliation loop stopped")
			return
		case <-ticker.C:
			reconcile()
		}
	}
}

func buildGateway(logger *slog.Logger) payment.Gateway {
	if key := os.Getenv("STRIPE_SECRET_KEY"); key != "" {
		logger.Info("using stripe gateway")
		return stripe.New(key)
	}
	logger.Warn("STRIPE_SECRET_KEY unset; using in-memory mock gateway")
	return mock.New()
}

func envDuration(k string, def time.Duration) time.Duration {
	if v := os.Getenv(k); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}

func envStr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func envInt(k string, def int64) int64 {
	if v := os.Getenv(k); v != "" {
		var n int64
		for _, c := range v {
			if c < '0' || c > '9' {
				return def
			}
			n = n*10 + int64(c-'0')
		}
		return n
	}
	return def
}
