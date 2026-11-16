package creditnote_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/audit"
	"github.com/gethardcount/hardcount/internal/creditnote"
	"github.com/gethardcount/hardcount/internal/customer"
	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/invoice"
	"github.com/gethardcount/hardcount/internal/platform/clock"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/testutil"
)

var refundEventFrozen = time.Date(2027, 2, 3, 9, 0, 0, 0, time.UTC)

// refundEventResolver puts every invoice on clock clk_r at refundEventFrozen
// (or, with noClock, on no clock: the machine's time, as the engine
// answers), or fails with err, and counts the reads.
type refundEventResolver struct {
	reads   int
	err     error
	noClock bool
}

func (r *refundEventResolver) answer() (clock.Sim, error) {
	r.reads++
	switch {
	case r.err != nil:
		return clock.Sim{}, r.err
	case r.noClock:
		return clock.Sim{At: time.Now()}, nil
	}
	return clock.Sim{At: refundEventFrozen, TestClockID: "clk_r"}, nil
}

// auditStamp reads the clock stamp of the newest audit row whose metadata
// action is action, for the resource id.
func auditStamp(t *testing.T, db *postgres.DB, resourceID, action string) (*time.Time, string) {
	t.Helper()
	tx, err := db.BeginTx(context.Background(), postgres.TxBypass, "")
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer postgres.Rollback(tx)
	var at *time.Time
	var clockID string
	if err := tx.QueryRowContext(context.Background(), `
		SELECT sim_effective_at, COALESCE(test_clock_id, '') FROM audit_log
		WHERE resource_id = $1 AND metadata->>'action' = $2 ORDER BY created_at DESC LIMIT 1`, resourceID, action,
	).Scan(&at, &clockID); err != nil {
		t.Fatalf("read the %s audit row of %s: %v", action, resourceID, err)
	}
	return at, clockID
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
// credit note holding the refund, before the write: a clock invoice's note,
// and the audit row of its status change, carry the clock's time; a note on
// no clock, real time. A time that cannot be decided writes nothing and fails
// with clock.ErrUndecided, which the webhook delivers again. In live mode the
// event reads no clock.
func TestApplyRefundWebhook_DecidesFromTheNotesInvoice(t *testing.T) {
	db := testutil.SetupTestDB(t)
	test := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "CN Refund Event Time")
	store := creditnote.NewPostgresStore(db)
	service := func(res clock.Resolver) *creditnote.Service {
		svc := creditnote.NewService(store, invoice.NewPostgresStore(db), nil)
		svc.SetResolver(res)
		svc.SetAuditLogger(audit.NewLogger(db))
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
		if at, clockID := auditStamp(t, db, cn.ID, "refund_status_changed"); at == nil || !at.Equal(refundEventFrozen) || clockID != "clk_r" {
			t.Errorf("the status change's audit row is stamped %v on clock %q; want %s on clk_r", at, clockID, refundEventFrozen)
		}
	})

	t.Run("a note on no clock is written at real time", func(t *testing.T) {
		cn := seedRefundableCN(t, db, tenantID, "evt-noclock", "re_evt_noclock", domain.RefundPending)
		if err := service(&refundEventResolver{noClock: true}).ApplyRefundWebhook(test, tenantID, "re_evt_noclock", domain.RefundSucceeded, ""); err != nil {
			t.Fatalf("ApplyRefundWebhook: %v", err)
		}
		got, err := store.Get(test, tenantID, cn.ID)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if got.RefundStatus != domain.RefundSucceeded || time.Since(got.UpdatedAt) > time.Minute {
			t.Errorf("refund %s, updated_at %s; want succeeded at real time", got.RefundStatus, got.UpdatedAt)
		}
		if at, clockID := auditStamp(t, db, cn.ID, "refund_status_changed"); at != nil || clockID != "" {
			t.Errorf("the status change's audit row is stamped %v on clock %q; want no clock stamp", at, clockID)
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
		live := postgres.WithLivemode(context.Background(), true)
		cn := seedRefundableCNIn(t, db, live, tenantID, "evt-live", "re_evt_live", domain.RefundPending)
		res := &refundEventResolver{err: errors.New("connection reset by peer")}
		if err := service(res).ApplyRefundWebhook(live, tenantID, "re_evt_live", domain.RefundSucceeded, ""); err != nil {
			t.Fatalf("ApplyRefundWebhook: %v", err)
		}
		got, err := store.Get(live, tenantID, cn.ID)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if res.reads != 0 || got.RefundStatus != domain.RefundSucceeded {
			t.Errorf("clock reads %d, refund %s; want no read and the refund succeeded", res.reads, got.RefundStatus)
		}
	})
}

// A refund made in Stripe decides its time from the invoice that holds its
// payment, right after that lookup: on a clock invoice it is recorded, and
// audited, at the clock's time.
func TestRecordStripeRefund_IsRecordedAtTheClocksTime(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "CN Stripe Refund Clock")
	store := creditnote.NewPostgresStore(db)
	invStore := invoice.NewPostgresStore(db)
	_, inv := seedInvoiceForCN(t, db, tenantID, "clockrefund", domain.InvoicePaid, domain.PaymentSucceeded, "pi_clockrefund")

	svc := creditnote.NewService(store, invStore, nil)
	svc.SetNumberGenerator(&seqNumbers{prefix: "CLKREF"})
	svc.SetStripeRefundRecording(store, invStore, nopOutbox{})
	svc.SetAuditLogger(audit.NewLogger(db))
	svc.SetResolver(&refundEventResolver{})

	made := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
	if _, err := svc.RecordStripeRefund(ctx, tenantID, domain.StripeRefundSnapshot{
		StripeEventID: "evt_clockrefund", EventAt: made, StripeRefundID: "re_clockrefund",
		StripePaymentIntentID: "pi_clockrefund", AmountCents: 4000, Currency: "usd",
		Status: domain.StripeRefundSucceeded, CreatedAt: made,
	}); err != nil {
		t.Fatalf("RecordStripeRefund: %v", err)
	}
	if recorded, err := store.StripeRefundRecorded(ctx, tenantID, "re_clockrefund"); err != nil || !recorded {
		t.Fatalf("recorded %v (err %v); want the refund recorded", recorded, err)
	}
	if at, clockID := auditStamp(t, db, inv.ID, "refunded_in_stripe"); at == nil || !at.Equal(refundEventFrozen) || clockID != "clk_r" {
		t.Errorf("the refund's audit row is stamped %v on clock %q; want %s on clk_r", at, clockID, refundEventFrozen)
	}
}

// A time that cannot be decided records nothing and fails with
// clock.ErrUndecided.
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

// refundServer is the server's clock: it moves a second each time it answers
// a read itself (no instant bound), and counts those reads. It is this
// file's own because the package's cnSteppingClock is in its internal tests,
// which this external test package cannot see.
type refundServer struct {
	at    time.Time
	reads int
}

func (c *refundServer) Now(ctx context.Context) time.Time {
	if at, ok := clock.EffectiveNow(ctx); ok {
		return at
	}
	c.reads++
	c.at = c.at.Add(time.Second)
	return c.at
}

// seedPaidInvoiceIn is a customer and a paid invoice holding paymentIntentID,
// in the mode ctx carries (seedInvoiceForCN's, for either mode).
func seedPaidInvoiceIn(t *testing.T, db *postgres.DB, ctx context.Context, tenantID, suffix, paymentIntentID string) domain.Invoice {
	t.Helper()
	cust, err := customer.NewPostgresStore(db).Create(ctx, tenantID, domain.Customer{ExternalID: "cus_one_" + suffix, DisplayName: "One " + suffix})
	if err != nil {
		t.Fatalf("create customer: %v", err)
	}
	now := time.Now().UTC()
	inv, err := invoice.NewPostgresStore(db).CreateWithLineItems(ctx, tenantID, domain.Invoice{
		CustomerID: cust.ID, InvoiceNumber: "INV-ONE-" + suffix, Status: domain.InvoicePaid,
		PaymentStatus: domain.PaymentSucceeded, Currency: "USD", SubtotalCents: 10000, TotalAmountCents: 10000,
		AmountPaidCents: 10000, BillingPeriodStart: now.Add(-30 * 24 * time.Hour), BillingPeriodEnd: now, IssuedAt: &now,
	}, nil)
	if err != nil {
		t.Fatalf("create invoice: %v", err)
	}
	tx, err := db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer postgres.Rollback(tx)
	if _, err := tx.ExecContext(ctx, `UPDATE invoices SET stripe_payment_intent_id = $1 WHERE id = $2`, paymentIntentID, inv.ID); err != nil {
		t.Fatalf("stamp payment intent: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	return inv
}

// A refund made in Stripe is recorded on the event's one instant, bound right
// after its decision: the note, its line and its issue all carry it. On a
// clock invoice, the clock's time with no read of the server's clock; on an
// invoice on no clock, and in live mode, one read of the server's clock.
func TestRecordStripeRefund_IsRecordedOnOneInstant(t *testing.T) {
	for _, c := range []struct {
		name            string
		live            bool
		res             *refundEventResolver
		onClock         bool
		wantServerReads int
	}{
		{"a clock invoice", false, &refundEventResolver{}, true, 0},
		{"an invoice on no clock", false, &refundEventResolver{noClock: true}, false, 1},
		{"live mode", true, &refundEventResolver{err: errors.New("must not be read")}, false, 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			db := testutil.SetupTestDB(t)
			ctx := postgres.WithLivemode(context.Background(), c.live)
			tenantID := testutil.CreateTestTenant(t, db, "CN Stripe Refund One Instant")
			store := creditnote.NewPostgresStore(db)
			invStore := invoice.NewPostgresStore(db)
			inv := seedPaidInvoiceIn(t, db, ctx, tenantID, "oneinstant", "pi_oneinstant")

			svc := creditnote.NewService(store, invStore, nil)
			svc.SetNumberGenerator(&seqNumbers{prefix: "ONEREF"})
			svc.SetStripeRefundRecording(store, invStore, nopOutbox{})
			svc.SetResolver(c.res)
			server := &refundServer{at: time.Now().UTC().Truncate(time.Second)}
			svc.SetClock(server)

			made := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
			if _, err := svc.RecordStripeRefund(ctx, tenantID, domain.StripeRefundSnapshot{
				StripeEventID: "evt_oneinstant", EventAt: made, StripeRefundID: "re_oneinstant",
				StripePaymentIntentID: "pi_oneinstant", AmountCents: 4000, Currency: "usd",
				Status: domain.StripeRefundSucceeded, CreatedAt: made,
			}); err != nil {
				t.Fatalf("RecordStripeRefund: %v", err)
			}
			notes, err := store.ListIssuedByInvoice(ctx, tenantID, inv.ID)
			if err != nil || len(notes) != 1 {
				t.Fatalf("issued notes %d, %v; want the recorded one", len(notes), err)
			}
			cn := notes[0]
			lines, err := store.ListLineItems(ctx, tenantID, cn.ID)
			if err != nil || len(lines) == 0 {
				t.Fatalf("lines %d, %v; want the note's line", len(lines), err)
			}
			want := server.at
			if c.onClock {
				want = refundEventFrozen
			}
			if server.reads != c.wantServerReads || !cn.CreatedAt.Equal(want) || cn.IssuedAt == nil || !cn.IssuedAt.Equal(want) || !lines[0].CreatedAt.Equal(want) {
				t.Errorf("%d server reads; created %v, issued %v, line %v; want %d reads, all at %v",
					server.reads, cn.CreatedAt, cn.IssuedAt, lines[0].CreatedAt, c.wantServerReads, want)
			}
		})
	}
}
