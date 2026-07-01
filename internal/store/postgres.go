package store

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/barakahfund/payments/internal/domain"
	"github.com/barakahfund/payments/internal/money"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Postgres is a Cloud SQL / PostgreSQL-backed Store.
type Postgres struct {
	pool *pgxpool.Pool
}

// NewPostgres connects to Postgres using a DSN (Cloud SQL unix socket or TCP).
func NewPostgres(ctx context.Context, dsn string) (*Postgres, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, err
	}
	cfg.MaxConns = 5 // conservative for Cloud Run fan-out + small instances
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return &Postgres{pool: pool}, nil
}

// Close releases the connection pool.
func (p *Postgres) Close() { p.pool.Close() }

// Migrate creates tables if they do not exist.
func (p *Postgres) Migrate(ctx context.Context) error {
	_, err := p.pool.Exec(ctx, schemaSQL)
	return err
}

const schemaSQL = `
CREATE TABLE IF NOT EXISTS tenants (
	id TEXT PRIMARY KEY,
	name TEXT NOT NULL DEFAULT '',
	stripe_account_id TEXT NOT NULL DEFAULT '',
	charges_enabled BOOLEAN NOT NULL DEFAULT false,
	payouts_enabled BOOLEAN NOT NULL DEFAULT false
);
CREATE TABLE IF NOT EXISTS donors (
	tenant_id TEXT NOT NULL,
	id TEXT NOT NULL,
	email TEXT NOT NULL DEFAULT '',
	name TEXT NOT NULL DEFAULT '',
	stripe_customer_id TEXT NOT NULL DEFAULT '',
	PRIMARY KEY (tenant_id, id)
);
CREATE INDEX IF NOT EXISTS donors_email_idx ON donors (tenant_id, email);
CREATE TABLE IF NOT EXISTS payments (
	tenant_id TEXT NOT NULL,
	stripe_payment_intent_id TEXT NOT NULL,
	donor_id TEXT NOT NULL DEFAULT '',
	product_id TEXT NOT NULL DEFAULT '',
	amount_minor BIGINT NOT NULL DEFAULT 0,
	currency TEXT NOT NULL DEFAULT '',
	status TEXT NOT NULL DEFAULT '',
	application_fee_minor BIGINT NOT NULL DEFAULT 0,
	source TEXT NOT NULL DEFAULT '',
	failure_reason TEXT NOT NULL DEFAULT '',
	created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
	updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
	PRIMARY KEY (tenant_id, stripe_payment_intent_id)
);
CREATE INDEX IF NOT EXISTS payments_product_idx ON payments (tenant_id, product_id);
CREATE TABLE IF NOT EXISTS subscriptions (
	tenant_id TEXT NOT NULL,
	stripe_subscription_id TEXT NOT NULL,
	donor_id TEXT NOT NULL DEFAULT '',
	product_id TEXT NOT NULL DEFAULT '',
	price_id TEXT NOT NULL DEFAULT '',
	status TEXT NOT NULL DEFAULT '',
	current_period_end TIMESTAMPTZ NOT NULL DEFAULT now(),
	PRIMARY KEY (tenant_id, stripe_subscription_id)
);
CREATE TABLE IF NOT EXISTS ledger_entries (
	stripe_balance_txn_id TEXT PRIMARY KEY,
	tenant_id TEXT NOT NULL,
	type TEXT NOT NULL DEFAULT '',
	amount_minor BIGINT NOT NULL DEFAULT 0,
	currency TEXT NOT NULL DEFAULT '',
	fee_minor BIGINT NOT NULL DEFAULT 0,
	created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE TABLE IF NOT EXISTS webhook_events (
	stripe_event_id TEXT PRIMARY KEY,
	processed_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE TABLE IF NOT EXISTS idempotency_keys (
	key TEXT PRIMARY KEY,
	payment_intent_id TEXT NOT NULL,
	created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
`

// --- tenants ---

func (p *Postgres) SaveTenant(ctx context.Context, t domain.Tenant) error {
	_, err := p.pool.Exec(ctx, `
		INSERT INTO tenants (id, name, stripe_account_id, charges_enabled, payouts_enabled)
		VALUES ($1,$2,$3,$4,$5)
		ON CONFLICT (id) DO UPDATE SET
			name=EXCLUDED.name, stripe_account_id=EXCLUDED.stripe_account_id,
			charges_enabled=EXCLUDED.charges_enabled, payouts_enabled=EXCLUDED.payouts_enabled`,
		t.ID, t.Name, t.StripeAccountID, t.ChargesEnabled, t.PayoutsEnabled)
	return err
}

func (p *Postgres) GetTenant(ctx context.Context, id string) (domain.Tenant, error) {
	var t domain.Tenant
	err := p.pool.QueryRow(ctx,
		`SELECT id, name, stripe_account_id, charges_enabled, payouts_enabled FROM tenants WHERE id=$1`, id).
		Scan(&t.ID, &t.Name, &t.StripeAccountID, &t.ChargesEnabled, &t.PayoutsEnabled)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Tenant{}, ErrNotFound
	}
	return t, err
}

func (p *Postgres) ListTenants(ctx context.Context) ([]domain.Tenant, error) {
	rows, err := p.pool.Query(ctx,
		`SELECT id, name, stripe_account_id, charges_enabled, payouts_enabled FROM tenants`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.Tenant
	for rows.Next() {
		var t domain.Tenant
		if err := rows.Scan(&t.ID, &t.Name, &t.StripeAccountID, &t.ChargesEnabled, &t.PayoutsEnabled); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// --- donors ---

func (p *Postgres) SaveDonor(ctx context.Context, d domain.Donor) error {
	_, err := p.pool.Exec(ctx, `
		INSERT INTO donors (tenant_id, id, email, name, stripe_customer_id)
		VALUES ($1,$2,$3,$4,$5)
		ON CONFLICT (tenant_id, id) DO UPDATE SET
			email=EXCLUDED.email, name=EXCLUDED.name, stripe_customer_id=EXCLUDED.stripe_customer_id`,
		d.TenantID, d.ID, d.Email, d.Name, d.StripeCustomerID)
	return err
}

func (p *Postgres) GetDonor(ctx context.Context, tenantID, donorID string) (domain.Donor, error) {
	return p.scanDonor(ctx,
		`SELECT tenant_id, id, email, name, stripe_customer_id FROM donors WHERE tenant_id=$1 AND id=$2`,
		tenantID, donorID)
}

func (p *Postgres) FindDonorByEmail(ctx context.Context, tenantID, email string) (domain.Donor, error) {
	return p.scanDonor(ctx,
		`SELECT tenant_id, id, email, name, stripe_customer_id FROM donors WHERE tenant_id=$1 AND email=$2 LIMIT 1`,
		tenantID, email)
}

func (p *Postgres) scanDonor(ctx context.Context, q string, args ...any) (domain.Donor, error) {
	var d domain.Donor
	err := p.pool.QueryRow(ctx, q, args...).Scan(&d.TenantID, &d.ID, &d.Email, &d.Name, &d.StripeCustomerID)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Donor{}, ErrNotFound
	}
	return d, err
}

// --- payments ---

func (p *Postgres) UpsertPayment(ctx context.Context, pm domain.Payment) error {
	_, err := p.pool.Exec(ctx, `
		INSERT INTO payments (tenant_id, stripe_payment_intent_id, donor_id, product_id,
			amount_minor, currency, status, application_fee_minor, source, failure_reason, created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)
		ON CONFLICT (tenant_id, stripe_payment_intent_id) DO UPDATE SET
			donor_id=EXCLUDED.donor_id, product_id=EXCLUDED.product_id, amount_minor=EXCLUDED.amount_minor,
			currency=EXCLUDED.currency, status=EXCLUDED.status, application_fee_minor=EXCLUDED.application_fee_minor,
			source=EXCLUDED.source, failure_reason=EXCLUDED.failure_reason, updated_at=EXCLUDED.updated_at`,
		pm.TenantID, pm.StripePaymentIntentID, pm.DonorID, pm.ProductID, pm.Amount.Amount, pm.Amount.Currency,
		string(pm.Status), pm.ApplicationFee.Amount, string(pm.Source), pm.FailureReason,
		nz(pm.CreatedAt), nz(pm.UpdatedAt))
	return err
}

func (p *Postgres) GetPayment(ctx context.Context, tenantID, piID string) (domain.Payment, error) {
	row := p.pool.QueryRow(ctx, `
		SELECT tenant_id, stripe_payment_intent_id, donor_id, product_id, amount_minor, currency,
			status, application_fee_minor, source, failure_reason, created_at, updated_at
		FROM payments WHERE tenant_id=$1 AND stripe_payment_intent_id=$2`, tenantID, piID)
	pm, err := scanPayment(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Payment{}, ErrNotFound
	}
	return pm, err
}

func (p *Postgres) ListPayments(ctx context.Context, f PaymentFilter) ([]domain.Payment, error) {
	q := `SELECT tenant_id, stripe_payment_intent_id, donor_id, product_id, amount_minor, currency,
			status, application_fee_minor, source, failure_reason, created_at, updated_at
		FROM payments WHERE tenant_id=$1`
	args := []any{f.TenantID}
	if f.ProductID != "" {
		args = append(args, f.ProductID)
		q += " AND product_id=$2"
	}
	if !f.From.IsZero() {
		args = append(args, f.From)
		q += " AND created_at >= $" + strconv.Itoa(len(args))
	}
	if !f.To.IsZero() {
		args = append(args, f.To)
		q += " AND created_at <= $" + strconv.Itoa(len(args))
	}
	rows, err := p.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.Payment
	for rows.Next() {
		pm, err := scanPayment(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, pm)
	}
	return out, rows.Err()
}

type scanner interface {
	Scan(dest ...any) error
}

func scanPayment(row scanner) (domain.Payment, error) {
	var pm domain.Payment
	var amount, fee int64
	var currency, status, source string
	if err := row.Scan(&pm.TenantID, &pm.StripePaymentIntentID, &pm.DonorID, &pm.ProductID,
		&amount, &currency, &status, &fee, &source, &pm.FailureReason, &pm.CreatedAt, &pm.UpdatedAt); err != nil {
		return domain.Payment{}, err
	}
	pm.Amount = money.New(amount, currency)
	pm.ApplicationFee = money.New(fee, currency)
	pm.Status = domain.PaymentStatus(status)
	pm.Source = domain.Source(source)
	return pm, nil
}

// --- subscriptions ---

func (p *Postgres) UpsertSubscription(ctx context.Context, s domain.Subscription) error {
	_, err := p.pool.Exec(ctx, `
		INSERT INTO subscriptions (tenant_id, stripe_subscription_id, donor_id, product_id, price_id, status, current_period_end)
		VALUES ($1,$2,$3,$4,$5,$6,$7)
		ON CONFLICT (tenant_id, stripe_subscription_id) DO UPDATE SET
			donor_id=EXCLUDED.donor_id, product_id=EXCLUDED.product_id, price_id=EXCLUDED.price_id,
			status=EXCLUDED.status, current_period_end=EXCLUDED.current_period_end`,
		s.TenantID, s.StripeSubscriptionID, s.DonorID, s.ProductID, s.PriceID, string(s.Status), nz(s.CurrentPeriodEnd))
	return err
}

// --- ledger / dedup / idempotency ---

func (p *Postgres) UpsertLedgerEntry(ctx context.Context, e domain.LedgerEntry) (bool, error) {
	tag, err := p.pool.Exec(ctx, `
		INSERT INTO ledger_entries (stripe_balance_txn_id, tenant_id, type, amount_minor, currency, fee_minor, created_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7)
		ON CONFLICT (stripe_balance_txn_id) DO NOTHING`,
		e.StripeBalanceTxnID, e.TenantID, e.Type, e.Amount.Amount, e.Amount.Currency, e.Fee.Amount, nz(e.CreatedAt))
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

func (p *Postgres) MarkEventProcessed(ctx context.Context, eventID string) (bool, error) {
	tag, err := p.pool.Exec(ctx,
		`INSERT INTO webhook_events (stripe_event_id) VALUES ($1) ON CONFLICT (stripe_event_id) DO NOTHING`, eventID)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

func (p *Postgres) GetIdempotent(ctx context.Context, key string) (string, bool) {
	var piID string
	err := p.pool.QueryRow(ctx, `SELECT payment_intent_id FROM idempotency_keys WHERE key=$1`, key).Scan(&piID)
	if err != nil {
		return "", false
	}
	return piID, true
}

func (p *Postgres) SaveIdempotent(ctx context.Context, key, piID string) error {
	_, err := p.pool.Exec(ctx,
		`INSERT INTO idempotency_keys (key, payment_intent_id) VALUES ($1,$2) ON CONFLICT (key) DO NOTHING`, key, piID)
	return err
}

// helpers

func nz(t time.Time) time.Time {
	if t.IsZero() {
		return time.Now().UTC()
	}
	return t
}

// compile-time check that Postgres implements Store.
var _ Store = (*Postgres)(nil)
