package webhook

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	webhookDeliveries = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "hardcount_webhook_deliveries_total",
			Help: "Total outbound webhook deliveries by status.",
		},
		[]string{"status"},
	)

	// The outbox dead-letter counter counts a row once, when the mark that
	// moved it to 'failed' applied. Every series starts at 0 (init below):
	// a series born at 1 has no earlier sample, so increase() would miss the
	// first dead letter.
	webhookOutboxDeadLettered = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "hardcount_webhook_outbox_dead_lettered_total",
			Help: "Webhook outbox rows given up on (webhook_outbox status='failed') after 15 tries over about 52 hours, by cause (always max_attempts) and mode: the event and its deliveries were never written, so no endpoint gets it. Endpoint failures are hardcount_webhook_deliveries_total{status=\"failed\"}.",
		},
		[]string{"cause", "mode"},
	)
)

// deadLetterMaxAttempts is the outbox dead-letter cause: every try failed.
const deadLetterMaxAttempts = "max_attempts"

func init() {
	for _, mode := range []string{"live", "test"} {
		webhookOutboxDeadLettered.WithLabelValues(deadLetterMaxAttempts, mode)
	}
}

func modeLabel(live bool) string {
	if live {
		return "live"
	}
	return "test"
}

// recordWebhookDelivery counts a delivery's final outcome: "succeeded" (a 2xx
// whose mark applied) or "failed" (its last retry failed, or it was given up
// while still pending).
func recordWebhookDelivery(status string) {
	webhookDeliveries.WithLabelValues(status).Inc()
}

// recordOutboxDeadLettered counts one webhook outbox row moved to
// 'failed' after its last attempt. Call it only when the mark applied.
func recordOutboxDeadLettered(live bool) {
	webhookOutboxDeadLettered.WithLabelValues(deadLetterMaxAttempts, modeLabel(live)).Inc()
}
