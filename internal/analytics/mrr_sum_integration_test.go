package analytics

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/auth"
	"github.com/sagarsuperuser/velox/internal/customer"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/pricing"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

// The MRR movement report adds each day's new MRR into the period's total.
// A total past int64 cents fails the request; it is never answered with a
// wrapped figure. Two subscriptions of 5e18 cents a month each (not a
// realistic amount) activated on different days: each day fits, the total
// does not. One of them alone is answered as before.
func TestMRRMovement_TotalPastInt64Fails(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "MRR Sum")
	cust, err := customer.NewPostgresStore(db).Create(ctx, tenantID, domain.Customer{ExternalID: "cus_mrr_sum", DisplayName: "MRR"})
	if err != nil {
		t.Fatalf("create customer: %v", err)
	}
	pstore := pricing.NewPostgresStore(db)
	now := time.Now().UTC().Truncate(time.Microsecond)
	addSub := func(code string, daysAgo int) {
		t.Helper()
		p, err := pstore.CreatePlan(ctx, tenantID, domain.Plan{
			Code: "pln-" + code, Name: code, Currency: "USD", BillingInterval: domain.BillingMonthly,
			Status: domain.PlanActive, BaseAmountCents: 5_000_000_000_000_000_000,
		})
		if err != nil {
			t.Fatalf("create plan: %v", err)
		}
		at := now.Add(-time.Duration(daysAgo) * 24 * time.Hour)
		tx, err := db.BeginTx(ctx, postgres.TxTenant, tenantID)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer postgres.Rollback(tx)
		subID := postgres.NewID("hc_sub")
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO subscriptions (id, tenant_id, code, display_name, customer_id, status, billing_time,
				activated_at, current_billing_period_start, current_billing_period_end, next_billing_at, created_at, updated_at)
			VALUES ($1, $2, $3, $3, $4, 'active', 'anniversary', $5, $5, $6, $6, $5, $5)
		`, subID, tenantID, code, cust.ID, at, now.Add(24*time.Hour)); err != nil {
			t.Fatalf("insert sub: %v", err)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO subscription_items (id, tenant_id, subscription_id, plan_id, quantity, metadata, created_at, updated_at)
			VALUES ($1, $2, $3, $4, 1, '{}'::jsonb, $5, $5)
		`, postgres.NewID("hc_si"), tenantID, subID, p.ID, at); err != nil {
			t.Fatalf("insert item: %v", err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("commit: %v", err)
		}
	}
	get := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", "/mrr-movement?period=30d", nil)
		req = req.WithContext(auth.WithTenantID(postgres.WithLivemode(req.Context(), false), tenantID))
		rr := httptest.NewRecorder()
		NewHandler(db).mrrMovement(rr, req)
		return rr
	}

	addSub("one", 3)
	rr := get()
	if rr.Code != http.StatusOK {
		t.Fatalf("one subscription: status %d, body %s", rr.Code, rr.Body.String())
	}
	var resp struct {
		Totals MRRMovementTotals `json:"totals"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Totals.New != 5_000_000_000_000_000_000 || resp.Totals.Net != 5_000_000_000_000_000_000 {
		t.Errorf("totals %+v, want new and net 5000000000000000000", resp.Totals)
	}

	addSub("two", 5)
	if rr := get(); rr.Code != http.StatusInternalServerError {
		t.Errorf("total past int64: status %d, body %s; want 500", rr.Code, rr.Body.String())
	}
}
