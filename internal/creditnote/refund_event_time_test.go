package creditnote

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/platform/clock"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
)

// countingResolver puts every invoice on a clock, or fails with err, and
// counts the reads.
type countingResolver struct {
	reads int
	err   error
}

func (r *countingResolver) answer() (clock.Sim, error) {
	r.reads++
	if r.err != nil {
		return clock.Sim{}, r.err
	}
	return clock.Sim{At: time.Date(2027, 2, 3, 9, 0, 0, 0, time.UTC), TestClockID: "clk_u"}, nil
}
func (r *countingResolver) SimForCustomer(context.Context, string, string) (clock.Sim, error) {
	return r.answer()
}
func (r *countingResolver) SimForSubscription(context.Context, string, string) (clock.Sim, error) {
	return r.answer()
}
func (r *countingResolver) SimForInvoice(context.Context, string, string) (clock.Sim, error) {
	return r.answer()
}

// A refund's status event reads the clock of the note's invoice in test mode
// only. In live mode it reads none, so a clock that cannot be read never
// refuses a live refund. With no mode on ctx the event is refused. The store
// here has no row-level security, so the live event finds the note.
func TestApplyRefundWebhook_ReadsAClockInTestModeOnly(t *testing.T) {
	for _, c := range []struct {
		name      string
		ctx       context.Context
		wantReads int
		wantErr   bool
	}{
		{"test mode", postgres.WithLivemode(context.Background(), false), 1, true},
		{"live mode", postgres.WithLivemode(context.Background(), true), 0, false},
		{"no mode", context.Background(), 0, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			store := newMemStore()
			store.notes["cn_u"] = domain.CreditNote{ID: "cn_u", TenantID: "t1", InvoiceID: "inv_u", StripeRefundID: "re_u", RefundStatus: domain.RefundPending}
			svc := NewService(store, &memInvoiceReader{invoices: map[string]domain.Invoice{}}, nil)
			res := &countingResolver{err: errors.New("connection reset by peer")}
			svc.SetResolver(res)
			err := svc.ApplyRefundWebhook(c.ctx, "t1", "re_u", domain.RefundSucceeded, "")
			if res.reads != c.wantReads || (err != nil) != c.wantErr || (err != nil && !errors.Is(err, clock.ErrUndecided)) {
				t.Errorf("clock reads %d, err %v; want %d reads, error %v (clock.ErrUndecided)", res.reads, err, c.wantReads, c.wantErr)
			}
			if got := store.notes["cn_u"].RefundStatus; c.wantErr && got != domain.RefundPending {
				t.Errorf("a refused event moved the refund to %s", got)
			}
		})
	}
}
