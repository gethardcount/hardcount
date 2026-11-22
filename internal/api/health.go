package api

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gethardcount/hardcount/internal/api/respond"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	schedulerMu       sync.RWMutex
	schedulerLastRun  time.Time
	schedulerInterval time.Duration

	// draining turns /health/ready into 503 while /health stays 200, so a
	// load balancer stops sending traffic to a replica that is shutting down
	// before it stops listening, and an orchestrator does not kill it early.
	// Nothing clears it: the process is exiting.
	draining atomic.Bool

	// schedulerLastRunTimestamp backs the runbook's "scheduler is alive" alert:
	// `time() - hardcount_scheduler_last_run_timestamp_seconds > 2 * interval`.
	schedulerLastRunTimestamp = promauto.NewGauge(
		prometheus.GaugeOpts{
			Name: "hardcount_scheduler_last_run_timestamp_seconds",
			Help: "Unix timestamp of this replica's last billing-loop liveness stamp: every poll, and every page of a long billing drain. Alert when time() - this > 2x the scheduler interval.",
		},
	)
)

// SetDraining announces shutdown: readiness answers 503 from now on.
func SetDraining() { draining.Store(true) }

// RecordSchedulerRun is called by the billing scheduler after each cycle,
// so the health check can tell whether the scheduler is alive.
func RecordSchedulerRun() {
	schedulerMu.Lock()
	schedulerLastRun = time.Now()
	schedulerMu.Unlock()
	// The runbook's scheduler alert reads /metrics, not /health/ready.
	schedulerLastRunTimestamp.SetToCurrentTime()
}

// SetSchedulerInterval stores the configured scheduler interval. The health
// check flags the scheduler as degraded when more than two intervals pass
// after its last run.
func SetSchedulerInterval(d time.Duration) {
	schedulerMu.Lock()
	schedulerInterval = d
	schedulerMu.Unlock()
}

// handleHealth answers liveness. It never reads the database: a probe that
// did would restart every replica when Postgres is down.
func handleHealth(w http.ResponseWriter, r *http.Request) {
	respond.JSON(w, r, http.StatusOK, map[string]string{"status": "ok"})
}

// handleDeepHealth answers /health/ready. Its status is for the load
// balancer: 503 only when this replica cannot serve (draining, or the
// database unreachable). Its body also reports the billing tick, which
// routing elsewhere cannot fix.
func handleDeepHealth(db *postgres.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		defer cancel()

		overallStatus := "ok"
		status := http.StatusOK
		checks := map[string]string{"api": "ok"}

		if draining.Load() {
			checks["api"] = "draining"
			respond.JSON(w, r, http.StatusServiceUnavailable, map[string]any{
				"status": "draining",
				"checks": checks,
			})
			return
		}

		// The route is public, and the driver's error names the database user,
		// database and host, so the body gets a fixed word and the log the rest.
		if err := db.Pool.PingContext(ctx); err != nil {
			slog.ErrorContext(ctx, "readiness: database ping failed", "error", err)
			checks["database"] = "error"
			overallStatus = "degraded"
			status = http.StatusServiceUnavailable
		} else {
			checks["database"] = "ok"
		}

		// A stale tick (no run within two intervals) stays at 200: pulling the
		// replica does not restart the tick, and on one replica it would take
		// the API down. Alert on hardcount_scheduler_last_run_timestamp_seconds,
		// which RecordSchedulerRun sets, and on
		// hardcount_leader_last_tick_age_seconds (docs/ops/runbook/monitoring.md).
		schedulerMu.RLock()
		lastRun := schedulerLastRun
		interval := schedulerInterval
		schedulerMu.RUnlock()

		switch since := time.Since(lastRun); {
		case interval <= 0:
			checks["scheduler"] = "ok: not configured"
		case lastRun.IsZero():
			checks["scheduler"] = "ok: awaiting first run"
		case since > 2*interval:
			checks["scheduler"] = fmt.Sprintf("degraded: last run %s ago (interval %s)", since.Truncate(time.Second), interval)
			overallStatus = "degraded"
		default:
			checks["scheduler"] = fmt.Sprintf("ok: last run %s ago", since.Truncate(time.Second))
		}

		respond.JSON(w, r, status, map[string]any{
			"status": overallStatus,
			"checks": checks,
		})
	}
}
