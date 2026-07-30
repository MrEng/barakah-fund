package store

import (
	"context"
	"encoding/json"
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

// Migrate creates tables if they do not exist, first renaming any legacy
// tenant-named tables/columns from before the account_id rename.
func (p *Postgres) Migrate(ctx context.Context) error {
	if _, err := p.pool.Exec(ctx, renameSQL); err != nil {
		return err
	}
	_, err := p.pool.Exec(ctx, schemaSQL)
	return err
}

// renameSQL migrates databases created before tenant_id was renamed to
// account_id. Each rename is a no-op when the old name is already gone.
const renameSQL = `
DO $$ BEGIN
	ALTER TABLE tenants RENAME TO accounts;
EXCEPTION WHEN undefined_table THEN NULL; END $$;
DO $$ BEGIN
	ALTER TABLE donors RENAME COLUMN tenant_id TO account_id;
EXCEPTION WHEN undefined_table OR undefined_column THEN NULL; END $$;
DO $$ BEGIN
	ALTER TABLE payments RENAME COLUMN tenant_id TO account_id;
EXCEPTION WHEN undefined_table OR undefined_column THEN NULL; END $$;
DO $$ BEGIN
	ALTER TABLE subscriptions RENAME COLUMN tenant_id TO account_id;
EXCEPTION WHEN undefined_table OR undefined_column THEN NULL; END $$;
DO $$ BEGIN
	ALTER TABLE ledger_entries RENAME COLUMN tenant_id TO account_id;
EXCEPTION WHEN undefined_table OR undefined_column THEN NULL; END $$;
`

const schemaSQL = `
CREATE TABLE IF NOT EXISTS accounts (
	id TEXT PRIMARY KEY,
	name TEXT NOT NULL DEFAULT '',
	stripe_account_id TEXT NOT NULL DEFAULT '',
	charges_enabled BOOLEAN NOT NULL DEFAULT false,
	payouts_enabled BOOLEAN NOT NULL DEFAULT false
);
CREATE TABLE IF NOT EXISTS donors (
	account_id TEXT NOT NULL,
	id TEXT NOT NULL,
	email TEXT NOT NULL DEFAULT '',
	name TEXT NOT NULL DEFAULT '',
	stripe_customer_id TEXT NOT NULL DEFAULT '',
	PRIMARY KEY (account_id, id)
);
CREATE INDEX IF NOT EXISTS donors_email_idx ON donors (account_id, email);
CREATE TABLE IF NOT EXISTS payments (
	account_id TEXT NOT NULL,
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
	metadata JSONB NOT NULL DEFAULT '{}',
	PRIMARY KEY (account_id, stripe_payment_intent_id)
);
ALTER TABLE payments ADD COLUMN IF NOT EXISTS metadata JSONB NOT NULL DEFAULT '{}';
CREATE INDEX IF NOT EXISTS payments_product_idx ON payments (account_id, product_id);
CREATE TABLE IF NOT EXISTS subscriptions (
	account_id TEXT NOT NULL,
	stripe_subscription_id TEXT NOT NULL,
	donor_id TEXT NOT NULL DEFAULT '',
	product_id TEXT NOT NULL DEFAULT '',
	price_id TEXT NOT NULL DEFAULT '',
	status TEXT NOT NULL DEFAULT '',
	current_period_end TIMESTAMPTZ NOT NULL DEFAULT now(),
	PRIMARY KEY (account_id, stripe_subscription_id)
);
CREATE TABLE IF NOT EXISTS ledger_entries (
	stripe_balance_txn_id TEXT PRIMARY KEY,
	account_id TEXT NOT NULL,
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

// --- accounts ---

func (p *Postgres) SaveAccount(ctx context.Context, t domain.Account) error {
	_, err := p.pool.Exec(ctx, `
		INSERT INTO accounts (id, name, stripe_account_id, charges_enabled, payouts_enabled)
		VALUES ($1,$2,$3,$4,$5)
		ON CONFLICT (id) DO UPDATE SET
			name=EXCLUDED.name, stripe_account_id=EXCLUDED.stripe_account_id,
			charges_enabled=EXCLUDED.charges_enabled, payouts_enabled=EXCLUDED.payouts_enabled`,
		t.ID, t.Name, t.StripeAccountID, t.ChargesEnabled, t.PayoutsEnabled)
	return err
}

func (p *Postgres) GetAccount(ctx context.Context, id string) (domain.Account, error) {
	var t domain.Account
	err := p.pool.QueryRow(ctx,
		`SELECT id, name, stripe_account_id, charges_enabled, payouts_enabled FROM accounts WHERE id=$1`, id).
		Scan(&t.ID, &t.Name, &t.StripeAccountID, &t.ChargesEnabled, &t.PayoutsEnabled)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Account{}, ErrNotFound
	}
	return t, err
}

func (p *Postgres) ListAccounts(ctx context.Context) ([]domain.Account, error) {
	rows, err := p.pool.Query(ctx,
		`SELECT id, name, stripe_account_id, charges_enabled, payouts_enabled FROM accounts`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.Account
	for rows.Next() {
		var t domain.Account
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
		INSERT INTO donors (account_id, id, email, name, stripe_customer_id)
		VALUES ($1,$2,$3,$4,$5)
		ON CONFLICT (account_id, id) DO UPDATE SET
			email=EXCLUDED.email, name=EXCLUDED.name, stripe_customer_id=EXCLUDED.stripe_customer_id`,
		d.AccountID, d.ID, d.Email, d.Name, d.StripeCustomerID)
	return err
}

func (p *Postgres) GetDonor(ctx context.Context, accountID, donorID string) (domain.Donor, error) {
	return p.scanDonor(ctx,
		`SELECT account_id, id, email, name, stripe_customer_id FROM donors WHERE account_id=$1 AND id=$2`,
		accountID, donorID)
}

func (p *Postgres) FindDonorByEmail(ctx context.Context, accountID, email string) (domain.Donor, error) {
	return p.scanDonor(ctx,
		`SELECT account_id, id, email, name, stripe_customer_id FROM donors WHERE account_id=$1 AND email=$2 LIMIT 1`,
		accountID, email)
}

func (p *Postgres) scanDonor(ctx context.Context, q string, args ...any) (domain.Donor, error) {
	var d domain.Donor
	err := p.pool.QueryRow(ctx, q, args...).Scan(&d.AccountID, &d.ID, &d.Email, &d.Name, &d.StripeCustomerID)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Donor{}, ErrNotFound
	}
	return d, err
}

// --- payments ---

func (p *Postgres) UpsertPayment(ctx context.Context, pm domain.Payment) error {
	meta, err := json.Marshal(orEmpty(pm.Metadata))
	if err != nil {
		return err
	}
	_, err = p.pool.Exec(ctx, `
		INSERT INTO payments (account_id, stripe_payment_intent_id, donor_id, product_id,
			amount_minor, currency, status, application_fee_minor, source, failure_reason, created_at, updated_at, metadata)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)
		ON CONFLICT (account_id, stripe_payment_intent_id) DO UPDATE SET
			donor_id=EXCLUDED.donor_id, product_id=EXCLUDED.product_id, amount_minor=EXCLUDED.amount_minor,
			currency=EXCLUDED.currency, status=EXCLUDED.status, application_fee_minor=EXCLUDED.application_fee_minor,
			source=EXCLUDED.source, failure_reason=EXCLUDED.failure_reason, updated_at=EXCLUDED.updated_at,
			metadata=EXCLUDED.metadata`,
		pm.AccountID, pm.StripePaymentIntentID, pm.DonorID, pm.ProductID, pm.Amount.Amount, pm.Amount.Currency,
		string(pm.Status), pm.ApplicationFee.Amount, string(pm.Source), pm.FailureReason,
		nz(pm.CreatedAt), nz(pm.UpdatedAt), meta)
	return err
}

// orEmpty keeps the stored JSON an object (not null) for absent metadata.
func orEmpty(m map[string]string) map[string]string {
	if m == nil {
		return map[string]string{}
	}
	return m
}

func (p *Postgres) GetPayment(ctx context.Context, accountID, piID string) (domain.Payment, error) {
	row := p.pool.QueryRow(ctx, `
		SELECT account_id, stripe_payment_intent_id, donor_id, product_id, amount_minor, currency,
			status, application_fee_minor, source, failure_reason, created_at, updated_at, metadata
		FROM payments WHERE account_id=$1 AND stripe_payment_intent_id=$2`, accountID, piID)
	pm, err := scanPayment(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Payment{}, ErrNotFound
	}
	return pm, err
}

func (p *Postgres) ListPayments(ctx context.Context, f PaymentFilter) ([]domain.Payment, error) {
	q := `SELECT account_id, stripe_payment_intent_id, donor_id, product_id, amount_minor, currency,
			status, application_fee_minor, source, failure_reason, created_at, updated_at, metadata
		FROM payments WHERE account_id=$1`
	args := []any{f.AccountID}
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
	var meta []byte
	if err := row.Scan(&pm.AccountID, &pm.StripePaymentIntentID, &pm.DonorID, &pm.ProductID,
		&amount, &currency, &status, &fee, &source, &pm.FailureReason, &pm.CreatedAt, &pm.UpdatedAt, &meta); err != nil {
		return domain.Payment{}, err
	}
	pm.Amount = money.New(amount, currency)
	pm.ApplicationFee = money.New(fee, currency)
	pm.Status = domain.PaymentStatus(status)
	pm.Source = domain.Source(source)
	if len(meta) > 0 {
		if err := json.Unmarshal(meta, &pm.Metadata); err != nil {
			return domain.Payment{}, err
		}
	}
	if len(pm.Metadata) == 0 {
		pm.Metadata = nil
	}
	return pm, nil
}

// --- subscriptions ---

func (p *Postgres) UpsertSubscription(ctx context.Context, s domain.Subscription) error {
	_, err := p.pool.Exec(ctx, `
		INSERT INTO subscriptions (account_id, stripe_subscription_id, donor_id, product_id, price_id, status, current_period_end)
		VALUES ($1,$2,$3,$4,$5,$6,$7)
		ON CONFLICT (account_id, stripe_subscription_id) DO UPDATE SET
			donor_id=EXCLUDED.donor_id, product_id=EXCLUDED.product_id, price_id=EXCLUDED.price_id,
			status=EXCLUDED.status, current_period_end=EXCLUDED.current_period_end`,
		s.AccountID, s.StripeSubscriptionID, s.DonorID, s.ProductID, s.PriceID, string(s.Status), nz(s.CurrentPeriodEnd))
	return err
}

// --- ledger / dedup / idempotency ---

func (p *Postgres) UpsertLedgerEntry(ctx context.Context, e domain.LedgerEntry) (bool, error) {
	tag, err := p.pool.Exec(ctx, `
		INSERT INTO ledger_entries (stripe_balance_txn_id, account_id, type, amount_minor, currency, fee_minor, created_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7)
		ON CONFLICT (stripe_balance_txn_id) DO NOTHING`,
		e.StripeBalanceTxnID, e.AccountID, e.Type, e.Amount.Amount, e.Amount.Currency, e.Fee.Amount, nz(e.CreatedAt))
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
