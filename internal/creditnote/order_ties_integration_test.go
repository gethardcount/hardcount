package creditnote_test

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/creditnote"
	"github.com/gethardcount/hardcount/internal/customer"
	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/invoice"
	"github.com/gethardcount/hardcount/internal/platform/clock"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/testutil"
)

// tieRig is a customer with two paid invoices, on a context bound to one
// instant, so every row written through it shares its created_at.
type tieRig struct {
	db       *postgres.DB
	ctx      context.Context
	tenantID string
	custID   string
	invoices [2]string
	store    *creditnote.PostgresStore
}

func newTieRig(t *testing.T, name string) *tieRig {
	t.Helper()
	db := testutil.SetupTestDB(t)
	at := time.Date(2026, 4, 1, 0, 0, 1, 0, time.UTC)
	ctx := clock.WithEffectiveNow(postgres.WithLivemode(context.Background(), false), at)
	tenantID := testutil.CreateTestTenant(t, db, name)
	cust, err := customer.NewPostgresStore(db).Create(ctx, tenantID, domain.Customer{ExternalID: "cus_ties", DisplayName: name})
	if err != nil {
		t.Fatalf("create customer: %v", err)
	}
	r := &tieRig{db: db, ctx: ctx, tenantID: tenantID, custID: cust.ID, store: creditnote.NewPostgresStore(db)}
	ps := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	for i, num := range []string{"INV-TIES-1", "INV-TIES-2"} {
		inv, err := invoice.NewPostgresStore(db).CreateWithLineItems(ctx, tenantID, domain.Invoice{
			CustomerID: cust.ID, InvoiceNumber: num, Status: domain.InvoicePaid,
			PaymentStatus: domain.PaymentSucceeded, Currency: "USD",
			SubtotalCents: 10000, TotalAmountCents: 10000, AmountPaidCents: 10000,
			BillingPeriodStart: ps, BillingPeriodEnd: ps.AddDate(0, 1, 0), IssuedAt: &at,
		}, nil)
		if err != nil {
			t.Fatalf("create invoice %s: %v", num, err)
		}
		r.invoices[i] = inv.ID
	}
	return r
}

// The orders a test reads in: each tied row stored first in turn.
func bothOrders(lo, hi string) []struct{ name, first, second string } {
	return []struct{ name, first, second string }{
		{"the lower id stored first", lo, hi},
		{"the higher id stored first", hi, lo},
	}
}

// A credit note's lines share their created_at: one action writes them on its
// one instant. They come back in one order on every read, by id, the order
// they were written in (ids are xids), with the rows stored in both orders.
func TestCreditNoteLineItems_ATieOnCreatedAtSortsByID(t *testing.T) {
	r := newTieRig(t, "Credit Note Line Ties")
	lines := []domain.CreditNoteLineItem{
		{Description: "first", Quantity: 1, UnitAmountCents: 1000, AmountCents: 1000},
		{Description: "second", Quantity: 1, UnitAmountCents: 1000, AmountCents: 1000},
	}
	cn, err := r.store.CreateUnderInvoiceLock(r.ctx, r.tenantID, r.invoices[0], lines, func(*sql.Tx, []domain.CreditNote) (domain.CreditNote, error) {
		return domain.CreditNote{
			InvoiceID: r.invoices[0], CustomerID: r.custID, CreditNoteNumber: "CN-LINE-TIES",
			Status: domain.CreditNoteDraft, Reason: "order_change",
			SubtotalCents: 2000, TotalCents: 2000, CreditAmountCents: 2000, Currency: "USD",
			RefundStatus: domain.RefundNone,
		}, nil
	})
	if err != nil {
		t.Fatalf("create credit note: %v", err)
	}
	items, err := r.store.ListLineItems(r.ctx, r.tenantID, cn.ID)
	if err != nil || len(items) != 2 {
		t.Fatalf("ListLineItems: %d lines, %v; want 2", len(items), err)
	}
	if !items[0].CreatedAt.Equal(items[1].CreatedAt) {
		t.Fatalf("lines created at %v and %v; want one shared instant", items[0].CreatedAt, items[1].CreatedAt)
	}
	lo, hi := items[0].ID, items[1].ID
	if hi < lo {
		lo, hi = hi, lo
	}
	for _, o := range bothOrders(lo, hi) {
		reinsertRow(t, r, "credit_note_line_items", o.first)
		reinsertRow(t, r, "credit_note_line_items", o.second)
		got, err := r.store.ListLineItems(r.ctx, r.tenantID, cn.ID)
		if err != nil {
			t.Fatalf("ListLineItems: %v", err)
		}
		if got[0].ID != lo || got[1].ID != hi {
			t.Errorf("%s: got [%s %s]; want [%s %s]", o.name, got[0].ID, got[1].ID, lo, hi)
		}
	}
}

// Two credit notes that hold one Stripe refund and share a created_at: each of
// the three lookups by refund id answers the note with the lower id (or its
// invoice), with the rows stored in both orders.
func TestRefundLookups_ATieOnCreatedAtSortsByID(t *testing.T) {
	r := newTieRig(t, "Refund Lookup Ties")
	const refundID = "re_ties_1"
	noteOn := map[string]string{} // credit note id → its invoice
	var ids []string
	for i, inv := range r.invoices {
		cn, err := r.store.Create(r.ctx, r.tenantID, domain.CreditNote{
			InvoiceID: inv, CustomerID: r.custID, CreditNoteNumber: []string{"CN-REFUND-TIES-1", "CN-REFUND-TIES-2"}[i],
			Status: domain.CreditNoteIssued, Reason: "fraudulent",
			SubtotalCents: 5000, TotalCents: 5000, RefundAmountCents: 5000, Currency: "USD",
			RefundStatus: domain.RefundNone,
		})
		if err != nil {
			t.Fatalf("create credit note: %v", err)
		}
		if err := r.store.UpdateRefundStatus(r.ctx, r.tenantID, cn.ID, domain.RefundPending, refundID); err != nil {
			t.Fatalf("stamp the refund id: %v", err)
		}
		got, err := r.store.Get(r.ctx, r.tenantID, cn.ID)
		if err != nil {
			t.Fatalf("re-read credit note: %v", err)
		}
		noteOn[cn.ID] = inv
		ids = append(ids, cn.ID)
		if i == 1 && !got.CreatedAt.Equal(r.mustGet(t, ids[0]).CreatedAt) {
			t.Fatalf("the two notes' created_at differ; want one shared instant")
		}
	}
	lo, hi := ids[0], ids[1]
	if hi < lo {
		lo, hi = hi, lo
	}

	for _, o := range bothOrders(lo, hi) {
		touchNote(t, r, o.first)
		touchNote(t, r, o.second)

		if inv, err := r.store.InvoiceOfRefundNote(r.ctx, r.tenantID, refundID); err != nil || inv != noteOn[lo] {
			t.Errorf("%s: InvoiceOfRefundNote = %s, %v; want %s, the lower id's invoice", o.name, inv, err, noteOn[lo])
		}
		inTx(t, r, func(tx *sql.Tx) {
			if inv, err := r.store.InvoiceForStripeRefundTx(r.ctx, tx, refundID); err != nil || inv != noteOn[lo] {
				t.Errorf("%s: InvoiceForStripeRefundTx = %s, %v; want %s, the lower id's invoice", o.name, inv, err, noteOn[lo])
			}
			if cn, ok, err := r.store.CreditNoteByStripeRefundTx(r.ctx, tx, refundID); err != nil || !ok || cn.ID != lo {
				t.Errorf("%s: CreditNoteByStripeRefundTx = %s, %v, %v; want %s", o.name, cn.ID, ok, err, lo)
			}
		})
	}
}

func (r *tieRig) mustGet(t *testing.T, id string) domain.CreditNote {
	t.Helper()
	cn, err := r.store.Get(r.ctx, r.tenantID, id)
	if err != nil {
		t.Fatalf("get credit note %s: %v", id, err)
	}
	return cn
}

func inTx(t *testing.T, r *tieRig, fn func(tx *sql.Tx)) {
	t.Helper()
	tx, err := r.db.BeginTx(r.ctx, postgres.TxTenant, r.tenantID)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer postgres.Rollback(tx)
	fn(tx)
}

// touchNote writes a new version of a credit note, stored after the others.
// It changes updated_at, an indexed column, so the new version gets new index
// entries too, and an index scan reads the new order as well. (A credit note
// cannot be deleted and inserted again: its lines' foreign keys restrict it.)
func touchNote(t *testing.T, r *tieRig, id string) {
	t.Helper()
	inTxCommit(t, r, `UPDATE credit_notes SET updated_at = updated_at + interval '1 microsecond' WHERE id = $1`, id)
}

// reinsertRow deletes a row and inserts it again, unchanged, in one statement:
// its new version is stored after every other row, with new index entries.
func reinsertRow(t *testing.T, r *tieRig, table, id string) {
	t.Helper()
	inTxCommit(t, r, `WITH d AS (DELETE FROM `+table+` WHERE id = $1 RETURNING *) INSERT INTO `+table+` SELECT * FROM d`, id)
}

func inTxCommit(t *testing.T, r *tieRig, query string, args ...any) {
	t.Helper()
	tx, err := r.db.BeginTx(r.ctx, postgres.TxTenant, r.tenantID)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer postgres.Rollback(tx)
	if _, err := tx.ExecContext(r.ctx, query, args...); err != nil {
		t.Fatalf("exec %q: %v", query, err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
}
