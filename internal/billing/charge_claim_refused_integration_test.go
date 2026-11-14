package billing_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/errs"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
)

// A clock's collection job whose claim of an invoice is refused does what the
// store answers, through the engine and the real store. The customer paying
// on the hosted page holds collection, so the advance has nothing to do (a
// card-less clock customer opened the page and left it, then a card was
// added). An earlier attempt's hold fails the advance, saying to wait for it.
func TestCollectForClock_RefusedClaimDoesWhatTheStoreSays(t *testing.T) {
	for _, c := range []struct {
		name    string
		block   string // run with $1 = the invoice's id, $2 = the tenant
		wantErr string // "" = no error; else in what the operator reads
	}{
		{"the customer on the hosted page",
			`INSERT INTO checkout_sessions (id, tenant_id, invoice_id, livemode, stripe_session_id, amount_cents, currency, status, expires_at)
			 VALUES ('cks_' || $1, $2, $1, false, 'cs_' || $1, 5000, 'USD', 'open', now() + interval '50 minutes')`, ""},
		{"an earlier attempt's stale hold",
			`UPDATE invoices SET auto_charge_claimed_until = now() - interval '1 hour' WHERE id = $1 AND tenant_id = $2`,
			"an earlier charge attempt has not finished, so it was not charged again"},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newCollectHarness(t)
			inv := h.seed(t, "INV-REFUSED-1")
			clockID := postgres.NewID("hc_tclk")
			tx, err := h.db.BeginTx(h.ctx, postgres.TxBypass, "")
			if err != nil {
				t.Fatalf("begin: %v", err)
			}
			for _, step := range []struct {
				sql  string
				args []any
			}{
				{`INSERT INTO test_clocks (id, tenant_id, name, frozen_time, status, livemode) VALUES ($1, $2, 'refused', now(), 'ready', false)`, []any{clockID, h.tenantID}},
				{`UPDATE customers SET test_clock_id = $1 WHERE id = $2`, []any{clockID, inv.CustomerID}},
				{c.block, []any{inv.ID, h.tenantID}},
			} {
				if _, err := tx.ExecContext(h.ctx, step.sql, step.args...); err != nil {
					t.Fatalf("seed: %v", err)
				}
			}
			if err := tx.Commit(); err != nil {
				t.Fatalf("commit: %v", err)
			}

			charger := &countingCharger{store: h.invoices}
			e := h.newEng(cardFor{inv.CustomerID: true}, charger, nil)
			_, runErrs := e.RetryPendingChargesForClock(h.ctx, h.tenantID, clockID, 50)
			if n := charger.count(inv.ID); n != 0 {
				t.Fatalf("charged %d times, want none", n)
			}
			if c.wantErr == "" {
				if len(runErrs) != 0 {
					t.Fatalf("errors = %v, want none: the hosted page holds collection", runErrs)
				}
				return
			}
			if len(runErrs) != 1 || !errors.Is(runErrs[0], domain.ErrLeftForNextRun) {
				t.Fatalf("errors = %v, want one ErrLeftForNextRun", runErrs)
			}
			if got := errs.SummarizeForOperator(runErrs, ""); !strings.Contains(got, c.wantErr) {
				t.Errorf("the operator reads %q, want it to say %q", got, c.wantErr)
			}
		})
	}
}
