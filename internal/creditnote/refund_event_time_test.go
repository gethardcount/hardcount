package creditnote

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/audit"
	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/errs"
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

// instantAudit records the instant each audit row is written on (zero when
// none is bound).
type instantAudit struct{ at []time.Time }

func (a *instantAudit) LogInTx(ctx context.Context, _ *sql.Tx, _ audit.Entry) error {
	at, _ := clock.EffectiveNow(ctx)
	a.at = append(a.at, at)
	return nil
}

// A refund's status event runs on one instant: the note's invoice's clock,
// with no read of the server's clock; for an invoice on no clock, and in
// live mode, one read of the server's clock. The status write's audit row
// shows it.
func TestApplyRefundWebhook_RunsOnOneInstant(t *testing.T) {
	frozen := time.Date(2027, 2, 3, 9, 0, 0, 0, time.UTC)
	past := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	onClock := &countingResolver{}
	noClock := &invoiceAnswer{sim: clock.Sim{At: time.Now()}}
	live := &countingResolver{err: errors.New("must not be read")}
	for _, c := range []struct {
		name            string
		ctx             context.Context
		res             clock.Resolver
		decisions       func() int
		wantAt          time.Time
		wantServerReads int
		wantDecisions   int
	}{
		{"an invoice on a clock", postgres.WithLivemode(context.Background(), false), onClock, func() int { return onClock.reads }, frozen, 0, 1},
		{"an invoice on no clock", postgres.WithLivemode(context.Background(), false), noClock, func() int { return noClock.reads }, past.Add(time.Second), 1, 1},
		{"live mode", postgres.WithLivemode(context.Background(), true), live, func() int { return live.reads }, past.Add(time.Second), 1, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			store := newMemStore()
			store.notes["cn_u"] = domain.CreditNote{ID: "cn_u", TenantID: "t1", InvoiceID: "inv_u", StripeRefundID: "re_u", RefundStatus: domain.RefundPending}
			svc := NewService(store, &memInvoiceReader{invoices: map[string]domain.Invoice{}}, nil)
			svc.SetResolver(c.res)
			server := &cnSteppingClock{at: past}
			svc.SetClock(server)
			rows := &instantAudit{}
			svc.SetAuditLogger(rows)
			if err := svc.ApplyRefundWebhook(c.ctx, "t1", "re_u", domain.RefundSucceeded, ""); err != nil {
				t.Fatalf("ApplyRefundWebhook: %v", err)
			}
			if len(rows.at) != 1 || !rows.at[0].Equal(c.wantAt) || server.reads != c.wantServerReads || c.decisions() != c.wantDecisions {
				t.Errorf("audit rows at %v after %d server reads and %d decisions; want one at %v after %d and %d",
					rows.at, server.reads, c.decisions(), c.wantAt, c.wantServerReads, c.wantDecisions)
			}
		})
	}
}

// A refund status event that no note holds is answered not-found by the
// write; its decision found no invoice, so it binds one read of the service's
// clock, and nothing is written or audited.
func TestApplyRefundWebhook_NoNoteBindsOneReadAndWritesNothing(t *testing.T) {
	past := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	store := newMemStore()
	svc := NewService(store, &memInvoiceReader{invoices: map[string]domain.Invoice{}}, nil)
	res := &countingResolver{}
	svc.SetResolver(res)
	server := &cnSteppingClock{at: past}
	svc.SetClock(server)
	rows := &instantAudit{}
	svc.SetAuditLogger(rows)
	err := svc.ApplyRefundWebhook(postgres.WithLivemode(context.Background(), false), "t1", "re_nobody", domain.RefundSucceeded, "")
	if !errors.Is(err, errs.ErrNotFound) || res.reads != 0 || server.reads != 1 || len(rows.at) != 0 {
		t.Errorf("err %v, %d resolver reads, %d server reads, audit rows %v; want not-found, none, one, none", err, res.reads, server.reads, rows.at)
	}
}
