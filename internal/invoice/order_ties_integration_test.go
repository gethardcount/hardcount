package invoice_test

import (
	"context"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/customer"
	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/invoice"
	"github.com/gethardcount/hardcount/internal/platform/clock"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/pricing"
	"github.com/gethardcount/hardcount/internal/subscription"
	"github.com/gethardcount/hardcount/internal/testutil"
)

// Invoices that share a created_at come back from these four reads in one
// order on every read: by created_at, then by id. Two invoices are created on
// one instant, and each read runs with their rows stored in both orders: a
// tie the query leaves open comes back in the order Postgres reads the rows,
// so one of the two orders shows it.
func TestInvoiceOrders_ATieOnCreatedAtSortsByID(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Order Ties")
	store := invoice.NewPostgresStore(db)

	cust, err := customer.NewPostgresStore(db).Create(ctx, tenantID, domain.Customer{ExternalID: "cus_ties", DisplayName: "Ties"})
	if err != nil {
		t.Fatalf("create customer: %v", err)
	}
	plan, err := pricing.NewPostgresStore(db).CreatePlan(ctx, tenantID, domain.Plan{
		Code: "plan-ties", Name: "Ties", Currency: "USD", BillingInterval: domain.BillingMonthly,
		Status: domain.PlanActive, BaseAmountCents: 5000, BaseBillTiming: domain.BillInAdvance,
	})
	if err != nil {
		t.Fatalf("create plan: %v", err)
	}
	ps := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	pe := ps.AddDate(0, 1, 0)
	sub, err := subscription.NewPostgresStore(db).Create(ctx, tenantID, domain.Subscription{
		Code: "sub-ties", DisplayName: "Ties", CustomerID: cust.ID,
		Status: domain.SubscriptionActive, BillingTime: domain.BillingTimeCalendar,
		StartedAt: &ps, Items: []domain.SubscriptionItem{{PlanID: plan.ID, Quantity: 1}},
		CurrentBillingPeriodStart: &ps, CurrentBillingPeriodEnd: &pe,
	})
	if err != nil {
		t.Fatalf("create subscription: %v", err)
	}

	// Two drafts on one created_at, each carrying the period's base line: the
	// shape of a base re-issued within one action. Their headers differ by a
	// day, as one subscription's invoices for one period must (the billing
	// idempotency index); both reads match the base line, not the header.
	at := time.Date(2026, 4, 1, 0, 0, 1, 0, time.UTC)
	create := func(num string, headerEnd time.Time) domain.Invoice {
		t.Helper()
		inv, err := store.CreateWithLineItems(ctx, tenantID, domain.Invoice{
			CustomerID: cust.ID, SubscriptionID: sub.ID, InvoiceNumber: num,
			Status: domain.InvoiceDraft, PaymentStatus: domain.PaymentPending,
			BillingReason: domain.BillingReasonSubscriptionCycle, Currency: "USD",
			SubtotalCents: 5000, TotalAmountCents: 5000, AmountDueCents: 5000,
			BillingPeriodStart: ps, BillingPeriodEnd: headerEnd, CreatedAt: at,
		}, []domain.InvoiceLineItem{{
			LineType: domain.LineTypeBaseFee, Description: "Base", Quantity: 1,
			UnitAmountCents: 5000, AmountCents: 5000, TotalAmountCents: 5000, Currency: "USD",
			BillingPeriodStart: &ps, BillingPeriodEnd: &pe,
		}})
		if err != nil {
			t.Fatalf("create invoice %s: %v", num, err)
		}
		if !inv.CreatedAt.Equal(at) {
			t.Fatalf("invoice %s created_at %v; want the shared %v", num, inv.CreatedAt, at)
		}
		return inv
	}
	lo, hi := create("HC-TIE-1", pe).ID, create("HC-TIE-2", pe.AddDate(0, 0, -1)).ID
	if hi < lo {
		lo, hi = hi, lo
	}

	// touch writes a new version of the row, after the rows already stored.
	// It changes updated_at, an indexed column, so the new version gets new
	// index entries too, and an index scan reads the new order as well.
	touch := func(id string) {
		bypassExec(t, db, `UPDATE invoices SET updated_at = updated_at + interval '1 microsecond' WHERE id = $1`, id)
	}
	orders := []struct {
		name          string
		first, second string
	}{
		{"the lower id stored first", lo, hi},
		{"the higher id stored first", hi, lo},
	}
	each := func(t *testing.T, read func(t *testing.T) []string, want []string) {
		for _, o := range orders {
			touch(o.first)
			touch(o.second)
			if got := read(t); !equalIDs(got, want) {
				t.Errorf("%s: got %v; want %v", o.name, got, want)
			}
		}
	}

	t.Run("the base invoice of a period is the later id", func(t *testing.T) {
		each(t, func(t *testing.T) []string {
			inv, err := store.FindBaseInvoiceForPeriod(ctx, tenantID, sub.ID, ps)
			if err != nil {
				t.Fatalf("FindBaseInvoiceForPeriod: %v", err)
			}
			return []string{inv.ID}
		}, []string{hi})
	})
	t.Run("the funding invoices of a period come lower id first", func(t *testing.T) {
		each(t, func(t *testing.T) []string {
			invs, err := store.FindFundingInvoicesForPeriod(ctx, tenantID, sub.ID, ps, pe)
			if err != nil {
				t.Fatalf("FindFundingInvoicesForPeriod: %v", err)
			}
			return idsOf(invs)
		}, []string{lo, hi})
	})
	t.Run("the provider-configuration retry list comes lower id first", func(t *testing.T) {
		bypassExec(t, db, `UPDATE invoices SET tax_status = 'pending', tax_error_code = 'provider_not_configured' WHERE id IN ($1, $2)`, lo, hi)
		each(t, func(t *testing.T) []string {
			invs, err := store.ListProviderConfigErrors(ctx, tenantID, false)
			if err != nil {
				t.Fatalf("ListProviderConfigErrors: %v", err)
			}
			return idsOf(invs)
		}, []string{lo, hi})
	})
	t.Run("the customer-data retry list comes lower id first", func(t *testing.T) {
		bypassExec(t, db, `UPDATE invoices SET tax_status = 'pending', tax_error_code = 'customer_data_invalid' WHERE id IN ($1, $2)`, lo, hi)
		each(t, func(t *testing.T) []string {
			invs, err := store.ListCustomerDataInvalidErrors(ctx, tenantID, cust.ID)
			if err != nil {
				t.Fatalf("ListCustomerDataInvalidErrors: %v", err)
			}
			return idsOf(invs)
		}, []string{lo, hi})
	})
}

// An invoice's lines share their created_at: one action writes them on its
// one instant. They come back in one order on every read, by id, which is the
// order they were written in (ids are xids). Read with the rows stored in both
// orders, as above.
func TestInvoiceLineItems_ATieOnCreatedAtSortsByID(t *testing.T) {
	db := testutil.SetupTestDB(t)
	at := time.Date(2026, 4, 1, 0, 0, 1, 0, time.UTC)
	ctx := clock.WithEffectiveNow(postgres.WithLivemode(context.Background(), false), at)
	tenantID := testutil.CreateTestTenant(t, db, "Line Ties")
	store := invoice.NewPostgresStore(db)

	cust, err := customer.NewPostgresStore(db).Create(ctx, tenantID, domain.Customer{ExternalID: "cus_line_ties", DisplayName: "Line Ties"})
	if err != nil {
		t.Fatalf("create customer: %v", err)
	}
	ps := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	line := func(desc string) domain.InvoiceLineItem {
		return domain.InvoiceLineItem{LineType: domain.LineTypeBaseFee, Description: desc, Quantity: 1,
			UnitAmountCents: 1000, AmountCents: 1000, TotalAmountCents: 1000, Currency: "USD"}
	}
	inv, err := store.CreateWithLineItems(ctx, tenantID, domain.Invoice{
		CustomerID: cust.ID, InvoiceNumber: "HC-LINE-TIES", Status: domain.InvoiceDraft,
		PaymentStatus: domain.PaymentPending, Currency: "USD",
		SubtotalCents: 2000, TotalAmountCents: 2000, AmountDueCents: 2000,
		BillingPeriodStart: ps, BillingPeriodEnd: ps.AddDate(0, 1, 0),
	}, []domain.InvoiceLineItem{line("first"), line("second")})
	if err != nil {
		t.Fatalf("create invoice: %v", err)
	}
	items, err := store.ListLineItems(ctx, tenantID, inv.ID)
	if err != nil || len(items) != 2 {
		t.Fatalf("ListLineItems: %d lines, %v; want 2", len(items), err)
	}
	if !items[0].CreatedAt.Equal(items[1].CreatedAt) {
		t.Fatalf("lines created at %v and %v; want one shared instant", items[0].CreatedAt, items[1].CreatedAt)
	}
	lo, hi := items[0].ID, items[1].ID
	if hi < lo {
		lo, hi = hi, lo
	}

	for _, o := range []struct{ name, first, second string }{
		{"the lower id stored first", lo, hi},
		{"the higher id stored first", hi, lo},
	} {
		reinsert(t, db, ctx, tenantID, "invoice_line_items", o.first)
		reinsert(t, db, ctx, tenantID, "invoice_line_items", o.second)
		got, err := store.ListLineItems(ctx, tenantID, inv.ID)
		if err != nil {
			t.Fatalf("ListLineItems: %v", err)
		}
		if ids := []string{got[0].ID, got[1].ID}; !equalIDs(ids, []string{lo, hi}) {
			t.Errorf("%s: got %v; want %v", o.name, ids, []string{lo, hi})
		}
	}
}

// reinsert deletes a row and inserts it again, unchanged, in one statement:
// its new version is stored after every other row, with new index entries.
// It runs in the tenant's transaction, so the row's mode trigger sees the
// session's mode.
func reinsert(t *testing.T, db *postgres.DB, ctx context.Context, tenantID, table, id string) {
	t.Helper()
	tx, err := db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer postgres.Rollback(tx)
	if _, err := tx.ExecContext(ctx, `WITH d AS (DELETE FROM `+table+` WHERE id = $1 RETURNING *) INSERT INTO `+table+` SELECT * FROM d`, id); err != nil {
		t.Fatalf("reinsert %s %s: %v", table, id, err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

func idsOf(invs []domain.Invoice) []string {
	out := make([]string, len(invs))
	for i, inv := range invs {
		out[i] = inv.ID
	}
	return out
}

func equalIDs(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
