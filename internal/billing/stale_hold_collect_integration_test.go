package billing_test

import (
	"database/sql"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
)

// The auto-charge sweep and the charge hold (#976, ADR-127), on the real
// invoice store. Only the charge is faked.

func (h collectHarness) execBypass(t *testing.T, q string, args ...any) {
	t.Helper()
	tx, err := h.db.BeginTx(h.ctx, postgres.TxBypass, "")
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer postgres.Rollback(tx)
	if _, err := tx.ExecContext(h.ctx, q, args...); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

func (h collectHarness) hold(t *testing.T, invoiceID string) *time.Time {
	t.Helper()
	tx, err := h.db.BeginTx(h.ctx, postgres.TxBypass, "")
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer postgres.Rollback(tx)
	var at sql.NullTime
	if err := tx.QueryRowContext(h.ctx, `SELECT auto_charge_claimed_until FROM invoices WHERE id = $1`, invoiceID).Scan(&at); err != nil {
		t.Fatalf("read hold: %v", err)
	}
	if !at.Valid {
		return nil
	}
	return &at.Time
}

// A charge whose result was lost left a stale hold. The sweep does not charge
// the invoice again, tick after tick — only the stale-hold sweep, after
// asking Stripe, can clear the hold — and it does not clear the hold itself.
func TestCollect_StaleHold_SweepDoesNotCharge(t *testing.T) {
	h := newCollectHarness(t)
	inv := h.seed(t, "INV-STALE-HOLD")
	h.execBypass(t, `UPDATE invoices SET auto_charge_claimed_until = now() - interval '11 minutes' WHERE id = $1`, inv.ID)

	charger := &countingCharger{store: h.invoices}
	e := h.newEng(testPaymentSetupsReady{}, charger, nil)
	for tick := 0; tick < 2; tick++ {
		e.RetryPendingCharges(h.ctx, 50, nil)
	}
	if got := charger.count(inv.ID); got != 0 {
		t.Fatalf("charges = %d on an invoice whose earlier charge may have taken the money, want 0", got)
	}
	if h.hold(t, inv.ID) == nil {
		t.Fatal("the sweep cleared a stale hold: only the stale-hold sweep may, after asking Stripe")
	}
}

// Nothing owed (credits covered it): the sweep settles the invoice with no
// charge and releases its hold. Left set, the hold would block the invoice's
// other paths until the stale-hold sweep looked at it, for nothing.
func TestCollect_ZeroDue_SettledAndHoldReleased(t *testing.T) {
	h := newCollectHarness(t)
	inv := h.seed(t, "INV-ZERO-HOLD")
	h.execBypass(t, `UPDATE invoices SET amount_due_cents = 0, credits_applied_cents = total_amount_cents WHERE id = $1`, inv.ID)

	charger := &countingCharger{store: h.invoices}
	e := h.newEng(testPaymentSetupsReady{}, charger, nil)
	e.RetryPendingCharges(h.ctx, 50, nil)

	got, err := h.invoices.Get(h.ctx, h.tenantID, inv.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Status != domain.InvoicePaid {
		t.Fatalf("status %s, want paid", got.Status)
	}
	if n := charger.count(inv.ID); n != 0 {
		t.Fatalf("charged %d times an invoice with nothing owed", n)
	}
	if at := h.hold(t, inv.ID); at != nil {
		t.Fatalf("charge hold still set (%v) after a settle that sent no charge", *at)
	}
}
