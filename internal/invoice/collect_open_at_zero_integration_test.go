package invoice_test

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/invoice"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/testutil"
	"github.com/gethardcount/hardcount/internal/webhook"
)

// Collect on an invoice that credit left open with nothing due: finalized,
// payment pending or failed, amount due 0. A credit-covered settle that failed
// after its credit was applied leaves this state, a deadlock that answered 409
// concurrent_change included (ADR-136). Collect sent again settles it with no
// charge claim. The store's settle re-reads the invoice under its row lock and
// refuses while a charge hold is set, because that charge may have taken the
// money.
//
// The invoice store and the webhook outbox are real Postgres. Collect has no
// charger and no payment setup here, so a settle that reached for a card would
// answer 422 "payment provider not configured".
//
// Mutation-verify:
//   - send collectPayment's nothing-due branch back to its 422: every test
//     here goes red;
//   - settle it with Service.SettleZeroDue (MarkPaid, which does not read the
//     hold): ..._ChargeHold_SettlesNothing goes red;
//   - gate the dunning resolve on settled.Status instead of the store's paid
//     flag: ..._SettledByAnother_ResolvesNothing goes red.

// leaveOpenAtZero puts the invoice where a failed credit-covered settle leaves
// it: its 5000c covered by credit, nothing due, still finalized and unpaid.
func leaveOpenAtZero(t *testing.T, db *postgres.DB, invoiceID string) {
	t.Helper()
	tx, err := db.BeginTx(context.Background(), postgres.TxBypass, "")
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer postgres.Rollback(tx)
	if _, err := tx.Exec(`UPDATE invoices SET amount_due_cents = 0, credits_applied_cents = 5000 WHERE id = $1`, invoiceID); err != nil {
		t.Fatalf("leave the invoice open with nothing due: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

// countingDunning counts the dunning resolutions Collect writes.
type countingDunning struct {
	mu          sync.Mutex
	resolutions []domain.DunningResolution
}

func (d *countingDunning) ResolveByInvoice(_ context.Context, _, _ string, resolution domain.DunningResolution) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.resolutions = append(d.resolutions, resolution)
	return nil
}

func (d *countingDunning) count() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.resolutions)
}

type openAtZeroRig struct {
	t        *testing.T
	db       *postgres.DB
	ctx      context.Context
	tenantID string
	inv      domain.Invoice
	dunning  *countingDunning
	h        *invoice.Handler
}

// newOpenAtZeroRig seeds a finalized 5000c invoice with the payment status
// given, left open with nothing due, behind a Collect with no card.
func newOpenAtZeroRig(t *testing.T, db *postgres.DB, name string, paymentStatus domain.InvoicePaymentStatus, store invoice.Store) *openAtZeroRig {
	t.Helper()
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, name)
	inv := seedInvoice(t, db, ctx, tenantID, domain.Invoice{
		InvoiceNumber: "INV-" + strings.ReplaceAll(name, " ", "-"), Status: domain.InvoiceFinalized, PaymentStatus: paymentStatus,
		SubtotalCents: 5000, TotalAmountCents: 5000, AmountDueCents: 5000,
	})
	leaveOpenAtZero(t, db, inv.ID)
	if store == nil {
		pg := invoice.NewPostgresStore(db)
		pg.SetOutboxEnqueuer(webhook.NewOutboxStore(db))
		store = pg
	}
	dunning := &countingDunning{}
	h := invoice.NewHandler(invoice.NewService(store, nil, nil), nil, nil, invoice.HandlerDeps{Dunning: dunning})
	return &openAtZeroRig{t: t, db: db, ctx: ctx, tenantID: tenantID, inv: inv, dunning: dunning, h: h}
}

func (r *openAtZeroRig) invoice() domain.Invoice {
	r.t.Helper()
	got, err := invoice.NewPostgresStore(r.db).Get(r.ctx, r.tenantID, r.inv.ID)
	if err != nil {
		r.t.Fatalf("get invoice: %v", err)
	}
	return got
}

func (r *openAtZeroRig) paidEvents() int {
	r.t.Helper()
	return len(queuedEvents(r.t, r.db, r.tenantID, domain.EventInvoicePaid, r.inv.ID))
}

// Collect settles the invoice with no card, once. Sent again, it refuses the
// paid invoice and settles nothing more.
func TestCollect_OpenAtZero_SettlesWithNoCharge(t *testing.T) {
	db := testutil.SetupTestDB(t)
	for _, ps := range []domain.InvoicePaymentStatus{domain.PaymentPending, domain.PaymentFailed} {
		t.Run(string(ps), func(t *testing.T) {
			r := newOpenAtZeroRig(t, db, "Collect Open At Zero "+string(ps), ps, nil)

			rr := postCollect(t, r.h, r.tenantID, r.inv.ID)
			if rr.Code != http.StatusOK {
				t.Fatalf("collect: %d %s, want 200", rr.Code, rr.Body.String())
			}
			got := r.invoice()
			if got.Status != domain.InvoicePaid || got.PaymentStatus != domain.PaymentSucceeded ||
				got.AmountPaidCents != 0 || got.CreditsAppliedCents != 5000 {
				t.Fatalf("after collect: %s/%s, %dc paid, %dc credit applied; want paid/succeeded, 0c paid, 5000c credit applied",
					got.Status, got.PaymentStatus, got.AmountPaidCents, got.CreditsAppliedCents)
			}
			if n := r.paidEvents(); n != 1 {
				t.Fatalf("%d invoice.paid rows, want 1", n)
			}
			if n := r.dunning.count(); n != 1 {
				t.Fatalf("%d dunning resolutions, want 1", n)
			}

			rr = postCollect(t, r.h, r.tenantID, r.inv.ID)
			if rr.Code != http.StatusUnprocessableEntity {
				t.Fatalf("collect again: %d %s, want 422", rr.Code, rr.Body.String())
			}
			if n := r.paidEvents(); n != 1 {
				t.Fatalf("%d invoice.paid rows after collect again, want 1", n)
			}
			if n := r.dunning.count(); n != 1 {
				t.Fatalf("%d dunning resolutions after collect again, want 1", n)
			}
		})
	}
}

// A charge hold, live or stale, means a charge is running or one's result was
// lost, and either may have taken the money. Collect settles nothing and says
// a payment is in flight.
func TestCollect_OpenAtZero_ChargeHold_SettlesNothing(t *testing.T) {
	db := testutil.SetupTestDB(t)
	for _, tc := range []struct {
		name string
		ago  time.Duration
	}{
		{"live", -5 * time.Minute},
		{"stale", 11 * time.Minute},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newOpenAtZeroRig(t, db, "Collect Open At Zero Hold "+tc.name, domain.PaymentPending, nil)
			setHoldAgo(t, db, r.inv.ID, tc.ago)

			rr := postCollect(t, r.h, r.tenantID, r.inv.ID)
			if rr.Code != http.StatusConflict || !strings.Contains(rr.Body.String(), "payment_in_flight") {
				t.Fatalf("collect: %d %s, want 409 payment_in_flight", rr.Code, rr.Body.String())
			}
			if got := r.invoice(); got.Status != domain.InvoiceFinalized || got.PaymentStatus != domain.PaymentPending {
				t.Fatalf("after collect: %s/%s, want finalized/pending", got.Status, got.PaymentStatus)
			}
			if n := r.paidEvents(); n != 0 {
				t.Fatalf("%d invoice.paid rows, want 0", n)
			}
			if n := r.dunning.count(); n != 0 {
				t.Fatalf("%d dunning resolutions, want 0", n)
			}
			if !holdIsSet(t, db, r.inv.ID) {
				t.Fatal("Collect cleared the charge hold")
			}
		})
	}
}

// settledByAnother is the real store, except that another writer (a dunning
// retry, say) settles the invoice between Collect's read and its settle.
type settledByAnother struct {
	*invoice.PostgresStore
}

func (s settledByAnother) SettleZeroDue(ctx context.Context, tenantID, id string, paidAt time.Time) (domain.Invoice, bool, error) {
	if _, _, err := s.PostgresStore.SettleZeroDue(ctx, tenantID, id, paidAt); err != nil {
		return domain.Invoice{}, false, err
	}
	return s.PostgresStore.SettleZeroDue(ctx, tenantID, id, paidAt)
}

// When another writer settles the invoice first, Collect's settle finds it
// paid. Collect answers that it is already paid and fires nothing: the
// writer that settled it owns the effects.
func TestCollect_OpenAtZero_SettledByAnother_ResolvesNothing(t *testing.T) {
	db := testutil.SetupTestDB(t)
	pg := invoice.NewPostgresStore(db)
	pg.SetOutboxEnqueuer(webhook.NewOutboxStore(db))
	r := newOpenAtZeroRig(t, db, "Collect Open At Zero Race", domain.PaymentPending, settledByAnother{PostgresStore: pg})

	rr := postCollect(t, r.h, r.tenantID, r.inv.ID)
	if rr.Code != http.StatusUnprocessableEntity || !strings.Contains(rr.Body.String(), "invoice is already paid") {
		t.Fatalf("collect: %d %s, want 422 'invoice is already paid'", rr.Code, rr.Body.String())
	}
	if got := r.invoice(); got.Status != domain.InvoicePaid {
		t.Fatalf("after collect: %s, want paid", got.Status)
	}
	if n := r.paidEvents(); n != 1 {
		t.Fatalf("%d invoice.paid rows, want 1", n)
	}
	if n := r.dunning.count(); n != 0 {
		t.Fatalf("%d dunning resolutions from Collect, want 0", n)
	}
}
