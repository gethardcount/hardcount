package credit_test

import (
	"testing"
	"time"
)

// TestExpireCredits_WaitsForTheInvoiceGrace (ADR-120): the wall-clock sweep
// retires a grant only once INVOICE_GRACE_PERIOD has passed after its
// expires_at. The close of a period draws on grants live at the period end,
// and with the grace that close runs up to an hour after the boundary. A
// grant that expired ten minutes ago may still have to pay the invoice of a
// period that ended before it expired, so it is not retired yet. One that
// expired two hours ago is.
//
// Mutation check: drop the grace from ListExpiredGrants → the recent grant is
// retired.
func TestExpireCredits_WaitsForTheInvoiceGrace(t *testing.T) {
	db, ctx, tenantID, cust, _, svc, _ := expirySetup(t, "expiry_grace")
	svc.SetExpiryGrace(time.Hour)
	now := time.Now().UTC()
	seedExpiredGrant(t, db, ctx, svc, tenantID, cust.ID, 500, now.Add(-10*time.Minute))
	seedExpiredGrant(t, db, ctx, svc, tenantID, cust.ID, 700, now.Add(-2*time.Hour))

	if _, errs := svc.ExpireCredits(ctx); len(errs) > 0 {
		t.Fatalf("expire: %v", errs)
	}
	sum, entries, total := customerLedgerFacts(t, db, ctx, cust.ID)
	if entries != 1 || total != 700 {
		t.Fatalf("expiry entries=%d total=%dc, want 1 entry of 700c: only the grant past expires_at + grace is retired", entries, total)
	}
	if sum != 500 {
		t.Fatalf("ledger sum = %dc, want 500c (the grant inside the grace is still on the ledger)", sum)
	}

	// With no grace the same sweep retires the recent grant too.
	svc.SetExpiryGrace(0)
	if _, errs := svc.ExpireCredits(ctx); len(errs) > 0 {
		t.Fatalf("expire with no grace: %v", errs)
	}
	if _, entries, total := customerLedgerFacts(t, db, ctx, cust.ID); entries != 2 || total != 1200 {
		t.Fatalf("with no grace: expiry entries=%d total=%dc, want 2 entries, 1200c", entries, total)
	}
}
