package invoice

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/audit"
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

// simSeeingRetrier records the clock and instant a tax retry ran on.
type simSeeingRetrier struct {
	*stubTaxRetrier
	saw clock.Sim
}

func (r *simSeeingRetrier) RetryTaxForInvoice(ctx context.Context, tenantID, invoiceID string,
	then func(ctx context.Context, tx *sql.Tx, taxed domain.Invoice) error,
) (domain.Invoice, error) {
	r.saw, _ = clock.SimOf(ctx)
	return r.stubTaxRetrier.RetryTaxForInvoice(ctx, tenantID, invoiceID, then)
}

// atRecordingApplier records the instant a credit is applied at, and the
// instant its ctx carries (the ledger stamps its row from it).
type atRecordingApplier struct {
	*fakeInvCreditApplier
	at, ctxAt time.Time
}

func (a *atRecordingApplier) ApplyToInvoiceAt(ctx context.Context, tenantID, customerID, invoiceID string, amountCents int64, at time.Time, extra ...string) (int64, error) {
	a.at = at
	a.ctxAt, _ = clock.EffectiveNow(ctx)
	return a.fakeInvCreditApplier.ApplyToInvoiceAt(ctx, tenantID, customerID, invoiceID, amountCents, at, extra...)
}

// Each invoice entry point runs on the time its caller decided (the API's
// door, a sweep, a fan-out) and reads no clock of its own. The writes it
// makes, which the database store and the audit row stamp from their ctx,
// run on that time too.
func TestEntryPoints_RunOnTheDecidedTime(t *testing.T) {
	newSvc := func() (*Service, *instantStore, *otherClock) {
		store := &instantStore{memStore: newMemStore()}
		res := &otherClock{}
		svc := NewService(store, clock.Real(), newMemNumberer())
		svc.SetResolver(res)
		return svc, store, res
	}
	seed := func(t *testing.T, store *instantStore, inv domain.Invoice) domain.Invoice {
		t.Helper()
		inv.CustomerID = "cus_1"
		created, err := store.Create(context.Background(), "t1", inv)
		if err != nil {
			t.Fatalf("seed: %v", err)
		}
		return created
	}
	paidAt := func(t *testing.T, store *instantStore, inv domain.Invoice, err error, res *otherClock) {
		t.Helper()
		if err != nil {
			t.Fatalf("err = %v", err)
		}
		if inv.PaidAt == nil || !inv.PaidAt.Equal(entryDecidedAt) || !store.at.Equal(entryDecidedAt) || res.reads != 0 {
			t.Errorf("paid_at %v, the store's write at %v, clock reads %d; want both %v, none", inv.PaidAt, store.at, res.reads, entryDecidedAt)
		}
	}

	t.Run("a zero-due settle is paid at the decided time", func(t *testing.T) {
		svc, store, res := newSvc()
		inv := seed(t, store, domain.Invoice{Status: domain.InvoiceFinalized, PaymentStatus: domain.PaymentPending})
		got, err := svc.SettleZeroDue(onDecidedClock(), "t1", inv.ID)
		paidAt(t, store, got, err, res)
	})

	t.Run("a zero-due settle with no charge claim is paid at the decided time", func(t *testing.T) {
		svc, store, res := newSvc()
		inv := seed(t, store, domain.Invoice{Status: domain.InvoiceFinalized, PaymentStatus: domain.PaymentPending})
		got, _, err := svc.SettleZeroDueWithoutClaim(onDecidedClock(), "t1", inv.ID)
		paidAt(t, store, got, err, res)
	})

	t.Run("an offline payment is recorded at the decided time", func(t *testing.T) {
		svc, store, res := newSvc()
		inv := seed(t, store, domain.Invoice{Status: domain.InvoiceFinalized, PaymentStatus: domain.PaymentFailed, AmountDueCents: 5000, TotalAmountCents: 5000})
		got, err := svc.RecordOfflinePayment(onDecidedClock(), "t1", inv.ID, "Wire 1042")
		paidAt(t, store, got, err, res)
	})

	// The draft is composed two days earlier on the same clock, so a finalize
	// that kept the draft's issue date would show.
	t.Run("a manual invoice is issued at the decided time of its finalize", func(t *testing.T) {
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
		aud := &instantAudit{}
		svc.SetAuditLogger(aud)
		final, err := svc.Finalize(onDecidedClock(), "t1", draft.ID)
		if err != nil {
			t.Fatalf("finalize: %v", err)
		}
		if final.IssuedAt == nil || !final.IssuedAt.Equal(entryDecidedAt) || !aud.at.Equal(entryDecidedAt) || res.reads != 0 {
			t.Errorf("issued_at %v, audit row at %v, clock reads %d; want both %v, none", final.IssuedAt, aud.at, res.reads, entryDecidedAt)
		}
		// The fake settings' Net 30, counted from the issue date.
		if want := domain.AddDaysIn(entryDecidedAt, 30, time.UTC); final.DueAt == nil || !final.DueAt.Equal(want) {
			t.Errorf("due_at %v; want %v, 30 days from the issue date", final.DueAt, want)
		}
	})

	t.Run("credit is applied at the decided time", func(t *testing.T) {
		svc, store, res := newSvc()
		inv := seed(t, store, domain.Invoice{Status: domain.InvoiceFinalized, AmountDueCents: 5000})
		applier := &atRecordingApplier{fakeInvCreditApplier: &fakeInvCreditApplier{store: store.memStore, applyCents: 2000}}
		svc.SetCreditApplier(applier)
		if _, err := svc.ApplyCreditBalance(onDecidedClock(), "t1", inv.ID); err != nil {
			t.Fatalf("ApplyCreditBalance: %v", err)
		}
		if !applier.at.Equal(entryDecidedAt) || !applier.ctxAt.Equal(entryDecidedAt) || res.reads != 0 {
			t.Errorf("credit applied at %v, its write at %v, clock reads %d; want both %v, none", applier.at, applier.ctxAt, res.reads, entryDecidedAt)
		}
	})

	t.Run("an added line is written at the decided time", func(t *testing.T) {
		svc, store, res := newSvc()
		inv := seed(t, store, domain.Invoice{Status: domain.InvoiceDraft})
		if _, err := svc.AddLineItem(onDecidedClock(), "t1", inv.ID, AddLineItemInput{Description: "Extra", Quantity: 1, UnitAmountCents: 500}); err != nil {
			t.Fatalf("AddLineItem: %v", err)
		}
		if !store.at.Equal(entryDecidedAt) || res.reads != 0 {
			t.Errorf("the line's write at %v, clock reads %d; want %v, none", store.at, res.reads, entryDecidedAt)
		}
	})

	// A commit line is checked against the decided time: one that expires an
	// hour before it is refused, though it expires after the machine's time.
	t.Run("a commit line's expiry is checked against the decided time", func(t *testing.T) {
		svc, store, res := newSvc()
		inv := seed(t, store, domain.Invoice{Status: domain.InvoiceDraft})
		expires := entryDecidedAt.Add(-time.Hour)
		if !expires.After(time.Now()) {
			t.Fatalf("the commit must expire after the machine's time (%v) for the case to show anything", time.Now())
		}
		granted := int64(10000)
		_, err := svc.AddLineItem(onDecidedClock(), "t1", inv.ID, AddLineItemInput{
			Description: "Commit", Quantity: 1, UnitAmountCents: 10000,
			CommitGrantedCents: &granted, CommitExpiresAt: &expires,
		})
		if err == nil || !strings.Contains(err.Error(), "must be in the future") || res.reads != 0 {
			t.Errorf("err %v, clock reads %d; want the commit refused as expired at the decided time, no read", err, res.reads)
		}
	})

	t.Run("a tax retry runs at the decided time", func(t *testing.T) {
		svc, store, res := newSvc()
		inv := seed(t, store, stuckDraft("", "cus_1", "provider_unavailable"))
		retrier := &simSeeingRetrier{stubTaxRetrier: &stubTaxRetrier{store: store.memStore}}
		svc.SetTaxRetrier(retrier)
		if _, err := svc.RetryTax(onDecidedClock(), "t1", inv.ID); err != nil {
			t.Fatalf("RetryTax: %v", err)
		}
		if retrier.saw != (clock.Sim{At: entryDecidedAt, TestClockID: "clk_d"}) || res.reads != 0 {
			t.Errorf("retry ran on %+v, clock reads %d; want clk_d at %v, none", retrier.saw, res.reads, entryDecidedAt)
		}
	})

	// Their stamps are written by the database store: TestVoidAndWriteOff_
	// StampTheDecidedTime pins voided_at and uncollectible_at. Here, that each
	// succeeds and reads no clock.
	t.Run("a void reads no clock of its own", func(t *testing.T) {
		svc, store, res := newSvc()
		inv := seed(t, store, domain.Invoice{Status: domain.InvoiceFinalized, AmountDueCents: 5000})
		if _, err := svc.Void(onDecidedClock(), "t1", inv.ID); err != nil {
			t.Fatalf("Void: %v", err)
		}
		if res.reads != 0 {
			t.Errorf("clock reads %d; want none", res.reads)
		}
	})
	t.Run("a write-off reads no clock of its own", func(t *testing.T) {
		svc, store, res := newSvc()
		inv := seed(t, store, domain.Invoice{Status: domain.InvoiceFinalized, AmountDueCents: 5000})
		if _, err := svc.MarkUncollectibleThen(onDecidedClock(), "t1", inv.ID, nil); err != nil {
			t.Fatalf("MarkUncollectibleThen: %v", err)
		}
		if res.reads != 0 {
			t.Errorf("clock reads %d; want none", res.reads)
		}
	})
}

// steppingClock is a server clock that moves a second each time it answers
// a read itself (no instant bound on ctx), and counts those reads.
type steppingClock struct {
	at    time.Time
	reads int
}

func (c *steppingClock) Now(ctx context.Context) time.Time {
	if t, ok := clock.EffectiveNow(ctx); ok {
		return t
	}
	c.reads++
	c.at = c.at.Add(time.Second)
	return c.at
}

// instantAudit records the instant an in-transaction audit row's ctx carries
// (the audit store stamps the row with it).
type instantAudit struct{ at time.Time }

func (a *instantAudit) Log(context.Context, string, string, string, string, string, map[string]any) error {
	return nil
}
func (a *instantAudit) LogInTx(ctx context.Context, _ *sql.Tx, _ audit.Entry) error {
	a.at, _ = clock.EffectiveNow(ctx)
	return nil
}

// instantStore records the instant the ctx of a paying write, or of an added
// line, carries: the database store stamps its own columns from it.
type instantStore struct {
	*memStore
	at time.Time
}

func (s *instantStore) SettleZeroDue(ctx context.Context, tenantID, id string, paidAt time.Time) (domain.Invoice, bool, error) {
	s.at, _ = clock.EffectiveNow(ctx)
	return s.memStore.SettleZeroDue(ctx, tenantID, id, paidAt)
}

func (s *instantStore) MarkPaidOffline(ctx context.Context, tenantID, id string, paidAt time.Time, check func(tx *sql.Tx, locked domain.Invoice) error) (domain.Invoice, error) {
	s.at, _ = clock.EffectiveNow(ctx)
	return s.memStore.MarkPaidOffline(ctx, tenantID, id, paidAt, check)
}

func (s *instantStore) MarkPaid(ctx context.Context, tenantID, id, stripeID string, paidAt time.Time) (domain.Invoice, error) {
	s.at, _ = clock.EffectiveNow(ctx)
	return s.memStore.MarkPaid(ctx, tenantID, id, stripeID, paidAt)
}

func (s *instantStore) AddLineItemAtomicAudited(ctx context.Context, tenantID, invoiceID string, item domain.InvoiceLineItem, emit func(tx *sql.Tx, item domain.InvoiceLineItem, inv domain.Invoice) error) (domain.InvoiceLineItem, domain.Invoice, error) {
	s.at, _ = clock.EffectiveNow(ctx)
	return s.memStore.AddLineItemAtomicAudited(ctx, tenantID, invoiceID, item, emit)
}
