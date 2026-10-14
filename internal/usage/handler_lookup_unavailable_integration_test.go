package usage_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sagarsuperuser/velox/internal/auth"
	"github.com/sagarsuperuser/velox/internal/customer"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/pricing"
	"github.com/sagarsuperuser/velox/internal/testutil"
	"github.com/sagarsuperuser/velox/internal/usage"
)

// TestIngestLookup_RealStoreContract proves the 422-versus-503 split against
// the REAL lookup stores (fake-fidelity rule): a customer that does not exist
// on a live database is errs.ErrNotFound → 422; the same lookup on a cancelled
// context never reaches a verdict and is a raw driver error → 503. If a store
// ever returned ErrNotFound for a failure to query, the second half goes red.
func TestIngestLookup_RealStoreContract(t *testing.T) {
	db := testutil.SetupTestDB(t)
	tenantID := testutil.CreateTestTenant(t, db, "Ingest Lookup 503 Contract")

	h := usage.NewHandler(
		usage.NewService(usage.NewPostgresStore(db)),
		customer.NewPostgresStore(db),
		pricing.NewService(pricing.NewPostgresStore(db)),
	)
	post := func(t *testing.T, ctx context.Context) (int, string) {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/",
			strings.NewReader(`{"external_customer_id":"cus_never_created","event_name":"api_call","quantity":1,"idempotency_key":"evt_lookup"}`))
		req = req.WithContext(auth.WithTenantID(ctx, tenantID))
		rec := httptest.NewRecorder()
		h.Routes().ServeHTTP(rec, req)
		return rec.Code, rec.Body.String()
	}

	live := postgres.WithLivemode(context.Background(), false)
	code, body := post(t, live)
	if code != http.StatusUnprocessableEntity || !strings.Contains(body, "not found") {
		t.Fatalf("unknown customer on a live DB: status %d body %s, want 422 naming the customer", code, body)
	}

	cancelled, cancel := context.WithCancel(live)
	cancel() // the lookup can no longer be answered
	code, body = post(t, cancelled)
	if code != http.StatusServiceUnavailable || !strings.Contains(body, "ingest_unavailable") {
		t.Fatalf("cancelled lookup: status %d body %s, want 503 ingest_unavailable (a failed lookup must not masquerade as not-found)", code, body)
	}
	if strings.Contains(body, "not found") || strings.Contains(body, "context canceled") {
		t.Fatalf("cancelled lookup: body leaks or misclassifies: %s", body)
	}
}
