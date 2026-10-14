package tenantstripe

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/errs"
	"github.com/sagarsuperuser/velox/internal/platform/crypto"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
)

// Store persists per-tenant Stripe credentials with secrets encrypted at rest.
// A request-scoped view returns metadata only (last4 + verified status); the
// internal path returns plaintext secrets for making Stripe API calls.
//
// All operations run under TxBypass and filter by (tenant_id, livemode)
// explicitly in WHERE clauses. RLS is skipped because this store is the
// single boundary that administers credentials across BOTH modes for the
// same tenant — a settings handler authenticated as one mode must still
// write to the other. Callers (handler, resolver, webhook verifier) are
// responsible for authorizing tenant access before invoking store methods.
type Store struct {
	db  *postgres.DB
	enc *crypto.Encryptor
}

func NewStore(db *postgres.DB) *Store {
	return &Store{db: db, enc: crypto.NewNoop()}
}

// SetEncryptor wires the AES-GCM encryptor used for secret_key and
// webhook_secret columns. When nil / noop, values are stored as plaintext —
// still acceptable in local-dev (no VELOX_ENCRYPTION_KEY) but the encrypted
// path is mandatory in production.
func (s *Store) SetEncryptor(enc *crypto.Encryptor) {
	if enc == nil {
		s.enc = crypto.NewNoop()
		return
	}
	s.enc = enc
}

// UpsertInput is the plaintext input passed to the store. It never lands on
// disk untransformed — only last4 suffixes and the encrypted envelope do.
// The service-level ConnectInput (in service.go) is the HTTP-facing shape;
// this is its storage-facing counterpart.
type UpsertInput struct {
	TenantID       string
	Livemode       bool
	SecretKey      string
	PublishableKey string
	WebhookSecret  string // optional on initial connect
}

// PlaintextSecrets carries the decrypted credentials needed to build a Stripe
// client or verify a webhook signature. Callers must treat it as sensitive —
// never log, never serialize to the API surface.
type PlaintextSecrets struct {
	SecretKey     string
	WebhookSecret string
}

// Upsert writes a new credential row or updates the existing one for
// (tenant, livemode). Returns the public view (no secrets).
func (s *Store) Upsert(ctx context.Context, in UpsertInput) (domain.StripeProviderCredentials, error) {
	if in.TenantID == "" {
		return domain.StripeProviderCredentials{}, errs.Required("tenant_id")
	}
	if in.SecretKey == "" {
		return domain.StripeProviderCredentials{}, errs.Required("secret_key")
	}
	if in.PublishableKey == "" {
		return domain.StripeProviderCredentials{}, errs.Required("publishable_key")
	}

	secretEnc, err := s.enc.Encrypt(in.SecretKey)
	if err != nil {
		return domain.StripeProviderCredentials{}, fmt.Errorf("encrypt secret key: %w", err)
	}
	secretLast4 := last4(in.SecretKey)
	secretPrefix := keyPrefix(in.SecretKey)

	var webhookEnc, webhookLast4 sql.NullString
	if in.WebhookSecret != "" {
		enc, err := s.enc.Encrypt(in.WebhookSecret)
		if err != nil {
			return domain.StripeProviderCredentials{}, fmt.Errorf("encrypt webhook secret: %w", err)
		}
		webhookEnc = sql.NullString{String: enc, Valid: true}
		webhookLast4 = sql.NullString{String: last4(in.WebhookSecret), Valid: true}
	}

	tx, err := s.db.BeginTx(ctx, postgres.TxBypass, "")
	if err != nil {
		return domain.StripeProviderCredentials{}, err
	}
	defer func() { _ = tx.Rollback() }()

	const q = `
		INSERT INTO stripe_provider_credentials (
			tenant_id, livemode,
			secret_key_encrypted, secret_key_last4, secret_key_prefix, publishable_key,
			webhook_secret_encrypted, webhook_secret_last4
		)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		ON CONFLICT (tenant_id, livemode) DO UPDATE SET
			secret_key_encrypted     = EXCLUDED.secret_key_encrypted,
			secret_key_last4         = EXCLUDED.secret_key_last4,
			secret_key_prefix        = EXCLUDED.secret_key_prefix,
			publishable_key          = EXCLUDED.publishable_key,
			webhook_secret_encrypted = COALESCE(EXCLUDED.webhook_secret_encrypted, stripe_provider_credentials.webhook_secret_encrypted),
			webhook_secret_last4     = COALESCE(EXCLUDED.webhook_secret_last4, stripe_provider_credentials.webhook_secret_last4),
			verified_at              = NULL,
			last_verified_error      = NULL,
			last_verified_error_kind = NULL,
			updated_at               = NOW()
		RETURNING id, tenant_id, livemode, stripe_account_id, stripe_account_name,
			secret_key_prefix, secret_key_last4, publishable_key, webhook_secret_last4,
			(webhook_secret_encrypted IS NOT NULL),
			verified_at, last_verified_error, last_verified_error_kind, created_at, updated_at
	`

	row := tx.QueryRowContext(ctx, q,
		in.TenantID, in.Livemode,
		secretEnc, secretLast4, secretPrefix, in.PublishableKey,
		webhookEnc, webhookLast4,
	)

	out, err := scanPublic(row)
	if err != nil {
		return domain.StripeProviderCredentials{}, fmt.Errorf("upsert stripe credentials: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return domain.StripeProviderCredentials{}, fmt.Errorf("commit: %w", err)
	}
	return out, nil
}

// MarkVerified stamps verified_at, clears last_verified_error, and records
// the verified Stripe account identity returned by Stripe on a successful
// V1Accounts.Retrieve call.
func (s *Store) MarkVerified(ctx context.Context, tenantID string, livemode bool, accountID, accountName string) error {
	tx, err := s.db.BeginTx(ctx, postgres.TxBypass, "")
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	_, err = tx.ExecContext(ctx, `
		UPDATE stripe_provider_credentials
		SET stripe_account_id    = $3,
		    stripe_account_name  = $4,
		    verified_at          = NOW(),
		    last_verified_error  = NULL,
		    last_verified_error_kind = NULL,
		    updated_at           = NOW()
		WHERE tenant_id = $1 AND livemode = $2
	`, tenantID, livemode, accountID, accountName)
	if err != nil {
		return fmt.Errorf("mark verified: %w", err)
	}
	return tx.Commit()
}

// MarkVerifyFailed records the error from a failed verify attempt: Stripe's
// own sentence and the kind of failure (VerifyErrorRejected or
// VerifyErrorUnavailable). The row remains present — the operator may have a
// transient Stripe outage, and we don't want to drop credentials they'll
// still need.
func (s *Store) MarkVerifyFailed(ctx context.Context, tenantID string, livemode bool, errMsg, kind string) error {
	tx, err := s.db.BeginTx(ctx, postgres.TxBypass, "")
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	_, err = tx.ExecContext(ctx, `
		UPDATE stripe_provider_credentials
		SET last_verified_error      = $3,
		    last_verified_error_kind = $4,
		    updated_at               = NOW()
		WHERE tenant_id = $1 AND livemode = $2
	`, tenantID, livemode, errMsg, kind)
	if err != nil {
		return fmt.Errorf("mark verify failed: %w", err)
	}
	return tx.Commit()
}

// ListByTenant returns the public view (no secrets) for all modes connected
// under the tenant. Never more than two rows (test + live).
func (s *Store) ListByTenant(ctx context.Context, tenantID string) ([]domain.StripeProviderCredentials, error) {
	// Settings/credentials UI needs to show both modes side by side, but the
	// tenant ctx carries only one livemode at a time. Bypass RLS and scope
	// by tenant_id explicitly — the handler enforces that the caller's auth
	// matches tenantID before we reach here.
	tx, err := s.db.BeginTx(ctx, postgres.TxBypass, "")
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	rows, err := tx.QueryContext(ctx, `
		SELECT id, tenant_id, livemode, stripe_account_id, stripe_account_name,
		       secret_key_prefix, secret_key_last4, publishable_key, webhook_secret_last4,
		       (webhook_secret_encrypted IS NOT NULL),
		       verified_at, last_verified_error, last_verified_error_kind, created_at, updated_at
		FROM stripe_provider_credentials
		WHERE tenant_id = $1
		ORDER BY livemode DESC
	`, tenantID)
	if err != nil {
		return nil, fmt.Errorf("list stripe credentials: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []domain.StripeProviderCredentials
	for rows.Next() {
		c, err := scanPublic(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// GetPlaintext returns the decrypted secrets for a specific (tenant, mode).
// Used by the Stripe client resolver and webhook verifier — never exposed
// over the API.
func (s *Store) GetPlaintext(ctx context.Context, tenantID string, livemode bool) (PlaintextSecrets, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxBypass, "")
	if err != nil {
		return PlaintextSecrets{}, err
	}
	defer func() { _ = tx.Rollback() }()

	var secretEnc string
	var webhookEnc sql.NullString
	err = tx.QueryRowContext(ctx, `
		SELECT secret_key_encrypted, webhook_secret_encrypted
		FROM stripe_provider_credentials
		WHERE tenant_id = $1 AND livemode = $2
	`, tenantID, livemode).Scan(&secretEnc, &webhookEnc)
	if errors.Is(err, sql.ErrNoRows) {
		return PlaintextSecrets{}, errs.ErrNotFound
	}
	if err != nil {
		return PlaintextSecrets{}, fmt.Errorf("load stripe credentials: %w", err)
	}

	out := PlaintextSecrets{}
	if out.SecretKey, err = s.enc.Decrypt(secretEnc); err != nil {
		return PlaintextSecrets{}, fmt.Errorf("decrypt secret key: %w", err)
	}
	if webhookEnc.Valid {
		if out.WebhookSecret, err = s.enc.Decrypt(webhookEnc.String); err != nil {
			return PlaintextSecrets{}, fmt.Errorf("decrypt webhook secret: %w", err)
		}
	}
	return out, nil
}

// SetWebhookSecret updates only the webhook_secret_encrypted / _last4 columns
// on an existing (tenant, livemode) row. Returns errs.ErrNotFound when no row
// exists — the tenant must complete Connect (API keys) first. The API-key
// verify status is left untouched: verified_at / last_verified_error are
// unchanged because we haven't re-probed Stripe.
func (s *Store) SetWebhookSecret(ctx context.Context, tenantID string, livemode bool, plaintext string) (domain.StripeProviderCredentials, error) {
	if tenantID == "" {
		return domain.StripeProviderCredentials{}, errs.Required("tenant_id")
	}
	if plaintext == "" {
		return domain.StripeProviderCredentials{}, errs.Required("webhook_secret")
	}

	enc, err := s.enc.Encrypt(plaintext)
	if err != nil {
		return domain.StripeProviderCredentials{}, fmt.Errorf("encrypt webhook secret: %w", err)
	}
	lastFour := last4(plaintext)

	tx, err := s.db.BeginTx(ctx, postgres.TxBypass, "")
	if err != nil {
		return domain.StripeProviderCredentials{}, err
	}
	defer func() { _ = tx.Rollback() }()

	const q = `
		UPDATE stripe_provider_credentials
		SET webhook_secret_encrypted = $3,
		    webhook_secret_last4     = $4,
		    updated_at               = NOW()
		WHERE tenant_id = $1 AND livemode = $2
		RETURNING id, tenant_id, livemode, stripe_account_id, stripe_account_name,
			secret_key_prefix, secret_key_last4, publishable_key, webhook_secret_last4,
			(webhook_secret_encrypted IS NOT NULL),
			verified_at, last_verified_error, last_verified_error_kind, created_at, updated_at
	`

	row := tx.QueryRowContext(ctx, q, tenantID, livemode, enc, lastFour)
	out, err := scanPublic(row)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.StripeProviderCredentials{}, errs.ErrNotFound
	}
	if err != nil {
		return domain.StripeProviderCredentials{}, fmt.Errorf("update webhook secret: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return domain.StripeProviderCredentials{}, fmt.Errorf("commit: %w", err)
	}
	return out, nil
}

// EndpointLookup is the narrow view returned by GetByID — enough to verify a
// webhook signature and then process the event under the owning tenant.
// Tenant/livemode come from the row itself, not the URL or payload, so a
// forged Stripe-Signature cannot misroute an event.
type EndpointLookup struct {
	ID            string
	TenantID      string
	Livemode      bool
	WebhookSecret string
}

// GetByID resolves an opaque webhook endpoint id (vlx_spc_XXX, exposed in the
// Stripe dashboard's endpoint URL) to the owning tenant and the decrypted
// webhook signing secret. Returns errs.ErrNotFound when the row is gone
// (tenant disconnected or rotated).
//
// Defense in depth: the id is not a secret — it's in the URL anyone can see
// — but knowing it doesn't grant anything, because forging a webhook still
// requires the per-endpoint whsec_ secret to produce a matching HMAC.
func (s *Store) GetByID(ctx context.Context, id string) (EndpointLookup, error) {
	if id == "" {
		return EndpointLookup{}, errs.Required("id")
	}

	tx, err := s.db.BeginTx(ctx, postgres.TxBypass, "")
	if err != nil {
		return EndpointLookup{}, err
	}
	defer func() { _ = tx.Rollback() }()

	var out EndpointLookup
	var webhookEnc sql.NullString
	err = tx.QueryRowContext(ctx, `
		SELECT id, tenant_id, livemode, webhook_secret_encrypted
		FROM stripe_provider_credentials
		WHERE id = $1
	`, id).Scan(&out.ID, &out.TenantID, &out.Livemode, &webhookEnc)
	if errors.Is(err, sql.ErrNoRows) {
		return EndpointLookup{}, errs.ErrNotFound
	}
	if err != nil {
		return EndpointLookup{}, fmt.Errorf("load stripe credentials by id: %w", err)
	}
	if !webhookEnc.Valid {
		// Row exists but no webhook secret registered yet — tenant connected
		// API keys but never completed the webhook half. Treat as not found
		// so the handler 404s rather than accepting unsigned events.
		return EndpointLookup{}, errs.ErrNotFound
	}

	secret, err := s.enc.Decrypt(webhookEnc.String)
	if err != nil {
		return EndpointLookup{}, fmt.Errorf("decrypt webhook secret: %w", err)
	}
	out.WebhookSecret = secret
	return out, nil
}

// Delete removes the credentials for a single (tenant, mode).
func (s *Store) Delete(ctx context.Context, tenantID string, livemode bool) error {
	tx, err := s.db.BeginTx(ctx, postgres.TxBypass, "")
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	res, err := tx.ExecContext(ctx, `
		DELETE FROM stripe_provider_credentials
		WHERE tenant_id = $1 AND livemode = $2
	`, tenantID, livemode)
	if err != nil {
		return fmt.Errorf("delete stripe credentials: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return errs.ErrNotFound
	}
	return tx.Commit()
}

// --- helpers ---

type scanner interface {
	Scan(dest ...any) error
}

func scanPublic(r scanner) (domain.StripeProviderCredentials, error) {
	var c domain.StripeProviderCredentials
	var accountID, accountName, webhookLast4, verifyErr, verifyErrKind sql.NullString
	var verifiedAt sql.NullTime
	err := r.Scan(
		&c.ID, &c.TenantID, &c.Livemode,
		&accountID, &accountName,
		&c.SecretKeyPrefix, &c.SecretKeyLast4, &c.PublishableKey, &webhookLast4, &c.HasWebhookSecret,
		&verifiedAt, &verifyErr, &verifyErrKind, &c.CreatedAt, &c.UpdatedAt,
	)
	if err != nil {
		return c, err
	}
	c.StripeAccountID = accountID.String
	c.StripeAccountName = accountName.String
	c.WebhookSecretLast4 = webhookLast4.String
	c.LastVerifiedError = verifyErr.String
	c.LastVerifiedErrorKind = verifyErrKind.String
	if verifiedAt.Valid {
		t := verifiedAt.Time
		c.VerifiedAt = &t
	}
	return c, nil
}

func last4(s string) string {
	if len(s) <= 4 {
		return s
	}
	return s[len(s)-4:]
}

// keyPrefix captures the leading portion of a Stripe API key to display
// Stripe-dashboard-style ("sk_live_51ab••••••••wxyz"). 12 chars covers the
// type prefix (sk_live_ / rk_test_ / etc., 8 chars) plus 4 account-identifying
// chars — matches what tenants see in their Stripe dashboard, enough to tell
// keys apart at a glance, too short to be useful to an attacker.
func keyPrefix(s string) string {
	const n = 12
	if len(s) > n {
		return s[:n]
	}
	return s
}
