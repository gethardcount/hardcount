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

// instantAudit records the instant each audit row is written on (zero when
// none is bound).
type instantAudit struct{ at []time.Time }

func (a *instantAudit) LogInTx(ctx context.Context, _ *sql.Tx, _ audit.Entry) error {
	at, _ := clock.EffectiveNow(ctx)
	a.at = append(a.at, at)
	return nil
}

// A refund's status event runs on one instant, one read of the server's
// clock, in either mode. The status write's audit row shows it.
func TestApplyRefundWebhook_RunsOnOneInstant(t *testing.T) {
	past := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	for _, live := range []bool{false, true} {
		store := newMemStore()
		store.notes["cn_u"] = domain.CreditNote{ID: "cn_u", TenantID: "t1", InvoiceID: "inv_u", StripeRefundID: "re_u", RefundStatus: domain.RefundPending}
		svc := NewService(store, &memInvoiceReader{invoices: map[string]domain.Invoice{}}, nil)
		server := &cnSteppingClock{at: past}
		svc.SetClock(server)
		rows := &instantAudit{}
		svc.SetAuditLogger(rows)
		if err := svc.ApplyRefundWebhook(postgres.WithLivemode(context.Background(), live), "t1", "re_u", domain.RefundSucceeded, ""); err != nil {
			t.Fatalf("live %v: ApplyRefundWebhook: %v", live, err)
		}
		if len(rows.at) != 1 || !rows.at[0].Equal(past.Add(time.Second)) || server.reads != 1 {
			t.Errorf("live %v: audit rows at %v after %d server reads; want one at %v after 1",
				live, rows.at, server.reads, past.Add(time.Second))
		}
	}
}

// A refund status event that no note holds is answered not-found by the
// write; it binds one read of the service's clock, and nothing is written or
// audited.
func TestApplyRefundWebhook_NoNoteBindsOneReadAndWritesNothing(t *testing.T) {
	past := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	store := newMemStore()
	svc := NewService(store, &memInvoiceReader{invoices: map[string]domain.Invoice{}}, nil)
	server := &cnSteppingClock{at: past}
	svc.SetClock(server)
	rows := &instantAudit{}
	svc.SetAuditLogger(rows)
	err := svc.ApplyRefundWebhook(postgres.WithLivemode(context.Background(), false), "t1", "re_nobody", domain.RefundSucceeded, "")
	if !errors.Is(err, errs.ErrNotFound) || server.reads != 1 || len(rows.at) != 0 {
		t.Errorf("err %v, %d server reads, audit rows %v; want not-found, one, none", err, server.reads, rows.at)
	}
}
