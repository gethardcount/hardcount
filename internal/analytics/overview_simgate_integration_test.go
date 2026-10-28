package analytics

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/auth"
	"github.com/sagarsuperuser/velox/internal/customer"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/invoice"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

// TestOverview_ExcludesSimulatedData is the drift-guard for the ADR-086 analytics
// gating: analytics metrics aggregate onto a shared WALL-CLOCK axis, so a
// test-clock SIMULATED row must never move a number. The invariant is behavioral
// and drift-proof — it does not grep queries (that predicate is undecidable):
//
//	Snapshot the overview, then add a full SIMULATED customer graph with every
//	discriminator set and every timestamp inside the analytics window (so WITHOUT
//	the exclusion filters these rows WOULD move the numbers). Adding them must
//	change NOTHING.
//
// deep-equal covers every response field automatically, so a NEW aggregate that
// forgets its is_simulated / test_clock_id filter fails here. Coverage today:
// customer counts, all invoice metrics (revenue / AR / avg / paid / failed /
// open), and the credit balance. The subscription/MRR, usage, and dunning
// metrics carry the same filters (see simfilter.go) but need meter/plan/dunning
// fixtures to exercise behaviorally — tracked as a coverage follow-up.
func TestOverview_ExcludesSimulatedData(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Analytics SimGate")
	custStore := customer.NewPostgresStore(db)
	invStore := invoice.NewPostgresStore(db)
	now := time.Now().UTC()

	// A wall-clock baseline so the snapshot isn't trivially all-zero.
	real, err := custStore.Create(ctx, tenantID, domain.Customer{ExternalID: "cus_real", DisplayName: "Real"})
	if err != nil {
		t.Fatalf("real customer: %v", err)
	}
	execBypass(t, db, `UPDATE customers SET status = 'active' WHERE id = $1`, real.ID)
	rInv := makePaidInvoice(t, ctx, invStore, tenantID, real.ID, "USD", 100_00)
	markPaidAt(t, db, rInv.ID, now.Add(-1*time.Hour))
	seedLedger(t, db, tenantID, real.ID, now)

	before := getOverview(t, db, tenantID)

	// A SIMULATED customer graph: clock-pinned customer + a simulated paid
	// invoice + a simulated finalized-unpaid (failed) invoice + credit ledger.
	// Every discriminator is set and every timestamp is inside the 30d window,
	// so without the exclusion filters each of these WOULD move a number.
	clockID := seedClock(t, db, tenantID)
	sim, err := custStore.Create(ctx, tenantID, domain.Customer{ExternalID: "cus_sim", DisplayName: "Sim", TestClockID: clockID})
	if err != nil {
		t.Fatalf("sim customer: %v", err)
	}
	execBypass(t, db, `UPDATE customers SET status = 'active' WHERE id = $1`, sim.ID)

	sPaid := makePaidInvoice(t, ctx, invStore, tenantID, sim.ID, "USD", 777_00)
	markPaidAt(t, db, sPaid.ID, now.Add(-1*time.Hour))
	execBypass(t, db, `UPDATE invoices SET is_simulated = true WHERE id = $1`, sPaid.ID)

	sFailed := makePaidInvoice(t, ctx, invStore, tenantID, sim.ID, "USD", 55_00)
	execBypass(t, db, `UPDATE invoices SET is_simulated = true, status = 'finalized',
		payment_status = 'failed', paid_at = NULL, amount_due_cents = 5500, created_at = $2 WHERE id = $1`,
		sFailed.ID, now.Add(-1*time.Hour))

	seedLedger(t, db, tenantID, sim.ID, now)

	after := getOverview(t, db, tenantID)

	before.Period, after.Period = "", ""
	if !reflect.DeepEqual(before, after) {
		t.Errorf("adding a simulated customer graph moved an analytics number — a metric is "+
			"missing its is_simulated / test_clock_id exclusion (simfilter.go).\nbefore: %+v\nafter:  %+v", before, after)
	}
}

func getOverview(t *testing.T, db *postgres.DB, tenantID string) OverviewResponse {
	t.Helper()
	req := httptest.NewRequest("GET", "/overview?period=30d", nil)
	req = req.WithContext(auth.WithTenantID(postgres.WithLivemode(req.Context(), false), tenantID))
	rr := httptest.NewRecorder()
	NewHandler(db).overview(rr, req)
	if rr.Code != 200 {
		t.Fatalf("overview status: got %d, body=%s", rr.Code, rr.Body.String())
	}
	var resp OverviewResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode overview: %v", err)
	}
	return resp
}

// seedClock inserts a test clock (always livemode=false by CHECK) and returns
// its id. TxBypass with an explicit tenant_id, mirroring the fixture helpers.
func seedClock(t *testing.T, db *postgres.DB, tenantID string) string {
	t.Helper()
	id := postgres.NewID("hc_tclk")
	execBypass(t, db, `INSERT INTO test_clocks (id, tenant_id, frozen_time) VALUES ($1, $2, now())`, id, tenantID)
	return id
}
