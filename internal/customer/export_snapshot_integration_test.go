package customer_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/customer"
	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/testutil"
)

// TestStreamForExport_IsASnapshot is the regression lock for issue #475 — the
// bug that let the ONE artifact an operator hands to an auditor lie.
//
// The exports used to page each store with LIMIT/OFFSET, every page in its own
// transaction, over a newest-first ordering. A row inserted while the export ran
// entered at the HEAD of that ordering and shifted every later row down by one —
// so the row sitting on each page boundary was written to the CSV TWICE, and a
// delete symmetrically SKIPPED one. No error, no warning; just a file with a
// duplicated customer in it.
//
// The fix is a single snapshot transaction, and this test writes rows CONCURRENTLY
// while the stream is running — from a separate connection that commits — which
// is precisely the interleaving the old code could not survive. Three properties,
// each of which the old paging violated:
//
//  1. no row appears twice        (the page-boundary duplicate)
//  2. every pre-existing row appears  (the delete/shift skip)
//  3. rows created AFTER the snapshot opened do NOT appear — the artifact is the
//     data at ONE moment, not a smear across the minutes the export took
//
// Seeded well past a page boundary: at the old exportPageSize of 100, 250 rows
// meant three pages and two boundaries to corrupt.
func TestStreamForExport_IsASnapshot(t *testing.T) {
	db := testutil.SetupTestDB(t)
	store := customer.NewPostgresStore(db)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Export Snapshot")

	const seeded = 250
	want := make(map[string]bool, seeded)
	for i := 0; i < seeded; i++ {
		c, err := store.Create(ctx, tenantID, domain.Customer{
			ExternalID:  fmt.Sprintf("cus_seed_%03d", i),
			DisplayName: fmt.Sprintf("Seed %03d", i),
			Email:       fmt.Sprintf("seed%03d@export.test", i),
		})
		if err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
		want[c.ID] = true
	}

	// Rows written WHILE the stream is open, from a connection of their own.
	// Under the old offset paging these are what shifted the window.
	var intruders []string
	seen := map[string]int{}
	var order []string

	n := 0
	err := store.StreamForExport(ctx, tenantID, time.Time{}, time.Time{}, func(c domain.Customer) error {
		seen[c.ID]++
		order = append(order, c.ID)
		n++
		// Insert at the first row and again around each old page boundary — the
		// exact points at which the offset walk lost or repeated a row.
		if n == 1 || n == 100 || n == 200 {
			c, err := store.Create(ctx, tenantID, domain.Customer{
				ExternalID:  fmt.Sprintf("cus_intruder_%d", n),
				DisplayName: fmt.Sprintf("Intruder %d", n),
				Email:       fmt.Sprintf("intruder%d@export.test", n),
			})
			if err != nil {
				return fmt.Errorf("concurrent insert at row %d: %w", n, err)
			}
			intruders = append(intruders, c.ID)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("StreamForExport: %v", err)
	}
	if len(intruders) != 3 {
		t.Fatalf("fixture did not write its concurrent rows (%d) — the test proves nothing", len(intruders))
	}

	// (1) No duplicates. This is the bug an auditor would have received.
	for id, count := range seen {
		if count > 1 {
			t.Errorf("customer %s appears %d times in the export — a duplicated row in the artifact", id, count)
		}
	}

	// (2) Nothing dropped.
	for id := range want {
		if seen[id] == 0 {
			t.Errorf("customer %s is missing from the export", id)
		}
	}

	// (3) The snapshot holds: rows committed after it opened are not in it. This
	//     is what makes the file "the tenant's data as of T" rather than a smear
	//     across however long the export happened to take.
	for _, id := range intruders {
		if seen[id] > 0 {
			t.Errorf("customer %s was created DURING the export and still appears in it — the export is not a snapshot", id)
		}
	}

	if len(order) != seeded {
		t.Errorf("streamed %d rows, want exactly the %d that existed when the snapshot opened", len(order), seeded)
	}
}

// The export's columns are its OWN contract. It used to borrow customer.List,
// whose SELECT omits email_status — so `email_status` in customers.csv was
// always the empty string: a column the artifact promised and never filled.
// Nothing failed; the field was just silently blank in every export ever taken.
func TestStreamForExport_CarriesEmailStatus(t *testing.T) {
	db := testutil.SetupTestDB(t)
	store := customer.NewPostgresStore(db)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Export Columns")

	c, err := store.Create(ctx, tenantID, domain.Customer{
		ExternalID: "cus_bounced", DisplayName: "Bounced", Email: "bounced@export.test",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := store.MarkEmailBounced(ctx, tenantID, c.ID, "hard bounce"); err != nil {
		t.Fatalf("mark bounced: %v", err)
	}

	var got domain.CustomerEmailStatus
	if err := store.StreamForExport(ctx, tenantID, time.Time{}, time.Time{}, func(e domain.Customer) error {
		if e.ID == c.ID {
			got = e.EmailStatus
		}
		return nil
	}); err != nil {
		t.Fatalf("StreamForExport: %v", err)
	}

	if got != domain.EmailStatusBounced {
		t.Errorf("email_status = %q, want %q — the export ships this column and it must not be blank",
			got, domain.EmailStatusBounced)
	}
}
