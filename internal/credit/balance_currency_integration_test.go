package credit_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/credit"
	"github.com/sagarsuperuser/velox/internal/customer"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/invoice"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/pricing"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

// TestBalances_CarryTheCustomersCurrency: the ledger has no currency of its
// own (ADR-100), so the Credits page printed every balance with the
// workspace symbol: a EUR customer's €20 read "$20.00", and the page header
// added it into one dollar total. A balance now carries the currency the
// customer is billed in, in the order the engine picks an invoice's
// currency: billing profile, then the live subscription's plan, then the
// newest invoice, then the workspace default.
//
// Mutation-verify: drop any arm of customerCurrenciesSQL and the matching
// customer below reads the next arm's currency → red. Read the oldest
// invoice instead of the newest and the invoiced customer reads EUR → red.
func TestBalances_CarryTheCustomersCurrency(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: skipped in -short mode")
	}
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "BalanceCurrency")
	customers := customer.NewPostgresStore(db)
	plans := pricing.NewPostgresStore(db)
	invoices := invoice.NewPostgresStore(db)
	svc := credit.NewService(credit.NewPostgresStore(db))

	exec := func(q string, args ...any) {
		t.Helper()
		tx, err := db.BeginTx(ctx, postgres.TxTenant, tenantID)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer postgres.Rollback(tx)
		if _, err := tx.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("exec %q: %v", q, err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("commit: %v", err)
		}
	}
	exec(`INSERT INTO tenant_settings (tenant_id, default_currency) VALUES ($1, 'GBP')
	      ON CONFLICT (tenant_id) DO UPDATE SET default_currency = 'GBP'`, tenantID)

	newCustomer := func(name string) string {
		t.Helper()
		c, err := customers.Create(ctx, tenantID, domain.Customer{ExternalID: "cus_" + name, DisplayName: name})
		if err != nil {
			t.Fatalf("create customer %s: %v", name, err)
		}
		if _, err := svc.Grant(ctx, tenantID, credit.GrantInput{CustomerID: c.ID, AmountCents: 2000, Description: "goodwill"}); err != nil {
			t.Fatalf("grant %s: %v", name, err)
		}
		return c.ID
	}
	subscribe := func(customerID, status, currency string) {
		t.Helper()
		plan, err := plans.CreatePlan(ctx, tenantID, domain.Plan{
			Code: "plan-" + customerID + "-" + status, Name: currency + " plan", Currency: currency,
			BillingInterval: domain.BillingMonthly, Status: domain.PlanActive,
		})
		if err != nil {
			t.Fatalf("create plan: %v", err)
		}
		subID := postgres.NewID("vlx_sub")
		now := time.Now().UTC()
		exec(`INSERT INTO subscriptions (id, tenant_id, code, display_name, customer_id, status, billing_time,
		        current_billing_period_start, current_billing_period_end, next_billing_at, created_at, updated_at)
		      VALUES ($1, $2, $3, $3, $4, $5, 'anniversary', $6, $7, $7, $6, $6)`,
			subID, tenantID, "sub-"+subID, customerID, status, now.Add(-24*time.Hour), now.Add(29*24*time.Hour))
		exec(`INSERT INTO subscription_items (id, tenant_id, subscription_id, plan_id, quantity, metadata, created_at, updated_at)
		      VALUES ($1, $2, $3, $4, 1, '{}'::jsonb, $5, $5)`,
			postgres.NewID("vlx_si"), tenantID, subID, plan.ID, now)
	}

	// Profile beats the live subscription's plan.
	profiled := newCustomer("profiled")
	subscribe(profiled, "active", "USD")
	if _, err := customers.UpsertBillingProfile(ctx, tenantID, domain.CustomerBillingProfile{CustomerID: profiled, Currency: "EUR", ProfileStatus: domain.BillingProfileIncomplete}); err != nil {
		t.Fatalf("billing profile: %v", err)
	}
	// The live subscription's plan beats an older invoice; a canceled
	// subscription does not count.
	subscribed := newCustomer("subscribed")
	subscribe(subscribed, "canceled", "USD")
	subscribe(subscribed, "active", "CAD")
	createOpenInvoice(t, ctx, invoices, tenantID, subscribed, 500) // a USD invoice
	// No profile, no live subscription: the newest invoice, not an older
	// one in the currency the customer used to be billed in.
	invoiced := newCustomer("invoiced")
	subscribe(invoiced, "canceled", "CAD")
	older := createOpenInvoice(t, ctx, invoices, tenantID, invoiced, 500)
	exec(`UPDATE invoices SET currency = 'EUR', created_at = created_at - interval '60 days' WHERE id = $1`, older.ID)
	createOpenInvoice(t, ctx, invoices, tenantID, invoiced, 500) // USD, newer
	// Nothing at all: the workspace default.
	bare := newCustomer("bare")

	want := map[string]string{profiled: "EUR", subscribed: "CAD", invoiced: "USD", bare: "GBP"}

	// The JSON is what the dashboard reads.
	currencyOf := func(v any) string {
		t.Helper()
		raw, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		c, _ := m["currency"].(string)
		return c
	}

	list, err := svc.ListBalances(ctx, tenantID)
	if err != nil {
		t.Fatalf("list balances: %v", err)
	}
	if len(list) != len(want) {
		t.Fatalf("list balances: %d rows, want %d", len(list), len(want))
	}
	for _, b := range list {
		if got := currencyOf(b); got != want[b.CustomerID] {
			t.Errorf("list: customer %s currency = %q, want %q", b.CustomerID, got, want[b.CustomerID])
		}
	}
	for id, w := range want {
		b, err := svc.GetBalance(ctx, tenantID, id)
		if err != nil {
			t.Fatalf("get balance %s: %v", id, err)
		}
		if got := currencyOf(b); got != w {
			t.Errorf("get: customer %s currency = %q, want %q", id, got, w)
		}
	}
}
