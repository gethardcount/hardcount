package api

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/gethardcount/hardcount/internal/platform/postgres"
)

// TestIngest_ValidEventsBillTheSame is the B5 money check for the ingest
// changes (quantity enforced as the spec says, `properties` refused, the
// Idempotency-Key header ignored on usage routes). Valid events must bill
// exactly what they billed before: the same quantity on the usage line and
// the same cents. The expected values below were captured on main before B5
// (c0f24b68) with this same test, and are hard-coded so the test fails if
// either side moves.
//
// The events use every quantity form the spec accepts (number, string,
// decimal, negative correction, exponent, 12 decimal places) through all
// three doors, a retried batch (same event keys, and an Idempotency-Key
// header), a single event sent twice with its key, and an event with
// dimensions.
func TestIngest_ValidEventsBillTheSame(t *testing.T) {
	skipShort(t)
	r := newLagRig(t, "in_arrears", false)
	ev := func(key string, qty any, ts time.Time) map[string]any {
		return map[string]any{
			"external_customer_id": r.custExt, "event_name": "calls", "quantity": qty,
			"timestamp": ts.Format(time.RFC3339), "idempotency_key": key,
		}
	}
	d := func(days int) time.Time { return lagP0.Add(time.Duration(days) * 24 * time.Hour) }

	r.must(http.MethodPost, "/v1/usage-events", d(2), ev("same-1", 5, d(1)), http.StatusCreated)
	r.must(http.MethodPost, "/v1/usage-events", d(3), ev("same-2", "2.5", d(2)), http.StatusCreated)
	r.must(http.MethodPost, "/v1/usage-events", d(4), ev("same-3", -1, d(3)), http.StatusCreated)
	r.must(http.MethodPost, "/v1/usage-events", d(5), ev("same-4", 1e2, d(4)), http.StatusCreated)
	r.must(http.MethodPost, "/v1/usage-events", d(6), ev("same-5", "0.123456789012", d(5)), http.StatusCreated)
	// The same event again: stored once.
	r.must(http.MethodPost, "/v1/usage-events", d(6), ev("same-5", "0.123456789012", d(5)), http.StatusOK)

	batch := []map[string]any{ev("same-b1", 3, d(7)), ev("same-b2", "4.25", d(8)), ev("same-b3", "-0.75", d(9))}
	r.must(http.MethodPost, "/v1/usage-events/batch", d(10), batch, http.StatusCreated)
	// The batch retried, with an Idempotency-Key header on the retry.
	rec := r.doKey(http.MethodPost, "/v1/usage-events/batch", "batch-retry", d(11), batch)
	if rec.Code != http.StatusCreated && rec.Code != http.StatusOK {
		t.Fatalf("retried batch: %d %s", rec.Code, rec.Body.String())
	}

	r.must(http.MethodPost, "/v1/usage-events/backfill", d(20), ev("same-bf", 7, d(12)), http.StatusCreated)
	withDims := ev("same-dims", "2", d(13))
	withDims["dimensions"] = map[string]any{"model": "m1", "region": "eu"}
	r.must(http.MethodPost, "/v1/usage-events", d(14), withDims, http.StatusCreated)

	if n := r.run(lagB.Add(2 * time.Hour)); n < 1 {
		t.Fatalf("billing run generated %d invoices, want the April cycle invoice", n)
	}
	inv := r.cycleInvoice(r.invoices(), lagP0)

	ctx := context.Background()
	tx, err := r.db.BeginTx(ctx, postgres.TxBypass, "")
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer postgres.Rollback(tx)
	var qty int64
	var qtyDec decimal.Decimal
	var unit, amount int64
	if err := tx.QueryRowContext(ctx, `SELECT quantity, quantity_decimal, unit_amount_cents, amount_cents
		FROM invoice_line_items WHERE invoice_id = $1 AND line_type = 'usage'`, inv.id).Scan(&qty, &qtyDec, &unit, &amount); err != nil {
		t.Fatalf("read usage line: %v", err)
	}
	var events, withDimensions int
	var stored decimal.Decimal
	if err := tx.QueryRowContext(ctx, `SELECT count(*), COALESCE(sum(quantity), 0), count(*) FILTER (WHERE properties @> '{"model":"m1","region":"eu"}')
		FROM usage_events WHERE tenant_id = $1`, r.tenantID).Scan(&events, &stored, &withDimensions); err != nil {
		t.Fatalf("read events: %v", err)
	}
	if withDimensions != 1 {
		t.Errorf("%d events stored with their dimensions, want 1", withDimensions)
	}

	// Captured on main before B5 (c0f24b68).
	const (
		wantEvents   = 10
		wantStored   = "122.123456789012"
		wantQty      = 122
		wantQtyDec   = "122.123456789012"
		wantAmount   = 122
		wantSubtotal = 3122
	)
	if events != wantEvents || stored.String() != wantStored {
		t.Errorf("stored %d events summing %s, want %d summing %s", events, stored, wantEvents, wantStored)
	}
	if qty != wantQty || qtyDec.String() != wantQtyDec || amount != wantAmount {
		t.Errorf("usage line quantity=%d quantity_decimal=%s unit=%d amount=%dc, want quantity=%d quantity_decimal=%s amount=%dc",
			qty, qtyDec, unit, amount, wantQty, wantQtyDec, wantAmount)
	}
	if inv.subtotal != wantSubtotal {
		t.Errorf("subtotal %dc, want %dc", inv.subtotal, wantSubtotal)
	}
}
