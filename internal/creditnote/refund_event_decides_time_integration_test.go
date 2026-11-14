package creditnote_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/creditnote"
	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/invoice"
	"github.com/gethardcount/hardcount/internal/platform/clock"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/testutil"
)

var refundEventFrozen = time.Date(2027, 2, 3, 9, 0, 0, 0, time.UTC)

// refundEventResolver puts every invoice on clock clk_r at refundEventFrozen,
// or fails with err, and counts the reads.
type refundEventResolver struct {
	reads int
	err   error
}

func (r *refundEventResolver) answer() (clock.Sim, error) {
	r.reads++
	if r.err != nil {
		return clock.Sim{}, r.err
	}
	return clock.Sim{At: refundEventFrozen, TestClockID: "clk_r"}, nil
}
func (r *refundEventResolver) SimForCustomer(context.Context, string, string) (clock.Sim, error) {
	return r.answer()
}
func (r *refundEventResolver) SimForSubscription(context.Context, string, string) (clock.Sim, error) {
	return r.answer()
}
func (r *refundEventResolver) SimForInvoice(context.Context, string, string) (clock.Sim, error) {
	return r.answer()
}

// A refund's status event decides its write's time from the invoice of the
// credit note holding the refund, right after the store finds the note: a
// clock invoice's note is written at the clock's time. A time that cannot be
// decided writes nothing and fails with clock.ErrUndecided, which the webhook
// delivers again. In live mode the event reads no clock.
func TestApplyRefundWebhook_DecidesFromTheNotesInvoice(t *testing.T) {
	db := testutil.SetupTestDB(t)
	test := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "CN Refund Event Time")
	store := creditnote.NewPostgresStore(db)
	service := func(res clock.Resolver) *creditnote.Service {
		svc := creditnote.NewService(store, invoice.NewPostgresStore(db), nil)
		svc.SetResolver(res)
		return svc
	}

	t.Run("a clock invoice's note is written at the clock's time", func(t *testing.T) {
		cn := seedRefundableCN(t, db, tenantID, "evt-clock", "re_evt_clock", domain.RefundPending)
		if err := service(&refundEventResolver{}).ApplyRefundWebhook(test, tenantID, "re_evt_clock", domain.RefundSucceeded, ""); err != nil {
			t.Fatalf("ApplyRefundWebhook: %v", err)
		}
		got, err := store.Get(test, tenantID, cn.ID)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if got.RefundStatus != domain.RefundSucceeded || !got.UpdatedAt.Equal(refundEventFrozen) {
			t.Errorf("refund %s, updated_at %s; want succeeded at the clock's %s", got.RefundStatus, got.UpdatedAt, refundEventFrozen)
		}
	})

	t.Run("a clock that cannot be read writes nothing", func(t *testing.T) {
		cn := seedRefundableCN(t, db, tenantID, "evt-undecided", "re_evt_undecided", domain.RefundPending)
		err := service(&refundEventResolver{err: errors.New("connection reset by peer")}).
			ApplyRefundWebhook(test, tenantID, "re_evt_undecided", domain.RefundSucceeded, "")
		if !errors.Is(err, clock.ErrUndecided) {
			t.Fatalf("err = %v; want clock.ErrUndecided", err)
		}
		got, err := store.Get(test, tenantID, cn.ID)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if got.RefundStatus != domain.RefundPending || !got.UpdatedAt.Equal(cn.UpdatedAt) {
			t.Errorf("refund %s, updated_at %s; want it untouched (pending, %s)", got.RefundStatus, got.UpdatedAt, cn.UpdatedAt)
		}
	})

	t.Run("live mode reads no clock", func(t *testing.T) {
		res := &refundEventResolver{err: errors.New("connection reset by peer")}
		live := postgres.WithLivemode(context.Background(), true)
		_ = service(res).ApplyRefundWebhook(live, tenantID, "re_evt_none", domain.RefundSucceeded, "")
		if res.reads != 0 {
			t.Errorf("a live event read the clock %d times; want none", res.reads)
		}
	})
}

// A refund made in Stripe decides its time from the invoice that holds its
// payment, right after that lookup. A time that cannot be decided records
// nothing and fails with clock.ErrUndecided.
func TestRecordStripeRefund_AnUndecidableTimeRecordsNothing(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "CN Stripe Refund Undecided")
	store := creditnote.NewPostgresStore(db)
	invStore := invoice.NewPostgresStore(db)
	seedInvoiceForCN(t, db, tenantID, "undecided", domain.InvoicePaid, domain.PaymentSucceeded, "pi_undecided")

	svc := creditnote.NewService(store, invStore, nil)
	svc.SetStripeRefundRecording(store, invStore, nopOutbox{})
	svc.SetResolver(&refundEventResolver{err: errors.New("connection reset by peer")})

	made := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
	_, err := svc.RecordStripeRefund(ctx, tenantID, domain.StripeRefundSnapshot{
		StripeEventID: "evt_undecided", EventAt: made, StripeRefundID: "re_undecided",
		StripePaymentIntentID: "pi_undecided", AmountCents: 4000, Currency: "usd",
		Status: domain.StripeRefundSucceeded, CreatedAt: made,
	})
	if !errors.Is(err, clock.ErrUndecided) {
		t.Fatalf("err = %v; want clock.ErrUndecided", err)
	}
	recorded, err := store.StripeRefundRecorded(ctx, tenantID, "re_undecided")
	if err != nil || recorded {
		t.Errorf("recorded %v (err %v); want nothing recorded", recorded, err)
	}
}
