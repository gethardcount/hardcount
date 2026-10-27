package pricing_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/errs"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/pricing"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

// A PATCH writes every meter column from its own earlier read. When another
// write changed the meter after that read, the PATCH must refuse rather than
// put the old aggregation back: a rename would otherwise revert an
// aggregation change past the has-usage lock. The other write is held open
// on the meter row so it commits between the rename's read and its write.
func TestMeterRename_DoesNotRevertAConcurrentChange(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: skipped in -short mode")
	}
	db := testutil.SetupTestDB(t)
	svc := pricing.NewService(pricing.NewPostgresStore(db))
	tenantID := testutil.CreateTestTenant(t, db, "meter rename race")
	ctx := postgres.WithLivemode(context.Background(), false)
	m, err := svc.CreateMeter(ctx, tenantID, pricing.CreateMeterInput{EventName: "stale_rename", Name: "Stale", Aggregation: "sum"})
	if err != nil {
		t.Fatalf("create meter: %v", err)
	}

	other, err := db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer postgres.Rollback(other)
	if _, err := other.ExecContext(ctx, `UPDATE meters SET aggregation = 'max' WHERE id = $1`, m.ID); err != nil {
		t.Fatalf("hold meter update: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		name := "Renamed"
		_, err := svc.UpdateMeter(ctx, tenantID, m.ID, pricing.UpdateMeterInput{Name: &name})
		done <- err
	}()
	testutil.WaitForLockWaiter(t, ctx, testutil.AdminPool(t), "FROM meters WHERE id = $1 FOR UPDATE")
	if err := other.Commit(); err != nil {
		t.Fatalf("commit other write: %v", err)
	}
	select {
	case err := <-done:
		if !errors.Is(err, errs.ErrInvalidState) || !strings.Contains(err.Error(), "changed while the request was running") {
			t.Fatalf("want the meter-changed refusal, got %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the rename never finished")
	}
	got, err := svc.GetMeter(ctx, tenantID, m.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Aggregation != "max" || got.Name == "Renamed" {
		t.Errorf("the other write must stand and the rename must not land: aggregation=%q name=%q", got.Aggregation, got.Name)
	}
}
