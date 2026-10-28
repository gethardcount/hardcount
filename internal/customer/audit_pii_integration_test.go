package customer_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/audit"
	"github.com/gethardcount/hardcount/internal/auth"
	"github.com/gethardcount/hardcount/internal/customer"
	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/testutil"
)

// TestAudit_NoCustomerPIIInMetadata pins the PII-hygiene rule on the two
// structured customer audit writers, through the REAL wiring (handler →
// audit.Logger → Postgres): audit_log is append-only at the DB level, so a
// customer email or tax id copied into metadata could never be erased — a
// GDPR-deletion dead end. The rows must record THAT something changed
// (email_changed / tax_id_set) and link the customer; the values live only on
// the mutable, erasable customer/profile rows.
func TestAudit_NoCustomerPIIInMetadata(t *testing.T) {
	db := testutil.SetupTestDB(t)
	store := customer.NewPostgresStore(db)
	svc := customer.NewService(store)
	h := customer.NewHandler(svc)
	logger := audit.NewLogger(db)
	h.SetAuditLogger(logger)

	ctx, cancel := context.WithTimeout(postgres.WithLivemode(context.Background(), false), 15*time.Second)
	defer cancel()
	tenantID := testutil.CreateTestTenant(t, db, "PII Hygiene")
	ctx = auth.WithTenantID(ctx, tenantID)

	c, err := store.Create(ctx, tenantID, domain.Customer{
		ExternalID: "cus_pii", DisplayName: "PII Co", Email: "old@example.com",
	})
	if err != nil {
		t.Fatalf("seed customer: %v", err)
	}

	router := h.Routes()

	// 1. Update the customer's email through the handler.
	req := httptest.NewRequest(http.MethodPatch, "/"+c.ID,
		strings.NewReader(`{"email":"new-secret@example.com"}`)).WithContext(ctx)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("update: got %d: %s", rr.Code, rr.Body.String())
	}

	// 2. Upsert a billing profile with a tax id through the handler.
	req = httptest.NewRequest(http.MethodPut, "/"+c.ID+"/billing-profile",
		strings.NewReader(`{"legal_name":"PII Co GmbH","country":"DE","tax_status":"standard","tax_id_type":"eu_vat","tax_id":"DE123456789"}`)).WithContext(ctx)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("billing profile upsert: got %d: %s", rr.Code, rr.Body.String())
	}

	// Read back every audit row for this customer and assert the contract.
	entries, _, err := logger.Query(ctx, tenantID, audit.QueryFilter{
		ResourceType: "customer", ResourceID: c.ID,
	})
	if err != nil {
		t.Fatalf("query audit: %v", err)
	}
	if len(entries) < 2 {
		t.Fatalf("audit rows: got %d, want >= 2 (update + billing profile)", len(entries))
	}

	var sawEmailChanged, sawTaxIDSet bool
	for _, e := range entries {
		for k, v := range e.Metadata {
			sv, _ := v.(string)
			if strings.Contains(sv, "@") {
				t.Errorf("row %s metadata[%q] carries an email address (%q) — PII must not enter the append-only log", e.ID, k, sv)
			}
			if strings.Contains(sv, "DE123456789") {
				t.Errorf("row %s metadata[%q] carries the raw tax id — record tax_id_set instead", e.ID, k)
			}
		}
		if e.Metadata["email_changed"] == true {
			sawEmailChanged = true
		}
		if e.Metadata["tax_id_set"] == true {
			sawTaxIDSet = true
		}
	}
	if !sawEmailChanged {
		t.Error("expected an email_changed=true row for the customer update")
	}
	if !sawTaxIDSet {
		t.Error("expected a tax_id_set=true row for the billing-profile upsert")
	}
}
