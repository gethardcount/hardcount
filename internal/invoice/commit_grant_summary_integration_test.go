package invoice_test

import (
	"context"
	"testing"

	"github.com/sagarsuperuser/velox/internal/credit"
	"github.com/sagarsuperuser/velox/internal/creditnote"
)

// The grant summary behind the customer page's credit rows splits a grant's
// consumed_cents into what invoices used and what was written off. A commit
// unwind (void of the funding invoice, or a credit note's relief) bumps
// consumed by the retired part, so before retired_cents the page counted the
// retired credit as "Used" and labelled a voided commit "Used up".
// Mutation-verify: drop the commit-retire arm of ListGrantSummaries' JOIN
// (retired_cents = 0), or the retire arm of balanceWriteOffColumns
// (total_retired = 0) → both tests red.

// assertRetiredTotal: the balance totals name the retired part as
// total_retired (the page's "Canceled"), not as a deduction, and the totals
// add up to what was granted.
func assertRetiredTotal(t *testing.T, h *commitHarness, ctx context.Context, wantRetired int64) {
	t.Helper()
	bal, err := h.creditSvc.GetBalance(ctx, h.tenantID, h.custID)
	if err != nil {
		t.Fatalf("balance: %v", err)
	}
	if bal.TotalRetired != wantRetired || bal.TotalDeducted != 0 {
		t.Errorf("total_retired=%d total_deducted=%d, want %d 0", bal.TotalRetired, bal.TotalDeducted, wantRetired)
	}
	if got := bal.TotalUsed + bal.TotalExpired + bal.TotalRetired + bal.TotalDeducted + bal.BalanceCents; got != bal.TotalGranted {
		t.Errorf("used+expired+retired+deducted+balance = %d, want granted %d (%+v)", got, bal.TotalGranted, bal)
	}
}

func grantSummaryFor(t *testing.T, h *commitHarness, ctx context.Context, grantID string) credit.GrantSummary {
	t.Helper()
	resp, err := h.creditSvc.ListGrants(ctx, h.tenantID, h.custID, true)
	if err != nil {
		t.Fatalf("list grants: %v", err)
	}
	for _, g := range resp.Grants {
		if g.ID == grantID {
			return g
		}
	}
	t.Fatalf("grant %s missing from the summary", grantID)
	return credit.GrantSummary{}
}

// TestGrantSummary_VoidRetiredIsNotUsed: $100 commit, $40 drawn, funding
// invoice voided → the $60 the void retired is retired_cents, not used.
func TestGrantSummary_VoidRetiredIsNotUsed(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: skipped in -short mode")
	}
	h, ctx := newCommitHarness(t, "Grant Summary Void Retired")

	inv := h.createCommitInvoice(t, ctx, 10000, 10000, nil)
	if _, err := h.invSvc.Finalize(ctx, h.tenantID, inv.ID); err != nil {
		t.Fatalf("finalize: %v", err)
	}
	drain(t, h, ctx, 4000, 1)
	if _, err := h.invSvc.Void(ctx, h.tenantID, inv.ID); err != nil {
		t.Fatalf("void: %v", err)
	}

	grant, ok := h.commitGrant(t, ctx, inv.ID)
	if !ok {
		t.Fatal("no commit grant for the funding invoice")
	}
	g := grantSummaryFor(t, h, ctx, grant.ID)
	if g.ConsumedCents != 10000 || g.RemainingCents != 0 {
		t.Errorf("consumed=%d remaining=%d, want 10000 0 (the void retires the remainder)", g.ConsumedCents, g.RemainingCents)
	}
	if g.RetiredCents != 6000 {
		t.Errorf("retired_cents = %d, want 6000 (the remainder the void retired)", g.RetiredCents)
	}
	if g.ExpiredCents != 0 {
		t.Errorf("expired_cents = %d, want 0", g.ExpiredCents)
	}
	if used := g.ConsumedCents - g.ExpiredCents - g.RetiredCents; used != 4000 {
		t.Errorf("used = %d, want 4000 (only the drawdown)", used)
	}
	assertRetiredTotal(t, h, ctx, 6000)
}

// TestGrantSummary_ReliefRetiredIsNotUsed: $100 commit paid, $40 drawn, a
// credit note relieves $25 of it → retired 2500, used 4000, 3500 left.
func TestGrantSummary_ReliefRetiredIsNotUsed(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: skipped in -short mode")
	}
	h, ctx := newCommitHarness(t, "Grant Summary Relief Retired")

	inv := h.createCommitInvoice(t, ctx, 10000, 10000, nil)
	if _, err := h.invSvc.Finalize(ctx, h.tenantID, inv.ID); err != nil {
		t.Fatalf("finalize: %v", err)
	}
	payOffline(t, h, ctx, inv.ID)
	drain(t, h, ctx, 4000, 1)
	if _, err := newReliefCNService(h).CreateAndIssueCommitRelief(ctx, h.tenantID, creditnote.CommitReliefInput{
		InvoiceID: inv.ID, RetireCents: 2500, Reason: "downsized",
	}); err != nil {
		t.Fatalf("relief: %v", err)
	}

	grant, ok := h.commitGrant(t, ctx, inv.ID)
	if !ok {
		t.Fatal("no commit grant for the funding invoice")
	}
	g := grantSummaryFor(t, h, ctx, grant.ID)
	if g.ConsumedCents != 6500 || g.RemainingCents != 3500 {
		t.Errorf("consumed=%d remaining=%d, want 6500 3500", g.ConsumedCents, g.RemainingCents)
	}
	if g.RetiredCents != 2500 {
		t.Errorf("retired_cents = %d, want 2500 (the relief slice)", g.RetiredCents)
	}
	if used := g.ConsumedCents - g.ExpiredCents - g.RetiredCents; used != 4000 {
		t.Errorf("used = %d, want 4000 (only the drawdown)", used)
	}
	assertRetiredTotal(t, h, ctx, 2500)
}
