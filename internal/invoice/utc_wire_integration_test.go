package invoice_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/customer"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/invoice"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

// TestInvoice_Timestamps_SerializeAsCanonicalUTC locks the ADR-075 wire contract:
// a timestamp that round-trips through Postgres (pgx decodes timestamptz via
// time.Unix → time.Local) must serialize on the API wire as canonical "…Z", not a
// host-timezone offset like "+05:30". SetupTestDB pins the process to UTC exactly
// as cmd/hardcount/main does, so this observes production behavior on any host.
//
// The DB round-trip is essential: the in-memory store echoes whatever the caller
// built (already UTC via clock.Now().UTC()), so only a value scanned back out of
// Postgres exercises the decode path this ADR fixes.
//
// Mutation-verify: comment out `time.Local = time.UTC` in testutil.SetupTestDB and
// run on a non-UTC host — created_at scans back with a local offset and the
// HasSuffix("Z") assertions fail.
func TestInvoice_Timestamps_SerializeAsCanonicalUTC(t *testing.T) {
	db := testutil.SetupTestDB(t) // skips on -short; pins process to UTC
	ctx := postgres.WithLivemode(context.Background(), false)

	custStore := customer.NewPostgresStore(db)
	invStore := invoice.NewPostgresStore(db)

	tenantID := testutil.CreateTestTenant(t, db, "UTC Wire Corp")
	cust, err := custStore.Create(ctx, tenantID, domain.Customer{ExternalID: "cus_utc", DisplayName: "UTC Cust"})
	if err != nil {
		t.Fatalf("create customer: %v", err)
	}

	start := time.Date(2026, 5, 1, 18, 30, 0, 0, time.UTC)
	end := time.Date(2026, 5, 31, 18, 30, 0, 0, time.UTC)
	created, err := invStore.Create(ctx, tenantID, domain.Invoice{
		CustomerID:         cust.ID,
		InvoiceNumber:      "HC-UTC-1",
		Status:             domain.InvoiceDraft,
		PaymentStatus:      domain.PaymentPending,
		Currency:           "USD",
		BillingPeriodStart: start,
		BillingPeriodEnd:   end,
	})
	if err != nil {
		t.Fatalf("create invoice: %v", err)
	}

	// The value came straight back out of Postgres — its Location must be UTC,
	// not the host zone.
	if loc := created.CreatedAt.Location(); loc != time.UTC {
		t.Errorf("CreatedAt.Location() = %s, want UTC (DB read path must decode UTC under ADR-075)", loc)
	}

	// And the serialized wire form must be canonical "…Z", never "+HH:MM".
	blob, err := json.Marshal(created)
	if err != nil {
		t.Fatalf("marshal invoice: %v", err)
	}
	var wire map[string]any
	if err := json.Unmarshal(blob, &wire); err != nil {
		t.Fatalf("unmarshal wire: %v", err)
	}
	for _, field := range []string{"created_at", "updated_at", "billing_period_start", "billing_period_end"} {
		v, _ := wire[field].(string)
		if v == "" {
			t.Errorf("wire field %q missing/empty", field)
			continue
		}
		if !strings.HasSuffix(v, "Z") || strings.ContainsAny(v[len(v)-6:], "+") {
			t.Errorf("wire field %q = %q, want canonical UTC ending in 'Z' (no host offset)", field, v)
		}
	}
}
