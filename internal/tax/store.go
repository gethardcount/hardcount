package tax

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/sagarsuperuser/velox/internal/errs"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
)

// CalculationRecord is the durable audit row written after every provider
// Calculate. Stored separately from invoices because Stripe tax_calculations
// expire in 24 hours upstream — without this snapshot we can't answer audit
// questions about how an invoice's tax was derived after the expiry window.
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

// LookupCalculationCreatedAt returns the created_at of the OLDEST
// tax_calculations row that matches (tenant, invoice, provider_ref). One
// upstream calculation can be recorded more than once — a retried billing
// step reuses it through its idempotency key (WithCalculationScope) — and its
// real age runs from the first record: reading the newest would make an
// expired calculation look fresh to the finalize expiry guard.
// When the same invoice has been retried (RetryTaxForInvoice writes a
// new row with a fresh provider_ref), the lookup still uniquely
// resolves because each retry uses a different upstream calc id —
// the provider_ref filter pins to the calc whose age is being checked.
// Returns errs.ErrNotFound when no row matches; CommitTax interprets
// that as "no audit row, skip the guard" (manual/none provider rows
// can have empty provider_ref, in which case the guard is meaningless
// anyway since manual/none Commits are no-ops).
func (s *PostgresStore) LookupCalculationCreatedAt(ctx context.Context, tenantID, invoiceID, providerRef string) (time.Time, error) {
	if providerRef == "" {
		return time.Time{}, errs.ErrNotFound
	}
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return time.Time{}, fmt.Errorf("tax: begin tx: %w", err)
	}
	defer postgres.Rollback(tx)

	var createdAt time.Time
	err = tx.QueryRowContext(ctx, `
		SELECT created_at FROM tax_calculations
		WHERE tenant_id = $1 AND invoice_id = $2 AND provider_ref = $3
		ORDER BY created_at ASC
		LIMIT 1
	`, tenantID, invoiceID, providerRef).Scan(&createdAt)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, errs.ErrNotFound
	}
	if err != nil {
		return time.Time{}, fmt.Errorf("tax: lookup calc created_at: %w", err)
	}
	return createdAt, nil
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
