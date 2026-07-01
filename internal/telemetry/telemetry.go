// Package telemetry exports application metrics to Google Cloud Monitoring via
// OpenTelemetry. Metrics are counters recorded at the authoritative points
// (link creation, terminal payment events, reconciliation). Cloud Monitoring
// persists the time series, so they survive container restarts.
//
// A nil *Metrics (or one built with NewNoop) is safe to use — every Record
// method is a no-op — so tests and local runs need no exporter.
package telemetry

import (
	"context"
	"time"

	mexporter "github.com/GoogleCloudPlatform/opentelemetry-operations-go/exporter/metric"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
)

// Metrics holds the instruments. The zero value / nil is a safe no-op.
type Metrics struct {
	linksCreated   metric.Int64Counter
	donations      metric.Int64Counter
	amountCaptured metric.Int64Counter
	backfills      metric.Int64Counter
	events         metric.Int64Counter
}

// NewNoop returns a Metrics whose methods do nothing.
func NewNoop() *Metrics { return &Metrics{} }

// Setup builds a Cloud Monitoring exporter and meter provider and returns the
// Metrics plus a shutdown func. projectID may be empty to auto-detect on GCP.
func Setup(_ context.Context, projectID string) (*Metrics, func(context.Context) error, error) {
	var opts []mexporter.Option
	if projectID != "" {
		opts = append(opts, mexporter.WithProjectID(projectID))
	}
	exp, err := mexporter.New(opts...)
	if err != nil {
		return nil, nil, err
	}
	reader := sdkmetric.NewPeriodicReader(exp, sdkmetric.WithInterval(60*time.Second))
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	otel.SetMeterProvider(mp)

	m, err := newInstruments(mp.Meter("barakah-payments"))
	if err != nil {
		return nil, nil, err
	}
	return m, mp.Shutdown, nil
}

func newInstruments(m metric.Meter) (*Metrics, error) {
	linksCreated, err := m.Int64Counter("barakah/payment_links_created",
		metric.WithDescription("Payment links created"))
	if err != nil {
		return nil, err
	}
	donations, err := m.Int64Counter("barakah/donations_total",
		metric.WithDescription("Donation attempts by terminal status"))
	if err != nil {
		return nil, err
	}
	amountCaptured, err := m.Int64Counter("barakah/amount_captured_total",
		metric.WithDescription("Captured donation amount in minor units"), metric.WithUnit("{minor}"))
	if err != nil {
		return nil, err
	}
	backfills, err := m.Int64Counter("barakah/reconciliation_backfills_total",
		metric.WithDescription("Rows recovered by reconciliation"))
	if err != nil {
		return nil, err
	}
	events, err := m.Int64Counter("barakah/webhook_events_total",
		metric.WithDescription("Stripe webhook events handled"))
	if err != nil {
		return nil, err
	}
	return &Metrics{linksCreated, donations, amountCaptured, backfills, events}, nil
}

// RecordPaymentLink counts a created link, tagged by tenant and mode.
func (t *Metrics) RecordPaymentLink(ctx context.Context, tenant, mode string) {
	if t == nil || t.linksCreated == nil {
		return
	}
	t.linksCreated.Add(ctx, 1, metric.WithAttributes(
		attribute.String("tenant", tenant), attribute.String("mode", mode)))
}

// RecordDonation counts a donation reaching a terminal status.
func (t *Metrics) RecordDonation(ctx context.Context, tenant, status string) {
	if t == nil || t.donations == nil {
		return
	}
	t.donations.Add(ctx, 1, metric.WithAttributes(
		attribute.String("tenant", tenant), attribute.String("status", status)))
}

// RecordCaptured adds captured amount (minor units) for a tenant/currency.
func (t *Metrics) RecordCaptured(ctx context.Context, tenant, currency string, minor int64) {
	if t == nil || t.amountCaptured == nil || minor <= 0 {
		return
	}
	t.amountCaptured.Add(ctx, minor, metric.WithAttributes(
		attribute.String("tenant", tenant), attribute.String("currency", currency)))
}

// RecordBackfill counts a reconciliation backfill for a tenant.
func (t *Metrics) RecordBackfill(ctx context.Context, tenant string, n int) {
	if t == nil || t.backfills == nil || n <= 0 {
		return
	}
	t.backfills.Add(ctx, int64(n), metric.WithAttributes(attribute.String("tenant", tenant)))
}

// RecordEvent counts a handled webhook event by type.
func (t *Metrics) RecordEvent(ctx context.Context, eventType string) {
	if t == nil || t.events == nil {
		return
	}
	t.events.Add(ctx, 1, metric.WithAttributes(attribute.String("type", eventType)))
}
