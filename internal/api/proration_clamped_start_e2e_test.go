package api

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/platform/postgres"
)

// A subscription billed on the 31st has a period that starts on a shortened
// month end: Feb 28 to Mar 31 is 31 days, a full cycle. A change in that
// period is prorated over its 31 days. The share used to be worked out from
// the start's own day (28), so the cycle was 28 days, and a $100 base going
// from 1 to 2 seats on Mar 16 billed 5714 + 10714 = 16428c, where 5161 +
// 9677 = 14838c is right.
//
// Real server over HTTP, real Postgres. The subscription starts Jan 31 2027
// on a monthly plan billed in arrears with a 10000c base fee, and the Feb 28
// close opens [Feb 28, Mar 31).

var (
	clampedStart = time.Date(2027, 1, 31, 0, 0, 0, 0, time.UTC)
	clampedFrom  = time.Date(2027, 2, 28, 0, 0, 0, 0, time.UTC)
	clampedTo    = time.Date(2027, 3, 31, 0, 0, 0, 0, time.UTC)
)

type clampedLine struct {
	qty    int64
	amount int64
}

// newClampedRig is the clamped period's subscription with one item on the
// 10000c plan, and with addOn a second item on a 5000c plan (r.planC).
func newClampedRig(t *testing.T, addOn bool) *lagRig {
	t.Helper()
	r := newLagRigBare(t, "in_arrears", 0)
	plan := func(code string, base int64) string {
		p := r.must(http.MethodPost, "/v1/plans", clampedStart, map[string]any{
			"code": code, "name": code, "currency": "USD", "billing_interval": "monthly",
			"base_amount_cents": base, "base_bill_timing": "in_arrears", "prices": []map[string]any{},
		}, http.StatusCreated)
		return p["id"].(string)
	}
	items := []map[string]any{{"plan_id": plan("Clamped", 10000), "quantity": 1}}
	if addOn {
		r.planC = plan("ClampedAddOn", 5000)
		items = append(items, map[string]any{"plan_id": r.planC, "quantity": 1})
	}
	sub := r.must(http.MethodPost, "/v1/subscriptions", clampedStart, map[string]any{
		"code": "clamped-sub", "display_name": "Clamped", "customer_id": r.custID, "billing_time": "anniversary",
		"items": items,
	}, http.StatusCreated)
	r.subID = sub["id"].(string)
	r.run(clampedFrom.Add(time.Hour))
	s := r.sub()
	if !s.CurrentBillingPeriodStart.Equal(clampedFrom) || !s.CurrentBillingPeriodEnd.Equal(clampedTo) || s.BillingAnchorDay != 31 {
		t.Fatalf("fixture period = [%v, %v) anchor %d, want [%v, %v) anchor 31",
			s.CurrentBillingPeriodStart, s.CurrentBillingPeriodEnd, s.BillingAnchorDay, clampedFrom, clampedTo)
	}
	for _, it := range s.Items {
		if it.PlanID != r.planC {
			r.itemA = it.ID
		}
	}
	return r
}

// clampedBaseLines reads the base-fee lines of the subscription's invoice for
// reason, in period order, and checks each says it is prorated over 31 days.
func clampedBaseLines(t *testing.T, r *lagRig, reason string) []clampedLine {
	t.Helper()
	ctx := context.Background()
	tx, err := r.db.BeginTx(ctx, postgres.TxBypass, "")
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer postgres.Rollback(tx)
	rows, err := tx.QueryContext(ctx, `SELECT li.quantity, li.amount_cents, li.description
		FROM invoice_line_items li JOIN invoices i ON i.id = li.invoice_id
		WHERE i.subscription_id = $1 AND i.billing_reason = $2 AND i.status <> 'voided' AND li.line_type = 'base_fee'
		  AND i.billing_period_start = $3
		ORDER BY li.billing_period_start, li.amount_cents`, r.subID, reason, clampedFrom)
	if err != nil {
		t.Fatalf("list base lines: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var out []clampedLine
	for rows.Next() {
		var l clampedLine
		var desc string
		if err := rows.Scan(&l.qty, &l.amount, &desc); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if !strings.Contains(desc, "/31 days") {
			t.Errorf("base line %q: want it prorated over the period's 31 days", desc)
		}
		out = append(out, l)
	}
	return out
}

func wantClampedLines(t *testing.T, what string, got, want []clampedLine) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: base lines %+v, want %+v", what, got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("%s: base line %d = %+v, want %+v", what, i, got[i], want[i])
		}
	}
}

// overBilledLinesQuery is the query the operator runs (from the CHANGELOG) to
// find base-fee lines this fix may have changed: base-fee lines on cycle and
// cancel invoices of subscriptions anchored on the 29th or later, whose
// invoice period starts on the last day of its month before the anchor day,
// and that are a share of the period.
const overBilledLinesQuery = `SELECT i.tenant_id, i.id AS invoice_id, i.invoice_number, i.billing_reason,
       li.id AS line_id, li.description, li.amount_cents,
       s.id AS subscription_id, s.billing_anchor_day
FROM invoice_line_items li
JOIN invoices i ON i.id = li.invoice_id
JOIN subscriptions s ON s.id = i.subscription_id
CROSS JOIN LATERAL (
  SELECT i.billing_period_start AT TIME ZONE COALESCE(NULLIF(i.billing_timezone, ''), 'UTC') AS local_start
) z
WHERE li.line_type = 'base_fee'
  AND i.billing_reason IN ('subscription_cycle', 'subscription_cancel')
  AND i.status <> 'voided'
  AND s.billing_anchor_day >= 29
  -- the invoice's period starts on the last day of its month, before the anchor day
  AND z.local_start::date = (date_trunc('month', z.local_start) + interval '1 month - 1 day')::date
  AND extract(day FROM z.local_start) < s.billing_anchor_day
  -- a share of the period: a narrower line, or a cancel's final invoice
  AND (i.billing_reason = 'subscription_cancel'
       OR li.billing_period_start > i.billing_period_start
       OR li.billing_period_end < i.billing_period_end)
  AND s.id = $1`

func overBilledLines(t *testing.T, r *lagRig) int {
	t.Helper()
	ctx := context.Background()
	tx, err := r.db.BeginTx(ctx, postgres.TxBypass, "")
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer postgres.Rollback(tx)
	rows, err := tx.QueryContext(ctx, overBilledLinesQuery, r.subID)
	if err != nil {
		t.Fatalf("over-billed lines query: %v", err)
	}
	defer func() { _ = rows.Close() }()
	n := 0
	for rows.Next() {
		n++
	}
	return n
}

func TestProration_ClampedStart_ChargesOverTheFullCycle(t *testing.T) {
	change := func(r *lagRig, at time.Time) {
		r.must(http.MethodPatch, "/v1/subscriptions/"+r.subID+"/items/"+r.itemA, at, map[string]any{"quantity": 2}, http.StatusOK)
	}

	t.Run("close after a change on Mar 16, and the preview equals it", func(t *testing.T) {
		r := newClampedRig(t, false)
		change(r, time.Date(2027, 3, 16, 0, 0, 0, 0, time.UTC))

		preview := r.must(http.MethodPost, "/v1/invoices/create_preview", time.Date(2027, 3, 30, 12, 0, 0, 0, time.UTC),
			map[string]any{"customer_id": r.custID, "subscription_id": r.subID}, http.StatusOK)
		var previewBase int64
		lines, _ := preview["lines"].([]any)
		for _, raw := range lines {
			l, _ := raw.(map[string]any)
			if l["line_type"] == "base_fee" {
				previewBase += int64(l["amount_cents"].(float64))
			}
		}

		r.run(clampedTo.Add(time.Hour))
		want := []clampedLine{{1, 5161}, {2, 9677}} // 10000 x 16/31, 20000 x 15/31
		wantClampedLines(t, "close", clampedBaseLines(t, r, "subscription_cycle"), want)
		if previewBase != 5161+9677 {
			t.Errorf("preview base fee = %dc, want %dc, as the close bills", previewBase, 5161+9677)
		}
		if n := overBilledLines(t, r); n != 2 {
			t.Errorf("the operator's query finds %d lines, want the close's 2 base lines", n)
		}
	})

	t.Run("cancel on Mar 20 after a change on Mar 16", func(t *testing.T) {
		r := newClampedRig(t, false)
		change(r, time.Date(2027, 3, 16, 0, 0, 0, 0, time.UTC))
		r.cancelNow(time.Date(2027, 3, 20, 0, 0, 0, 0, time.UTC))
		want := []clampedLine{{1, 5161}, {2, 2581}} // 10000 x 16/31, 20000 x 4/31
		wantClampedLines(t, "cancel", clampedBaseLines(t, r, "subscription_cancel"), want)
		if n := overBilledLines(t, r); n != 2 {
			t.Errorf("the operator's query finds %d lines, want the cancel's 2 base lines", n)
		}
	})

	t.Run("cancel on Mar 20 after an item was removed on Mar 16", func(t *testing.T) {
		r := newClampedRig(t, true)
		var addOn string
		for _, it := range r.sub().Items {
			if it.PlanID == r.planC {
				addOn = it.ID
			}
		}
		r.must(http.MethodDelete, "/v1/subscriptions/"+r.subID+"/items/"+addOn, time.Date(2027, 3, 16, 0, 0, 0, 0, time.UTC), nil, http.StatusNoContent)
		r.cancelNow(time.Date(2027, 3, 20, 0, 0, 0, 0, time.UTC))
		// The removed add-on: 5000 x 16/31; the kept item: 10000 x 20/31.
		want := []clampedLine{{1, 2581}, {1, 6452}}
		wantClampedLines(t, "cancel", clampedBaseLines(t, r, "subscription_cancel"), want)
	})

	t.Run("a change on Mar 30 bills 30 of the 31 days, not the full fee", func(t *testing.T) {
		r := newClampedRig(t, false)
		change(r, time.Date(2027, 3, 30, 0, 0, 0, 0, time.UTC))
		r.run(clampedTo.Add(time.Hour))
		want := []clampedLine{{1, 9677}, {2, 645}} // 10000 x 30/31, 20000 x 1/31
		wantClampedLines(t, "close", clampedBaseLines(t, r, "subscription_cycle"), want)
	})
}
