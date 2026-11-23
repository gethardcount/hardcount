package billing_test

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/audit"
	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/subscription"
	"github.com/gethardcount/hardcount/internal/testutil"
)

// TestThresholdFire_FinalizeRowIsBornFinalized is P-1 for the spend
// threshold: the invoice a crossed threshold issues is created finalized, so
// its finalize audit row carries born_finalized: true.
func TestThresholdFire_FinalizeRowIsBornFinalized(t *testing.T) {
	f := newThresholdFixture(t, "Threshold Born Finalized")
	f.engine.SetAuditLogger(audit.NewLogger(f.db))
	ctx, cancel := context.WithTimeout(postgres.WithLivemode(context.Background(), false), 60*time.Second)
	defer cancel()

	f.ingestUsage(t, ctx, 100, 10)
	if _, err := f.subSvc.SetBillingThresholds(ctx, f.tenantID, f.subID, subscription.BillingThresholdsInput{AmountGTE: 500}); err != nil {
		t.Fatalf("set threshold: %v", err)
	}
	if fired, errs := f.engine.ScanThresholds(ctx, 50); fired != 1 || len(errs) != 0 {
		t.Fatalf("threshold scan: fired=%d errs=%v, want 1 and none", fired, errs)
	}
	invs := f.listInvoices(t, ctx)
	if len(invs) != 1 || invs[0].BillingReason != domain.BillingReasonThreshold {
		t.Fatalf("invoices %+v, want one threshold invoice", invs)
	}

	var born sql.NullBool
	if err := testutil.AdminPool(t).QueryRow(
		`SELECT (metadata->>'born_finalized')::boolean FROM audit_log WHERE resource_type = 'invoice' AND resource_id = $1 AND action = $2`,
		invs[0].ID, domain.AuditActionFinalize).Scan(&born); err != nil {
		t.Fatalf("finalize row: %v", err)
	}
	if !born.Valid || !born.Bool {
		t.Errorf("threshold invoice finalize row born_finalized = %v, want true", born)
	}
}
