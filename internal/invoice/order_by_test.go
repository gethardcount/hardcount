package invoice

import "testing"

// TestInvoiceOrderBy_DefaultsToCreatedAtDesc asserts the no-input
// path: empty sort + empty dir resolves to the canonical default
// (created_at DESC, id DESC). Tie-break direction matches the
// primary sort so consecutive ties read as a single ordered group.
func TestInvoiceOrderBy_DefaultsToCreatedAtDesc(t *testing.T) {
	if got, err := invoiceOrderBy("", ""); err != nil || got != "created_at DESC, id DESC" {
		t.Errorf("default order: got %q, %v; want %q", got, err, "created_at DESC, id DESC")
	}
}

// TestInvoiceOrderBy_AllowedColumns asserts each known SPA sort key
// resolves to a real column. New SPA columns must be added to the
// allow-list (invoiceSort) — this test catches the asymmetry.
func TestInvoiceOrderBy_AllowedColumns(t *testing.T) {
	cases := map[string]string{
		"created_at":           "created_at",
		"invoice_number":       "invoice_number",
		"amount_due_cents":     invoiceAmountShown,
		"amount_due":           invoiceAmountShown, // alias
		"billing_period_start": "billing_period_start",
		"period":               "billing_period_start", // alias
		"due_at":               "due_at",
		"issued_at":            "issued_at",
		"status":               "status",
		"payment_status":       "payment_status",
	}
	for key, col := range cases {
		t.Run(key, func(t *testing.T) {
			got, err := invoiceOrderBy(key, "asc")
			if want := col + " ASC, id ASC"; err != nil || got != want {
				t.Errorf("invoiceOrderBy(%q) = %q, %v; want %q", key, got, err, want)
			}
		})
	}
}

// TestInvoiceOrderBy_RefusesUnknownKeyAndDir asserts the closed set: a key
// or a dir outside it is an error, never a column. Unknown keys used to fall
// back to created_at and unknown dirs to desc without a word; raw input still
// never reaches the SQL.
func TestInvoiceOrderBy_RefusesUnknownKeyAndDir(t *testing.T) {
	for _, c := range []struct{ sort, dir string }{
		{"DROP TABLE invoices", ""},
		{"random_garbage", ""},
		{"id; --", ""},
		{"", "random"},
		{"", "ASC"}, // case-sensitive — must be lowercase
		{"", "DROP --"},
	} {
		if got, err := invoiceOrderBy(c.sort, c.dir); err == nil {
			t.Errorf("invoiceOrderBy(%q, %q) = %q, want an error", c.sort, c.dir, got)
		}
	}
}

// TestInvoiceOrderBy_TieBreakMatchesPrimaryDir asserts the id
// tie-break direction matches the primary sort direction. Without
// this, an ASC sort would zig-zag on ties (primary ASC, secondary
// DESC) — visible to operators as inconsistent ordering within a
// run of same-amount or same-period invoices.
func TestInvoiceOrderBy_TieBreakMatchesPrimaryDir(t *testing.T) {
	if got, _ := invoiceOrderBy("invoice_number", "asc"); got != "invoice_number ASC, id ASC" {
		t.Errorf("ASC tie-break: got %q", got)
	}
	if got, _ := invoiceOrderBy("invoice_number", "desc"); got != "invoice_number DESC, id DESC" {
		t.Errorf("DESC tie-break: got %q", got)
	}
}
