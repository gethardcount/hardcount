package credit_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gethardcount/hardcount/internal/auth"
	"github.com/gethardcount/hardcount/internal/credit"
	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/testutil"
)

// TestListBalances_BalanceFilter: GET /v1/credits/balances?balance= picks
// the (customer, currency) rows by what is left, and total counts the rows
// picked, not every row.
//
// Seeded world: out (USD 0 after a grant and a deduction), minus (USD -200),
// low (USD 400), twice (USD 300 and EUR 5000), above (USD 1000, the
// threshold), and two archived customers, gone (USD 0) and goneLow (USD 100):
// a filter lists the balances that need the operator, so it leaves them out,
// and the unfiltered list keeps them.
func TestListBalances_BalanceFilter(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Credit Balance Filter")
	srv := credit.NewHandler(credit.NewService(credit.NewPostgresStore(db))).Routes()

	ledger := func(custID, currency string, cents int64) {
		t.Helper()
		tx, err := db.BeginTx(ctx, postgres.TxTenant, tenantID)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer postgres.Rollback(tx)
		if _, err := tx.Exec(`INSERT INTO customer_credit_ledger (id, tenant_id, customer_id, entry_type, currency, amount_cents, balance_after, description)
			VALUES ($1, $2, $3, CASE WHEN $5::bigint >= 0 THEN 'grant' ELSE 'adjustment' END, $4, $5, 0, 'seed')`,
			postgres.NewID("hc_ccl"), tenantID, custID, currency, cents); err != nil {
			t.Fatalf("seed ledger: %v", err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("commit: %v", err)
		}
	}
	out := seedCustomer(t, db, ctx, tenantID, "cus_bf_out")
	ledger(out, "USD", 500)
	ledger(out, "USD", -500)
	minus := seedCustomer(t, db, ctx, tenantID, "cus_bf_minus")
	ledger(minus, "USD", -200)
	low := seedCustomer(t, db, ctx, tenantID, "cus_bf_low")
	ledger(low, "USD", 400)
	twice := seedCustomer(t, db, ctx, tenantID, "cus_bf_twice")
	ledger(twice, "USD", 300)
	ledger(twice, "EUR", 5000)
	above := seedCustomer(t, db, ctx, tenantID, "cus_bf_above")
	ledger(above, "USD", 1000)
	gone := seedCustomer(t, db, ctx, tenantID, "cus_bf_gone")
	ledger(gone, "USD", 500)
	ledger(gone, "USD", -500)
	goneLow := seedCustomer(t, db, ctx, tenantID, "cus_bf_gone_low")
	ledger(goneLow, "USD", 100)
	archive, err := db.BeginTx(ctx, postgres.TxBypass, "")
	if err != nil {
		t.Fatalf("begin bypass: %v", err)
	}
	if _, err := archive.Exec(`UPDATE customers SET status = 'archived' WHERE id IN ($1, $2)`, gone, goneLow); err != nil {
		t.Fatalf("archive: %v", err)
	}
	if err := archive.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	type page struct {
		Data  []domain.CreditBalance `json:"data"`
		Total int                    `json:"total"`
	}
	get := func(query string) (int, page) {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/balances"+query, nil)
		rr := httptest.NewRecorder()
		srv.ServeHTTP(rr, req.WithContext(auth.WithTenantID(ctx, tenantID)))
		var p page
		if rr.Code == http.StatusOK {
			if err := json.Unmarshal(rr.Body.Bytes(), &p); err != nil {
				t.Fatalf("%s: decode: %v", query, err)
			}
		}
		return rr.Code, p
	}
	rows := func(p page) map[string]int64 {
		m := map[string]int64{}
		for _, b := range p.Data {
			m[b.CustomerID+"/"+b.Currency] = b.BalanceCents
		}
		return m
	}

	if code, p := get(""); code != http.StatusOK || p.Total != 8 || len(p.Data) != 8 {
		t.Fatalf("no filter: %d, total %d, %d rows; want 200 and every one of 8 rows", code, p.Total, len(p.Data))
	}

	code, p := get("?balance=out&limit=1")
	if code != http.StatusOK || p.Total != 2 || len(p.Data) != 1 {
		t.Fatalf("balance=out&limit=1: %d, total %d, %d rows; want 200, total 2 (the filtered set, without the archived customer), one row", code, p.Total, len(p.Data))
	}
	if _, p = get("?balance=out"); len(rows(p)) != 2 || rows(p)[out+"/USD"] != 0 || rows(p)[minus+"/USD"] != -200 {
		t.Errorf("balance=out: %+v; want out's USD 0 and minus's USD -200", p.Data)
	}

	// No threshold set: low is an empty list.
	if code, p = get("?balance=low"); code != http.StatusOK || p.Total != 0 || len(p.Data) != 0 {
		t.Errorf("balance=low with no threshold: %d, total %d, %+v; want 200 and an empty list", code, p.Total, p.Data)
	}

	tx, err := db.BeginTx(ctx, postgres.TxBypass, "")
	if err != nil {
		t.Fatalf("begin bypass: %v", err)
	}
	if _, err := tx.Exec(`INSERT INTO tenant_settings (tenant_id, credit_balance_low_threshold_cents) VALUES ($1, 1000)
		ON CONFLICT (tenant_id) DO UPDATE SET credit_balance_low_threshold_cents = 1000`, tenantID); err != nil {
		t.Fatalf("set threshold: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if code, p = get("?balance=low"); code != http.StatusOK || p.Total != 2 || len(rows(p)) != 2 ||
		rows(p)[low+"/USD"] != 400 || rows(p)[twice+"/USD"] != 300 {
		t.Errorf("balance=low: %d, total %d, %+v; want low's USD 400 and twice's USD 300 (not EUR 5000, not 1000 at the threshold, not the archived customer)", code, p.Total, p.Data)
	}

	if code, _ = get("?balance=empty"); code != http.StatusUnprocessableEntity {
		t.Errorf("balance=empty: %d, want 422", code)
	}
}
