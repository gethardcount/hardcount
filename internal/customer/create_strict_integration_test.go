package customer_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sagarsuperuser/velox/internal/auth"
	"github.com/sagarsuperuser/velox/internal/customer"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

// TestCustomerCreateAndStatus_Validated covers two refusals on the customer
// routes:
//   - A-S2: POST /v1/customers with dunning_policy_id was a 201 that dropped
//     the policy, so the customer silently ran the default policy. Create has
//     no such field; the key is now a 422 naming it, and PATCH stays the one
//     validated assign path.
//   - D-09: PATCH with a status outside {active, archived} reached the
//     customers_status_check constraint and came back a 500, which the
//     idempotency middleware caches under the key. It is now a 422 and the
//     row is unchanged.
func TestCustomerCreateAndStatus_Validated(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Customer Strict Create")

	svc := customer.NewService(customer.NewPostgresStore(db))
	srv := customer.NewHandler(svc).Routes()
	do := func(method, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		srv.ServeHTTP(rr, req.WithContext(context.WithValue(ctx, auth.TestTenantIDKey(), tenantID)))
		return rr
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
	count := func(externalID string) int {
		tx, err := db.BeginTx(ctx, postgres.TxTenant, tenantID)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer postgres.Rollback(tx)
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM customers WHERE external_id = $1`, externalID).Scan(&n); err != nil {
			t.Fatalf("count customers: %v", err)
		}
		return n
	}

	rr := do(http.MethodPost, "/", `{"external_id":"cus_dpol","display_name":"Dunning","dunning_policy_id":"vlx_dpol_x"}`)
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("dunning_policy_id on create: status %d, want 422; body %s", rr.Code, rr.Body.String())
	}
	if p := param(rr); p != "dunning_policy_id" {
		t.Errorf("param: got %q, want dunning_policy_id", p)
	}
	if n := count("cus_dpol"); n != 0 {
		t.Fatalf("refused create wrote %d customer rows", n)
	}

	// Control: the known body is created.
	rr = do(http.MethodPost, "/", `{"external_id":"cus_ok","display_name":"Known"}`)
	if rr.Code != http.StatusCreated {
		t.Fatalf("known body: status %d, want 201; body %s", rr.Code, rr.Body.String())
	}
	var cust domain.Customer
	if err := json.Unmarshal(rr.Body.Bytes(), &cust); err != nil {
		t.Fatalf("decode customer: %v", err)
	}

	for _, status := range []string{"inactive", "Archived"} {
		rr = do(http.MethodPatch, "/"+cust.ID, `{"status":"`+status+`"}`)
		if rr.Code != http.StatusUnprocessableEntity {
			t.Fatalf("PATCH status %q: status %d, want 422; body %s", status, rr.Code, rr.Body.String())
		}
		if p := param(rr); p != "status" {
			t.Errorf("PATCH status %q: param %q, want status", status, p)
		}
	}
	got, err := svc.Get(ctx, tenantID, cust.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Status != domain.CustomerStatusActive {
		t.Fatalf("status after refused PATCHes: %q, want active", got.Status)
	}
}

// TestBillingProfileUpsert_RefusesUnknownField: PUT
// /v1/customers/{id}/billing-profile dropped Stripe's `tax_exempt:"exempt"`,
// so tax_status kept its default and an exempt customer was taxed. The
// unknown key is now a 422 naming it and the profile is unchanged. Control: a
// body read back from GET (read-only fields included) is still accepted.
func TestBillingProfileUpsert_RefusesUnknownField(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Billing Profile Strict")

	svc := customer.NewService(customer.NewPostgresStore(db))
	srv := customer.NewHandler(svc).Routes()
	do := func(method, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		srv.ServeHTTP(rr, req.WithContext(context.WithValue(ctx, auth.TestTenantIDKey(), tenantID)))
		return rr
	}

	cust, err := svc.Create(ctx, tenantID, customer.CreateInput{ExternalID: "cus_bp", DisplayName: "Profile"})
	if err != nil {
		t.Fatalf("create customer: %v", err)
	}
	path := "/" + cust.ID + "/billing-profile"

	rr := do(http.MethodPut, path, `{"legal_name":"Exempt Co","country":"US","tax_exempt":"exempt"}`)
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("tax_exempt: status %d, want 422; body %s", rr.Code, rr.Body.String())
	}
	var env struct {
		Error struct {
			Param string `json:"param"`
		} `json:"error"`
	}
	_ = json.Unmarshal(rr.Body.Bytes(), &env)
	if env.Error.Param != "tax_exempt" {
		t.Errorf("param: got %q, want tax_exempt", env.Error.Param)
	}
	if rr = do(http.MethodGet, path, ""); rr.Code != http.StatusNotFound {
		t.Fatalf("refused upsert wrote a profile: GET status %d; body %s", rr.Code, rr.Body.String())
	}

	// Control: a known body saves, and the GET body sent back as-is saves too.
	rr = do(http.MethodPut, path, `{"legal_name":"Exempt Co","country":"US","tax_status":"exempt","tax_exempt_reason":"501(c)(3)"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("known body: status %d, want 200; body %s", rr.Code, rr.Body.String())
	}
	rr = do(http.MethodGet, path, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("GET profile: status %d; body %s", rr.Code, rr.Body.String())
	}
	rr = do(http.MethodPut, path, rr.Body.String())
	if rr.Code != http.StatusOK {
		t.Fatalf("GET body sent back: status %d, want 200; body %s", rr.Code, rr.Body.String())
	}
	var bp domain.CustomerBillingProfile
	if err := json.Unmarshal(rr.Body.Bytes(), &bp); err != nil {
		t.Fatalf("decode profile: %v", err)
	}
	if bp.TaxStatus != domain.TaxStatusExempt {
		t.Fatalf("tax_status after round-trip: %q, want exempt", bp.TaxStatus)
	}
}
