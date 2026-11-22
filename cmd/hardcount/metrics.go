package main

import (
	"context"
	"database/sql"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"

	"github.com/gethardcount/hardcount/internal/platform/leader"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
)

// registerMetrics adds to reg the collectors that need the database: the
// pool's wait counts, the leader status gauges and the queue-depth gauges.
// The server passes the default registry, which /metrics serves; every other
// metric registers itself there when its package loads. The collectors query
// on each scrape, never here, so registering connects to nothing.
func registerMetrics(reg prometheus.Registerer, appPool *sql.DB, db *postgres.DB, invoiceGrace time.Duration) {
	// appPool serves every request and background job, so its waits are the
	// ones an operator needs to see. The start-up pool, when it is a separate
	// pool, is not exported.
	registerPoolStats(reg, appPool)

	roles := make([]string, 0, len(leader.Roles))
	for _, r := range leader.Roles {
		roles = append(roles, string(r))
	}
	leader.InitLeaseLost(roles...)
	leader.RegisterStatusMetrics(reg, func(ctx context.Context) ([]leader.StatusRow, error) {
		return leader.Status(ctx, appPool)
	})

	// The counts run on a TxBypass transaction because they span tenants.
	// Under the app role's RLS they would always report a healthy-looking 0.
	registerQueueDepthGauges(reg, invoiceGrace, func(query string) (float64, error) {
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

// registerPoolStats exports the connection-pool counters of pool on /metrics,
// as the go_sql_* series with db_name="app". go_sql_wait_count_total counts
// the callers that had to wait for a free connection. It stays flat while
// DB_MAX_OPEN_CONNS is enough, so the runbook alerts on its rate. The series
// are database/sql's own counters: a scrape reads them from memory and runs
// no query.
func registerPoolStats(reg prometheus.Registerer, pool *sql.DB) {
	reg.MustRegister(collectors.NewDBStatsCollector(pool, "app"))
}
