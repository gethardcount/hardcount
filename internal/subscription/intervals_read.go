package subscription

import (
	"context"
	"sort"
	"time"

	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
)

// ListItemIntervals reads the sub's full billing_intervals history —
// the single segment source post-ADR-101-Phase-4. Deliberately
// unwindowed: the billing reader clips in Go, and its missing-interval
// invariant must distinguish "no rows overlap this window" (legitimate
// zero) from "no rows AT ALL" (a writer bug).
func (s *PostgresStore) ListItemIntervals(ctx context.Context, tenantID, subscriptionID string) ([]domain.ItemInterval, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return nil, err
	}
	defer postgres.Rollback(tx)

	rows, err := tx.QueryContext(ctx, `
		SELECT subscription_item_id, plan_id, quantity, starts_at, ends_at
		FROM billing_intervals
		WHERE subscription_id = $1
	`, subscriptionID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var intervals []domain.ItemInterval
	for rows.Next() {
		var iv domain.ItemInterval
		if err := rows.Scan(&iv.SubscriptionItemID, &iv.PlanID, &iv.Quantity, &iv.StartsAt, &iv.EndsAt); err != nil {
			return nil, err
		}
		intervals = append(intervals, iv)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Same order the reader's merge step expects: starts_at, with
	// zero-width/closed rows before the row that reopened at the same
	// instant (open/end-later rows last).
	sort.SliceStable(intervals, func(i, j int) bool {
		if !intervals[i].StartsAt.Equal(intervals[j].StartsAt) {
			return intervals[i].StartsAt.Before(intervals[j].StartsAt)
		}
		ie, je := intervals[i].EndsAt, intervals[j].EndsAt
		if ie == nil {
			return false
		}
		if je == nil {
			return true
		}
		return ie.Before(*je)
	})
	return intervals, nil
}

// ListCustomerMeterCoverageEnds returns the instants in the open window
// (from, to) at which the customer's subscriptions other than excludeSubID
// stopped covering each meter. The billing engine starts a subscription's
// usage window for a meter no earlier than the latest of them (ADR-012
// amendment 2026-10-22): usage is aggregated per customer and meter, so a
// subscription whose period was backdated to midnight would otherwise bill
// the usage another subscription already billed that day.
//
// Two sources, with plan meters unnested as in FindMeterConflicts:
//   - billing_intervals of the other subscriptions that ended in the window,
//     zero-width rows left out (they covered nothing). This covers an
//     immediate or scheduled cancel, an item removal and a plan swap away.
//   - a never-activated trial that was canceled in the window, on its live
//     items' plans. A trial writes no billing_intervals, and its usage is
//     free, so its cancel instant is where its coverage ended. A draft never
//     covered anything and has no trial end.
func (s *PostgresStore) ListCustomerMeterCoverageEnds(ctx context.Context, tenantID, customerID, excludeSubID string, from, to time.Time) ([]domain.MeterCoverageEnd, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return nil, err
	}
	defer postgres.Rollback(tx)

	rows, err := tx.QueryContext(ctx, `
		SELECT m.meter_id, bi.ends_at
		FROM billing_intervals bi
		JOIN subscriptions sub ON sub.id = bi.subscription_id
		JOIN plans p ON p.id = bi.plan_id
		-- meter_ids is 'null' (not '[]') for meterless plans.
		CROSS JOIN LATERAL jsonb_array_elements_text(
			CASE WHEN jsonb_typeof(p.meter_ids) = 'array' THEN p.meter_ids ELSE '[]'::jsonb END
		) AS m(meter_id)
		WHERE sub.customer_id = $1
		  AND sub.id <> $2
		  AND bi.ends_at > bi.starts_at
		  AND bi.ends_at > $3 AND bi.ends_at < $4
		UNION ALL
		SELECT m.meter_id, sub.canceled_at
		FROM subscriptions sub
		JOIN subscription_items si
		  ON si.subscription_id = sub.id AND si.deleted_at IS NULL
		JOIN plans p ON p.id = si.plan_id
		CROSS JOIN LATERAL jsonb_array_elements_text(
			CASE WHEN jsonb_typeof(p.meter_ids) = 'array' THEN p.meter_ids ELSE '[]'::jsonb END
		) AS m(meter_id)
		WHERE sub.customer_id = $1
		  AND sub.id <> $2
		  AND sub.status = 'canceled'
		  AND sub.activated_at IS NULL
		  AND sub.trial_end_at IS NOT NULL
		  AND sub.canceled_at > $3 AND sub.canceled_at < $4`,
		customerID, excludeSubID, from, to)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []domain.MeterCoverageEnd
	for rows.Next() {
		var e domain.MeterCoverageEnd
		if err := rows.Scan(&e.MeterID, &e.EndsAt); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
