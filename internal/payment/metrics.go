package payment

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	paymentCharges = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "hardcount_payment_charges_total",
			Help: "Total payment charges by result.",
		},
		[]string{"result"},
	)

	stripeKeyUnusable = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "hardcount_stripe_key_unusable_total",
			Help: "Charges not sent because Stripe refused the tenant's API key (reason=rejected) or no key is connected for the mode (reason=not_configured). Charges are paused, not failed, until the key is fixed.",
		},
		[]string{"reason"},
	)

	stripeBreakerState = promauto.NewGauge(
		prometheus.GaugeOpts{
			Name: "hardcount_stripe_breaker_state",
			Help: "Global circuit breaker state for Stripe calls. 0=closed, 1=half_open, 2=open.",
		},
	)

	staleChargeHoldResolutions = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "hardcount_stale_charge_hold_resolutions_total",
			Help: "Stale charge holds cleared by the stale-hold sweep, which asks Stripe nothing (ADR-127, ADR-137), by result: released (the charge never confirmed; the invoice can be charged again), cleared (a leftover hold), failed (the write failed), earlier (a hold or park an earlier version left, kept for a person), lost_race.",
		},
		[]string{"result"},
	)
)

// recordPaymentCharge records a payment charge result ("succeeded" or "failed").
func recordPaymentCharge(result string) {
	paymentCharges.WithLabelValues(result).Inc()
}

// recordStripeKeyUnusable counts a charge not sent because the tenant's
// Stripe key was refused ("rejected") or is not connected ("not_configured").
func recordStripeKeyUnusable(reason string) {
	stripeKeyUnusable.WithLabelValues(reason).Inc()
}

// RecordStripeBreakerState updates the global breaker state gauge. Called
// from the breaker's OnStateChange hook. Values mirror gobreaker's semantics:
// 0=closed (normal), 1=half_open (probing), 2=open (rejecting).
func RecordStripeBreakerState(state string) {
	var v float64
	switch state {
	case "half_open":
		v = 1
	case "open":
		v = 2
	}
	stripeBreakerState.Set(v)
}

// recordStaleChargeHoldResolution counts one stale-hold check (ADR-127,
// ADR-137). "earlier" is a hold or park an earlier version left on an open
// invoice: it stays blocked from charging until a person settles it, so alert
// on it being non-zero. A steady "failed" is the database refusing the clear.
func recordStaleChargeHoldResolution(result string) {
	staleChargeHoldResolutions.WithLabelValues(result).Inc()
}
