package invoice_test

import (
	"context"
	"sort"
	"testing"

	"github.com/gethardcount/hardcount/internal/invoice"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/testutil"
)

// TestList_SearchMatchesTheCustomer: the invoice list's search matches the
// invoice number or the customer's name or email (C-15). It matched the
// number only, so an operator looking for a customer's invoices found none.
// Another tenant's customer with the same name never matches.
func TestList_SearchMatchesTheCustomer(t *testing.T) {
	db := testutil.SetupTestDB(t)
	store := invoice.NewPostgresStore(db)
	ctx := postgres.WithLivemode(context.Background(), false)

	const tenantID = "hc_ten_inv_search"
	const otherTenant = "hc_ten_inv_search_other"

	tx, err := db.BeginTx(ctx, postgres.TxBypass, "")
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer postgres.Rollback(tx)
	mustExec := func(q string, args ...any) {
		t.Helper()
		if _, err := tx.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("seed: %v\n%s", err, q)
		}
	}
	mustExec(`SELECT set_config('app.livemode', 'off', true)`)
	for _, id := range []string{tenantID, otherTenant} {
		mustExec(`INSERT INTO tenants (id, name, status) VALUES ($1, $1, 'active')`, id)
	}
	seedCustomer := func(id, tenant, name, email string) {
		mustExec(`INSERT INTO customers (id, tenant_id, external_id, display_name, email, created_at, updated_at)
			VALUES ($1, $2, $1, $3, $4, now(), now())`, id, tenant, name, email)
	}
	seedCustomer("cus_srch_acme", tenantID, "Acme Corp", "billing@acme.test")
	seedCustomer("cus_srch_globex", tenantID, "Globex", "ap@globex.test")
	seedCustomer("cus_srch_other_acme", otherTenant, "Acme Corp", "billing@acme.test")
	seedInv := func(id, tenant, custID string) {
		mustExec(`INSERT INTO invoices (id, tenant_id, customer_id, invoice_number,
			status, payment_status, currency,
			subtotal_cents, total_amount_cents, amount_due_cents, tax_status,
			billing_period_start, billing_period_end, created_at, updated_at)
		VALUES ($1, $2, $3, $1, 'finalized', 'pending', 'USD',
			2500, 2500, 2500, 'ok', now(), now(), now(), now())`, id, tenant, custID)
	}
	seedInv("SRCH-0001", tenantID, "cus_srch_acme")
	seedInv("SRCH-0002", tenantID, "cus_srch_acme")
	seedInv("SRCH-0003", tenantID, "cus_srch_globex")
	seedInv("SRCH-9999", otherTenant, "cus_srch_other_acme")
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	for _, tc := range []struct {
		search string
		want   []string
	}{
		{"acme", []string{"SRCH-0001", "SRCH-0002"}}, // name, any case
		{"GLOBEX", []string{"SRCH-0003"}},            // name
		{"ap@globex", []string{"SRCH-0003"}},         // email
		{"srch-0003", []string{"SRCH-0003"}},         // number, as before
		{"SRCH-", []string{"SRCH-0001", "SRCH-0002", "SRCH-0003"}},
		{"initech", nil},
	} {
		list, total, err := store.List(ctx, invoice.ListFilter{TenantID: tenantID, Search: tc.search})
		if err != nil {
			t.Fatalf("search %q: %v", tc.search, err)
		}
		var got []string
		for _, inv := range list {
			got = append(got, inv.InvoiceNumber)
		}
		sort.Strings(got)
		if len(got) != len(tc.want) || total != len(tc.want) {
			t.Fatalf("search %q: got %v (total %d), want %v", tc.search, got, total, tc.want)
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Fatalf("search %q: got %v, want %v", tc.search, got, tc.want)
			}
		}
	}

	// The customer filter and the search narrow together.
	list, _, err := store.List(ctx, invoice.ListFilter{TenantID: tenantID, CustomerID: "cus_srch_globex", Search: "acme"})
	if err != nil {
		t.Fatalf("customer + search: %v", err)
	}
	if len(list) != 0 {
		t.Fatalf("customer Globex + search acme: want none, got %d", len(list))
	}
}
