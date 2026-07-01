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
	"github.com/barakahfund/payments/internal/webhook"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	gw := buildGateway(logger)
	st := store.NewMemory() // swap for a Cloud SQL-backed store in production
	notifier := email.LogNotifier{Logger: logger}

	svc := app.New(gw, st, notifier, app.Options{ApplicationFeeBps: envInt("APPLICATION_FEE_BPS", 0)})
	router := webhook.New(st, notifier, time.Now)
	engine := recon.New(gw, st, time.Now)

	srv := httpapi.NewServer(httpapi.Deps{
		Service: svc, Router: router, Engine: engine, Gateway: gw, Store: st,
		WebhookSecret: os.Getenv("STRIPE_WEBHOOK_SECRET"),
		Currency:      envStr("REPORTING_CURRENCY", "USD"),
		Logger:        logger,
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
	<-ctx.Done()

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		logger.Error("graceful shutdown failed", "err", err)
	}
	logger.Info("stopped")
}

func buildGateway(logger *slog.Logger) payment.Gateway {
	if key := os.Getenv("STRIPE_SECRET_KEY"); key != "" {
		logger.Info("using stripe gateway")
		return stripe.New(key)
	}
	logger.Warn("STRIPE_SECRET_KEY unset; using in-memory mock gateway")
	return mock.New()
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
