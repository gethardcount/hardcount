package invoice_test

import (
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/credit"
	"github.com/gethardcount/hardcount/internal/domain"
)

// ADR-132 with commits (ADR-078): voiding an invoice that drew commit credit
// returns that credit to the commit grant. When the commit's own funding
// invoice is already voided, the void retired everything left on the grant,
// so the returned share is retired again: commit credit with no money behind
// it never comes back spendable. Through the real Void path, both orders.
//
// Mutation-verify: drop the FundingVoided branch of classifyRestore → "funding
// voided first" leaves 4000 spendable on the voided commit (remaining,
// balance and retired_cents red).
func TestVoid_ReturnsCommitCreditAndReRetiresIt(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: skipped in -short mode")
	}

	t.Run("funding voided first: the returned share is retired again", func(t *testing.T) {
		h, ctx := newCommitHarness(t, "Return Commit ReRetire")
		funding := h.createCommitInvoice(t, ctx, 10000, 10000, nil)
		if _, err := h.invSvc.Finalize(ctx, h.tenantID, funding.ID); err != nil {
			t.Fatalf("finalize: %v", err)
		}
		grant, ok := h.commitGrant(t, ctx, funding.ID)
		if !ok {
			t.Fatal("no commit grant")
		}
		user := h.seedPayable(t, ctx, h.custID, 4000, 1)
		if n, err := credit.NewPostgresStore(h.db).ApplyToInvoiceAtomic(ctx, h.tenantID, h.custID, user, "draw", 4000, time.Time{}); err != nil || n != 4000 {
			t.Fatalf("draw: %d, %v", n, err)
		}
		if _, err := h.invSvc.Void(ctx, h.tenantID, funding.ID); err != nil {
			t.Fatalf("void funding: %v", err)
		}

		if _, err := h.invSvc.Void(ctx, h.tenantID, user); err != nil {
			t.Fatalf("void the invoice that drew: %v", err)
		}

		g := grantSummaryFor(t, h, ctx, grant.ID)
		if g.RemainingCents != 0 || g.ConsumedCents != 10000 || g.RetiredCents != 10000 {
			t.Errorf("commit grant: remaining %d consumed %d retired %d, want 0 10000 10000 (6000 by the funding void, 4000 again on return)",
				g.RemainingCents, g.ConsumedCents, g.RetiredCents)
		}
		if got := h.balance(t, ctx); got != 0 {
			t.Errorf("balance %d, want 0", got)
		}
		assertRetiredTotal(t, h, ctx, 10000)
		resp, err := h.creditSvc.ListGrants(ctx, h.tenantID, h.custID, true)
		if err != nil {
			t.Fatalf("list grants: %v", err)
		}
		if len(resp.Grants) != 1 {
			t.Errorf("%d grant rows, want the commit grant only (the reversal restored everything to it)", len(resp.Grants))
		}
	})

	t.Run("drawing invoice voided first: the share is back on the commit, then the funding void retires it", func(t *testing.T) {
		h, ctx := newCommitHarness(t, "Return Commit Then Fund Void")
		funding := h.createCommitInvoice(t, ctx, 10000, 10000, nil)
		if _, err := h.invSvc.Finalize(ctx, h.tenantID, funding.ID); err != nil {
			t.Fatalf("finalize: %v", err)
		}
		grant, _ := h.commitGrant(t, ctx, funding.ID)
		user := h.seedPayable(t, ctx, h.custID, 4000, 1)
		if n, err := credit.NewPostgresStore(h.db).ApplyToInvoiceAtomic(ctx, h.tenantID, h.custID, user, "draw", 4000, time.Time{}); err != nil || n != 4000 {
			t.Fatalf("draw: %d, %v", n, err)
		}

		if _, err := h.invSvc.Void(ctx, h.tenantID, user); err != nil {
			t.Fatalf("void the invoice that drew: %v", err)
		}
		g := grantSummaryFor(t, h, ctx, grant.ID)
		if g.RemainingCents != 10000 || g.GrantKind != string(domain.GrantKindCommit) {
			t.Errorf("commit grant after the return: remaining %d kind %q, want 10000 commit", g.RemainingCents, g.GrantKind)
		}

		if _, err := h.invSvc.Void(ctx, h.tenantID, funding.ID); err != nil {
			t.Fatalf("void funding: %v", err)
		}
		g = grantSummaryFor(t, h, ctx, grant.ID)
		if g.RemainingCents != 0 || g.RetiredCents != 10000 {
			t.Errorf("commit grant after the funding void: remaining %d retired %d, want 0 10000", g.RemainingCents, g.RetiredCents)
		}
		assertRetiredTotal(t, h, ctx, 10000)
	})
}
