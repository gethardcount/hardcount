package credit_test

import (
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/credit"
)

// TestGrantSummary_SplitsExpiredFromUsed: the expiry sweep sets a grant's
// consumed_cents to amount_cents, so consumed alone counts the expired
// remainder as used. The customer page read "Used up" and "Used $100.00" for
// a $100 grant of which $30 was applied and $70 expired. GrantSummary now
// carries expired_cents (the grant's expiry entry), and used is consumed -
// expired. Mutation-verify: return 0 for expired_cents in
// ListGrantSummaries → red.
func TestGrantSummary_SplitsExpiredFromUsed(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: skipped in -short mode")
	}
	db, ctx, tenantID, cust, _, svc, invoiceStore := expirySetup(t, "GrantSummaryExpired")

	expiresAt := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
	expiring := seedExpiredGrant(t, db, ctx, svc, tenantID, cust.ID, 10000, expiresAt)

	// $30 applied while the grant was live, then the sweep expires $70.
	inv := createOpenInvoice(t, ctx, invoiceStore, tenantID, cust.ID, 3000)
	applied, err := svc.ApplyToInvoiceAt(ctx, tenantID, cust.ID, inv.ID, 3000, expiresAt.Add(-time.Minute))
	if err != nil || applied != 3000 {
		t.Fatalf("apply before expiry: applied %d, err %v; want 3000", applied, err)
	}
	if _, errs := svc.ExpireCredits(ctx); len(errs) != 0 {
		t.Fatalf("expire: %v", errs)
	}

	// Control: a grant with no expiry, fully applied, has nothing expired.
	used, err := svc.Grant(ctx, tenantID, credit.GrantInput{CustomerID: cust.ID, AmountCents: 2000, Description: "no expiry"})
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	inv2 := createOpenInvoice(t, ctx, invoiceStore, tenantID, cust.ID, 2000)
	if applied, err := svc.ApplyToInvoice(ctx, tenantID, cust.ID, inv2.ID, 2000); err != nil || applied != 2000 {
		t.Fatalf("apply control: applied %d, err %v; want 2000", applied, err)
	}

	resp, err := svc.ListGrants(ctx, tenantID, cust.ID, true)
	if err != nil {
		t.Fatalf("list grants: %v", err)
	}
	byID := map[string]credit.GrantSummary{}
	for _, g := range resp.Grants {
		byID[g.ID] = g
	}

	g, ok := byID[expiring]
	if !ok {
		t.Fatalf("expired grant %s missing from the summary", expiring)
	}
	if g.ConsumedCents != 10000 || g.RemainingCents != 0 {
		t.Errorf("expired grant consumed=%d remaining=%d, want 10000 0 (the sweep retires the block)", g.ConsumedCents, g.RemainingCents)
	}
	if g.ExpiredCents != 7000 || g.RetiredCents != 0 {
		t.Errorf("expired grant expired_cents=%d retired_cents=%d, want 7000 0", g.ExpiredCents, g.RetiredCents)
	}
	if usedCents := g.ConsumedCents - g.ExpiredCents; usedCents != 3000 {
		t.Errorf("expired grant used = %d, want 3000 (the part applied to the invoice)", usedCents)
	}

	c, ok := byID[used.ID]
	if !ok {
		t.Fatalf("used-up grant %s missing from the summary", used.ID)
	}
	if c.ConsumedCents != 2000 || c.ExpiredCents != 0 || c.RetiredCents != 0 {
		t.Errorf("used-up grant consumed=%d expired=%d retired=%d, want 2000 0 0", c.ConsumedCents, c.ExpiredCents, c.RetiredCents)
	}
}
