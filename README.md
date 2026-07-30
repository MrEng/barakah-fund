# Barakah Fund — payment service

A Go service that wraps Stripe to power a **multi-tenant donation platform**. Each
organization collects donations through its own Stripe connected account, passed
to the API as `account_id`. Callers may also send their own `tenant_id`, which is
not used for routing — it rides through Stripe metadata purely so it comes back
in results and webhook notifications. The service configures Stripe and records
outcomes — it never processes payments itself.

See [DESIGN.md](DESIGN.md) for the full end-to-end design and rationale.

## Architecture

Ports and adapters. Application services depend only on the `PaymentGateway`
port, which both the real Stripe adapter and an in-memory mock implement. This
is what keeps the gateway functions tiny (one call, one mapping) and lets the
whole test suite run offline against the mock.

```
cmd/api                     Cloud Run entrypoint ($PORT, graceful shutdown)
internal/money              Money value type (integer minor units)
internal/domain             persisted entities + status enums
internal/payment            PaymentGateway port + DTOs + typed errors
internal/payment/mock       stateful in-memory gateway (integration tests)
internal/payment/stripe     real gateway: net/http against Stripe REST (no SDK)
internal/store              persistence port + in-memory store (Cloud SQL in prod)
internal/email              donor-notification port (+ log + recorder impls)
internal/app                use-case services (donations, cards, subs, links)
internal/webhook            verified-event router (authoritative status)
internal/recon              reconciliation engine (backstop) + fan-out helper
internal/metrics            per-account / per-product dashboard aggregation
internal/httpapi            thin HTTP handlers
```

Status is learned three ways (see design): **webhooks** (authoritative),
**client pull** (UX only), and a **6-hourly reconciliation** backstop.

## Supported operations

Donor: ensure donor (Stripe customer id), add card (SetupIntent), list cards,
remove card. Donations: start one-off custom-amount donation (returns client
secret), refund. Recurring: create / cancel / suspend / resume subscription.
Hosted: create donation payment link (single custom-amount or recurring).
Ops: verify webhook signature, reconcile an account over a window.

## Running the tests

```
go test ./...            # all unit + integration tests (offline, uses the mock)
go test -race ./...      # with the race detector
go test -cover ./...     # coverage
```

Integration tests wire the application services to the in-memory mock gateway and
in-memory store, so no network or Stripe key is required. Parity between the mock
and real Stripe is intended to be guarded by contract tests run against Stripe
test mode (network, run out of band).

## Running locally

```
go run ./cmd/api          # starts on :8080 with the in-memory mock gateway
```

With `STRIPE_SECRET_KEY` set, the real Stripe adapter is used instead.

### HTTP endpoints

| Method + path | Purpose |
|---|---|
| `GET /health` | liveness |
| `POST /v1/donations` | start a custom-amount donation, returns client secret |
| `POST /v1/webhooks/stripe` | receive + verify + apply Stripe events |
| `GET /v1/metrics/accounts/{accountID}/summary?from&to` | dashboard summary |
| `POST /admin/reconcile` | reconcile `{account_id?, from, to}` (backstop / manual) |

## Configuration (environment)

| Var | Default | Meaning |
|---|---|---|
| `PORT` | `8080` | provided by Cloud Run |
| `STRIPE_SECRET_KEY` | — | if set, use real Stripe; else mock |
| `STRIPE_WEBHOOK_SECRET` | — | webhook signing secret |
| `DEFAULT_ACCOUNT_ID` | — | Stripe account used when a request omits `account_id`; empty = platform-direct |
| `REPORTING_CURRENCY` | `USD` | dashboard reporting currency |
| `APPLICATION_FEE_BPS` | `0` | platform fee in basis points (100 = 1%) |

## Deploying to Google Cloud

- **API + webhook + worker**: Cloud Run (this container).
- **Database**: Cloud SQL for PostgreSQL — implement `store.Store` against it and
  swap `store.NewMemory()` in `cmd/api`.
- **Secrets**: Secret Manager (`STRIPE_SECRET_KEY`, `STRIPE_WEBHOOK_SECRET`).
- **Reconciliation loop**: there is no in-process timer. **Cloud Scheduler**
  (every 6h) triggers a dispatcher that publishes one **Pub/Sub** message per
  account; a Cloud Run push subscription runs `recon.Engine.Reconcile` for that
  account. Idempotent upserts make Pub/Sub redelivery and concurrent runs safe.
  The `POST /admin/reconcile` endpoint reuses the same engine for manual
  from/to backfills.

Build the image:

```
docker build -t gcr.io/PROJECT/barakah-payments .
```
