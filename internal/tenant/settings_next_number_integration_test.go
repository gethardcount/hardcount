package tenant

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gethardcount/hardcount/internal/auth"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/testutil"
)

// Sweep C10 (2026-10-21): the invoice-prefix preview always showed -000001,
// whatever the tenant had already issued. The settings response now carries
// the number the next invoice gets in the request's mode, so the preview can
// show the real next number. Test and live count separately.
func TestSettingsResponse_NextInvoiceSequencePerMode(t *testing.T) {
	db := testutil.SetupTestDB(t)
	tenantID := testutil.CreateTestTenant(t, db, "Settings Next Number")
	store := NewSettingsStore(db)
	h := NewSettingsHandler(store)

	testCtx := postgres.WithLivemode(context.Background(), false)
	for i := 0; i < 2; i++ {
		if _, err := store.NextInvoiceNumber(testCtx, tenantID); err != nil {
			t.Fatalf("allocate test invoice number: %v", err)
		}
	}

	call := func(method, body string, livemode bool) int {
		t.Helper()
		var req *http.Request
		if body == "" {
			req = httptest.NewRequest(method, "/v1/settings", nil)
		} else {
			req = httptest.NewRequest(method, "/v1/settings", strings.NewReader(body))
		}
		ctx := context.WithValue(req.Context(), auth.TestTenantIDKey(), tenantID)
		req = req.WithContext(postgres.WithLivemode(ctx, livemode))
		rr := httptest.NewRecorder()
		if method == http.MethodGet {
			h.get(rr, req)
		} else {
			h.upsert(rr, req)
		}
		if rr.Code != http.StatusOK {
			t.Fatalf("%s status %d: %s", method, rr.Code, rr.Body.String())
		}
		var got struct {
			NextInvoiceSequence *int `json:"next_invoice_sequence"`
		}
		if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if got.NextInvoiceSequence == nil {
			t.Fatalf("%s response has no next_invoice_sequence: %s", method, rr.Body.String())
		}
		return *got.NextInvoiceSequence
	}

	if got := call(http.MethodGet, "", false); got != 3 {
		t.Errorf("test mode GET next_invoice_sequence = %d, want 3 (two issued)", got)
	}
	if got := call(http.MethodGet, "", true); got != 1 {
		t.Errorf("live mode GET next_invoice_sequence = %d, want 1 (none issued in live)", got)
	}
	// A prefix change keeps the count: the save's response says so too.
	if got := call(http.MethodPut, `{"invoice_prefix":"ACME"}`, false); got != 3 {
		t.Errorf("test mode PUT next_invoice_sequence = %d, want 3", got)
	}
	num, err := store.NextInvoiceNumber(testCtx, tenantID)
	if err != nil {
		t.Fatalf("allocate after prefix change: %v", err)
	}
	if num != "ACME-000003" {
		t.Errorf("next number after prefix change = %q, want ACME-000003", num)
	}
}
