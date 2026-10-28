package subscription

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
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
		SELECT bi.subscription_item_id, bi.plan_id, bi.quantity, bi.starts_at, bi.ends_at, si.created_at
		FROM billing_intervals bi
		LEFT JOIN subscription_items si ON si.id = bi.subscription_item_id
		WHERE bi.subscription_id = $1
	`, subscriptionID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var intervals []domain.ItemInterval
	for rows.Next() {
		var iv domain.ItemInterval
		var itemCreated *time.Time
		if err := rows.Scan(&iv.SubscriptionItemID, &iv.PlanID, &iv.Quantity, &iv.StartsAt, &iv.EndsAt, &itemCreated); err != nil {
			return nil, err
		}
		// The rater pins an added item's window at its creation (ADR-130
		// §4 step 3). An interval whose item row is gone cannot be pinned.
		if itemCreated == nil {
			return nil, fmt.Errorf("billing interval of subscription %s names item %s, which does not exist", subscriptionID, iv.SubscriptionItemID)
		}
		iv.ItemCreatedAt = *itemCreated
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
// Two sources, with a plan's meters every meter it has ever priced, as in
// FindMeterConflicts:
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
		JOIN (SELECT DISTINCT plan_id, meter_id FROM plan_prices) m ON m.plan_id = bi.plan_id
		WHERE sub.customer_id = $1
		  AND sub.id <> $2
		  AND bi.ends_at > bi.starts_at
		  AND bi.ends_at > $3 AND bi.ends_at < $4
		UNION ALL
		SELECT m.meter_id, sub.canceled_at
		FROM subscriptions sub
		JOIN subscription_items si
		  ON si.subscription_id = sub.id AND si.deleted_at IS NULL
		JOIN (SELECT DISTINCT plan_id, meter_id FROM plan_prices) m ON m.plan_id = si.plan_id
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
