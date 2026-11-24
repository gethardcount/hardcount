package creditnote_test

import (
	"context"
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

// A refund's status event writes the note's status change at real time, in
// either mode.
func TestApplyRefundWebhook_WritesAtRealTime(t *testing.T) {
	db := testutil.SetupTestDB(t)
	tenantID := testutil.CreateTestTenant(t, db, "CN Refund Event Time")
	store := creditnote.NewPostgresStore(db)
	svc := creditnote.NewService(store, invoice.NewPostgresStore(db), nil)
	svc.SetAuditLogger(audit.NewLogger(db))
	for _, live := range []bool{false, true} {
		ctx := postgres.WithLivemode(context.Background(), live)
		suffix := map[bool]string{false: "test", true: "live"}[live]
		cn := seedRefundableCNIn(t, db, ctx, tenantID, "evt-"+suffix, "re_evt_"+suffix, domain.RefundPending)
		if err := svc.ApplyRefundWebhook(ctx, tenantID, "re_evt_"+suffix, domain.RefundSucceeded, ""); err != nil {
			t.Fatalf("live %v: ApplyRefundWebhook: %v", live, err)
		}
		got, err := store.Get(ctx, tenantID, cn.ID)
		if err != nil {
			t.Fatalf("live %v: get: %v", live, err)
		}
		if got.RefundStatus != domain.RefundSucceeded || time.Since(got.UpdatedAt) > time.Minute {
			t.Errorf("live %v: refund %s, updated_at %s; want succeeded at real time", live, got.RefundStatus, got.UpdatedAt)
		}
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

// A refund made in Stripe is recorded on the event's one instant, one read of
// the server's clock in either mode: the note, its line and its issue all
// carry it.
func TestRecordStripeRefund_IsRecordedOnOneInstant(t *testing.T) {
	for name, live := range map[string]bool{"test mode": false, "live mode": true} {
		t.Run(name, func(t *testing.T) {
			db := testutil.SetupTestDB(t)
			ctx := postgres.WithLivemode(context.Background(), live)
			tenantID := testutil.CreateTestTenant(t, db, "CN Stripe Refund One Instant")
			store := creditnote.NewPostgresStore(db)
			invStore := invoice.NewPostgresStore(db)
			inv := seedPaidInvoiceIn(t, db, ctx, tenantID, "oneinstant", "pi_oneinstant")

			svc := creditnote.NewService(store, invStore, nil)
			svc.SetNumberGenerator(&seqNumbers{prefix: "ONEREF"})
			svc.SetStripeRefundRecording(store, invStore, nopOutbox{})
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
			if server.reads != 1 || !cn.CreatedAt.Equal(want) || cn.IssuedAt == nil || !cn.IssuedAt.Equal(want) || !lines[0].CreatedAt.Equal(want) {
				t.Errorf("%d server reads; created %v, issued %v, line %v; want 1 read, all at %v",
					server.reads, cn.CreatedAt, cn.IssuedAt, lines[0].CreatedAt, want)
			}
		})
	}
}
