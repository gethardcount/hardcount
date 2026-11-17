package usage

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	usageEventsIngested = promauto.NewCounter(
		prometheus.CounterOpts{
			Name: "hardcount_usage_events_ingested_total",
			Help: "Total usage events ingested.",
		},
	)
)

// recordUsageIngested records usage event ingestion.
func recordUsageIngested(count int) {
	usageEventsIngested.Add(float64(count))
}
