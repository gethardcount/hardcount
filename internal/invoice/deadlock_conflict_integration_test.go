package invoice_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/gethardcount/hardcount/internal/api/middleware"
	"github.com/gethardcount/hardcount/internal/auth"
	"github.com/gethardcount/hardcount/internal/credit"
	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/invoice"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/testutil"
	"github.com/gethardcount/hardcount/internal/webhook"
)

// Record payment and Collect, when the paid flip loses a database deadlock,
// answer 409 concurrent_change, and the idempotency layer does not store it.
// The same request with the same Idempotency-Key then runs again and finishes
// (ADR-136).
//
// The invoice store, the credit ledger, the idempotency middleware and the
// webhook outbox are real Postgres. deadlockOnceInvoices makes the first paid
// flip fail with SQLSTATE 40P01, as Postgres fails the side it stops to
// break a deadlock with a cancel (a payment holds the invoice and waits for
// the subscription; the cancel holds the subscription and waits for the
// invoice).
//
// Mutation-verify: drop respond.DeadlockAsConflict from recordOfflinePayment
// (record_payment), from collectPayment's credit-covered branch
// (collect_zero_due) or from its settle of an invoice open with nothing due
// (collect_open_at_zero): the first answer is a 500, and the same key replays
// it. Send collectPayment's nothing-due branch back to the 422: the resend in
// collect_zero_due answers 422 and the invoice stays open.

// errPaidFlipDeadlock is the error Postgres gives the paid flip it stops to
// break a deadlock, wrapped as the store wraps it.
func errPaidFlipDeadlock() error {
	return fmt.Errorf("release the dunning pause: %w", &pgconn.PgError{Severity: "ERROR", Code: "40P01", Message: "deadlock detected"})
}

// deadlockOnceInvoices is the real store, except that the first offline paid
// flip, the first MarkPaid or the first SettleZeroDue, whichever is armed,
// loses a deadlock.
type deadlockOnceInvoices struct {
	*invoice.PostgresStore
	offline, markPaid, settleZeroDue atomic.Bool
}

// MarkPaidOffline runs the real transaction. When armed, it fails under the
// row lock, before anything is written.
func (s *deadlockOnceInvoices) MarkPaidOffline(ctx context.Context, tenantID, id string, paidAt time.Time, check func(tx *sql.Tx, locked domain.Invoice) error) (domain.Invoice, error) {
	if s.offline.CompareAndSwap(true, false) {
		check = func(*sql.Tx, domain.Invoice) error { return errPaidFlipDeadlock() }
	}
	return s.PostgresStore.MarkPaidOffline(ctx, tenantID, id, paidAt, check)
}

func (s *deadlockOnceInvoices) MarkPaid(ctx context.Context, tenantID, id, paymentIntentID string, paidAt time.Time) (domain.Invoice, error) {
	if s.markPaid.CompareAndSwap(true, false) {
		return domain.Invoice{}, errPaidFlipDeadlock()
	}
	return s.PostgresStore.MarkPaid(ctx, tenantID, id, paymentIntentID, paidAt)
}

func (s *deadlockOnceInvoices) SettleZeroDue(ctx context.Context, tenantID, id string, paidAt time.Time) (domain.Invoice, bool, error) {
	if s.settleZeroDue.CompareAndSwap(true, false) {
		return domain.Invoice{}, false, errPaidFlipDeadlock()
	}
	return s.PostgresStore.SettleZeroDue(ctx, tenantID, id, paidAt)
}

type deadlockInvoiceRig struct {
	t        *testing.T
	db       *postgres.DB
	ctx      context.Context
	tenantID string
	store    *deadlockOnceInvoices
	inv      domain.Invoice
	srv      http.Handler
}

// newDeadlockInvoiceRig seeds a finalized invoice of 5000c, due, behind the
// invoice routes and the idempotency middleware.
func newDeadlockInvoiceRig(t *testing.T, db *postgres.DB, name string) *deadlockInvoiceRig {
	t.Helper()
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, name)
	inv := seedInvoice(t, db, ctx, tenantID, domain.Invoice{
		InvoiceNumber: "INV-" + strings.ReplaceAll(name, " ", "-"), Status: domain.InvoiceFinalized,
		SubtotalCents: 5000, TotalAmountCents: 5000, AmountDueCents: 5000,
	})
	pg := invoice.NewPostgresStore(db)
	pg.SetOutboxEnqueuer(webhook.NewOutboxStore(db))
	store := &deadlockOnceInvoices{PostgresStore: pg}
	svc := invoice.NewService(store, nil, nil)
	svc.SetCreditApplier(credit.NewService(credit.NewPostgresStore(db)))
	h := invoice.NewHandler(svc, nil, nil, invoice.HandlerDeps{Charger: &collectCharger{}, PaymentSetups: readyPaymentSetup{}})
	return &deadlockInvoiceRig{
		t: t, db: db, ctx: ctx, tenantID: tenantID, store: store, inv: inv,
		srv: middleware.Idempotency(db, nil)(h.Routes()),
	}
}

func (r *deadlockInvoiceRig) post(action, key, body string) *httptest.ResponseRecorder {
	r.t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/"+r.inv.ID+"/"+action, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", key)
	rr := httptest.NewRecorder()
	r.srv.ServeHTTP(rr, req.WithContext(auth.WithTenantID(r.ctx, r.tenantID)))
	return rr
}

func (r *deadlockInvoiceRig) invoice() domain.Invoice {
	r.t.Helper()
	got, err := r.store.Get(r.ctx, r.tenantID, r.inv.ID)
	if err != nil {
		r.t.Fatalf("get invoice: %v", err)
	}
	return got
}

func (r *deadlockInvoiceRig) events(eventType string) int {
	r.t.Helper()
	return len(queuedEvents(r.t, r.db, r.tenantID, eventType, r.inv.ID))
}

// usageEntries counts the ledger entries that drew customer credit for the
// invoice.
func (r *deadlockInvoiceRig) usageEntries() int {
	r.t.Helper()
	tx, err := r.db.BeginTx(r.ctx, postgres.TxTenant, r.tenantID)
	if err != nil {
		r.t.Fatalf("begin: %v", err)
	}
	defer postgres.Rollback(tx)
	var n int
	if err := tx.QueryRowContext(r.ctx, `SELECT count(*) FROM customer_credit_ledger WHERE invoice_id = $1 AND entry_type = 'usage'`, r.inv.ID).Scan(&n); err != nil {
		r.t.Fatalf("count usage entries: %v", err)
	}
	return n
}

// wantInvoiceConcurrentChange checks the deadlock loser's answer: 409
// concurrent_change, answered by the handler, not replayed.
func wantInvoiceConcurrentChange(t *testing.T, rr *httptest.ResponseRecorder) {
	t.Helper()
	var env struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	_ = json.Unmarshal(rr.Body.Bytes(), &env)
	if rr.Code != http.StatusConflict || env.Error.Code != "concurrent_change" {
		t.Fatalf("deadlock loser: %d %s, want 409 concurrent_change", rr.Code, rr.Body.String())
	}
	if rr.Header().Get("Idempotent-Replayed") != "" {
		t.Fatalf("deadlock loser answered from the idempotency cache")
	}
}

func TestDeadlockLoss_AnswersConflict_NotStored(t *testing.T) {
	db := testutil.SetupTestDB(t)

	t.Run("record_payment", func(t *testing.T) {
		r := newDeadlockInvoiceRig(t, db, "Deadlock Record Payment")
		r.store.offline.Store(true)

		wantInvoiceConcurrentChange(t, r.post("record-payment", "idem-deadlock-record", `{"note":"cheque 1234"}`))
		if got := r.invoice(); got.Status != domain.InvoiceFinalized || got.AmountDueCents != 5000 {
			t.Fatalf("after the lost payment: %s with %dc due, want finalized with 5000c due", got.Status, got.AmountDueCents)
		}
		if n := r.events(domain.EventInvoicePaid); n != 0 {
			t.Fatalf("%d invoice.paid rows after the lost payment, want 0", n)
		}

		rr := r.post("record-payment", "idem-deadlock-record", `{"note":"cheque 1234"}`)
		if rr.Code != http.StatusOK || rr.Header().Get("Idempotent-Replayed") != "" {
			t.Fatalf("resend with the same key: %d (replayed=%q) %s, want 200 from the handler",
				rr.Code, rr.Header().Get("Idempotent-Replayed"), rr.Body.String())
		}
		if got := r.invoice(); got.Status != domain.InvoicePaid || !got.PaidOutOfBand {
			t.Fatalf("after the resend: %s (out of band %v), want paid out of band", got.Status, got.PaidOutOfBand)
		}
		if n := r.events(domain.EventInvoicePaid); n != 1 {
			t.Fatalf("%d invoice.paid rows, want 1", n)
		}
	})

	t.Run("collect_zero_due", func(t *testing.T) {
		// The customer's credit covers the whole invoice, so Collect applies it
		// and settles with no card charge. The settle loses the deadlock; the
		// credit, applied in its own transaction before it, stays applied.
		r := newDeadlockInvoiceRig(t, db, "Deadlock Collect")
		if _, err := credit.NewService(credit.NewPostgresStore(db)).Grant(r.ctx, r.tenantID, credit.GrantInput{
			CustomerID: r.inv.CustomerID, Currency: "USD", AmountCents: 5000, Description: "covers the invoice",
		}); err != nil {
			t.Fatalf("grant credit: %v", err)
		}
		r.store.markPaid.Store(true)

		wantInvoiceConcurrentChange(t, r.post("collect", "idem-deadlock-collect", ""))
		got := r.invoice()
		if got.Status != domain.InvoiceFinalized || got.AmountDueCents != 0 || got.CreditsAppliedCents != 5000 {
			t.Fatalf("after the lost settle: %s, %dc due, %dc credit applied; want finalized, 0c due, 5000c applied",
				got.Status, got.AmountDueCents, got.CreditsAppliedCents)
		}
		if n := r.usageEntries(); n != 1 {
			t.Fatalf("%d credit usage entries, want 1", n)
		}
		if n := r.events(domain.EventInvoicePaid); n != 0 {
			t.Fatalf("%d invoice.paid rows after the lost settle, want 0", n)
		}
		if holdIsSet(t, db, r.inv.ID) {
			t.Fatal("the charge hold is still set after a settle that sent no charge")
		}

		// The same key runs the request again; it is not a replay. The
		// invoice has nothing due, so Collect settles it from the credit the
		// first attempt applied, and applies none again.
		rr := r.post("collect", "idem-deadlock-collect", "")
		if rr.Code != http.StatusOK || rr.Header().Get("Idempotent-Replayed") != "" {
			t.Fatalf("resend with the same key: %d (replayed=%q) %s, want 200 from the handler",
				rr.Code, rr.Header().Get("Idempotent-Replayed"), rr.Body.String())
		}
		got = r.invoice()
		if got.Status != domain.InvoicePaid || got.CreditsAppliedCents != 5000 {
			t.Fatalf("after the resend: %s with %dc credit applied, want paid with 5000c", got.Status, got.CreditsAppliedCents)
		}
		if n := r.usageEntries(); n != 1 {
			t.Fatalf("%d credit usage entries after the resend, want 1", n)
		}
		if n := r.events(domain.EventInvoicePaid); n != 1 {
			t.Fatalf("%d invoice.paid rows after the resend, want 1", n)
		}
	})

	t.Run("collect_open_at_zero", func(t *testing.T) {
		// An earlier attempt applied credit that covers the invoice, and its
		// settle failed. Collect settles it now, and that settle loses the
		// deadlock. Nothing was written before it.
		r := newDeadlockInvoiceRig(t, db, "Deadlock Collect Open At Zero")
		leaveOpenAtZero(t, db, r.inv.ID)
		r.store.settleZeroDue.Store(true)

		wantInvoiceConcurrentChange(t, r.post("collect", "idem-deadlock-open-at-zero", ""))
		if got := r.invoice(); got.Status != domain.InvoiceFinalized || got.AmountDueCents != 0 {
			t.Fatalf("after the lost settle: %s with %dc due, want finalized with 0c due", got.Status, got.AmountDueCents)
		}

		rr := r.post("collect", "idem-deadlock-open-at-zero", "")
		if rr.Code != http.StatusOK || rr.Header().Get("Idempotent-Replayed") != "" {
			t.Fatalf("resend with the same key: %d (replayed=%q) %s, want 200 from the handler",
				rr.Code, rr.Header().Get("Idempotent-Replayed"), rr.Body.String())
		}
		if got := r.invoice(); got.Status != domain.InvoicePaid {
			t.Fatalf("after the resend: %s, want paid", got.Status)
		}
		if n := r.events(domain.EventInvoicePaid); n != 1 {
			t.Fatalf("%d invoice.paid rows, want 1", n)
		}
	})
}
