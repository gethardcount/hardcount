package analytics

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/auth"
	"github.com/sagarsuperuser/velox/internal/customer"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/invoice"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/pricing"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

// openNewYorkDB returns an app-role DB whose only connection has its session
// TimeZone set to America/New_York — a Postgres server not configured for UTC.
// One connection, so every handler tx runs on the session that was SET.
func openNewYorkDB(t *testing.T) *postgres.DB {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		url = "postgres://velox_test_app:velox_test_app@localhost:5432/velox_test?sslmode=disable"
	}
	pool, err := sql.Open("pgx", url)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = pool.Close() })
	pool.SetMaxOpenConns(1)
	pool.SetMaxIdleConns(1)
	if _, err := pool.ExecContext(context.Background(), `SET TimeZone = 'America/New_York'`); err != nil {
		t.Fatalf("set timezone: %v", err)
	}
	return postgres.NewDB(pool, 5*time.Second)
}

// Analytics buckets are UTC (handler.go). date_trunc/to_char on a timestamptz
// use the SESSION TimeZone, so on a server running America/New_York a row at
// 02:00Z (22:00 the previous evening in New York) was bucketed one day early.
// Locks revenue-chart, usage and mrr-movement (new MRR) buckets.
func TestAnalyticsBuckets_AreUTCWhateverTheSessionTimeZone(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "UTC Buckets")

	// 02:00Z on the latest UTC day for which that instant is already past,
	// so it falls inside the handler's trailing 30-day window.
	now := time.Now().UTC()
	at := time.Date(now.Year(), now.Month(), now.Day(), 2, 0, 0, 0, time.UTC)
	if at.After(now.Add(-time.Minute)) {
		at = at.AddDate(0, 0, -1)
	}
	wantBucket := at.Format("2006-01-02")

	// Revenue: one paid USD invoice at `at`.
	cust, err := customer.NewPostgresStore(db).Create(ctx, tenantID, domain.Customer{
		ExternalID: "cus_utc_buckets", DisplayName: "UTC Buckets",
	})
	if err != nil {
		t.Fatalf("create customer: %v", err)
	}
	inv := makePaidInvoice(t, ctx, invoice.NewPostgresStore(db), tenantID, cust.ID, "USD", 123_00)
	markPaidAt(t, db, inv.ID, at)

	// Usage: one event at `at`.
	meter := seedMeter(t, db, tenantID, "UTC Meter", "k_utc_buckets")
	seedUsageEvent(t, db, tenantID, cust.ID, meter, 7, at)

	// MRR: one subscription activated at `at` with a $10 monthly item.
	plan, err := pricing.NewPostgresStore(db).CreatePlan(ctx, tenantID, domain.Plan{
		Code: "pln-utc", Name: "pln-utc", Currency: "USD",
		BillingInterval: domain.BillingMonthly, Status: domain.PlanActive,
		BaseAmountCents: 1000, MeterIDs: []string{},
	})
	if err != nil {
		t.Fatalf("create plan: %v", err)
	}
	subID := postgres.NewID("vlx_sub")
	execInTenantTx(t, db, tenantID, `
		INSERT INTO subscriptions (id, tenant_id, code, display_name, customer_id, status, billing_time,
			activated_at, current_billing_period_start, current_billing_period_end, next_billing_at, created_at, updated_at)
		VALUES ($1, $2, 'sub-utc', 'sub-utc', $3, 'active', 'anniversary', $4, $4, $5, $5, $4, $4)
	`, subID, tenantID, cust.ID, at, at.AddDate(0, 1, 0))
	execInTenantTx(t, db, tenantID, `
		INSERT INTO subscription_items (id, tenant_id, subscription_id, plan_id, quantity, metadata, created_at, updated_at)
		VALUES ($1, $2, $3, $4, 1, '{}'::jsonb, $5, $5)
	`, postgres.NewID("vlx_si"), tenantID, subID, plan.ID, at)

	nyDB := openNewYorkDB(t)
	// Control: the handler's own tx really runs on a New York session.
	{
		tx, err := nyDB.BeginTx(ctx, postgres.TxTenant, tenantID)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		var tz string
		if err := tx.QueryRowContext(ctx, `SHOW TimeZone`).Scan(&tz); err != nil {
			t.Fatalf("show timezone: %v", err)
		}
		postgres.Rollback(tx)
		if tz != "America/New_York" {
			t.Fatalf("session TimeZone = %q, want America/New_York — the test would prove nothing", tz)
		}
	}
	h := NewHandler(nyDB)

	call := func(fn func(http.ResponseWriter, *http.Request), path string, out any) {
		t.Helper()
		req := httptest.NewRequest("GET", path, nil)
		req = req.WithContext(auth.WithTenantID(postgres.WithLivemode(req.Context(), false), tenantID))
		rr := httptest.NewRecorder()
		fn(rr, req)
		if rr.Code != 200 {
			t.Fatalf("%s: status %d body=%s", path, rr.Code, rr.Body.String())
		}
		if err := json.Unmarshal(rr.Body.Bytes(), out); err != nil {
			t.Fatalf("%s: decode: %v", path, err)
		}
	}

	var rev struct {
		Data []RevenueDataPoint `json:"data"`
	}
	call(h.revenueChart, "/revenue-chart?period=30d", &rev)
	if len(rev.Data) != 1 || rev.Data[0].Date != wantBucket || rev.Data[0].RevenueCents != 123_00 {
		t.Errorf("revenue-chart: got %+v, want one bucket %s with 12300 cents (payment at %s)", rev.Data, wantBucket, at.Format(time.RFC3339))
	}

	var use struct {
		Data []UsagePoint `json:"data"`
	}
	call(h.usageAnalytics, "/usage?period=30d", &use)
	if len(use.Data) != 1 || use.Data[0].Date != wantBucket {
		t.Errorf("usage: got %+v, want one bucket %s (event at %s)", use.Data, wantBucket, at.Format(time.RFC3339))
	}

	var mrr struct {
		Data []MRRMovementPoint `json:"data"`
	}
	call(h.mrrMovement, "/mrr-movement?period=30d", &mrr)
	if len(mrr.Data) != 1 || mrr.Data[0].Date != wantBucket || mrr.Data[0].New != 1000 {
		t.Errorf("mrr-movement: got %+v, want one bucket %s with new=1000 (activated at %s)", mrr.Data, wantBucket, at.Format(time.RFC3339))
	}
}
