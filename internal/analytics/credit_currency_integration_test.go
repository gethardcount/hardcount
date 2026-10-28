package analytics

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/gethardcount/hardcount/internal/auth"
	"github.com/gethardcount/hardcount/internal/customer"
	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/testutil"
)

// T10 (ADR-129): the dashboard's credit liability is one figure per currency,
// the workspace default first, never a sum across currencies. Test-clock
// customers stay out.
//
// Mutation-verify: drop the GROUP BY currency → one row, red; drop the
// test-clock filter → EUR reads 10500, red.
func TestOverview_CreditBalanceByCurrency(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Analytics Credit Currency")
	execBypass(t, db, `INSERT INTO tenant_settings (tenant_id, default_currency) VALUES ($1, 'EUR')
		ON CONFLICT (tenant_id) DO UPDATE SET default_currency = 'EUR'`, tenantID)

	customers := customer.NewPostgresStore(db)
	newCustomer := func(ext string) string {
		t.Helper()
		c, err := customers.Create(ctx, tenantID, domain.Customer{ExternalID: ext, DisplayName: ext})
		if err != nil {
			t.Fatalf("create customer %s: %v", ext, err)
		}
		return c.ID
	}
	seed := func(customerID, currency string, cents int64) {
		t.Helper()
		tx, err := db.BeginTx(ctx, postgres.TxTenant, tenantID)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer postgres.Rollback(tx)
		if _, err := tx.Exec(`INSERT INTO customer_credit_ledger (id, tenant_id, customer_id, entry_type, currency, amount_cents, balance_after, description)
			VALUES ($1, $2, $3, 'grant', $4, $5, $5, 'seed')`, postgres.NewID("hc_ccl"), tenantID, customerID, currency, cents); err != nil {
			t.Fatalf("seed %s ledger row: %v", currency, err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("commit: %v", err)
		}
	}
	usd := newCustomer("cus_an_usd")
	seed(usd, "USD", 3000)
	eur := newCustomer("cus_an_eur")
	seed(eur, "EUR", 1000)
	seed(eur, "EUR", 500)
	sim := newCustomer("cus_an_sim")
	execBypass(t, db, `UPDATE customers SET test_clock_id = $1 WHERE id = $2`, seedClock(t, db, tenantID), sim)
	seed(sim, "EUR", 9000)

	req := httptest.NewRequest("GET", "/overview?period=30d", nil)
	req = req.WithContext(auth.WithTenantID(postgres.WithLivemode(req.Context(), false), tenantID))
	rr := httptest.NewRecorder()
	NewHandler(db).overview(rr, req)
	if rr.Code != 200 {
		t.Fatalf("overview status %d: %s", rr.Code, rr.Body.String())
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, ok := body["credit_balance_total"]; ok {
		t.Error("credit_balance_total is still in the response: it added currencies together")
	}
	var got []struct {
		Currency    string `json:"currency"`
		AmountCents int64  `json:"amount_cents"`
	}
	if err := json.Unmarshal(body["credit_balance_by_currency"], &got); err != nil {
		t.Fatalf("credit_balance_by_currency: %v (body %s)", err, rr.Body.String())
	}
	if len(got) != 2 || got[0].Currency != "EUR" || got[0].AmountCents != 1500 || got[1].Currency != "USD" || got[1].AmountCents != 3000 {
		t.Errorf("credit_balance_by_currency = %+v, want [EUR 1500, USD 3000] (the default currency first, test clocks out)", got)
	}
}
