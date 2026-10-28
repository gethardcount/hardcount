package api

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/platform/postgres"
)

// #1274: an in-arrears base fee is split into one line per quantity segment.
// The days of the segments must add up to the period, however the change
// instants fall. Each segment used to be rounded to whole days on its own, so
// two mid-day changes billed 31 days of a 30-day period.
//
// The example from the issue: [Apr 1, May 1), 3000c a month per seat. The
// quantity goes to 2 at Apr 11 14:24 (day 10.6), then to 3 at Apr 22 04:48
// (day 21.2). Each change instant is rounded to the nearest day from the
// period start (11 and 21), so the segments are 11, 10 and 9 days:
// 1100c + 2000c + 2700c = 5800c. Rounding each segment on its own gave 11,
// 11 and 9 days, 6000c.
//
// The immediate cancel's final invoice uses the same rule. Cancelled at
// Apr 24 00:00 (day 23) after the same two changes, the segments are 11, 10
// and 2 days: 1100c + 2000c + 600c = 3700c. Rounding each segment gave 11, 11
// and 2 days of a 23-day window, 3900c.
//
// Real server over HTTP, real Postgres.
func TestArrearsBaseSegments_DaysAddUpToThePeriod(t *testing.T) {
	skipShort(t)
	first := time.Date(2026, 4, 11, 14, 24, 0, 0, time.UTC)
	second := time.Date(2026, 4, 22, 4, 48, 0, 0, time.UTC)

	type line struct {
		qty    int64
		amount int64
		desc   string
	}
	baseLines := func(r *lagRig, reason string) []line {
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
			ORDER BY li.billing_period_start`, r.subID, reason)
		if err != nil {
			t.Fatalf("list base lines: %v", err)
		}
		defer func() { _ = rows.Close() }()
		var out []line
		for rows.Next() {
			var l line
			if err := rows.Scan(&l.qty, &l.amount, &l.desc); err != nil {
				t.Fatalf("scan: %v", err)
			}
			out = append(out, l)
		}
		return out
	}
	check := func(t *testing.T, got []line, want []line) {
		t.Helper()
		if len(got) != len(want) {
			t.Fatalf("base lines %+v, want %+v", got, want)
		}
		var sum, wantSum int64
		for i := range want {
			sum += got[i].amount
			wantSum += want[i].amount
			if got[i] != want[i] {
				t.Errorf("base line %d = %+v, want %+v", i, got[i], want[i])
			}
		}
		if sum != wantSum {
			t.Errorf("base fee total %dc, want %dc", sum, wantSum)
		}
	}
	changeQuantities := func(r *lagRig) {
		r.must(http.MethodPatch, "/v1/subscriptions/"+r.subID+"/items/"+r.itemA, first, map[string]any{"quantity": 2}, http.StatusOK)
		r.must(http.MethodPatch, "/v1/subscriptions/"+r.subID+"/items/"+r.itemA, second, map[string]any{"quantity": 3}, http.StatusOK)
	}

	t.Run("cycle close", func(t *testing.T) {
		r := newLagRig(t, "in_arrears", false)
		changeQuantities(r)
		r.run(lagB.Add(time.Hour))
		check(t, baseLines(r, "subscription_cycle"), []line{
			{1, 1100, "plan_a - base fee (qty 1, prorated 11/30 days)"},
			{2, 2000, "plan_a - base fee (qty 2, prorated 10/30 days)"},
			{3, 2700, "plan_a - base fee (qty 3, prorated 9/30 days)"},
		})
	})

	t.Run("immediate cancel", func(t *testing.T) {
		r := newLagRig(t, "in_arrears", false)
		changeQuantities(r)
		r.cancelNow(time.Date(2026, 4, 24, 0, 0, 0, 0, time.UTC))
		check(t, baseLines(r, "subscription_cancel"), []line{
			{1, 1100, "plan_a - base fee (qty 1, prorated 11/30 days)"},
			{2, 2000, "plan_a - base fee (qty 2, prorated 10/30 days)"},
			{3, 600, "plan_a - base fee (qty 3, prorated 2/30 days)"},
		})
	})
}
