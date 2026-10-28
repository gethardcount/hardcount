package payment

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/sagarsuperuser/velox/internal/platform/postgres"
)

type TokenService struct {
	db *postgres.DB
}

func NewTokenService(db *postgres.DB) *TokenService {
	return &TokenService{db: db}
}

type PaymentUpdateToken struct {
	ID         string
	TenantID   string
	CustomerID string
	InvoiceID  string
	// Livemode is the mode of the referenced invoice, resolved at
	// validate-time via JOIN. Carrying it on the validated token lets
	// the public handler open a properly-scoped TxTenant for the
	// follow-on invoice / customer reads — RLS stays as defense-in-
	// depth. Without this, the handler would have to either bypass
	// RLS (loose) or do a separate livemode lookup (extra round-trip
	// + chicken-and-egg with RLS).
	Livemode  bool
	ExpiresAt time.Time
}

// Create generates a new payment update token. Returns the raw token (shown once in email).
func (s *TokenService) Create(ctx context.Context, tenantID, customerID, invoiceID string) (string, error) {
	// Generate 32 random bytes
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate token: %w", err)
	}
	rawToken := "hc_pt_" + hex.EncodeToString(buf)

	// Hash for storage
	hash := sha256.Sum256([]byte(rawToken))
	tokenHash := hex.EncodeToString(hash[:])

	id := postgres.NewID("hc_ptk")
	expiresAt := time.Now().UTC().Add(24 * time.Hour)

	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return "", fmt.Errorf("begin tx: %w", err)
	}
	defer postgres.Rollback(tx)

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO payment_update_tokens (id, tenant_id, customer_id, invoice_id, token_hash, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6)
	`, id, tenantID, customerID, invoiceID, tokenHash, expiresAt); err != nil {
		return "", fmt.Errorf("store token: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return "", fmt.Errorf("commit token: %w", err)
	}

	return rawToken, nil
}

// Validate checks a raw token and returns the associated data. Does NOT mark as used.
//
// Runs under TxBypass because the caller is unauthenticated: the public
// portal endpoint receives a raw token from a URL and must resolve it to a
// tenant_id BEFORE any tenant context can be set. The token itself is the
// credential — 256 bits of entropy, verified by constant-time hash match,
// so cross-tenant enumeration isn't feasible.
func (s *TokenService) Validate(ctx context.Context, rawToken string) (*PaymentUpdateToken, error) {
	hash := sha256.Sum256([]byte(rawToken))
	tokenHash := hex.EncodeToString(hash[:])

	tx, err := s.db.BeginTx(ctx, postgres.TxBypass, "")
	if err != nil {
		return nil, fmt.Errorf("invalid or expired token")
	}
	defer postgres.Rollback(tx)

	// JOIN invoices to grab the referenced invoice's livemode in the
	// same query. Two wins: (a) the validated token carries everything
	// the public handler needs to open a properly-scoped TxTenant —
	// no second bypass-required livemode lookup; (b) the JOIN
	// naturally fails when the invoice has been deleted, so a stale
	// token returns "invalid or expired" instead of an internal-error
	// downstream when the handler tries to fetch a vanished invoice.
	var t PaymentUpdateToken
	err = tx.QueryRowContext(ctx, `
		SELECT t.id, t.tenant_id, t.customer_id, t.invoice_id, i.livemode, t.expires_at
		FROM payment_update_tokens t
		JOIN invoices i ON i.id = t.invoice_id
		WHERE t.token_hash = $1 AND t.expires_at > NOW() AND t.used_at IS NULL
	`, tokenHash).Scan(&t.ID, &t.TenantID, &t.CustomerID, &t.InvoiceID, &t.Livemode, &t.ExpiresAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("invalid or expired token")
		}
		return nil, fmt.Errorf("invalid or expired token")
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("invalid or expired token")
	}

	return &t, nil
}

// Consume atomically marks a token used and reports whether THIS call was the
// one that consumed it. The conditional UPDATE (used_at IS NULL) is a
// compare-and-swap: under concurrent requests for the same single-use token,
// exactly one gets consumed=true and may proceed; the others get false and must
// abort. This closes the validate→mark-used TOCTOU where two requests both
// passed Validate's `used_at IS NULL` read and both opened a setup-mode checkout
// session. Callers must consume BEFORE the side effect, not after.
//
// The caller passes the tenantID returned by Validate so the update runs under
// tenant RLS.
func (s *TokenService) Consume(ctx context.Context, tenantID, rawToken string) (bool, error) {
	return s.ConsumeAudited(ctx, tenantID, rawToken, nil)
}

// ConsumeAudited is Consume with an in-tx audit emission hook (ADR-090).
// emit fires ONLY when this call won the CAS — the losing request of a
// concurrent double-click records nothing.
func (s *TokenService) ConsumeAudited(ctx context.Context, tenantID, rawToken string, emit func(tx *sql.Tx) error) (bool, error) {
	hash := sha256.Sum256([]byte(rawToken))
	tokenHash := hex.EncodeToString(hash[:])

	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return false, err
	}
	defer postgres.Rollback(tx)

	res, err := tx.ExecContext(ctx, `
		UPDATE payment_update_tokens SET used_at = NOW()
		WHERE token_hash = $1 AND used_at IS NULL
	`, tokenHash)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	if n == 1 && emit != nil {
		if err := emit(tx); err != nil {
			return false, fmt.Errorf("audit emission: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return n == 1, nil
}

// Restore un-consumes a token whose side effect FAILED moments after
// Consume. The consume-then-create ordering in createCheckoutSession is
// the exactly-once CAS; but when the Stripe create itself errors, the
// customer's emailed link would stay permanently dead over a transient
// upstream failure. The recency guard bounds resurrection to the
// consume-create window of THIS request — a token consumed by a
// successful session create minutes ago cannot be revived by a replay.
func (s *TokenService) Restore(ctx context.Context, tenantID, rawToken string) error {
	return s.RestoreAudited(ctx, tenantID, rawToken, nil)
}

// RestoreAudited is Restore with an in-tx audit emission hook (ADR-090):
// the revival row keeps the log truthful — without it, a consumed-then-
// restored token would show a "checkout started" row for a flow that never
// produced a session. emit fires only when a row was actually revived.
func (s *TokenService) RestoreAudited(ctx context.Context, tenantID, rawToken string, emit func(tx *sql.Tx) error) error {
	hash := sha256.Sum256([]byte(rawToken))
	tokenHash := hex.EncodeToString(hash[:])

	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return err
	}
	defer postgres.Rollback(tx)

	res, err := tx.ExecContext(ctx, `
		UPDATE payment_update_tokens SET used_at = NULL
		WHERE token_hash = $1 AND used_at IS NOT NULL AND used_at > NOW() - INTERVAL '1 minute'
	`, tokenHash)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 1 && emit != nil {
		if err := emit(tx); err != nil {
			return fmt.Errorf("audit emission: %w", err)
		}
	}
	return tx.Commit()
}

// Cleanup deletes expired tokens older than 7 days across all tenants. Runs
// cross-tenant by design (a background scheduler, not a per-request path),
// so it uses TxBypass.
func (s *TokenService) Cleanup(ctx context.Context) (int, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxBypass, "")
	if err != nil {
		return 0, err
	}
	defer postgres.Rollback(tx)

	result, err := tx.ExecContext(ctx, `
		DELETE FROM payment_update_tokens WHERE expires_at < NOW() - INTERVAL '7 days'
	`)
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	n, _ := result.RowsAffected()
	return int(n), nil
}
