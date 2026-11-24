package creditnote

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/platform/clock"
	"github.com/gethardcount/hardcount/internal/tax"
)

// refundWriteStore records the instant a refund-status write's ctx carries:
// the database store stamps its own columns and the queued event from it.
type refundWriteStore struct {
	*memStore
	at []time.Time
}

func (s *refundWriteStore) UpdateRefundStatusAudited(ctx context.Context, tenantID, id string, w RefundStatusWrite, emit func(tx *sql.Tx, updated domain.CreditNote, prior domain.RefundStatus, changed bool) error) error {
	at, _ := clock.EffectiveNow(ctx)
	s.at = append(s.at, at)
	return s.memStore.UpdateRefundStatusAudited(ctx, tenantID, id, w, emit)
}

// instantDunning records the instant each dunning close's ctx carries.
type instantDunning struct{ at []time.Time }

func (d *instantDunning) ResolveByInvoice(ctx context.Context, _, _ string, _ domain.DunningResolution) error {
	at, _ := clock.EffectiveNow(ctx)
	d.at = append(d.at, at)
	return nil
}

// instantReverser records the instant each tax reversal's ctx carries.
type instantReverser struct{ at []time.Time }

func (r *instantReverser) ReverseTax(ctx context.Context, _ string, req tax.ReversalRequest) (*tax.ReversalResult, error) {
	at, _ := clock.EffectiveNow(ctx)
	r.at = append(r.at, at)
	return &tax.ReversalResult{TransactionID: "taxrev_" + req.CreditNoteID}, nil
}

// instantSender records the instant an email's ctx carries; the outbox
// stamps the row's billing anchor from it.
type instantSender struct {
	cnCapturingSender
	at []time.Time
}

func (s *instantSender) SendCreditNote(ctx context.Context, tenantID, to string, cc []string, name, number, invoiceNumber string, amount int64, currency string, pdf []byte) error {
	at, _ := clock.EffectiveNow(ctx)
	s.at = append(s.at, at)
	return s.cnCapturingSender.SendCreditNote(ctx, tenantID, to, cc, name, number, invoiceNumber, amount, currency, pdf)
}

// spiedWrite is a credit-note entry point whose action's time is seen
// through spies at its writes: the refund status; the dunning close and the
// tax reversal; the email. run returns every instant the spies saw, and want
// is how many it must see.
type spiedWrite struct {
	name string
	want int
	run  func(t *testing.T, ctx context.Context) []time.Time
}

func spiedWrites() []spiedWrite {
	return []spiedWrite{
		{"a refund retry's write", 1, func(t *testing.T, ctx context.Context) []time.Time {
			store := &refundWriteStore{memStore: newMemStore()}
			store.notes["hc_cn_retry"] = domain.CreditNote{
				ID: "hc_cn_retry", TenantID: "t1", InvoiceID: "inv_card", CustomerID: "cus_1",
				Status: domain.CreditNoteIssued, Currency: "USD", RefundAmountCents: 1000, RefundStatus: domain.RefundFailed,
			}
			svc := cnService(store, cardInvoice(), nil)
			if _, err := svc.RetryRefund(ctx, "t1", "hc_cn_retry"); err != nil {
				t.Fatalf("RetryRefund: %v", err)
			}
			return store.at
		}},
		{"a finalize's held relief, two notes: each dunning close and tax reversal", 4, func(t *testing.T, ctx context.Context) []time.Time {
			store := newMemStore()
			for _, id := range []string{"hc_cn_held_1", "hc_cn_held_2"} {
				store.notes[id] = domain.CreditNote{
					ID: id, TenantID: "t1", InvoiceID: "inv_card", CustomerID: "cus_1",
					Status: domain.CreditNoteIssued, Currency: "USD", TotalCents: 1000, RefundStatus: domain.RefundNone,
				}
			}
			inv := cardInvoice()
			inv.TaxTransactionID = "tax_inv_card"
			svc := cnService(store, inv, nil)
			d, rev := &instantDunning{}, &instantReverser{}
			svc.SetDunningResolver(d)
			svc.SetTaxReverser(rev)
			svc.FinishHeldRelief(ctx, "t1", []HeldRelief{{CreditNoteID: "hc_cn_held_1", Settled: true}, {CreditNoteID: "hc_cn_held_2", Settled: true}})
			return append(d.at, rev.at...)
		}},
		{"a credit note's email", 1, func(t *testing.T, ctx context.Context) []time.Time {
			store := newMemStore()
			cn, err := store.Create(clock.WithEffectiveNow(context.Background(), cnDecidedAt.Add(-48*time.Hour)), "t1", domain.CreditNote{
				InvoiceID: "inv_1", CustomerID: "cus_1", CreditNoteNumber: "CN-1", Status: domain.CreditNoteIssued,
				Reason: "duplicate", SubtotalCents: 5400, TotalCents: 5400, Currency: "USD", RefundStatus: domain.RefundNone,
			})
			if err != nil {
				t.Fatalf("seed: %v", err)
			}
			h := NewHandler(cnService(store, cardInvoice(), nil), HandlerDeps{
				Customers: cnStaticCustomers{cust: domain.Customer{ID: "cus_1", DisplayName: "Acme", Email: "ap@acme.test"}},
				Invoices:  cnStaticInvoices{inv: domain.Invoice{InvoiceNumber: "INV-9", Currency: "USD"}},
			})
			sender := &instantSender{}
			h.SetEmailSender(sender)
			h.SetEmailConfigured(true)
			rr := cnSendReq(t, h, cn.ID, `{"email":"ap@acme.test"}`, ctx)
			if rr.Code != 200 {
				t.Fatalf("send: %d %s", rr.Code, rr.Body.String())
			}
			return sender.at
		}},
	}
}

func allAt(got []time.Time, want time.Time) bool {
	for _, at := range got {
		if !at.Equal(want) {
			return false
		}
	}
	return true
}

func TestCreditNoteWrites_RunOnTheDecidedTime(t *testing.T) {
	for _, e := range spiedWrites() {
		t.Run(e.name+" is at the decided time", func(t *testing.T) {
			got := e.run(t, cnAtDecidedTime())
			if len(got) != e.want || !allAt(got, cnDecidedAt) {
				t.Errorf("the writes ran at %v; want %d writes at %v", got, e.want, cnDecidedAt)
			}
		})
	}
}
