package creditnote

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/platform/clock"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
)

// refundWriteStore records the instant a refund-status write's ctx carries:
// the database store stamps its own columns and the queued event from it.
type refundWriteStore struct {
	*memStore
	at time.Time
}

func (s *refundWriteStore) UpdateRefundStatusAudited(ctx context.Context, tenantID, id string, w RefundStatusWrite, emit func(tx *sql.Tx, updated domain.CreditNote, prior domain.RefundStatus, changed bool) error) error {
	s.at, _ = clock.EffectiveNow(ctx)
	return s.memStore.UpdateRefundStatusAudited(ctx, tenantID, id, w, emit)
}

// instantDunning records the instant the ctx of a dunning close carries.
type instantDunning struct{ at time.Time }

func (d *instantDunning) ResolveByInvoice(ctx context.Context, _, _ string, _ domain.DunningResolution) error {
	d.at, _ = clock.EffectiveNow(ctx)
	return nil
}

// instantSender records the instant an email's ctx carries; the outbox
// stamps the row's billing anchor from it.
type instantSender struct {
	cnCapturingSender
	at time.Time
}

func (s *instantSender) SendCreditNote(ctx context.Context, tenantID, to string, cc []string, name, number, invoiceNumber string, amount int64, currency string, pdf []byte) error {
	s.at, _ = clock.EffectiveNow(ctx)
	return s.cnCapturingSender.SendCreditNote(ctx, tenantID, to, cc, name, number, invoiceNumber, amount, currency, pdf)
}

// moreEntry is a credit-note entry point whose action's instant is read at
// one of its writes: the refund status, the dunning close, the email.
type moreEntry struct {
	name string
	run  func(t *testing.T, ctx context.Context, clk clock.Clock, res clock.Resolver) time.Time
}

func moreEntries() []moreEntry {
	return []moreEntry{
		{"a refund retry's write", func(t *testing.T, ctx context.Context, clk clock.Clock, res clock.Resolver) time.Time {
			store := &refundWriteStore{memStore: newMemStore()}
			store.notes["hc_cn_retry"] = domain.CreditNote{
				ID: "hc_cn_retry", TenantID: "t1", InvoiceID: "inv_card", CustomerID: "cus_1",
				Status: domain.CreditNoteIssued, Currency: "USD", RefundAmountCents: 1000, RefundStatus: domain.RefundFailed,
			}
			svc := NewService(store, &memInvoiceReader{invoices: map[string]domain.Invoice{"inv_card": cardInvoice()}}, &fakeRefunder{})
			svc.SetResolver(res)
			if clk != nil {
				svc.SetClock(clk)
			}
			if _, err := svc.RetryRefund(ctx, "t1", "hc_cn_retry"); err != nil {
				t.Fatalf("RetryRefund: %v", err)
			}
			return store.at
		}},
		{"a finalize's held relief closing the dunning run", func(t *testing.T, ctx context.Context, clk clock.Clock, res clock.Resolver) time.Time {
			store := newMemStore()
			store.notes["hc_cn_held"] = domain.CreditNote{
				ID: "hc_cn_held", TenantID: "t1", InvoiceID: "inv_card", CustomerID: "cus_1",
				Status: domain.CreditNoteIssued, Currency: "USD",
			}
			svc := NewService(store, &memInvoiceReader{invoices: map[string]domain.Invoice{"inv_card": cardInvoice()}}, &fakeRefunder{})
			svc.SetResolver(res)
			if clk != nil {
				svc.SetClock(clk)
			}
			d := &instantDunning{}
			svc.SetDunningResolver(d)
			svc.FinishHeldRelief(ctx, "t1", []HeldRelief{{CreditNoteID: "hc_cn_held", Settled: true}})
			return d.at
		}},
		{"a credit note's email", func(t *testing.T, ctx context.Context, clk clock.Clock, res clock.Resolver) time.Time {
			store := newMemStore()
			cn, err := store.Create(clock.WithEffectiveNow(context.Background(), cnDecidedAt.Add(-48*time.Hour)), "t1", domain.CreditNote{
				InvoiceID: "inv_1", CustomerID: "cus_1", CreditNoteNumber: "CN-1", Status: domain.CreditNoteIssued,
				Reason: "duplicate", SubtotalCents: 5400, TotalCents: 5400, Currency: "USD", RefundStatus: domain.RefundNone,
			})
			if err != nil {
				t.Fatalf("seed: %v", err)
			}
			svc := NewService(store, nil, nil)
			svc.SetResolver(res)
			if clk != nil {
				svc.SetClock(clk)
			}
			h := NewHandler(svc, HandlerDeps{
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

func TestCreditNoteWrites_RunOnTheDecidedTime(t *testing.T) {
	for _, e := range moreEntries() {
		t.Run(e.name+" is at the decided time", func(t *testing.T) {
			res := &laterOnSameClock{}
			if at := e.run(t, cnOnDecidedClock(), nil, res); !at.Equal(cnDecidedAt) || res.reads != 0 {
				t.Errorf("the write ran at %v, clock reads %d; want %v, none", at, res.reads, cnDecidedAt)
			}
		})
	}
}

func TestCreditNoteWrites_AtRealTimeReadTheServersClockOnce(t *testing.T) {
	past := time.Date(2025, 1, 10, 12, 0, 0, 0, time.UTC)
	for _, e := range moreEntries() {
		t.Run(e.name+" reads the server's clock once", func(t *testing.T) {
			server := &cnSteppingClock{at: past}
			at := e.run(t, clock.WithRealTime(postgres.WithLivemode(context.Background(), false)), server, &laterOnSameClock{})
			if want := past.Add(time.Second); server.reads != 1 || !at.Equal(want) {
				t.Errorf("server clock read %d times, the write ran at %v; want one read, %v", server.reads, at, want)
			}
		})
	}
}
