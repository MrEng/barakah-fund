# Barakah Fund — payment service design

A Go service that wraps Stripe to power a **multi-tenant donation platform**. Each tenant is an
independent organization (charity, mosque, campaign) collecting donations. The service is a thin,
well-tested layer in front of Stripe: it configures Stripe and records outcomes — it does not
process payments itself.

---

## 1. Goals and non-goals

**Goals**
- Multi-tenant: many organizations, isolated money and data, one platform.
- Every exported function is as small and single-purpose as possible.
- 100% unit-test coverage of the wrapper functions.
- A stateful in-memory Stripe **mock** so integration tests run with no network.
- Custom donation amounts attached to a product/campaign.
- Donor emailed on both success and failure.
- Status known three ways: webhooks (server truth), client pull (UX), reconciliation (backstop).
- Per-tenant / per-product metrics and a dashboard.

**Non-goals**
- We never store card data (PAN/CVV). Stripe hosts all card entry.
- We never move money or run payment logic ourselves — Stripe authorizes, captures, retries, does SCA.
- No custom fraud engine (rely on Stripe Radar).

---

## 2. Multi-tenancy model — Stripe Connect

- Platform = the Barakah Stripe account.
- Each tenant = a **connected account** (`acct_...`), onboarded with **Express** (Stripe-hosted KYC,
  platform can set branding + email settings). Standard is supported but the tenant controls their
  own settings.
- Every Stripe call on behalf of a tenant carries the `Stripe-Account: acct_...` header. This single
  primitive scopes all customers, cards, payments, and subscriptions to the tenant automatically.
- **Direct charges** on the connected account (donor pays the charity directly; charity is merchant
  of record). Platform revenue via optional `application_fee_amount`.
- A donor's Stripe `Customer` lives inside one connected account. A "client id similar to Stripe" is
  a `cus_...`, meaningful only together with its `acct_...`. We always store the pair.

---

## 3. Architecture — ports and adapters

The seam is one interface, `PaymentGateway`. Two implementations satisfy it: the real `stripe-go`
adapter and the stateful mock. Application services depend only on the interface.

- **Adapter functions are tiny**: map input → attach tenant header + idempotency key → one Stripe
  call → map response. No orchestration, no money branching. A function that does one call and one
  mapping cannot grow — that is the "as small as possible" guarantee.
- **Application services** orchestrate: read/write DB, pick the tenant account, compute application
  fee, validate amount, dedup webhooks. Still no payment processing.
- **HTTP handlers are thin**: decode → call service → encode.

```
/cmd/api
/internal/domain          entities + value objects (Money, TenantID, Donor, Card, Subscription…)
/internal/payment         PaymentGateway interface + shared DTOs        <- the port
/internal/payment/stripe  real adapter (one file per resource)
/internal/payment/mock    stateful in-memory fake
/internal/app             use-case services (orchestration)
/internal/store           repositories (tenant-scoped)
/internal/webhook         signature verify + event router
/internal/recon           reconciliation engine + scheduler
/internal/metrics         business metrics aggregation + dashboard API
/internal/http            handlers
```

### Statelessness and state ownership
- Adapter/gateway functions are **stateless** — each call independent; idempotency key is passed in.
- Application services are stateless per request but read/write Postgres.
- **State of record**: Stripe is authoritative; Postgres is a projection kept in sync by webhooks +
  reconciliation; the mock is stateful and used only in tests.

---

## 4. Products, prices, and custom amounts

- **One Product per campaign/cause** (e.g. "Ramadan water wells").
- **Single donations (custom amount):** one reusable **one-time Price with `custom_unit_amount`
  enabled** (optional min/max/preset) under the product. The donor's amount rides on the
  PaymentIntent. `custom_unit_amount` is a **one-time-price-only** feature.
- **Recurring donations:** `custom_unit_amount` is **not allowed** on recurring prices. Mint a fixed
  recurring Price at the donor's chosen amount (`unit_amount`, `interval=month`) under the **same**
  product; create-or-reuse by amount to avoid price proliferation.
- Products/prices can be created inline (`price_data` + `product_data`) but we prefer **durable**
  per-campaign objects for clean per-tenant reporting.
- Always stamp `tenant_id` and `product_id`/`campaign_id` in Stripe **metadata** so reporting never
  depends on which price was used.

---

## 5. The PaymentGateway port — function catalogue

Every method is one small function implemented by both the real adapter and the mock. All methods
also take `ctx` and the tenant `stripe_account_id`.

### Tenant onboarding (Connect)
| Function | Input | Returns |
|---|---|---|
| `CreateConnectedAccount` | tenant profile | `acct_id` |
| `CreateOnboardingLink` | acct_id, return/refresh URLs | url |
| `GetAccountStatus` | acct_id | charges_enabled, payouts_enabled, details_submitted |

### Customer (donor) — "client with a Stripe-like id"
| `CreateCustomer` | email, name, metadata | `cus_id` |
| `GetCustomer` / `UpdateCustomer` / `DeleteCustomer` | cus_id | — |

### Cards — add / remove / list
| `CreateSetupIntent` | cus_id | `seti_id`, client_secret — "add card", finished on Stripe.js off-session |
| `ListPaymentMethods` | cus_id | []Card — "list cards" |
| `DetachPaymentMethod` | pm_id | — — "remove card" |
| `SetDefaultPaymentMethod` | cus_id, pm_id | — |

### Payments — start / status / refund
| `CreatePaymentIntent` | amount(Money), cus_id, pm_id?, application_fee?, receipt_email, metadata | `pi_id`, client_secret, status — "start payment" |
| `ConfirmPaymentIntent` | pi_id | server-side confirm for saved cards |
| `GetPaymentIntent` | pi_id | status — "get status" |
| `CancelPaymentIntent` | pi_id | — |
| `CreateRefund` | pi_id, amount? | `re_id` |

### Products and prices
| `CreateProduct` | name, metadata | `prod_id` |
| `CreatePrice` | prod_id, amount or custom_unit_amount, interval? | `price_id` |

### Subscriptions — create / cancel / suspend / resume
| `CreateSubscription` | cus_id, price_id, pm_id, metadata | `sub_id`, status, latest_invoice client_secret |
| `CancelSubscription` | sub_id, at_period_end bool | — |
| `PauseSubscription` | sub_id | — — "suspend" (`pause_collection`) |
| `ResumeSubscription` | sub_id | — |
| `UpdateSubscription` | sub_id, new price_id | change amount |
| `GetSubscription` / `ListSubscriptions` | — | — |

### Payment links
| `CreatePaymentLink` | price_id (or amount), mode: payment\|subscription, metadata | url |
| `DeactivatePaymentLink` | link_id | — |

### Webhooks
| `VerifyWebhookSignature` | payload, sig, secret | Event |

### Reconciliation (read side)
| `ListBalanceTransactions` | from, to, cursor | []BalanceTxn (money truth) |
| `ListPaymentIntents` | from, to, cursor, status | []PaymentIntent (catches failures) |
| `ListInvoices` | from, to, cursor | []Invoice (subscription charges) |

### Parameter completeness (params that must not be forgotten)
The table above lists the essential inputs; these additional params are required for correctness,
compliance, or good UX and are easy to omit:

- **Every write**: explicit `idempotency_key`, `currency`, and `description`. List calls return
  `next_cursor` + `has_more` (pagination is not optional at scale).
- **`CreatePaymentIntent`**: `statement_descriptor_suffix` (what the donor sees on their card
  statement — cuts disputes/chargebacks), `setup_future_usage=off_session` (to reuse a one-off card
  for later recurring), `automatic_payment_methods` (wallets/local methods), `application_fee_amount`
  (computed **server-side**, never from the client), `payment_method_types`.
- **`CreateSubscription`**: `payment_behavior=default_incomplete` + expand `latest_invoice.
  payment_intent` (required for SCA on the first charge), `proration_behavior`,
  `application_fee_percent`, optional `trial`/`cancel_at_period_end`.
- **`CreatePaymentLink`**: `application_fee_percent` (platform fee on Connect), `after_completion`
  (redirect — allowlist the URL, see §16), optional `custom_fields` (dedication), `restrictions`,
  tax behavior.
- **`CreateConnectedAccount`**: account `type`, `country`, `email`, requested `capabilities`
  (`card_payments`, `transfers`), `business_type`, `tos_acceptance`, and `settings` (email + branding
  + statement descriptor).
- **`CreateCustomer`**: `address` + `country` (needed for receipts, tax, and some SCA rules), `phone`.
- **`CreateRefund`**: `reason`, `refund_application_fee`, `reverse_transfer` (platform-fee handling).
- **`CreateSetupIntent`**: `usage=off_session`, `payment_method_types`.
- **`VerifyWebhookSignature`**: per-endpoint signing secret from Secret Manager + timestamp tolerance
  (replay window); for Connect, validate `event.account` maps to a known tenant.

---

## 6. Payment flows (end to end)

### A. One-off donation, custom amount
1. Client asks server to start a donation to a product.
2. Server `CreatePaymentIntent(amount, cus_id, receipt_email, metadata{tenant,product})`.
3. Server returns `client_secret`; client confirms with Stripe.js (direct to Stripe, handles SCA).
4. Success/failure recorded via webhook; donor emailed (see §8); client polls for its own UX.

### B. Recurring donation, custom amount
1. Donor chooses monthly amount up front.
2. Server create-or-reuses a recurring Price at that amount under the product, then
   `CreateSubscription`.
3. Stripe charges monthly off-session; each renewal recorded via webhook, donor emailed by Stripe.

### C. Payment link (hosted, no further API interaction)
- Single: one-time `custom_unit_amount` price → `CreatePaymentLink(mode=payment)`. Donor types the
  amount on Stripe's page. No client secret; status comes via webhook + success redirect.
- Recurring: amount fixed at link-creation time (custom recurring amount is not possible on a static
  link). For donor-typed recurring amount use a Checkout Session instead.

---

## 7. Status handling — three layers

| Layer | Role | Trust |
|---|---|---|
| Webhooks | Primary, near-real-time truth → writes DB, ledger, metrics | authoritative |
| Client pull (`retrievePaymentIntent(client_secret)`) | Donor's success screen only | UX only, never fulfills |
| 6-hour reconciliation | Backstop for anything webhooks missed | authoritative correction |

- Client pull is direct to Stripe with the publishable key; it updates the browser, not the DB.
- Client pull is impossible for subscription renewals (no browser) and payment links (no client
  secret) — those rely on webhooks + reconciliation.
- Webhook router handles: `payment_intent.succeeded/payment_failed`, `invoice.paid/payment_failed`,
  `charge.refunded`, `charge.dispute.created`, `customer.subscription.updated/deleted`,
  `account.updated`. Dedup by `stripe_event_id` (unique).

---

## 8. Emails — always on success and failure

Stripe's automatic emails do **not** cover one-off failures, so failure email is partly ours:

| Event | Source |
|---|---|
| One-off success | Stripe receipt (enabled per tenant, `receipt_email` always set) |
| One-off failure | **App-sent**, triggered by `payment_intent.payment_failed` webhook |
| Subscription success | Stripe receipt |
| Subscription failure | Stripe dunning (enabled per tenant) |

- "Always enable Stripe emails" is a **per-tenant onboarding step**: set the connected account's
  email settings during `CreateConnectedAccount` (Express) or prompt Standard tenants to enable.
- Always populate `receipt_email` (or a customer with email) on every charge, or nothing sends.
- Branding note: success emails are tenant-branded (Stripe); one-off failure emails are
  Barakah-branded (app). Accept the inconsistency, or own all four emails for one voice.
- Failed payments never hit `balance_transactions`; to backstop failure emails, reconciliation also
  runs a failed-PaymentIntent pass (§9).

---

## 9. Reconciliation

Purpose: guarantee the DB has a row for **every real transaction**, even if a webhook was dropped,
the endpoint was down, a tenant edited data in the Stripe dashboard, or an unsubscribed event fired.

- **Reconcile against Balance Transactions** — Stripe's authoritative ledger of everything that
  touched the account balance (charges, refunds, disputes, fees, payouts). "No transaction missing"
  = every money-movement balance transaction has a matching local ledger row.
- **Plus a failed-PaymentIntent pass** in the window (failures don't appear in balance transactions)
  to backstop failure emails and failure metrics.

### One engine, two callers
Core: `Reconcile(ctx, accountScope, from, to) -> Report{scanned, backfilled, updated, flagged}`.
Idempotent upsert by Stripe id; converges with whatever webhooks already wrote.

- **Scheduled (every 6h):** calls `Reconcile(now-24h, now)` for each tenant. The 24h window on a 6h
  cadence gives 4× overlap so a failed run or late-settling transaction is caught by later runs.
- **External API:** `POST /admin/reconcile { tenant_id?, from, to }` — arbitrary range for backfills,
  investigations, or importing a tenant's history. **Async**: returns a job id, runs in background,
  produces the same `Report`. Admin-auth only. Cap absurd ranges; auto-page results.

**Concurrency:** reconciling the same tenant concurrently is **allowed** — no per-tenant lock. Safe
because all writes are idempotent upserts keyed by the Stripe object / balance-transaction id, so a
scheduled run and a manual run converge to the same rows. (Rate-limit pressure is the only cost;
accepted.)

### Discrepancy handling
- Missing locally → **backfill**, tag `source=reconciliation`.
- Local stale vs Stripe → **update to Stripe** (Stripe wins).
- Local exists, Stripe doesn't → **flag for review**, never blind-delete.
- Amount/currency mismatch → **alert** (indicates a bug).

Health signal: in steady state the sweep finds ~nothing. Rising `source=reconciliation` backfills
mean the webhook path is degrading — emit as a metric and alert.

---

## 10. Metrics and dashboard

Two distinct metric surfaces.

### Operational metrics (OTel → Cloud Monitoring, for engineers)
- Webhook: received count, signature-verification failures, dedup hits, handler error rate,
  processing latency (p50/p95/p99), and **event lag** (Stripe `created` → processed).
- Reconciliation: run duration per tenant, runs failed, missed schedule, backfills by tenant
  (`source=reconciliation`), flagged discrepancies, amount-mismatch count.
- Stripe API: latency + error rate by resource and error type; **429 rate-limit** count; idempotency
  replay count.
- Email: send success/failure per event type; bounce/complaint rate (from the email provider);
  **failure-email send lag** (payment failed → email sent).
- Infra (GCP): Cloud Run request latency/5xx/instance count/cold starts/CPU/mem; Cloud SQL connection
  pool saturation + query latency; Pub/Sub backlog (unacked count, oldest-unacked age) + dead-letter
  count; Cloud Scheduler job success/failure.

### Business metrics + dashboard (for tenants and admins)
Sourced by **aggregating the payments/subscriptions/ledger projection** (kept correct by webhooks +
reconciliation), not from counters — so results are historically queryable, range-filterable, and
correct after reconciliation. This requires storing **every attempt**, including failed
PaymentIntents (from the failure webhook + reconciliation failed pass), each with amount + status.

Core definitions:
- **Requested** = sum of amounts of all attempts (succeeded + failed + pending).
- **Captured** = sum of succeeded amounts.
- **Failed** = count/amount of failed attempts.
- **Refunded** = sum of refunds (from balance transactions).
- **Success rate** = succeeded / attempted.

Additional metrics (per tenant, per product, over a range):
- **Donors**: unique donors, new vs returning, average and median donation amount.
- **Recurring**: active subscriptions, monthly recurring donations (MRR-equivalent), new vs canceled
  vs paused, voluntary churn rate, **involuntary churn** (failed renewals never recovered), and
  **dunning recovery rate** (failed renewals later paid).
- **Risk**: **dispute/chargeback rate** (disputes ÷ charges — watch Stripe's ~0.75% threshold) and
  **refund rate**. These protect the tenant's account standing.
- **Platform revenue**: application fees collected per tenant.
- **Payouts**: amount paid out per tenant and next scheduled payout (from balance/payout objects).
- **Mix**: payment-method mix (card/wallet/bank), decline-reason breakdown, currency breakdown.
- **Conversion**: PaymentIntents created vs succeeded, abandoned (created, never confirmed), and for
  links, links created vs paid.
- **Onboarding funnel** (admin): connected accounts created vs `charges_enabled`, and time-to-first
  charge / accounts stuck not-chargeable.

Dashboard API (read-only, aggregation over the projection):

| Endpoint | Returns |
|---|---|
| `GET /metrics/tenants/{tenant_id}/summary?from&to` | success_count, failure_count, success_rate, amount_requested, amount_captured, refunded, per-currency breakdown |
| `GET /metrics/tenants/{tenant_id}/products?from&to` | the same, grouped per product/campaign |
| `GET /metrics/products/{product_id}?from&to` | one product across time |
| `GET /metrics/tenants/{tenant_id}/timeseries?from&to&bucket=day` | requested/captured/failed per bucket for charts |
| `GET /metrics/overview?from&to` | platform-wide totals + top tenants (admin only) |

Dashboard UI: a per-tenant view (success vs failure, requested vs captured, top campaigns, trend
line) and an admin overview across all tenants. It only reads the endpoints above — no direct DB or
Stripe access.

Auth: tenant-scoped tokens see only their tenant; admin tokens see the overview.

### Alerts (Cloud Monitoring alerting policies → email/Slack/PagerDuty)

| Alert | Condition | Why it matters |
|---|---|---|
| Webhook signature failures | any sustained > 0 | rotated/misconfigured secret or forged calls |
| Webhook error rate / lag | error rate high or event lag > threshold | server truth is falling behind |
| Payments succeeded drop-to-zero | no successes for N min (per tenant / platform) | silent outage — absence alert |
| Reconciliation backfills spike | `source=reconciliation` count rising | the webhook path is degrading |
| Reconciliation discrepancy | any amount/currency mismatch flagged | a bug — money doesn't match Stripe |
| Reconciliation didn't run | expected 6h run missing (Scheduler failure) | backstop is down |
| Dispute rate near limit | rate approaching ~0.75% | Stripe may restrict the tenant's account |
| Refund / failed-renewal spike | rate above baseline | fraud, churn, or a broken flow |
| Failure-email not sent | failure events without a matching send | donor never told their payment failed |
| Stripe API 429 / error spike | rate-limit or 5xx rate high | throttling or Stripe incident |
| Email bounce/complaint rate | above provider threshold | deliverability/reputation at risk |
| Pub/Sub backlog / dead-letter | oldest-unacked age high, or DLQ > 0 | reconciliation fan-out is stuck |
| Cloud SQL connections saturated | pool near max | Cloud Run scale-out exhausting DB conns |
| Cloud Run 5xx / latency SLO burn | error-budget burn rate high | API availability regressing |
| Onboarding stuck | account not `charges_enabled` for > N days | tenant can't collect donations |

Define SLOs (webhook-processing success, API availability) with **error-budget burn-rate** alerts
rather than only static thresholds, so paging tracks user impact.

---

## 11. Deployment on Google Cloud

Target shape (managed, stateless, autoscaling — matches the stateless-function design):

| Component | GCP service |
|---|---|
| API + webhook endpoint | **Cloud Run** service (container, autoscaled) |
| Database | **Cloud SQL for PostgreSQL** (via the Cloud SQL Go connector) |
| Secrets (Stripe keys, webhook signing secrets) | **Secret Manager** |
| Scheduled reconciliation trigger | **Cloud Scheduler** (managed cron) |
| Reconciliation fan-out | **Pub/Sub** (one message per tenant) |
| Reconciliation worker | **Cloud Run** (push subscription) or a **Cloud Run Job** |
| Metrics / logs / traces / alerts | Cloud Monitoring, Cloud Logging, Cloud Trace |
| Container images | Artifact Registry; deploy via Cloud Build |

Notes:
- **DB connections**: Cloud Run scales to many instances; cap the per-instance pool and/or front
  Cloud SQL with a pooler so total connections stay under the instance limit.
- **Min instances** on the webhook service to avoid cold-start latency on Stripe deliveries; the
  webhook handler must **ack fast** (verify signature, dedup, enqueue heavy work) and stay idempotent.

### Running the reconciliation loop on Cloud Run

There is no long-lived process on Cloud Run to host a `time.Ticker`, and a container can be scaled to
zero — so the "loop" is **externally triggered**, not an in-process timer. Recommended pattern
(fan-out, which suits multi-tenant + concurrent-reconciliation-allowed):

1. **Cloud Scheduler** fires every 6h and hits an authenticated **dispatcher** endpoint (OIDC token)
   or publishes a "start reconciliation" message.
2. The **dispatcher** enumerates tenants from the `tenants` table and **publishes one Pub/Sub message
   per tenant** (`{tenant_id, from=now-24h, to=now}`).
3. A **Cloud Run push subscription** invokes the worker once per message; the worker runs
   `Reconcile(tenant, from, to)`. Tenants process **in parallel**, each independently retried.
4. Pub/Sub **at-least-once delivery** may re-invoke a tenant — harmless, because reconciliation writes
   are idempotent upserts (and concurrent same-tenant runs are explicitly allowed). Configure a
   **dead-letter topic** so a tenant that keeps failing is captured and alerted, not silently dropped.

The **external from/to reconcile API** reuses the exact same worker: it validates the range, then
publishes the same per-tenant messages with the supplied `from`/`to` (async), returns a `job_id`, and
writes results to `recon_runs`. So scheduled and manual reconciliation share one code path and one
Pub/Sub pipeline.

Alternative if you prefer no Pub/Sub: **Cloud Scheduler → Cloud Run Job** with task sharding (each
task index handles a slice of tenants). Simpler wiring, but less granular retry/observability than
per-tenant messages. Either way, the schedule lives in Cloud Scheduler, never in app code.

---

## 12. Data model (tenant-scoped)

`tenant_id` on every table; repositories enforce it on every query.

- `tenants` (stripe_account_id, charges_enabled, payouts_enabled, email_settings_ok, status)
- `donors` (tenant_id, stripe_customer_id, email, name?, address?, country?, marketing_consent,
  anonymized_at?, unique(tenant_id, email))
- `cards` (donor_id, stripe_pm_id, brand, last4, exp, is_default)
- `products` (tenant_id, stripe_product_id, name, campaign_id)
- `prices` (product_id, stripe_price_id, amount_minor, currency, interval, is_custom_amount,
  min_minor?, max_minor?)
- `payments` (tenant_id, donor_id, product_id, subscription_id?, stripe_payment_intent_id,
  stripe_charge_id?, stripe_balance_txn_id?, amount_minor, currency, status[requested|succeeded|
  failed|canceled], failure_code?, payment_method_type?, application_fee_minor, source[webhook|
  reconciliation], metadata) — **every attempt, including failures**, feeds metrics
- `subscriptions` (tenant_id, donor_id, product_id, stripe_subscription_id, price_id, status,
  current_period_end, cancel_at?, pause_reason?)
- `payment_links` (tenant_id, stripe_link_id, url, mode, active)
- `refunds` (payment_id, stripe_refund_id, amount_minor, reason?)
- `disputes` (payment_id, stripe_dispute_id, status, amount_minor, evidence_due_by?)
- `ledger_entries` (tenant_id, stripe_balance_txn_id UNIQUE, type, amount_minor, currency, fee_minor)
- `webhook_events` (stripe_event_id UNIQUE, type, processed_at) — dedup
- `outbox` (id, aggregate, payload, status, attempts) — transactional outbox for emails/events
- `email_log` (tenant_id, donor_id, payment_id?, type[success|failure|dunning], provider_id, status,
  sent_at) — powers the "failure-email-not-sent" alert
- `audit_log` (actor, role, action, target, tenant_id, at) — admin actions (refund, reconcile, access)
- `metric_rollups_daily` (tenant_id, product_id, day, requested_minor, captured_minor, failed_minor,
  refunded_minor, counts…) — precomputed dashboard aggregates (see §17)
- `idempotency_keys` (key, request_hash, response, expires_at)
- `recon_runs` (tenant_id, from, to, scanned, backfilled, updated, flagged, job_id, started_at,
  finished_at)

Every table carries `created_at` / `updated_at`. Money is always `int64` minor units + currency code.
Never float.

---

## 13. The Stripe mock (integration tests)

- A struct of maps keyed by id, guarded by a mutex, namespaced per connected account. Satisfies the
  same `PaymentGateway` interface, so services can't tell it apart.
- **Stripe-like ids**: generate `cus_…`, `pi_…`, `sub_…`, `seti_…`, `pm_…`, `plink_…`, `acct_…`,
  `re_…`, `prod_…`, `price_…` with random suffixes.
- **Keeps payments**: `CreatePaymentIntent` stores it (`requires_confirmation`); `Confirm` →
  `succeeded`; `GetPaymentIntent` returns the stored status — exactly "do a payment, keep it, and
  status queries return the same status Stripe would."
- **State machines**: PaymentIntent (`requires_confirmation → requires_action? → succeeded|canceled`),
  Subscription (`active → paused → active|canceled`), cards (attach/detach/list).
- **Test control surface** (mock-only, not on the port): `SetNextOutcome(decline|requires_action|
  success)`, `EmitWebhook(event)`, injectable clock for renewal/expiry tests.
- List endpoints (`ListBalanceTransactions`, `ListPaymentIntents`, `ListInvoices`) return stored
  objects filtered by range, so the reconciliation engine can be integration-tested against it.

---

## 14. Testing strategy

- **Unit, real adapter**: stub `stripe-go`'s HTTP backend with canned JSON; assert each tiny function
  builds the right params and maps the response. Table-driven, one behavior each. Target 100%.
- **Unit, mock**: test its state transitions.
- **Integration**: application services wired to the mock + sqlite/in-memory repo; exercise whole use
  cases (start payment → emit `payment_intent.succeeded` → status flips → ledger + metrics written).
- **Contract**: one shared suite run against both the mock and Stripe **test mode** (test-mode tagged/
  skipped in normal CI, run nightly) to keep the mock in parity with reality.
- **Idempotency**: same request id → one Stripe call, one row; concurrent reconciliation → convergent
  rows.

---

## 15. Cross-cutting

- **Idempotency key** on every Stripe write (`Stripe-Idempotency-Key`), derived from our request id.
- **Money**: `int64` minor units + currency value object; never float.
- **Errors**: map Stripe `card_error` / `rate_limit` / `api_error` to typed domain errors.
- **Webhook routing (Connect)**: one platform endpoint; events carry `account`; route to tenant.
- **Rate limits**: auto-page (`limit=100`), backoff, stagger/shard reconciliation across tenants.
- **Security**: tenant-scoped auth tokens; admin scope for reconcile + overview; secrets per env.
- **Observability**: structured logs + the operational metrics in §10.

---

## 16. Security and privacy

### Tenant isolation (the top risk)
This is a multi-tenant money system; a cross-tenant leak is the worst-case bug.
- Auth tokens carry `tenant_id` + `role` (donor / tenant-admin / platform-admin). The
  `Stripe-Account` header and every DB query derive the tenant from the **token**, never from a
  client-supplied path/body value.
- **IDOR guard**: dashboard/payment endpoints take `{tenant_id}` in the path — the middleware must
  verify the caller owns that tenant and reject mismatches. Same for donor-owned objects (a donor may
  only touch their own `cus_`/cards/payments).
- `reconcile` and `/metrics/overview` are **platform-admin only**.

### Trust boundaries and endpoint auth
- **Webhook endpoint** (public): verify Stripe signature with the per-endpoint secret + timestamp
  tolerance (replay defense), confirm `event.account` is a known tenant, ack fast, process
  idempotently. Reject unverified payloads before any work.
- **Cloud Scheduler → dispatcher** and **Pub/Sub → worker** must authenticate with **OIDC tokens**;
  the worker verifies the push token so nothing else can trigger reconciliation.
- **Redirect URLs** (onboarding return/refresh, payment-link `after_completion`) must be validated
  against an **allowlist** — prevents open-redirect/SSRF.
- CORS locked to known origins; CSRF protection on browser state-changing routes.

### Payment-specific abuse
- Public custom-amount donation endpoints/links are **card-testing magnets** (fraudsters validate
  stolen cards with small donations). Mitigate with per-IP/per-donor **rate limiting**, CAPTCHA on
  public donate endpoints, Stripe Radar rules, and min/max amount validation.
- **Amount tampering**: amount is client-supplied for custom donations — validate `> 0`, within
  min/max, integer (minor units, no overflow), correct currency, and that the `product` belongs to
  the tenant. Application fee is always computed server-side.
- **Double-charge**: accept a client idempotency key on "start payment" so client retries don't
  create duplicate PaymentIntents.

### Secrets
- Stripe secret keys + webhook signing secrets in **Secret Manager**, least-privilege service
  accounts (separate SA for webhook vs worker), rotation policy. Never in env dumps, images, or logs.
- Publishable key only on the client; client uses `client_secret` scoped to a single PaymentIntent —
  never expose the secret key or another tenant's context.

### Privacy / PII (donation data is sensitive)
- **Special-category data**: donations to a religious organization can reveal religious belief →
  treated as special-category personal data (GDPR Art. 9). Minimize what's stored, restrict access,
  and capture a lawful basis/consent for anything beyond the transaction itself.
- **Data minimization**: never store PAN/CVV (Stripe hosts entry); store Stripe ids + only the PII we
  need (email, name, optional address for receipts). Prefer fetching from Stripe over duplicating.
- **Right to erasure / DSAR**: support deleting/anonymizing a donor on request — but financial records
  must be retained for tax/audit, so **anonymize the donor while keeping amounts/ledger** rather than
  hard-deleting. Define a retention policy.
- **Consent**: transactional emails (receipts) are fine; marketing needs explicit, revocable consent —
  store the flag.
- **Encryption**: TLS in transit; Cloud SQL encryption at rest (CMEK optional). Choose the DB
  **region** for data residency (EU donors → EU region).
- **Log hygiene**: redact `client_secret`, tokens, `Authorization`, emails, and any PII from logs and
  traces. Add an **audit log** for admin actions (refunds, reconcile, data access).

---

## 17. Autoscaling and performance

The design is stateless by construction, which makes horizontal autoscale on Cloud Run natural — but
four things decide whether it *actually* scales cleanly:

- **Stateless instances**: no in-memory session, sticky routing, or local-disk state; all state in
  Postgres/Stripe. JWT auth (no server session store). The mock is the only stateful piece and is
  test-only. Handle `SIGTERM` for graceful drain on instance recycling.
- **DB connections are the real scaling ceiling** (the classic Cloud Run + Cloud SQL failure): N
  instances × pool size can blow past `max_connections`. Mitigate with a **small per-instance pool**,
  a **connection pooler** (PgBouncer / AlloyDB), and a **max-instances** cap sized to the DB. This is
  the #1 autoscaling risk, not CPU.
- **Spike absorption**: Stripe can burst webhooks. The handler **acks fast and enqueues** (transactional
  outbox → Pub/Sub) so processing scales independently of delivery spikes. Emails also go through the
  outbox/worker so slow SMTP never blocks request threads.
- **Stripe rate limits bound the workers**: autoscaling reconciliation workers can hit Stripe **429s**
  (per-account and per-platform limits). Add a **global token-bucket / concurrency cap** and backoff —
  scaling out past Stripe's limit just produces throttling. Chunk very large tenants by time/cursor so
  one Pub/Sub message never does unbounded work (noisy-neighbor fairness).
- **Dashboard reads must not fight the write path**: aggregating `payments` per request gets expensive
  under load. Precompute **daily rollup tables** (feeds the timeseries endpoint) and/or serve metrics
  from a **read replica**, so donation traffic and dashboard queries scale separately.
- **Idempotency everywhere** makes at-least-once delivery, retries, and concurrent instances safe —
  already required by the reconciliation design.
- **Min instances** on latency-sensitive services (webhook, start-payment) to avoid cold-start delays
  that would make Stripe retry deliveries.

---

## 18. Decisions log

- Multi-tenant via Stripe Connect, direct charges, one connected account per tenant.
- Custom amount always, attached to a product: one-time `custom_unit_amount` price for single gifts;
  fixed recurring price per amount for monthly.
- Functions are thin stateless forwarders; Stripe does all processing.
- Status: webhooks = truth; client pulls for UX; 6h reconciliation (24h window) backstop; external
  from/to reconcile API; concurrent reconciliation of the same tenant is allowed (idempotent upserts).
- Emails always on: Stripe success receipts + subscription dunning enabled per tenant; app sends the
  one-off failure email on `payment_intent.payment_failed`; `receipt_email` always set.
- Metrics per tenant and per product (requested / captured / failed / refunded / success rate, plus
  donors, recurring/churn, dispute + refund rate, payouts, method mix, conversion, onboarding funnel)
  served from the DB projection; operational metrics via OTel → Cloud Monitoring; alerting policies
  with SLO burn-rate; a read-only dashboard API + UI.
- Stateful in-memory mock mirrors the port for integration + contract tests.
- Deploys to Google Cloud: Cloud Run (API + webhook + worker), Cloud SQL, Secret Manager. The
  reconciliation loop is **externally triggered by Cloud Scheduler** (no in-process timer) and fanned
  out over **Pub/Sub**, one message per tenant; the external from/to API reuses the same pipeline.
- Security: token-derived tenant scoping (no trust in path params), OIDC on Scheduler/Pub/Sub,
  webhook signature + replay check, redirect allowlist, card-testing rate limits, server-side amount +
  fee validation, secrets in Secret Manager. Privacy: donation data treated as GDPR special-category —
  data minimization, anonymize-on-erasure while keeping the ledger, consent flag, log redaction, audit
  log, region-based residency.
- Autoscaling: stateless instances behind a DB **connection pooler** with max-instances cap;
  fast-ack webhooks + outbox → Pub/Sub for spikes; a **global Stripe rate-limit cap** on workers;
  dashboard served from **daily rollups / read replica** so reads don't fight the write path.

---

## 19. Donation use-cases to consider next (not yet in scope)

Must-have before real launch: tax/annual giving statements, dispute evidence handling, failed-renewal
"update your card" flow, per-tenant payout/balance view, internal ledger reconciled to Stripe.

Donation-specific: "cover the processing fee" option, anonymous/guest donations, campaigns/funds with
goal-vs-raised progress, dedications ("in memory of"), Gift Aid / Zakat vs Sadaqah categorization,
wallets + local methods (Apple/Google Pay, ACH/SEPA), Stripe billing customer portal for
self-service, min/max amount + velocity guards, multi-currency per campaign, per-tenant exports.
