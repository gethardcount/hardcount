package invoice_test

import (
	"sync"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/credit"
	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/invoice"
	"github.com/gethardcount/hardcount/internal/testutil"
)

// TestFinalize_ConcurrentDoubleFinalize pins the D5 CAS under true
// concurrency: two operators finalize the same draft at once. Both
// service-level draft guards can read the same pre-flip snapshot; the
// `AND status='draft'` predicate on FinalizeWithDates lets exactly one
// succeed, and the other fails.
func TestFinalize_ConcurrentDoubleFinalize(t *testing.T) {
	h, ctx := newCommitHarness(t, "Concurrent Finalize")

	inv, err := h.invSvc.Create(ctx, h.tenantID, invoice.CreateInput{
		CustomerID: h.custID,
		Currency:   "USD",
		LineItems:  []invoice.AddLineItemInput{{Description: "Setup", Quantity: 1, UnitAmountCents: 9000}},
	})
	if err != nil {
		t.Fatalf("create draft: %v", err)
	}

	const racers = 2
	var wg sync.WaitGroup
	errCh := make(chan error, racers)
	for range racers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := h.invSvc.Finalize(ctx, h.tenantID, inv.ID)
			errCh <- err
		}()
	}
	wg.Wait()
	close(errCh)

	var oks, fails int
	for err := range errCh {
		if err == nil {
			oks++
		} else {
			fails++
		}
	}
	if oks != 1 || fails != 1 {
		t.Fatalf("concurrent finalize: oks=%d fails=%d, want exactly 1/1", oks, fails)
	}
}

// TestTopupMarkPaid_ConcurrentRedelivery_GrantsOnce: two concurrent
// deliveries of one payment for a top-up settle once. The first issues it,
// pays it and grants the credit; the second waits on the invoice row, finds
// it paid and grants nothing, so the ledger holds exactly one grant.
func TestTopupMarkPaid_ConcurrentRedelivery_GrantsOnce(t *testing.T) {
	h, ctx := newCommitHarness(t, "Topup Concurrent MarkPaid")

	inv := h.createTopup(t, ctx, "USD", 9000, 10000)

	const racers = 2
	var wg sync.WaitGroup
	errCh := make(chan error, racers)
	for range racers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := h.invStore.MarkPaid(ctx, h.tenantID, inv.ID, "pi_concurrent_settle", time.Now())
			errCh <- err
		}()
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil {
			t.Fatalf("concurrent MarkPaid: %v", err)
		}
	}

	got, err := h.invSvc.Get(ctx, h.tenantID, inv.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Status != domain.InvoicePaid {
		t.Fatalf("status = %s, want paid", got.Status)
	}
	if bal := h.balance(t, ctx); bal != 10000 {
		t.Fatalf("balance = %d, want 10000 (one grant)", bal)
	}
	entries, err := h.creditSvc.ListEntries(ctx, credit.ListFilter{TenantID: h.tenantID, CustomerID: h.custID})
	if err != nil {
		t.Fatalf("list entries: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("ledger entries = %d, want exactly 1 (the payment's grant)", len(entries))
	}
}

// TestBalanceCrossings_ConcurrentGrantAndDrain pins the D6/D8 serialization
// under true concurrency: a grant (advisory-lock path) racing a drain
// (invoice-row → advisory → ledger-rows path) must produce a CONSISTENT
// serial event history — never the panel's misfire (a depleted event with no
// following recovered while the final balance is positive), which is exactly
// what unserialized before/after snapshots produced pre-ADR-078. Runs
// multiple rounds because the interleaving is scheduler-dependent.
func TestBalanceCrossings_ConcurrentGrantAndDrain(t *testing.T) {
	h, ctx := newCommitHarness(t, "Commit Concurrent Crossings")
	h.armLowThreshold(t, ctx, 0) // low alerts off; depleted/recovered only

	for round := range 15 {
		cust := h.newCustomer(t, ctx, round)

		// Seed balance = exactly the drain size, so a drain-first ordering
		// crosses >0→0 (depleted) and the racing grant then crosses 0→>0
		// (recovered); a grant-first ordering produces no crossing at all
		// beyond the seed's.
		if _, err := h.creditSvc.Grant(ctx, h.tenantID, credit.GrantInput{
			Currency:   "USD",
			CustomerID: cust, AmountCents: 500, Description: "seed",
		}); err != nil {
			t.Fatalf("seed grant: %v", err)
		}
		target := h.seedPayable(t, ctx, cust, 500, round)

		var wg sync.WaitGroup
		errCh := make(chan error, 2)
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, err := h.creditSvc.Grant(ctx, h.tenantID, credit.GrantInput{
				Currency:   "USD",
				CustomerID: cust, AmountCents: 10000, Description: "racing grant",
			})
			errCh <- err
		}()
		go func() {
			defer wg.Done()
			_, err := h.creditSvc.ApplyToInvoice(ctx, h.tenantID, cust, target, 500, "racing drain")
			errCh <- err
		}()
		wg.Wait()
		close(errCh)
		for err := range errCh {
			if err != nil {
				t.Fatalf("round %d: %v", round, err)
			}
		}

		// Final balance is 10000 under EVERY serial order (500+10000-500).
		bal, err := testutil.BalanceUSD(h.creditSvc.GetBalance(ctx, h.tenantID, cust))
		if err != nil {
			t.Fatalf("round %d balance: %v", round, err)
		}
		if bal.BalanceCents != 10000 {
			t.Fatalf("round %d balance = %d, want 10000", round, bal.BalanceCents)
		}

		// Event-history consistency: the seed fired one recovered (0→500).
		// If the drain serialized first, depleted fired and the racing
		// grant MUST have fired a second recovered. If the grant went
		// first, no depleted — and no extra recovered. depleted without
		// its recovered = the unserialized-snapshot misfire.
		depleted := h.countEvents(t, ctx, cust, domain.EventCreditBalanceDepleted)
		recovered := h.countEvents(t, ctx, cust, domain.EventCreditBalanceRecovered)
		switch depleted {
		case 0:
			if recovered != 1 {
				t.Fatalf("round %d: depleted=0 recovered=%d, want 1 (seed only)", round, recovered)
			}
		case 1:
			if recovered != 2 {
				t.Fatalf("round %d: depleted=1 recovered=%d, want 2 (crossing pair)", round, recovered)
			}
		default:
			t.Fatalf("round %d: depleted=%d, want 0 or 1", round, depleted)
		}
	}
}
