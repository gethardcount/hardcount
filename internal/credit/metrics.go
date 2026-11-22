package credit

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	creditOperations = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "hardcount_credit_operations_total",
			Help: "Total credit operations by type.",
		},
		[]string{"type"},
	)
)

// recordCreditOperation records a credit operation by type ("grant",
// "adjustment" or "expiry").
func recordCreditOperation(opType string) {
	creditOperations.WithLabelValues(opType).Inc()
}
