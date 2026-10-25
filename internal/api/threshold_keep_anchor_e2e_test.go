package api

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
)

// A spend threshold always keeps the billing period's anchor: the invoice it
// fires bills the period's usage so far, the period's next invoice bills the
// rest of the usage, and the base fee is billed as with no threshold. The
// option to start a new period at the fire
// (`reset_billing_cycle: true`) is removed (2026-10-23).
//
// That option under-billed an in_advance base on an anniversary subscription
// (ADR-066 §5): the new period started at the fire, its first stretch was
// covered by no prepaid base, and no invoice ever billed it.

var updateKeepAnchorGolden = flag.Bool("update-keep-anchor-golden", false, "rewrite testdata/threshold_keep_anchor.golden")

// TestThresholdInAdvanceBase_FireThenCloseBillsEveryMonth is the ADR-066 §5
// case. An anniversary subscription starts April 1 on a 3000c in_advance
// base, so its create invoice prepays April. 100 calls on April 10 cross a
// 100c spend threshold, which fires on April 20. The billing run then runs
// on May 1, May 21 and June 1.
//
// The operator first asks for a new billing period at each fire. That is
// refused with 422. Before the removal it was accepted: the fire started a
// period on April 20 that ran to May 20, April's prepaid base covered it
// only to May 1, and its close billed the base from May 20. May 1 to 20 was
// billed by no invoice.
//
// With the anchor kept, the base is billed for April, May and June in full
// (9000c), the threshold invoice carries no base (April was prepaid), and
// the subscription's period is June.
func TestThresholdInAdvanceBase_FireThenCloseBillsEveryMonth(t *testing.T) {
	skipShort(t)
	r := newLagRigBare(t, "in_advance", 0)
	r.newSub("adv", r.planA, lagP0, nil)

	setAt := lagP0.AddDate(0, 0, 4)
	path := "/v1/subscriptions/" + r.subID + "/billing-thresholds"
	if code, raw := r.do(http.MethodPut, path, setAt, map[string]any{"amount_gte": 100, "reset_billing_cycle": true}); code != http.StatusUnprocessableEntity {
		t.Errorf("reset_billing_cycle: true answered %d %s, want 422: the option is removed", code, raw)
	} else {
		r.must(http.MethodPut, path, setAt, map[string]any{"amount_gte": 100}, http.StatusOK)
	}

	r.ingest(time.Date(2026, 4, 10, 9, 0, 0, 0, time.UTC), 100)
	fireAt := time.Date(2026, 4, 20, 12, 0, 0, 0, time.UTC)
	if n := r.scanThresholds(fireAt); n != 1 {
		t.Fatalf("threshold scan fired %d invoices, want 1", n)
	}
	for _, at := range []time.Time{lagB, time.Date(2026, 5, 21, 0, 0, 0, 0, time.UTC), lagP2} {
		r.run(at.Add(2 * time.Hour))
	}

	invs := r.invoices()
	var base int64
	for _, i := range invs {
		base += i.sum("base_fee")
		if i.reason == string(domain.BillingReasonThreshold) && i.sum("base_fee") != 0 {
			t.Errorf("threshold invoice billed base %dc, want 0c: April's base was prepaid", i.sum("base_fee"))
		}
	}
	s := r.sub()
	if base != 3*3000 {
		r.logInvoices(invs)
		t.Errorf("base billed April 1 to %v = %dc, want 9000c (April, May and June in full)", s.CurrentBillingPeriodEnd, base)
	}
	if !s.CurrentBillingPeriodStart.Equal(lagP2) || !s.CurrentBillingPeriodEnd.Equal(lagP2.AddDate(0, 1, 0)) {
		t.Errorf("subscription period = [%v, %v), want [%v, %v): the fire keeps the anchor", s.CurrentBillingPeriodStart, s.CurrentBillingPeriodEnd, lagP2, lagP2.AddDate(0, 1, 0))
	}
	// An in_advance cycle invoice is headed at the month it prepays.
	for _, start := range []time.Time{lagB, lagP2} {
		if got := r.cycleInvoice(invs, start).sum("base_fee"); got != 3000 {
			r.logInvoices(invs)
			t.Errorf("cycle invoice for the month from %v billed base %dc, want 3000c", start, got)
		}
	}
}

// TestThresholdInArrearsBase_FireThenCancelBillsTheUsedDays: a spend
// threshold must not change what the base fee costs. A subscription starts
// April 1 on a 3000c in_arrears base. Canceled at once on April 25 with no
// threshold, it pays 24 of 30 days: 2400c. With a threshold that fired on
// April 20, the same cancel must also bill 2400c of base, in all.
//
// Before the fix the fire billed the full month's base (3000c) and the
// cancel billed none, so the threshold cost the customer 600c more.
func TestThresholdInArrearsBase_FireThenCancelBillsTheUsedDays(t *testing.T) {
	skipShort(t)
	cancelAt := lagP0.AddDate(0, 0, 24)

	control := newLagRigBare(t, "in_arrears", 0, WithInvoiceGrace(e2eGrace))
	peak := control.newMeter("peak", "max")
	control.newSub("ctl", control.planWith("plan_p", 3000, control.meterID, peak), lagP0, nil)
	control.cancelNow(cancelAt)
	var want int64
	for _, i := range control.invoices() {
		want += i.sum("base_fee")
	}
	if want != 2400 {
		control.logInvoices(control.invoices())
		t.Fatalf("control: an immediate cancel on April 25 billed %dc of base, want 2400c (24 of 30 days)", want)
	}

	r, _, _ := keepAnchorFired(t)
	r.cancelNow(cancelAt)
	var got int64
	invs := r.invoices()
	for _, i := range invs {
		got += i.sum("base_fee")
		if i.reason == string(domain.BillingReasonThreshold) && i.sum("base_fee") != 0 {
			t.Errorf("threshold invoice billed base %dc, want 0c: the base is billed by the period's own invoice", i.sum("base_fee"))
		}
	}
	if got != want {
		r.logInvoices(invs)
		t.Errorf("fire on April 20, cancel on April 25: base billed %dc in all, want %dc (the same as without the threshold)", got, want)
	}
}

// TestThresholdInArrearsBase_FireThenCloseBillsTheBaseOnClose: after a fire,
// the April close bills April's whole in_arrears base, once, and the preview
// of April's invoice shows it. The threshold invoice bills usage only.
func TestThresholdInArrearsBase_FireThenCloseBillsTheBaseOnClose(t *testing.T) {
	skipShort(t)
	r, fireAt, _ := keepAnchorFired(t)
	if got := r.previewSum(fireAt.Add(time.Hour), "base_fee"); got != 3000 {
		t.Errorf("preview of April's invoice after the fire shows base %dc, want 3000c", got)
	}
	r.run(lagB.Add(2 * time.Hour))
	invs := r.invoices()
	var total int64
	for _, i := range invs {
		total += i.sum("base_fee")
		if i.reason == string(domain.BillingReasonThreshold) && i.sum("base_fee") != 0 {
			t.Errorf("threshold invoice billed base %dc, want 0c", i.sum("base_fee"))
		}
	}
	if got := r.cycleInvoice(invs, lagP0).sum("base_fee"); got != 3000 || total != 3000 {
		r.logInvoices(invs)
		t.Errorf("April close billed base %dc (%dc in all), want 3000c on the close and 3000c in all", got, total)
	}
}

// TestThresholdInArrearsBase_LegacyFireWithBase_BilledOnce: a fire made
// before fires stopped billing the base carried the full in_arrears base. A
// period open across the upgrade must still bill its base exactly once, by
// the close or by an immediate cancel. The fixture adds the base line the old
// code wrote to the threshold invoice.
//
// The late-usage runbook's void and write-off guidance covers that base like
// the usage: written off, it stays billed on the threshold invoice and the
// close does not bill it again; voided before the close, the close bills it.
func TestThresholdInArrearsBase_LegacyFireWithBase_BilledOnce(t *testing.T) {
	skipShort(t)
	for _, tc := range []struct {
		name  string
		close func(r *lagRig, fireAt time.Time, thrID string)
	}{
		{"the April close", func(r *lagRig, _ time.Time, _ string) { r.run(lagB.Add(2 * time.Hour)) }},
		{"an immediate cancel", func(r *lagRig, fireAt time.Time, _ string) { r.cancelNow(fireAt.Add(5 * 24 * time.Hour)) }},
		{"written off, then the April close", func(r *lagRig, fireAt time.Time, thrID string) {
			r.invoiceAction(thrID, "mark-uncollectible", fireAt.Add(time.Hour))
			r.run(lagB.Add(2 * time.Hour))
		}},
		{"voided, then the April close", func(r *lagRig, fireAt time.Time, thrID string) {
			r.invoiceAction(thrID, "void", fireAt.Add(time.Hour))
			r.run(lagB.Add(2 * time.Hour))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, fireAt, thrID := keepAnchorFired(t)
			r.exec(`INSERT INTO invoice_line_items (tenant_id, invoice_id, line_type, description, quantity, unit_amount_cents, amount_cents, total_amount_cents, currency)
				SELECT tenant_id, id, 'base_fee', 'plan_p - base fee (qty 1)', 1, 3000, 3000, 3000, currency FROM invoices WHERE id = $1`, thrID)
			tc.close(r, fireAt, thrID)
			invs := r.invoices()
			var base int64
			for _, i := range invs {
				base += i.sum("base_fee")
			}
			if base != 3000 {
				r.logInvoices(invs)
				t.Errorf("base billed %dc in all, want 3000c: the legacy fire billed it, and nothing bills it again", base)
			}
		})
	}
}

// TestThresholdKeepAnchor_InvoicesUnchanged is the control for the removal:
// what a spend threshold bills when it keeps the anchor must not change by a
// single byte. Each case drives the real server and writes every invoice of
// the subscription, with its lines, in a fixed text form. The golden file
// was written on main before the removal (a08b5e2a) and still matched after
// it. It was rewritten once on purpose, by the review fix that moved the
// in_arrears base off the fire: the threshold invoices lost their base line,
// and the close and the cancel gained it (3000c full, 2400c prorated). Every
// usage line stayed the same. Since ADR-130 §3 (PR-3) the label of a line
// priced per unit is mode=per_unit, not mode=flat: the only edit since.
//
// The cases cover each reader of the threshold watermark: the fire's line
// build (usage only, a max bucket left for the close, a prepaid in_advance
// base skipped), the cycle close (the full base, usage from the fire on, the
// max bucket over the whole month), an item threshold (its config hydrated
// from the item rows alone) and the immediate cancel (the base prorated to
// the cancel).
//
// Regenerate only on purpose: go test ./internal/api -run
// TestThresholdKeepAnchor_InvoicesUnchanged -short=false
// -update-keep-anchor-golden.
func TestThresholdKeepAnchor_InvoicesUnchanged(t *testing.T) {
	skipShort(t)
	var out strings.Builder
	for _, tc := range []struct {
		name string
		run  func(t *testing.T) *lagRig
	}{
		{"in_arrears base, sum and max meters, then the April close", func(t *testing.T) *lagRig {
			r, _, _ := keepAnchorFired(t)
			r.run(lagB.Add(2 * time.Hour))
			return r
		}},
		{"in_arrears base, sum and max meters, then an immediate cancel", func(t *testing.T) *lagRig {
			r, fireAt, _ := keepAnchorFired(t)
			r.cancelNow(fireAt.Add(5 * 24 * time.Hour))
			return r
		}},
		{"in_advance base, then the April close", func(t *testing.T) *lagRig {
			r := newLagRigBare(t, "in_advance", 0)
			r.newSub("adv", r.planA, lagP0, nil)
			r.must(http.MethodPut, "/v1/subscriptions/"+r.subID+"/billing-thresholds", lagP0.AddDate(0, 0, 4),
				map[string]any{"amount_gte": 100}, http.StatusOK)
			r.ingest(time.Date(2026, 4, 10, 9, 0, 0, 0, time.UTC), 100)
			r.ingest(time.Date(2026, 4, 25, 9, 0, 0, 0, time.UTC), 40)
			if n := r.scanThresholds(time.Date(2026, 4, 20, 12, 0, 0, 0, time.UTC)); n != 1 {
				t.Fatalf("threshold scan fired %d invoices, want 1", n)
			}
			r.run(lagB.Add(2 * time.Hour))
			return r
		}},
		{"item threshold only, then the April close", func(t *testing.T) *lagRig {
			r := newLagRigBare(t, "in_arrears", 0)
			r.newSub("item", r.planA, lagP0, nil)
			item := r.sub().Items[0].ID
			r.must(http.MethodPut, "/v1/subscriptions/"+r.subID+"/billing-thresholds", lagP0.AddDate(0, 0, 4),
				map[string]any{"item_thresholds": []map[string]any{{"subscription_item_id": item, "usage_gte": "50"}}}, http.StatusOK)
			r.ingest(time.Date(2026, 4, 10, 9, 0, 0, 0, time.UTC), 60)
			r.ingest(time.Date(2026, 4, 22, 9, 0, 0, 0, time.UTC), 25)
			if n := r.scanThresholds(time.Date(2026, 4, 15, 12, 0, 0, 0, time.UTC)); n != 1 {
				t.Fatalf("threshold scan fired %d invoices, want 1", n)
			}
			r.run(lagB.Add(2 * time.Hour))
			return r
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := tc.run(t)
			fmt.Fprintf(&out, "== %s\n%s\n", tc.name, r.dumpForGolden())
		})
	}
	if t.Failed() {
		return
	}

	goldenPath := filepath.Join("testdata", "threshold_keep_anchor.golden")
	if *updateKeepAnchorGolden {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(goldenPath, []byte(out.String()), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("wrote %s", goldenPath)
		return
	}
	want, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("read %s: %v", goldenPath, err)
	}
	if got := out.String(); got != string(want) {
		t.Errorf("keep-anchor invoices differ from %s.\n--- got\n%s\n--- want\n%s", goldenPath, got, want)
	}
}

// keepAnchorFired is thresholdFired's rig with the threshold set by the
// only body a keep-anchor threshold needs: the amount, nothing else. It
// returns the fire instant and the threshold invoice.
func keepAnchorFired(t *testing.T) (*lagRig, time.Time, string) {
	t.Helper()
	r := newLagRigBare(t, "in_arrears", 0, WithInvoiceGrace(e2eGrace))
	peak := r.newMeter("peak", "max")
	r.newSub("thr", r.planWith("plan_p", 3000, r.meterID, peak), lagP0, nil)
	r.postEvent("/v1/usage-events", "calls", "calls-apr10", lagP0.AddDate(0, 0, 9), lagP0.AddDate(0, 0, 9), 100, http.StatusCreated)
	r.postEvent("/v1/usage-events", "peak", "peak-apr10", lagP0.AddDate(0, 0, 9), lagP0.AddDate(0, 0, 9), 5, http.StatusCreated)
	r.postEvent("/v1/usage-events", "calls", "calls-apr22", lagP0.AddDate(0, 0, 21), lagP0.AddDate(0, 0, 21), 30, http.StatusCreated)
	r.postEvent("/v1/usage-events", "peak", "peak-apr22", lagP0.AddDate(0, 0, 21), lagP0.AddDate(0, 0, 21), 7, http.StatusCreated)
	fireAt := lagP0.AddDate(0, 0, 19)
	r.must(http.MethodPut, "/v1/subscriptions/"+r.subID+"/billing-thresholds", fireAt.Add(-time.Hour),
		map[string]any{"amount_gte": 100}, http.StatusOK)
	if n := r.scanThresholds(fireAt); n != 1 {
		t.Fatalf("threshold scan fired %d invoices, want 1", n)
	}
	for _, inv := range r.invoices() {
		if inv.reason == string(domain.BillingReasonThreshold) {
			return r, fireAt, inv.id
		}
	}
	t.Fatal("no threshold invoice")
	return nil, time.Time{}, ""
}

// dumpForGolden writes the subscription's period and every invoice it has
// (voided ones too), with their lines, in a fixed text form. Ids, invoice
// numbers and creation times differ from run to run and are left out.
func (r *lagRig) dumpForGolden() string {
	r.t.Helper()
	ctx := context.Background()
	tx, err := r.db.BeginTx(ctx, postgres.TxBypass, "")
	if err != nil {
		r.t.Fatalf("begin: %v", err)
	}
	defer postgres.Rollback(tx)
	var b strings.Builder
	ts := func(p *time.Time) string {
		if p == nil {
			return "-"
		}
		return p.UTC().Format(time.RFC3339)
	}
	var ps, pe *time.Time
	var status string
	if err := tx.QueryRowContext(ctx, `SELECT status, current_billing_period_start, current_billing_period_end FROM subscriptions WHERE id = $1`, r.subID).Scan(&status, &ps, &pe); err != nil {
		r.t.Fatalf("read subscription: %v", err)
	}
	fmt.Fprintf(&b, "subscription status=%s period=[%s, %s)\n", status, ts(ps), ts(pe))
	rows, err := tx.QueryContext(ctx, `SELECT id, billing_reason, status, payment_status, billing_period_start, billing_period_end,
			subtotal_cents, discount_cents, tax_amount_cents, total_amount_cents, amount_due_cents, credits_applied_cents, currency
		FROM invoices WHERE subscription_id = $1 ORDER BY billing_period_start, billing_period_end, billing_reason`, r.subID)
	if err != nil {
		r.t.Fatalf("list invoices: %v", err)
	}
	type inv struct {
		id, line string
	}
	var invs []inv
	for rows.Next() {
		var id, reason, st, pay, cur string
		var start, end time.Time
		var sub, disc, tax, total, due, credits int64
		if err := rows.Scan(&id, &reason, &st, &pay, &start, &end, &sub, &disc, &tax, &total, &due, &credits, &cur); err != nil {
			r.t.Fatalf("scan invoice: %v", err)
		}
		invs = append(invs, inv{id, fmt.Sprintf("invoice reason=%s status=%s payment=%s period=[%s, %s) subtotal=%d discount=%d tax=%d total=%d due=%d credits=%d currency=%s",
			reason, st, pay, ts(&start), ts(&end), sub, disc, tax, total, due, credits, cur)})
	}
	_ = rows.Close()
	for _, i := range invs {
		b.WriteString(i.line + "\n")
		lr, err := tx.QueryContext(ctx, `SELECT line_type, description, quantity, quantity_decimal::text, unit_amount_cents,
				COALESCE(nominal_unit_amount_decimal::text, ''), amount_cents, tax_amount_cents, total_amount_cents,
				COALESCE(pricing_mode, ''), COALESCE(proration_factor::text, ''), billing_period_start, billing_period_end
			FROM invoice_line_items WHERE invoice_id = $1 ORDER BY line_type, description, amount_cents`, i.id)
		if err != nil {
			r.t.Fatalf("list lines: %v", err)
		}
		for lr.Next() {
			var typ, desc, qd, nominal, mode, factor string
			var qty, unit, amount, tax, total int64
			var ls, le *time.Time
			if err := lr.Scan(&typ, &desc, &qty, &qd, &unit, &nominal, &amount, &tax, &total, &mode, &factor, &ls, &le); err != nil {
				r.t.Fatalf("scan line: %v", err)
			}
			fmt.Fprintf(&b, "  line %s %q qty=%d qty_decimal=%s unit=%d nominal=%s amount=%d tax=%d total=%d mode=%s proration=%s period=[%s, %s)\n",
				typ, desc, qty, qd, unit, nominal, amount, tax, total, mode, factor, ts(ls), ts(le))
		}
		_ = lr.Close()
	}
	return b.String()
}
