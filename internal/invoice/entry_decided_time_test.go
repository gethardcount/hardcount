package invoice

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/platform/clock"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
)

// entryDecidedAt is the instant a caller decided, on clock clk_d (a cycle's
// boundary, say, before the clock's frozen time). The resolver below answers
// the clock's frozen time, three days later, and counts its reads.
var entryDecidedAt = time.Date(2027, 3, 4, 10, 0, 0, 0, time.UTC)

func onDecidedClock() context.Context {
	return clock.WithSim(postgres.WithLivemode(context.Background(), false), clock.Sim{At: entryDecidedAt, TestClockID: "clk_d"})
}

// otherClock puts every record on clock clk_d at its frozen time, three days
// after entryDecidedAt, and counts the reads.
type otherClock struct{ reads int }

func (o *otherClock) answer() (clock.Sim, error) {
	o.reads++
	return clock.Sim{At: entryDecidedAt.Add(72 * time.Hour), TestClockID: "clk_d"}, nil
}
func (o *otherClock) SimForInvoice(context.Context, string, string) (clock.Sim, error) {
	return o.answer()
}
func (o *otherClock) SimForSubscription(context.Context, string, string) (clock.Sim, error) {
	return o.answer()
}
func (o *otherClock) SimForCustomer(context.Context, string, string) (clock.Sim, error) {
	return o.answer()
}

// atRecordingApplier records the instant a credit is applied at.
type atRecordingApplier struct {
	*fakeInvCreditApplier
	at time.Time
}

func (a *atRecordingApplier) ApplyToInvoiceAt(ctx context.Context, tenantID, customerID, invoiceID string, amountCents int64, at time.Time, extra ...string) (int64, error) {
	a.at = at
	return a.fakeInvCreditApplier.ApplyToInvoiceAt(ctx, tenantID, customerID, invoiceID, amountCents, at, extra...)
}

// Each invoice entry point runs on the time its caller decided (the API's
// door, a sweep, a fan-out) and reads no clock of its own.
func TestEntryPoints_RunOnTheDecidedTime(t *testing.T) {
	newSvc := func() (*Service, *memStore, *otherClock) {
		store := newMemStore()
		res := &otherClock{}
		svc := NewService(store, clock.Real(), newMemNumberer())
		svc.SetResolver(res)
		return svc, store, res
	}
	seed := func(t *testing.T, store *memStore, inv domain.Invoice) domain.Invoice {
		t.Helper()
		inv.CustomerID = "cus_1"
		created, err := store.Create(context.Background(), "t1", inv)
		if err != nil {
			t.Fatalf("seed: %v", err)
		}
		return created
	}
	paidAt := func(t *testing.T, inv domain.Invoice, err error, res *otherClock) {
		t.Helper()
		if err != nil {
			t.Fatalf("err = %v", err)
		}
		if inv.PaidAt == nil || !inv.PaidAt.Equal(entryDecidedAt) || res.reads != 0 {
			t.Errorf("paid_at %v, clock reads %d; want %v, none", inv.PaidAt, res.reads, entryDecidedAt)
		}
	}

	t.Run("SettleZeroDue", func(t *testing.T) {
		svc, store, res := newSvc()
		inv := seed(t, store, domain.Invoice{Status: domain.InvoiceFinalized, PaymentStatus: domain.PaymentPending})
		got, err := svc.SettleZeroDue(onDecidedClock(), "t1", inv.ID)
		paidAt(t, got, err, res)
	})

	t.Run("SettleZeroDueWithoutClaim", func(t *testing.T) {
		svc, store, res := newSvc()
		inv := seed(t, store, domain.Invoice{Status: domain.InvoiceFinalized, PaymentStatus: domain.PaymentPending})
		got, _, err := svc.SettleZeroDueWithoutClaim(onDecidedClock(), "t1", inv.ID)
		paidAt(t, got, err, res)
	})

	t.Run("RecordOfflinePayment", func(t *testing.T) {
		svc, store, res := newSvc()
		inv := seed(t, store, domain.Invoice{Status: domain.InvoiceFinalized, PaymentStatus: domain.PaymentFailed, AmountDueCents: 5000, TotalAmountCents: 5000})
		got, err := svc.RecordOfflinePayment(onDecidedClock(), "t1", inv.ID, "Wire 1042")
		paidAt(t, got, err, res)
	})

	t.Run("Finalize", func(t *testing.T) {
		svc, _, res := newSvc()
		composed := clock.WithSim(postgres.WithLivemode(context.Background(), false), clock.Sim{At: entryDecidedAt.Add(-48 * time.Hour), TestClockID: "clk_d"})
		draft, err := svc.Create(composed, "t1", CreateInput{
			CustomerID: "cus_1",
			LineItems:  []AddLineItemInput{{Description: "Consulting", Quantity: 1, UnitAmountCents: 10000}},
		})
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		res.reads = 0
		final, err := svc.Finalize(onDecidedClock(), "t1", draft.ID)
		if err != nil {
			t.Fatalf("finalize: %v", err)
		}
		if final.IssuedAt == nil || !final.IssuedAt.Equal(entryDecidedAt) || res.reads != 0 {
			t.Errorf("issued_at %v, clock reads %d; want %v, none", final.IssuedAt, res.reads, entryDecidedAt)
		}
	})

	t.Run("ApplyCreditBalance", func(t *testing.T) {
		svc, store, res := newSvc()
		inv := seed(t, store, domain.Invoice{Status: domain.InvoiceFinalized, AmountDueCents: 5000})
		applier := &atRecordingApplier{fakeInvCreditApplier: &fakeInvCreditApplier{store: store, applyCents: 2000}}
		svc.SetCreditApplier(applier)
		if _, err := svc.ApplyCreditBalance(onDecidedClock(), "t1", inv.ID); err != nil {
			t.Fatalf("ApplyCreditBalance: %v", err)
		}
		if !applier.at.Equal(entryDecidedAt) || res.reads != 0 {
			t.Errorf("credit applied at %v, clock reads %d; want %v, none", applier.at, res.reads, entryDecidedAt)
		}
	})

	// A commit line is checked against the decided time: one that expires an
	// hour before it is refused, though the machine's time is earlier still.
	t.Run("AddLineItem", func(t *testing.T) {
		svc, store, res := newSvc()
		inv := seed(t, store, domain.Invoice{Status: domain.InvoiceDraft})
		expires := entryDecidedAt.Add(-time.Hour)
		granted := int64(10000)
		_, err := svc.AddLineItem(onDecidedClock(), "t1", inv.ID, AddLineItemInput{
			Description: "Commit", Quantity: 1, UnitAmountCents: 10000,
			CommitGrantedCents: &granted, CommitExpiresAt: &expires,
		})
		if err == nil || !strings.Contains(err.Error(), "must be in the future") || res.reads != 0 {
			t.Errorf("err %v, clock reads %d; want the commit refused as expired at the decided time, no read", err, res.reads)
		}
	})

	t.Run("RetryTax", func(t *testing.T) {
		svc, store, res := newSvc()
		inv := seed(t, store, stuckDraft("", "cus_1", "provider_unavailable"))
		retrier := &clockSeeingRetrier{stubTaxRetrier: &stubTaxRetrier{store: store}, clocks: map[string]string{}}
		svc.SetTaxRetrier(retrier)
		_, _ = svc.RetryTax(onDecidedClock(), "t1", inv.ID)
		if retrier.clocks[inv.ID] != "clk_d" || res.reads != 0 {
			t.Errorf("retry ran on clock %q, clock reads %d; want clk_d, none", retrier.clocks[inv.ID], res.reads)
		}
	})

	// Their writes are in the database store (charge_hold_guard and
	// commit_lifecycle integration tests); here, that they read no clock.
	t.Run("Void and MarkUncollectibleThen read no clock", func(t *testing.T) {
		svc, store, res := newSvc()
		inv := seed(t, store, domain.Invoice{Status: domain.InvoiceFinalized, AmountDueCents: 5000})
		_, voidErr := svc.Void(onDecidedClock(), "t1", inv.ID)
		_, offErr := svc.MarkUncollectibleThen(onDecidedClock(), "t1", inv.ID, nil)
		if res.reads != 0 {
			t.Errorf("clock reads %d; want none (void err %v, write-off err %v)", res.reads, voidErr, offErr)
		}
	})
}
