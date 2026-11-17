package api

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	mw "github.com/gethardcount/hardcount/internal/api/middleware"

	"github.com/gethardcount/hardcount/internal/api/respond"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"go.uber.org/atomic"
)

// --- Scheduler health tracking ---

var (
	schedulerMu       sync.RWMutex
	schedulerLastRun  time.Time
	schedulerInterval time.Duration

	// draining turns /health/ready into 503 {"status":"draining"} after
	// SIGTERM, while /health (liveness) stays 200. A load balancer then stops
	// sending traffic to this replica before it stops listening, and an
	// orchestrator does not kill it early. SetDraining sets it once, and
	// nothing clears it, because the process is exiting.
	draining atomic.Bool
)

// SetDraining announces shutdown: readiness answers 503 from now on.
func SetDraining() { draining.Store(true) }

// resetDrainingForTest clears the draining flag. Only tests call it.
func resetDrainingForTest() { draining.Store(false) }

// RecordSchedulerRun is called by the billing scheduler after each cycle,
// so the health check can tell whether the scheduler is alive.
func RecordSchedulerRun() {
	schedulerMu.Lock()
	schedulerLastRun = time.Now()
	schedulerMu.Unlock()
	// Export the same fact as a metric. The runbook's scheduler-liveness
	// alert reads /metrics, so the fact cannot live only inside
	// /health/ready.
	mw.RecordSchedulerTick()
}

// SetSchedulerInterval stores the configured scheduler interval. The health
// check flags the scheduler as degraded when more than two intervals pass
// after its last run.
func SetSchedulerInterval(d time.Duration) {
	schedulerMu.Lock()
	schedulerInterval = d
	schedulerMu.Unlock()
}

func handleHealth(w http.ResponseWriter, r *http.Request) {
	respond.JSON(w, r, http.StatusOK, map[string]string{"status": "ok"})
}

func handleDeepHealth(db *postgres.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		defer cancel()

		// The status code and the body answer different questions. The code
		// is for the load balancer: 503 only when this replica cannot serve
		// requests (draining, or the database is unreachable). The body also
		// reports the billing tick, which a balancer cannot fix by routing
		// elsewhere.
		overallStatus := "ok"
		status := http.StatusOK
		checks := map[string]string{"api": "ok"}

		// Draining: the replica is shutting down. Readiness fails first, so
		// the balancer stops sending traffic here before the listener closes
		// (cmd/hardcount/main.go).
		if draining.Load() {
			checks["api"] = "draining"
			respond.JSON(w, r, http.StatusServiceUnavailable, map[string]any{
				"status": "draining",
				"checks": checks,
			})
			return
		}

		// Database check. This endpoint is unauthenticated and the driver's
		// error names the database user, database and host, so the body gets
		// a fixed word and the detail goes to the log.
		if err := db.Pool.PingContext(ctx); err != nil {
			slog.ErrorContext(ctx, "readiness: database ping failed", "error", err)
			checks["database"] = "error"
			overallStatus = "degraded"
			status = http.StatusServiceUnavailable
		} else {
			checks["database"] = "ok"
		}

		// Scheduler check: degraded when no run is recorded within twice the
		// interval. Before the first run completes, the scheduler counts as
		// ok, because it has just started. This is reported in the body only.
		// A stale tick leaves the status code at 200, because pulling this
		// replica from the balancer does not restart the tick, and on a
		// single replica it would take the API down too. Alert instead on
		// hardcount_scheduler_last_run_timestamp_seconds and
		// hardcount_leader_last_tick_age_seconds
		// (docs/ops/runbook/monitoring.md).
		schedulerMu.RLock()
		lastRun := schedulerLastRun
		interval := schedulerInterval
		schedulerMu.RUnlock()

		if interval > 0 && !lastRun.IsZero() {
			sinceLastRun := time.Since(lastRun)
			if sinceLastRun > 2*interval {
				checks["scheduler"] = fmt.Sprintf("degraded: last run %s ago (interval %s)", sinceLastRun.Truncate(time.Second), interval)
				overallStatus = "degraded"
			} else {
				checks["scheduler"] = fmt.Sprintf("ok: last run %s ago", sinceLastRun.Truncate(time.Second))
			}
		} else if interval > 0 && lastRun.IsZero() {
			checks["scheduler"] = "ok: awaiting first run"
		} else {
			checks["scheduler"] = "ok: not configured"
		}

		respond.JSON(w, r, status, map[string]any{
			"status": overallStatus,
			"checks": checks,
		})
	}
}
