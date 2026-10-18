package usage_test

import (
	"context"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/testutil"
	"github.com/sagarsuperuser/velox/internal/usage"
)

// usage-events.csv ships an `origin` column, and until now it was ALWAYS empty:
// the export borrowed usage.List, whose SELECT does not carry `origin` (Get and
// Ingest do). Nothing failed — the field was just blank in every export ever
// taken, so the artifact finance reconciles against could not distinguish an
// operator BACKFILL from a metered API event. That distinction is the whole
// reason the column exists (ADR-090 audits backfill precisely because a
// backdated insert changes what a customer is billed).
//
// The export's columns are its own contract; this is the lock on it.
func TestStreamForExport_CarriesOrigin(t *testing.T) {
	db := testutil.SetupTestDB(t)
	store := usage.NewPostgresStore(db)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Export Origin")

	customerID := insertTestCustomer(t, db, tenantID, "cus_export_origin")
	meterID := insertTestMeter(t, db, tenantID, "mtr_export_origin", "origin_calls")

	at := time.Now().UTC().Add(-2 * time.Hour)
	svc := usage.NewService(store)

	if _, err := svc.Ingest(ctx, tenantID, usage.IngestInput{
		CustomerID: customerID, MeterID: meterID, Quantity: decimal.NewFromInt(1),
	}); err != nil {
		t.Fatalf("live ingest: %v", err)
	}
	if _, err := svc.Backfill(ctx, tenantID, usage.IngestInput{
		CustomerID: customerID, MeterID: meterID, Quantity: decimal.NewFromInt(2),
		Timestamp: &at,
	}); err != nil {
		t.Fatalf("backfill: %v", err)
	}

	from := time.Now().UTC().Add(-24 * time.Hour)
	to := time.Now().UTC().Add(time.Hour)

	byOrigin := map[domain.UsageEventOrigin]int{}
	if err := store.StreamForExport(ctx, tenantID, from, to, func(e domain.UsageEvent) error {
		byOrigin[e.Origin]++
		return nil
	}); err != nil {
		t.Fatalf("StreamForExport: %v", err)
	}

	if byOrigin[domain.UsageOriginAPI] != 1 {
		t.Errorf("origin=api rows: got %d, want 1 — the export's origin column is blank or wrong (%+v)", byOrigin[domain.UsageOriginAPI], byOrigin)
	}
	if byOrigin[domain.UsageOriginBackfill] != 1 {
		t.Errorf("origin=backfill rows: got %d, want 1 — an operator's BACKDATED insert is indistinguishable from metered usage in the artifact finance reconciles with (%+v)", byOrigin[domain.UsageOriginBackfill], byOrigin)
	}
	if byOrigin[""] != 0 {
		t.Errorf("%d exported rows carry an EMPTY origin — this is the bug: the column was promised and never filled", byOrigin[""])
	}
}

// The dashboard's Export CSV now streams from the server with the page's
// filters. Each filter must narrow the file exactly as it narrows the list,
// or the operator downloads rows the page never showed.
func TestStreamForExportFiltered_MatchesPageFilters(t *testing.T) {
	db := testutil.SetupTestDB(t)
	store := usage.NewPostgresStore(db)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Export Filters")
	custA := insertTestCustomer(t, db, tenantID, "cus_exp_a")
	custB := insertTestCustomer(t, db, tenantID, "cus_exp_b")
	mX := insertTestMeter(t, db, tenantID, "mtr_exp_x", "exp_x")
	mY := insertTestMeter(t, db, tenantID, "mtr_exp_y", "exp_y")

	at := time.Now().UTC().Add(-time.Hour)
	n := decimal.NewFromInt
	ingestAt(t, ctx, store, tenantID, custA, mX, n(1), map[string]any{"model": "gpt-4o", "token_type": "input"}, at)
	ingestAt(t, ctx, store, tenantID, custA, mX, n(2), map[string]any{"model": "gpt-4o", "token_type": "output"}, at)
	ingestAt(t, ctx, store, tenantID, custA, mY, n(3), nil, at)
	ingestAt(t, ctx, store, tenantID, custB, mX, n(4), map[string]any{"model": "gpt-4o-mini"}, at)

	from := time.Now().UTC().Add(-24 * time.Hour)
	to := time.Now().UTC().Add(time.Hour)
	sum := func(f usage.ExportFilter) string {
		total := decimal.Zero
		if err := store.StreamForExportFiltered(ctx, tenantID, from, to, f, func(e domain.UsageEvent) error {
			total = total.Add(e.Quantity)
			return nil
		}); err != nil {
			t.Fatalf("export %+v: %v", f, err)
		}
		return total.String()
	}
	cases := []struct {
		name string
		f    usage.ExportFilter
		want string
	}{
		{"no filter", usage.ExportFilter{}, "10"},
		{"customer", usage.ExportFilter{CustomerID: custA}, "6"},
		{"meter", usage.ExportFilter{MeterID: mX}, "7"},
		{"dimension", usage.ExportFilter{Dimensions: map[string]any{"model": "gpt-4o"}}, "3"},
		{"dimensions AND", usage.ExportFilter{Dimensions: map[string]any{"model": "gpt-4o", "token_type": "output"}}, "2"},
		{"all three", usage.ExportFilter{CustomerID: custB, MeterID: mX, Dimensions: map[string]any{"model": "gpt-4o-mini"}}, "4"},
	}
	for _, c := range cases {
		if got := sum(c.f); got != c.want {
			t.Errorf("%s: exported sum %s, want %s", c.name, got, c.want)
		}
	}
}
