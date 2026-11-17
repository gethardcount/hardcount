package main

import (
	"context"
	"database/sql"
	"time"

	mw "github.com/gethardcount/hardcount/internal/api/middleware"
	"github.com/gethardcount/hardcount/internal/platform/leader"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
)

// registerMetrics adds to /metrics the collectors that need the database:
// the pool's wait counts, the leader status gauges and the queue-depth
// gauges. Every other metric registers itself when its package loads. The
// collectors query on each scrape, never here, so registering connects to
// nothing.
func registerMetrics(appPool *sql.DB, db *postgres.DB, invoiceGrace time.Duration) {
	// appPool serves every request and background job, so its waits are the
	// ones an operator needs to see. The start-up pool is not exported.
	mw.RegisterDBPoolStats(appPool)

	roles := make([]string, 0, len(leader.Roles))
	for _, r := range leader.Roles {
		roles = append(roles, string(r))
	}
	mw.InitLeaderLeaseLost(roles...)
	mw.RegisterLeaderStatus(func(ctx context.Context) ([]mw.LeaderStatusRow, error) {
		rows, err := leader.Status(ctx, appPool)
		if err != nil {
			return nil, err
		}
		out := make([]mw.LeaderStatusRow, 0, len(rows))
		for _, r := range rows {
			out = append(out, mw.LeaderStatusRow{Role: string(r.Role), Held: r.Held, HasTicked: r.HasTicked, LastTickAgeSeconds: r.LastTickAgeS, Paused: r.Paused})
		}
		return out, nil
	})

	// The counts run on a TxBypass transaction because they span tenants.
	// Under the app role's RLS they would always report a healthy-looking 0.
	mw.RegisterQueueDepthGauges(invoiceGrace, func(query string) (float64, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		tx, err := db.BeginTx(ctx, postgres.TxBypass, "")
		if err != nil {
			return 0, err
		}
		defer postgres.Rollback(tx)
		var n float64
		if err := tx.QueryRowContext(ctx, query).Scan(&n); err != nil {
			return 0, err
		}
		return n, nil
	})
}
