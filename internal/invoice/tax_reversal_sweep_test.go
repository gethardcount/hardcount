package invoice

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
)

// TestRetryPendingTaxReversal drives the recovery sweep for a void whose
// upstream tax reversal failed: the symmetric sibling of RetryPendingTaxCommit.
// A pending row (voided + stripe_tax + tax_transaction_id set + tax_reversed_at
// NULL) is re-driven through reverseInvoiceTax; on success it stamps
// tax_reversed_at and falls out of the next scan; on failure it stays pending
// for the next tick and the error is aggregated.
func TestRetryPendingTaxReversal(t *testing.T) {
	ctx := postgres.WithLivemode(context.Background(), true)

	seed := func() (*Service, *memStore, *fakeTaxReverser) {
		store := newMemStore()
		svc := NewService(store, nil, newMemNumberer())
		rev := &fakeTaxReverser{}
		svc.SetTaxReverser(rev)
		// A voided stripe_tax invoice with a committed transaction and no
		// confirmed reversal — exactly what the sweep must pick up.
		store.invoices["inv_rev"] = domain.Invoice{
			ID: "inv_rev", TenantID: "t1", CustomerID: "c",
			Status:           domain.InvoiceVoided,
			TaxFacts:         domain.TaxFacts{TaxProvider: "stripe_tax", TaxStatus: domain.InvoiceTaxOK},
			TaxTransactionID: "tx_1",
			TotalAmountCents: 1000, AmountDueCents: 1000,
		}
		return svc, store, rev
	}

	t.Run("success re-reverses and stamps the marker (falls out of scan)", func(t *testing.T) {
		svc, store, rev := seed()

		recovered, errs := svc.RetryPendingTaxReversal(ctx, 50)
		if recovered != 1 || len(errs) != 0 {
			t.Fatalf("recovered=%d errs=%d, want 1/0", recovered, len(errs))
		}
		if len(rev.calls) != 1 {
			t.Errorf("ReverseTax calls=%d, want 1", len(rev.calls))
		}
		if store.invoices["inv_rev"].TaxReversedAt == nil {
			t.Error("tax_reversed_at must be stamped after a successful re-reversal")
		}
		// Second sweep: nothing pending (marker stamped).
		recovered2, _ := svc.RetryPendingTaxReversal(ctx, 50)
		if recovered2 != 0 {
			t.Errorf("second sweep recovered=%d, want 0 (row should have fallen out)", recovered2)
		}
	})

	// The invoice carries a committed transaction, so a reversal that names
	// no transaction reversed nothing. Stamping tax_reversed_at on it recorded
	// a reversal that was never made, and the sale stayed on the tax report.
	t.Run("a result naming no reversal transaction is a failure, not a reversal", func(t *testing.T) {
		svc, store, rev := seed()
		rev.empty = true

		recovered, errs := svc.RetryPendingTaxReversal(ctx, 50)
		if recovered != 0 || len(errs) != 1 {
			t.Fatalf("recovered=%d errs=%d, want 0/1", recovered, len(errs))
		}
		if store.invoices["inv_rev"].TaxReversedAt != nil {
			t.Error("tax_reversed_at must stay NULL when no reversal transaction came back")
		}
	})

	t.Run("failure leaves the row pending and aggregates the error", func(t *testing.T) {
		svc, store, rev := seed()
		rev.failErr = errors.New("stripe 503")

		recovered, errs := svc.RetryPendingTaxReversal(ctx, 50)
		if recovered != 0 || len(errs) != 1 {
			t.Fatalf("recovered=%d errs=%d, want 0/1", recovered, len(errs))
		}
		if store.invoices["inv_rev"].TaxReversedAt != nil {
			t.Error("tax_reversed_at must stay NULL on a failed reversal (re-scanned next tick)")
		}
		// The failed reversal waits before it is tried again, so the next
		// run leaves it (and Stripe) alone and takes other invoices first.
		rev.failErr = nil
		calls := len(rev.calls)
		if recovered, _ := svc.RetryPendingTaxReversal(ctx, 50); recovered != 0 || len(rev.calls) != calls {
			t.Fatalf("the run after a failure: recovered=%d, new calls=%d, want the invoice left to wait", recovered, len(rev.calls)-calls)
		}
		// Once its wait is over it recovers, and the wait is cleared.
		timePasses(store, "inv_rev")
		recovered2, errs2 := svc.RetryPendingTaxReversal(ctx, 50)
		if recovered2 != 1 || len(errs2) != 0 {
			t.Fatalf("retry after the wait: recovered=%d errs=%d, want 1/0", recovered2, len(errs2))
		}
	})

	t.Run("no reverser wired → inert", func(t *testing.T) {
		store := newMemStore()
		svc := NewService(store, nil, newMemNumberer())
		store.invoices["inv_rev"] = domain.Invoice{
			ID: "inv_rev", TenantID: "t1", Status: domain.InvoiceVoided,
			TaxFacts: domain.TaxFacts{TaxProvider: "stripe_tax"}, TaxTransactionID: "tx_1", TotalAmountCents: 1000, AmountDueCents: 1000,
		}
		recovered, errs := svc.RetryPendingTaxReversal(ctx, 50)
		if recovered != 0 || len(errs) != 0 {
			t.Fatalf("unwired reverser must no-op; recovered=%d errs=%d", recovered, len(errs))
		}
	})
}

// timePasses ends the wait a failed tax commit or reversal put on an invoice.
func timePasses(store *memStore, id string) {
	inv := store.invoices[id]
	past := time.Now().Add(-time.Minute)
	inv.TaxNextRetryAt = &past
	store.invoices[id] = inv
}

// A tax commit that fails waits before it is tried again. The pending list
// holds every tenant's invoices and takes a fixed number per run, oldest
// first; with nothing moving a failing invoice back, one tenant's failing
// invoices held the head of the list and no other tenant's tax was committed.
func TestRetryPendingTaxCommit_AFailedCommitWaits(t *testing.T) {
	store := newMemStore()
	svc := NewService(store, nil, nil)
	committer := &recordingTaxCommitter{err: errors.New("stripe: the key cannot write tax transactions")}
	svc.SetTaxCommitter(committer)
	ctx := postgres.WithLivemode(context.Background(), true)
	store.invoices["failing"] = domain.Invoice{
		ID: "failing", TenantID: "t1", Status: domain.InvoiceFinalized,
		TaxFacts:  domain.TaxFacts{TaxStatus: domain.InvoiceTaxOK, TaxProvider: "stripe_tax", TaxCalculationID: "taxcalc_1"},
		UpdatedAt: time.Now().Add(-3 * time.Hour),
	}

	if recovered, errs := svc.RetryPendingTaxCommit(ctx, 50); recovered != 0 || len(errs) != 1 {
		t.Fatalf("first run: recovered=%d errs=%d, want 0/1", recovered, len(errs))
	}
	next := store.invoices["failing"].TaxNextRetryAt
	if next == nil || next.Before(time.Now().Add(50*time.Minute)) || next.After(time.Now().Add(70*time.Minute)) {
		t.Fatalf("tax_next_retry_at = %v, want about an hour from now for a fault three hours old", next)
	}

	// The key is fixed, but the next run does not take the invoice yet.
	committer.err = nil
	if recovered, _ := svc.RetryPendingTaxCommit(ctx, 50); recovered != 0 || len(committer.calls) != 1 {
		t.Fatalf("the run after a failure: recovered=%d calls=%d, want the invoice left to wait", recovered, len(committer.calls))
	}
	timePasses(store, "failing")
	if recovered, errs := svc.RetryPendingTaxCommit(ctx, 50); recovered != 1 || len(errs) != 0 {
		t.Fatalf("after the wait: recovered=%d errs=%v, want 1 and none", recovered, errs)
	}
}

// The wait grows with how long the fault has lasted: an eighth of the time
// since the invoice's last write, between one hour and twelve.
func TestNextTaxWorkAttempt(t *testing.T) {
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name      string
		lastWrite time.Duration // before now
		want      time.Duration
	}{
		{"just failed", time.Minute, time.Hour},
		{"three hours old", 3 * time.Hour, time.Hour},
		{"two days old", 48 * time.Hour, 6 * time.Hour},
		{"a month old", 30 * 24 * time.Hour, 12 * time.Hour},
		{"a last write in the future (clock skew)", -time.Hour, time.Hour},
	} {
		if got := nextTaxWorkAttempt(now, now.Add(-tc.lastWrite)).Sub(now); got != tc.want {
			t.Errorf("%s: wait = %v, want %v", tc.name, got, tc.want)
		}
	}
}
