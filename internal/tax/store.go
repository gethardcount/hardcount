package tax

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/gethardcount/hardcount/internal/errs"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
)

// CalculationRecord is the durable audit row written after every provider
// Calculate. Stored separately from invoices because Stripe tax_calculations
// expire upstream (90 days after they are made) — without this snapshot we
// can't answer audit questions about how an invoice's tax was derived after
// that. The stored response also carries the calculation's expires_at, which
// the commit guard and the pending-commit list read.
type CalculationRecord struct {
	ID          string
	TenantID    string
	InvoiceID   string // empty for draft-time calculations without a persisted invoice
	Provider    string // none | manual | stripe_tax
	ProviderRef string // e.g. Stripe tax_calculation id; empty for providers with no durable ref
	Request     []byte // JSONB
	Response    []byte // JSONB
}

// Store persists provider calculations. Keeping it on an interface lets the
// billing engine depend on a narrow abstraction rather than the postgres
// implementation directly — easier to fake in engine tests.
type Store interface {
	RecordCalculation(ctx context.Context, tx *sql.Tx, rec CalculationRecord) (string, error)
}

// PostgresStore writes to the tax_calculations table. All writes happen
// inside a caller-provided transaction so persistence stays atomic with
// the invoice write that triggered the calculation.
type PostgresStore struct {
	db *postgres.DB
}

func NewPostgresStore(db *postgres.DB) *PostgresStore {
	return &PostgresStore{db: db}
}

// RecordCalculation inserts a row into tax_calculations and returns the
// generated id. The tx must have been opened with TxTenant for the record's
// tenant (or TxBypass) so RLS permits the insert. Empty request/response
// payloads are coerced to "{}" to satisfy the NOT NULL JSONB constraint.
func (s *PostgresStore) RecordCalculation(ctx context.Context, tx *sql.Tx, rec CalculationRecord) (string, error) {
	if tx == nil {
		return "", fmt.Errorf("tax: RecordCalculation requires a transaction")
	}
	if rec.TenantID == "" {
		return "", fmt.Errorf("tax: RecordCalculation requires tenant_id")
	}
	if rec.Provider == "" {
		return "", fmt.Errorf("tax: RecordCalculation requires provider")
	}

	reqPayload := rec.Request
	if len(reqPayload) == 0 {
		reqPayload = []byte(`{}`)
	}
	respPayload := rec.Response
	if len(respPayload) == 0 {
		respPayload = []byte(`{}`)
	}

	var invoiceID sql.NullString
	if rec.InvoiceID != "" {
		invoiceID = sql.NullString{String: rec.InvoiceID, Valid: true}
	}

	var id string
	err := tx.QueryRowContext(ctx, `
		INSERT INTO tax_calculations
			(tenant_id, invoice_id, provider, provider_ref, request, response)
		VALUES ($1, $2, $3, $4, $5, $6)
		RETURNING id
	`, rec.TenantID, invoiceID, rec.Provider, rec.ProviderRef, reqPayload, respPayload).Scan(&id)
	if err != nil {
		return "", fmt.Errorf("tax: insert tax_calculations: %w", err)
	}
	return id, nil
}

// Record opens its own tenant-scoped transaction and writes one calculation
// row. Wired into the billing engine via SetTaxCalculationStore; the engine
// uses this signature so tests can fake persistence without a real postgres.
// Returns the generated calculation id.
func (s *PostgresStore) Record(ctx context.Context, tenantID, invoiceID string, req Request, res *Result) (string, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return "", fmt.Errorf("tax: begin tx: %w", err)
	}
	defer postgres.Rollback(tx)

	id, err := s.RecordCalculation(ctx, tx, RecordFromResult(tenantID, invoiceID, req, res))
	if err != nil {
		return "", err
	}
	if err := tx.Commit(); err != nil {
		return "", fmt.Errorf("tax: commit tx: %w", err)
	}
	return id, nil
}

// LinkInvoice backfills invoice_id on the audit row(s) for a calculation
// once the invoice that triggered it has been persisted. RecordCalculation
// runs during tax application — before the invoice row exists — so it writes
// a NULL invoice_id and the only durable link is provider_ref (the upstream
// Stripe tax_calculation id, mirrored onto invoices.tax_calculation_id).
// CommitTax calls this with the now-known invoice id so audit queries can
// join on invoice_id, and so the expiry-guard lookup (which filters on
// invoice_id) matches.
//
// Idempotent and additive-only: it touches rows with a NULL invoice_id,
// never overwriting an existing link. A no-op when providerRef is empty
// (manual / none providers have no durable ref to match on). The tx is
// opened with TxTenant so RLS permits the update.
func (s *PostgresStore) LinkInvoice(ctx context.Context, tenantID, invoiceID, providerRef string) error {
	if tenantID == "" || invoiceID == "" || providerRef == "" {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return fmt.Errorf("tax: begin tx: %w", err)
	}
	defer postgres.Rollback(tx)

	if _, err := tx.ExecContext(ctx, `
		UPDATE tax_calculations
		SET invoice_id = $2
		WHERE tenant_id = $1 AND provider_ref = $3 AND invoice_id IS NULL
	`, tenantID, invoiceID, providerRef); err != nil {
		return fmt.Errorf("tax: link invoice to tax_calculations: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("tax: commit tx: %w", err)
	}
	return nil
}

// CalculationFallbackLifetime is how long a recorded calculation is taken to
// be usable when its stored response names no expiry.
const CalculationFallbackLifetime = 24 * time.Hour

// CalculationUsableUntilSQL is the instant a tax_calculations row (aliased c)
// stops being usable for a commit: the provider's own expires_at from the
// stored response, or, when the response names none, CalculationFallbackLifetime
// after the row was written. Stripe Tax keeps a calculation for 90 days and
// says so in expires_at (verified in test mode, 2026-11-10); the 24 hours
// assumed before was the lifetime of an idempotency key. Shared by the commit
// guard's lookup and the pending-commit list, so the two cannot disagree.
//
// Only a positive number of at most 12 digits counts as an expiry. The Stripe
// provider stores the calculation as the SDK marshals it, where a missing or
// null expires_at is written as 0: read as a time, that is 1970, and every
// commit and finalize would be refused. A longer digit string cannot be cast
// and would fail the whole cross-tenant list.
const CalculationUsableUntilSQL = `COALESCE(
	CASE WHEN c.response->>'expires_at' ~ '^[1-9][0-9]{0,11}$'
	     THEN to_timestamp((c.response->>'expires_at')::bigint) END,
	c.created_at + interval '24 hours')`

// LookupCalculationUsableUntil returns the instant the calculation recorded
// for (tenant, invoice, provider_ref) stops being usable for a commit
// (CalculationUsableUntilSQL), read from the OLDEST matching row. One
// upstream calculation can be recorded more than once — a retried billing
// step reuses it through its idempotency key (WithCalculationScope) — and a
// row with no expiry in its response ages from the first record: reading the
// newest would make an expired calculation look fresh to the finalize expiry
// guard. When the same invoice has been retried (RetryTaxForInvoice writes a
// new row with a fresh provider_ref), the lookup still uniquely resolves
// because each retry uses a different upstream calc id.
// Returns errs.ErrNotFound when no row matches; CommitTax interprets
// that as "no audit row, skip the guard" (manual/none provider rows
// can have empty provider_ref, in which case the guard is meaningless
// anyway since manual/none Commits are no-ops).
func (s *PostgresStore) LookupCalculationUsableUntil(ctx context.Context, tenantID, invoiceID, providerRef string) (time.Time, error) {
	if providerRef == "" {
		return time.Time{}, errs.ErrNotFound
	}
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return time.Time{}, fmt.Errorf("tax: begin tx: %w", err)
	}
	defer postgres.Rollback(tx)

	var usableUntil time.Time
	err = tx.QueryRowContext(ctx, `
		SELECT `+CalculationUsableUntilSQL+` FROM tax_calculations c
		WHERE c.tenant_id = $1 AND c.invoice_id = $2 AND c.provider_ref = $3
		ORDER BY c.created_at ASC
		LIMIT 1
	`, tenantID, invoiceID, providerRef).Scan(&usableUntil)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, errs.ErrNotFound
	}
	if err != nil {
		return time.Time{}, fmt.Errorf("tax: lookup calculation expiry: %w", err)
	}
	return usableUntil, nil
}

// RecordFromResult is a convenience that derives the CalculationRecord from a
// provider Result. It preserves Result.RequestRaw / ResponseRaw as-is when
// present (Stripe Tax), and falls back to a JSON dump of the request/result
// for providers without raw bytes (manual, none) so the audit row is still
// useful.
func RecordFromResult(tenantID, invoiceID string, req Request, res *Result) CalculationRecord {
	if res == nil {
		return CalculationRecord{
			TenantID: tenantID, InvoiceID: invoiceID,
			Provider: "none",
			Request:  marshalJSON(req),
			Response: []byte(`{}`),
		}
	}
	reqRaw := res.RequestRaw
	if len(reqRaw) == 0 {
		reqRaw = marshalJSON(req)
	}
	respRaw := res.ResponseRaw
	if len(respRaw) == 0 {
		respRaw = marshalJSON(res)
	}
	return CalculationRecord{
		TenantID:    tenantID,
		InvoiceID:   invoiceID,
		Provider:    res.Provider,
		ProviderRef: res.CalculationID,
		Request:     reqRaw,
		Response:    respRaw,
	}
}

func marshalJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		return []byte(`{}`)
	}
	return b
}
