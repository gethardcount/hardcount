package usage_test

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/sagarsuperuser/velox/internal/api/timefilter"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/testutil"
	"github.com/sagarsuperuser/velox/internal/usage"
)

// usage-events.csv?from=2026-09-01&to=2026-09-30 must include an event
// stamped 2026-09-30T23:59:59.4Z. The export compares `timestamp <= to`,
// and a date-only `to` used to parse to 23:59:59.000Z, so every event in
// the last second of the named day was silently missing from the file.
// Postgres keeps microseconds, so the bound must be 23:59:59.999999Z.
func TestStreamForExport_DateOnlyToIncludesLastSecondOfDay(t *testing.T) {
	db := testutil.SetupTestDB(t)
	store := usage.NewPostgresStore(db)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Export End Of Day")

	customerID := insertTestCustomer(t, db, tenantID, "cus_export_eod")
	meterID := insertTestMeter(t, db, tenantID, "mtr_export_eod", "eod_calls")
	svc := usage.NewService(store)

	lastSecond := time.Date(2026, 9, 30, 23, 59, 59, 400_000_000, time.UTC)
	nextDay := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	for _, at := range []time.Time{lastSecond, nextDay} {
		at := at
		if _, err := svc.Backfill(ctx, tenantID, usage.IngestInput{
			CustomerID: customerID, MeterID: meterID, Quantity: decimal.NewFromInt(1),
			Timestamp: &at,
		}); err != nil {
			t.Fatalf("backfill %s: %v", at.Format(time.RFC3339Nano), err)
		}
	}

	// Same parse the export handler runs on ?from=&to=.
	r := httptest.NewRequest("GET", "/v1/exports/usage-events.csv?from=2026-09-01&to=2026-09-30", nil)
	from, to, err := timefilter.ParseRange(r, "from", "to")
	if err != nil {
		t.Fatalf("ParseRange: %v", err)
	}

	var got []time.Time
	if err := store.StreamForExport(ctx, tenantID, from, to, func(e domain.UsageEvent) error {
		got = append(got, e.Timestamp.UTC())
		return nil
	}); err != nil {
		t.Fatalf("StreamForExport: %v", err)
	}

	if len(got) != 1 || !got[0].Equal(lastSecond) {
		t.Fatalf("exported timestamps for to=2026-09-30: got %v, want exactly [%s] (to parsed as %s)",
			got, lastSecond.Format(time.RFC3339Nano), to.Format(time.RFC3339Nano))
	}
}
