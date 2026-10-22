package invoice

import "testing"

// TestInvoiceOrderBy_DefaultsToCreatedAtDesc asserts the no-input
// path: empty sort + empty dir resolves to the canonical default
// (created_at DESC, id DESC). Tie-break direction matches the
// primary sort so consecutive ties read as a single ordered group.
func TestInvoiceOrderBy_DefaultsToCreatedAtDesc(t *testing.T) {
	if got := invoiceOrderBy("", ""); got != "created_at DESC, id DESC" {
		t.Errorf("default order: got %q, want %q", got, "created_at DESC, id DESC")
	}
}

// TestInvoiceOrderBy_AllowedColumns asserts each known SPA sort key
// resolves to a real column. New SPA columns must be added to the
// allow-list (invoiceSortColumn) — this test catches the asymmetry.
func TestInvoiceOrderBy_AllowedColumns(t *testing.T) {
	const amountShown = "(CASE WHEN status = 'paid' THEN amount_paid_cents + credits_applied_cents ELSE amount_due_cents END)"
	cases := map[string]string{
		"invoice_number":       "invoice_number",
		"amount_due_cents":     amountShown,
		"amount_due":           amountShown, // alias
		"billing_period_start": "billing_period_start",
		"period":               "billing_period_start", // alias
		"due_at":               "due_at",
		"issued_at":            "issued_at",
		"status":               "status",
		"payment_status":       "payment_status",
	}
	for key, col := range cases {
		t.Run(key, func(t *testing.T) {
			got := invoiceSortColumn(key)
			if got != col {
				t.Errorf("invoiceSortColumn(%q) = %q, want %q", key, got, col)
			}
		})
	}
}

// TestInvoiceOrderBy_UnknownKeyFallsBackToCreatedAt asserts the
// closed-allow-list invariant: any key not in the map silently
// resolves to created_at — never interpolates user input into SQL.
// This is the SQL-injection guard.
func TestInvoiceOrderBy_UnknownKeyFallsBackToCreatedAt(t *testing.T) {
	cases := []string{
		"DROP TABLE invoices",
		"random_garbage",
		"id; --",
		"",
	}
	for _, key := range cases {
		t.Run(key, func(t *testing.T) {
			got := invoiceSortColumn(key)
			if got != "created_at" {
				t.Errorf("invoiceSortColumn(%q) = %q, want created_at (default)", key, got)
			}
		})
	}
}

// TestInvoiceOrderBy_DirNormalization asserts dir is normalized to a
// fixed two-element set. Anything other than "asc" defaults to DESC.
func TestInvoiceOrderBy_DirNormalization(t *testing.T) {
	cases := map[string]string{
		"asc":     "created_at ASC, id ASC",
		"desc":    "created_at DESC, id DESC",
		"":        "created_at DESC, id DESC",
		"random":  "created_at DESC, id DESC",
		"ASC":     "created_at DESC, id DESC", // case-sensitive — must be lowercase
		"DROP --": "created_at DESC, id DESC",
	}
	for dir, want := range cases {
		t.Run(dir, func(t *testing.T) {
			got := invoiceOrderBy("", dir)
			if got != want {
				t.Errorf("invoiceOrderBy(%q): got %q, want %q", dir, got, want)
			}
		})
	}
}

// TestInvoiceOrderBy_TieBreakMatchesPrimaryDir asserts the id
// tie-break direction matches the primary sort direction. Without
// this, an ASC sort would zig-zag on ties (primary ASC, secondary
// DESC) — visible to operators as inconsistent ordering within a
// run of same-amount or same-period invoices.
func TestInvoiceOrderBy_TieBreakMatchesPrimaryDir(t *testing.T) {
	if got := invoiceOrderBy("invoice_number", "asc"); got != "invoice_number ASC, id ASC" {
		t.Errorf("ASC tie-break: got %q", got)
	}
	if got := invoiceOrderBy("invoice_number", "desc"); got != "invoice_number DESC, id DESC" {
		t.Errorf("DESC tie-break: got %q", got)
	}
}
