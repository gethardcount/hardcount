package credit_test

import (
	"testing"

	"github.com/sagarsuperuser/velox/internal/credit"
)

// TestGrantSummary_SplitsDeductedFromUsed: an operator deduction (a negative
// adjustment) drains grants the way an invoice does, bumping consumed_cents,
// so the customer page showed the deducted part as Used and a fully
// deducted grant as "Used up", while the totals line's Used counted only
// usage entries. GrantSummary now carries deducted_cents (from the
// deduction's drained_blocks), and the balance totals carry total_deducted.
// Mutation-verify: return 0 for deducted_cents in ListGrantSummaries, or
// drop the deduction arm of balanceWriteOffColumns → red.
func TestGrantSummary_SplitsDeductedFromUsed(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: skipped in -short mode")
	}
	_, ctx, tenantID, cust, _, svc, invoiceStore := expirySetup(t, "GrantSummaryDeducted")

	first, err := svc.Grant(ctx, tenantID, credit.GrantInput{CustomerID: cust.ID, AmountCents: 10000, Description: "first"})
	if err != nil {
		t.Fatalf("grant first: %v", err)
	}
	second, err := svc.Grant(ctx, tenantID, credit.GrantInput{CustomerID: cust.ID, AmountCents: 5000, Description: "second"})
	if err != nil {
		t.Fatalf("grant second: %v", err)
	}

	// $30 applied to an invoice, then $20 deducted: both from the first
	// grant (oldest first).
	inv := createOpenInvoice(t, ctx, invoiceStore, tenantID, cust.ID, 3000)
	if applied, err := svc.ApplyToInvoice(ctx, tenantID, cust.ID, inv.ID, 3000); err != nil || applied != 3000 {
		t.Fatalf("apply: applied %d, err %v; want 3000", applied, err)
	}
	if _, err := svc.Adjust(ctx, tenantID, credit.AdjustInput{CustomerID: cust.ID, AmountCents: -2000, Description: "goodwill reversed"}); err != nil {
		t.Fatalf("deduct 2000: %v", err)
	}

	byID := func() map[string]credit.GrantSummary {
		t.Helper()
		resp, err := svc.ListGrants(ctx, tenantID, cust.ID, true)
		if err != nil {
			t.Fatalf("list grants: %v", err)
		}
		m := map[string]credit.GrantSummary{}
		for _, g := range resp.Grants {
			m[g.ID] = g
		}
		return m
	}

	g := byID()[first.ID]
	if g.ConsumedCents != 5000 || g.DeductedCents != 2000 {
		t.Errorf("after deducting part of a grant: consumed=%d deducted=%d, want 5000 2000", g.ConsumedCents, g.DeductedCents)
	}
	if used := g.ConsumedCents - g.ExpiredCents - g.RetiredCents - g.DeductedCents; used != 3000 {
		t.Errorf("used = %d, want 3000 (the invoice, not the deduction)", used)
	}

	// A deduction spanning two grants: $50 left on the first, $30 from the second.
	if _, err := svc.Adjust(ctx, tenantID, credit.AdjustInput{CustomerID: cust.ID, AmountCents: -8000, Description: "cleanup"}); err != nil {
		t.Fatalf("deduct 8000: %v", err)
	}
	m := byID()
	if a := m[first.ID]; a.ConsumedCents != 10000 || a.DeductedCents != 7000 || a.ExpiredCents != 0 || a.RetiredCents != 0 {
		t.Errorf("first grant consumed=%d deducted=%d expired=%d retired=%d, want 10000 7000 0 0",
			a.ConsumedCents, a.DeductedCents, a.ExpiredCents, a.RetiredCents)
	}
	if b := m[second.ID]; b.ConsumedCents != 3000 || b.DeductedCents != 3000 {
		t.Errorf("second grant consumed=%d deducted=%d, want 3000 3000", b.ConsumedCents, b.DeductedCents)
	}

	bal, err := svc.GetBalance(ctx, tenantID, cust.ID)
	if err != nil {
		t.Fatalf("balance: %v", err)
	}
	if bal.TotalGranted != 15000 || bal.TotalUsed != 3000 || bal.TotalDeducted != 10000 ||
		bal.TotalExpired != 0 || bal.TotalRetired != 0 || bal.BalanceCents != 2000 {
		t.Errorf("balance = %+v, want granted 15000 used 3000 deducted 10000 expired 0 retired 0 balance 2000", bal)
	}
	if got := bal.TotalUsed + bal.TotalExpired + bal.TotalRetired + bal.TotalDeducted + bal.BalanceCents; got != bal.TotalGranted {
		t.Errorf("used+expired+retired+deducted+balance = %d, want granted %d", got, bal.TotalGranted)
	}

	// The tenant-wide list carries the same totals.
	all, err := svc.ListBalances(ctx, tenantID)
	if err != nil {
		t.Fatalf("list balances: %v", err)
	}
	if len(all) != 1 || all[0].TotalDeducted != 10000 || all[0].TotalRetired != 0 {
		t.Errorf("list balances = %+v, want one row with deducted 10000 retired 0", all)
	}
}
