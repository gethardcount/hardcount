package tax_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/tax"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

// TestPostgresStore_Record inserts one row via the convenience Record method
// and verifies the payload round-trips through the JSONB columns. Uses
// Record (not RecordCalculation) because that is the surface the billing
// engine depends on — a broken signature here would silently drop audit rows.
func TestPostgresStore_Record(t *testing.T) {
	db := testutil.SetupTestDB(t)
	tenantID := testutil.CreateTestTenant(t, db, "Tax Store Record")

	ctx, cancel := context.WithTimeout(postgres.WithLivemode(context.Background(), false), 10*time.Second)
	defer cancel()

	store := tax.NewPostgresStore(db)

	req := tax.Request{
		Currency: "USD",
		LineItems: []tax.RequestLine{
			{Ref: "line_0", AmountCents: 10000, TaxCode: "txcd_10103001"},
		},
	}
	res := &tax.Result{
		Provider:      "stripe_tax",
		CalculationID: "calc_test_abc",
		TotalTaxCents: 725,
		EffectiveRate: 7.25,
		TaxName:       "Sales Tax",
		Lines: []tax.ResultLine{{
			Ref:            "line_0",
			NetAmountCents: 10000,
			TaxAmountCents: 725,
			TaxRate:        7.25,
			Jurisdiction:   "US-CA",
		}},
	}

	// Empty invoice_id matches the draft-time call pattern in the billing
	// engine — the calculation is recorded before the invoice row exists.
	id, err := store.Record(ctx, tenantID, "", req, res)
	if err != nil {
		t.Fatalf("Record: %v", err)
	}
	if !strings.HasPrefix(id, "vlx_tcalc_") {
		t.Errorf("id %q does not look like a tax_calculation id (expected vlx_tcalc_ prefix)", id)
	}

	// Read back via TxBypass so we verify the write landed regardless of RLS.
	tx, err := db.BeginTx(ctx, postgres.TxBypass, "")
	if err != nil {
		t.Fatalf("begin read tx: %v", err)
	}
	defer func() { _ = tx.Rollback() }()

	var (
		gotTenant, gotInvoice, gotProvider, gotRef string
		reqJSON, resJSON                           []byte
	)
	err = tx.QueryRowContext(ctx, `
		SELECT tenant_id, COALESCE(invoice_id,''), provider, provider_ref, request, response
		FROM tax_calculations WHERE id = $1
	`, id).Scan(&gotTenant, &gotInvoice, &gotProvider, &gotRef, &reqJSON, &resJSON)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if gotTenant != tenantID {
		t.Errorf("tenant_id = %q, want %q", gotTenant, tenantID)
	}
	if gotInvoice != "" {
		t.Errorf("invoice_id = %q, want empty (draft-time calc)", gotInvoice)
	}
	if gotProvider != "stripe_tax" {
		t.Errorf("provider = %q, want stripe_tax", gotProvider)
	}
	if gotRef != "calc_test_abc" {
		t.Errorf("provider_ref = %q, want calc_test_abc", gotRef)
	}

	// Request round-trips through JSON — the Stripe calc ID and jurisdiction
	// must survive so future audit queries can reconstruct the decision.
	var parsed map[string]any
	if err := json.Unmarshal(resJSON, &parsed); err != nil {
		t.Fatalf("response JSON malformed: %v", err)
	}
	if parsed["CalculationID"] != "calc_test_abc" {
		t.Errorf("response.CalculationID = %v, want calc_test_abc", parsed["CalculationID"])
	}
}

// TestPostgresStore_LinkInvoice verifies the backfill that CommitTax runs once
// the invoice exists: Record writes the row with a NULL invoice_id (calc
// happens before the invoice is persisted), then LinkInvoice stamps the id by
// matching provider_ref. Without this, audit queries by invoice_id and the
// CommitTax expiry-guard lookup (which filters on invoice_id) silently miss.
func TestPostgresStore_LinkInvoice(t *testing.T) {
	db := testutil.SetupTestDB(t)
	tenantID := testutil.CreateTestTenant(t, db, "Tax Store Link")

	ctx, cancel := context.WithTimeout(postgres.WithLivemode(context.Background(), false), 10*time.Second)
	defer cancel()

	// invoice_id carries a FK to invoices, so the backfill target must be a
	// real persisted invoice — mirroring production, where CommitTax runs only
	// after the invoice row exists. Seed a minimal customer + invoice.
	var invoiceID string
	func() {
		tx, err := db.BeginTx(ctx, postgres.TxBypass, "")
		if err != nil {
			t.Fatalf("begin setup tx: %v", err)
		}
		defer func() { _ = tx.Rollback() }()
		var custID string
		if err := tx.QueryRowContext(ctx, `
			INSERT INTO customers (tenant_id, external_id, display_name)
			VALUES ($1, 'ext_link', 'Link Test Co') RETURNING id
		`, tenantID).Scan(&custID); err != nil {
			t.Fatalf("insert customer: %v", err)
		}
		if err := tx.QueryRowContext(ctx, `
			INSERT INTO invoices (tenant_id, customer_id, invoice_number, billing_period_start, billing_period_end)
			VALUES ($1, $2, 'INV-LINK-1', now(), now()) RETURNING id
		`, tenantID, custID).Scan(&invoiceID); err != nil {
			t.Fatalf("insert invoice: %v", err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("commit setup: %v", err)
		}
	}()

	store := tax.NewPostgresStore(db)
	req := tax.Request{LineItems: []tax.RequestLine{{Ref: "line_0", AmountCents: 10000}}}
	res := &tax.Result{Provider: "stripe_tax", CalculationID: "calc_link_xyz", TotalTaxCents: 888}

	if _, err := store.Record(ctx, tenantID, "", req, res); err != nil {
		t.Fatalf("Record: %v", err)
	}

	if err := store.LinkInvoice(ctx, tenantID, invoiceID, "calc_link_xyz"); err != nil {
		t.Fatalf("LinkInvoice: %v", err)
	}

	readInvoiceID := func() string {
		t.Helper()
		tx, err := db.BeginTx(ctx, postgres.TxBypass, "")
		if err != nil {
			t.Fatalf("begin read tx: %v", err)
		}
		defer func() { _ = tx.Rollback() }()
		var got string
		if err := tx.QueryRowContext(ctx, `
			SELECT COALESCE(invoice_id,'') FROM tax_calculations WHERE provider_ref = $1
		`, "calc_link_xyz").Scan(&got); err != nil {
			t.Fatalf("scan: %v", err)
		}
		return got
	}

	if got := readInvoiceID(); got != invoiceID {
		t.Fatalf("invoice_id = %q, want %q (LinkInvoice did not backfill)", got, invoiceID)
	}

	// Idempotent + non-clobbering: a second call with a different id must NOT
	// overwrite the existing link (the WHERE invoice_id IS NULL guard).
	if err := store.LinkInvoice(ctx, tenantID, "vlx_inv_other", "calc_link_xyz"); err != nil {
		t.Fatalf("LinkInvoice (second): %v", err)
	}
	if got := readInvoiceID(); got != invoiceID {
		t.Errorf("invoice_id = %q after second link, want unchanged %q (must not clobber)", got, invoiceID)
	}

	// Empty providerRef is a no-op (manual / none providers) — no error, no panic.
	if err := store.LinkInvoice(ctx, tenantID, invoiceID, ""); err != nil {
		t.Errorf("LinkInvoice with empty providerRef: %v, want nil no-op", err)
	}
}

// TestPostgresStore_Record_TenantIsolation verifies RLS prevents cross-tenant
// reads of tax_calculations — critical because calculation payloads can
// include customer addresses, tax IDs, and jurisdiction detail other tenants
// must never see.
func TestPostgresStore_Record_TenantIsolation(t *testing.T) {
	db := testutil.SetupTestDB(t)
	tenantA := testutil.CreateTestTenant(t, db, "Tenant A")
	tenantB := testutil.CreateTestTenant(t, db, "Tenant B")

	ctx, cancel := context.WithTimeout(postgres.WithLivemode(context.Background(), false), 10*time.Second)
	defer cancel()

	store := tax.NewPostgresStore(db)
	req := tax.Request{LineItems: []tax.RequestLine{{Ref: "line_0", AmountCents: 1000}}}
	res := &tax.Result{Provider: "manual", TotalTaxCents: 100}

	idA, err := store.Record(ctx, tenantA, "", req, res)
	if err != nil {
		t.Fatalf("Record tenantA: %v", err)
	}

	// Tenant B scoped tx must not see tenant A's row.
	tx, err := db.BeginTx(ctx, postgres.TxTenant, tenantB)
	if err != nil {
		t.Fatalf("begin tenantB tx: %v", err)
	}
	defer func() { _ = tx.Rollback() }()

	var count int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM tax_calculations WHERE id = $1`, idA,
	).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 0 {
		t.Errorf("tenant B sees tenant A's calculation row — RLS not enforced")
	}
}

// A calculation reused by a retried billing step (its idempotency key) is
// recorded twice. Its age — what the finalize expiry guard checks — runs from
// the FIRST record; reading the newest made an expired calculation look fresh.
func TestPostgresStore_LookupCalculationCreatedAt_OldestRecord(t *testing.T) {
	db := testutil.SetupTestDB(t)
	tenantID := testutil.CreateTestTenant(t, db, "Tax Store Oldest")
	ctx, cancel := context.WithTimeout(postgres.WithLivemode(context.Background(), false), 10*time.Second)
	defer cancel()

	var invoiceID string
	func() {
		tx, err := db.BeginTx(ctx, postgres.TxBypass, "")
		if err != nil {
			t.Fatalf("begin setup tx: %v", err)
		}
		defer func() { _ = tx.Rollback() }()
		var custID string
		if err := tx.QueryRowContext(ctx, `INSERT INTO customers (tenant_id, external_id, display_name) VALUES ($1, 'ext_oldest', 'Oldest Co') RETURNING id`, tenantID).Scan(&custID); err != nil {
			t.Fatalf("insert customer: %v", err)
		}
		if err := tx.QueryRowContext(ctx, `INSERT INTO invoices (tenant_id, customer_id, invoice_number, billing_period_start, billing_period_end) VALUES ($1, $2, 'INV-OLDEST-1', now(), now()) RETURNING id`, tenantID, custID).Scan(&invoiceID); err != nil {
			t.Fatalf("insert invoice: %v", err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("commit setup: %v", err)
		}
	}()

	store := tax.NewPostgresStore(db)
	req := tax.Request{LineItems: []tax.RequestLine{{Ref: "line_0", AmountCents: 10000}}}
	res := &tax.Result{Provider: "stripe_tax", CalculationID: "calc_reused"}
	for i := 0; i < 2; i++ { // the failed close, then its retry reusing the calculation
		if _, err := store.Record(ctx, tenantID, "", req, res); err != nil {
			t.Fatalf("Record %d: %v", i, err)
		}
	}
	first := time.Now().UTC().Add(-20 * time.Hour).Truncate(time.Second)
	func() {
		tx, err := db.BeginTx(ctx, postgres.TxBypass, "")
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer func() { _ = tx.Rollback() }()
		// Age the first record by 20h.
		if _, err := tx.ExecContext(ctx, `UPDATE tax_calculations SET created_at = $1 WHERE id = (SELECT id FROM tax_calculations WHERE provider_ref = 'calc_reused' ORDER BY created_at ASC LIMIT 1)`, first); err != nil {
			t.Fatalf("age first record: %v", err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("commit: %v", err)
		}
	}()
	if err := store.LinkInvoice(ctx, tenantID, invoiceID, "calc_reused"); err != nil {
		t.Fatalf("LinkInvoice: %v", err)
	}
	got, err := store.LookupCalculationCreatedAt(ctx, tenantID, invoiceID, "calc_reused")
	if err != nil {
		t.Fatalf("LookupCalculationCreatedAt: %v", err)
	}
	if !got.Equal(first) {
		t.Errorf("created_at = %v, want the first record's %v (the calculation's real age)", got, first)
	}
}
