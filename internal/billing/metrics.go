package billing

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	billingCyclesTotal = promauto.NewCounter(
		prometheus.CounterOpts{
			Name: "hardcount_billing_cycles_total",
			Help: "Billing passes run. A cycle runs one pass per mode, live then test.",
		},
	)

	invoicesGenerated = promauto.NewCounter(
		prometheus.CounterOpts{
			Name: "hardcount_invoices_generated_total",
			Help: "Invoices built by the scheduled billing run's period closes, both modes, including a close that ends a subscription canceled at its period end. Not counted: the final invoice of a cancel scheduled inside a period, spend-threshold invoices, cancels that take effect at once, POST /v1/billing/run, test-clock advances and invoices created through the API.",
		},
	)

	billingCycleDuration = promauto.NewHistogram(
		prometheus.HistogramOpts{
			Name: "hardcount_billing_cycle_duration_seconds",
			Help: "Duration of billing cycle runs in seconds.",
			// A tick bills everything due (ADR-006, amendment P27), so a busy
			// billing day runs for hours. The buckets reach 4h: capped at
			// 300s, histogram_quantile would report 300 for any long tick.
			Buckets: []float64{0.1, 0.5, 1, 2.5, 5, 10, 30, 60, 120, 300, 600, 1800, 3600, 7200, 14400},
		},
	)

	billingCycleErrors = promauto.NewCounter(
		prometheus.CounterOpts{
			Name: "hardcount_billing_cycle_errors_total",
			Help: "Total billing cycle errors.",
		},
	)

	dunningRunsProcessed = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "hardcount_dunning_runs_processed_total",
			Help: "Total dunning runs processed by outcome.",
		},
		[]string{"outcome"},
	)

	autoChargeRetries = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "hardcount_auto_charge_retries_total",
			Help: "Total auto-charge retries by result.",
		},
		[]string{"result"},
	)

	reconcilerSweeps = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "hardcount_reconciler_sweeps_total",
			Help: "Recovery reconciler sweeps by reconciler, mode, and outcome (run|advanced|error). 'run' increments every tick (liveness); 'advanced'/'error' carry the per-tick counts.",
		},
		[]string{"reconciler", "mode", "outcome"},
	)

	scheduledCleanupRows = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "hardcount_scheduled_cleanup_rows_total",
			Help: "Rows purged by scheduled cleanup tasks, labeled by target table.",
		},
		[]string{"table"},
	)

	// usageUnpricedQuantity is the operator's signal for usage that matched
	// no price and was left off an invoice (#981). It is a signal, not a
	// record: it resets on restart and is not per customer. The per-customer
	// view is the no_price row of GET /v1/customers/{id}/usage, and the WARN
	// names the meter by id and event name. A meter label would add a series for every meter of
	// every account, never dropped. Test-clock simulations are not counted.
	// Both modes start at 0 (init below), as the dead-letter counters do.
	usageUnpricedQuantity = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "hardcount_usage_unpriced_quantity_total",
			Help: "Usage quantity that matched no price of its subscription's plan, and so was left off an invoice (period close, cancel final invoice or spend-threshold invoice), by mode. Test-clock simulations are not counted. The WARN \"usage matched no price of its plan\" names the meter's id and event name. Bill it with a one-off invoice; add a price to the plan so later invoices include it.",
		},
		[]string{"mode"},
	)
)

func init() {
	for _, mode := range []string{"live", "test"} {
		usageUnpricedQuantity.WithLabelValues(mode)
	}
}

func modeLabel(live bool) string {
	if live {
		return "live"
	}
	return "test"
}

// recordBillingCycle records billing cycle metrics.
func recordBillingCycle(generated int) {
	billingCyclesTotal.Inc()
	invoicesGenerated.Add(float64(generated))
}

// recordBillingCycleDuration records how long a billing cycle took.
func recordBillingCycleDuration(seconds float64) {
	billingCycleDuration.Observe(seconds)
}

// recordBillingCycleError increments the billing cycle error counter.
func recordBillingCycleError() {
	billingCycleErrors.Inc()
}

// recordDunningRun records the outcome of a processed dunning run
// ("succeeded" or "failed"). The runbook's dunning-machinery alert on
// {outcome="failed"} needs this split, or failed runs would show in no metric.
func recordDunningRun(outcome string) {
	dunningRunsProcessed.WithLabelValues(outcome).Inc()
}

// recordAutoChargeRetry records an auto-charge retry result ("succeeded" or "failed").
func recordAutoChargeRetry(result string) {
	autoChargeRetries.WithLabelValues(result).Inc()
}

// recordScheduledCleanup records rows purged by a scheduled cleanup task.
// Label values match the target table (e.g. "idempotency_keys",
// "payment_tokens") so operators can alert per-table on sudden surges.
func recordScheduledCleanup(table string, rows int) {
	if rows <= 0 {
		return
	}
	scheduledCleanupRows.WithLabelValues(table).Add(float64(rows))
}

// recordUsageUnpriced adds usage quantity that an invoice was built without
// because no price matched it. live is the mode the invoice was built in.
func recordUsageUnpriced(live bool, quantity float64) {
	if quantity <= 0 {
		return
	}
	usageUnpricedQuantity.WithLabelValues(modeLabel(live)).Add(quantity)
}

// recordReconcilerSweep records one recovery-reconciler sweep, a var so tests
// can observe it. It always
// counts a 'run', so operators can alert on a reconciler that stops running.
// It also adds the items advanced and the per-row errors of this tick. mode
// is "live" or "test". The counts feed dashboards and alerts per reconciler,
// such as one for a stuck tax-reversal backlog.
var recordReconcilerSweep = func(reconciler, mode string, advanced, errs int) {
	reconcilerSweeps.WithLabelValues(reconciler, mode, "run").Inc()
	if advanced > 0 {
		reconcilerSweeps.WithLabelValues(reconciler, mode, "advanced").Add(float64(advanced))
	}
	if errs > 0 {
		reconcilerSweeps.WithLabelValues(reconciler, mode, "error").Add(float64(errs))
	}
}
