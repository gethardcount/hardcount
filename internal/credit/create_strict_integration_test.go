package credit_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gethardcount/hardcount/internal/api/middleware"
	"github.com/gethardcount/hardcount/internal/auth"
	"github.com/gethardcount/hardcount/internal/credit"
	"github.com/gethardcount/hardcount/internal/customer"
	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/testutil"
)

// TestGrantAndAdjust_UnknownFieldRefused: a grant sent with `expiry` instead
// of `expires_at` was decoded without it, so the customer got a grant that
// never expires and a 201. POST /v1/credits/grant and /adjust now refuse an
// unknown key with a 422 naming it, and write no ledger row. The dashboard
// sends an Idempotency-Key on both routes, so the refusal must be a status the
// idempotency middleware does not cache (422, not 400).
func TestGrantAndAdjust_UnknownFieldRefused(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Credit Strict Create")

	custs := customer.NewPostgresStore(db)
	cust, err := custs.Create(ctx, tenantID, domain.Customer{ExternalID: "cus_credit_strict", DisplayName: "Strict"})
	if err != nil {
		t.Fatalf("create customer: %v", err)
	}
	svc := credit.NewService(credit.NewPostgresStore(db))
	svc.SetCustomerReader(custs)
	srv := middleware.Idempotency(db, nil)(credit.NewHandler(svc).Routes())

	post := func(path, body, key string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		if key != "" {
			req.Header.Set("Idempotency-Key", key)
		}
		rr := httptest.NewRecorder()
		srv.ServeHTTP(rr, req.WithContext(context.WithValue(ctx, auth.TestTenantIDKey(), tenantID)))
		return rr
	}
	ledgerRows := func() int {
		tx, err := db.BeginTx(ctx, postgres.TxTenant, tenantID)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer postgres.Rollback(tx)
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM customer_credit_ledger WHERE customer_id = $1`, cust.ID).Scan(&n); err != nil {
			t.Fatalf("count ledger: %v", err)
		}
		return n
	}
	param := func(rr *httptest.ResponseRecorder) string {
		var env struct {
			Error struct {
				Param string `json:"param"`
			} `json:"error"`
		}
		_ = json.Unmarshal(rr.Body.Bytes(), &env)
		return env.Error.Param
	}

	base := `"customer_id":"` + cust.ID + `","currency":"USD","amount_cents":5000,"description":"promo"`

	rr := post("/grant", `{`+base+`,"expiry":"2027-01-01T00:00:00Z"}`, "idem-grant-1")
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("grant with expiry: status %d, want 422; body %s", rr.Code, rr.Body.String())
	}
	if p := param(rr); p != "expiry" {
		t.Errorf("grant param: got %q, want expiry", p)
	}
	rr = post("/adjust", `{`+base+`,"reason":"typo"}`, "idem-adjust-1")
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("adjust with reason: status %d, want 422; body %s", rr.Code, rr.Body.String())
	}
	if p := param(rr); p != "reason" {
		t.Errorf("adjust param: got %q, want reason", p)
	}
	if n := ledgerRows(); n != 0 {
		t.Fatalf("refused grant/adjust wrote %d ledger rows", n)
	}

	// Control: the corrected grant under the same key is created, with its expiry.
	rr = post("/grant", `{`+base+`,"expires_at":"2027-01-01T00:00:00Z"}`, "idem-grant-1")
	if rr.Code != http.StatusCreated {
		t.Fatalf("corrected grant: status %d, want 201; body %s", rr.Code, rr.Body.String())
	}
	var entry domain.CreditLedgerEntry
	if err := json.Unmarshal(rr.Body.Bytes(), &entry); err != nil {
		t.Fatalf("decode grant: %v", err)
	}
	if entry.ExpiresAt == nil {
		t.Fatal("corrected grant has no expiry")
	}
	if n := ledgerRows(); n != 1 {
		t.Fatalf("ledger rows after the corrected grant: %d, want 1", n)
	}
}
