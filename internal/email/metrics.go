package email

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	// The outbox dead-letter counter counts a row once, when the mark that
	// moved it to 'failed' applied. Every series starts at 0 (init below):
	// a series born at 1 has no earlier sample, so increase() would miss the
	// first dead letter.
	emailOutboxDeadLettered = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "hardcount_email_outbox_dead_lettered_total",
			Help: "Emails given up on (email_outbox status='failed'), by cause and mode. permanent = the send can never succeed: the address bounced, is suppressed or is not a valid address, or the row cannot be read; max_attempts = 15 tries over about 52 hours failed, often a relay that refuses the sender or credentials. The email is not sent unless an operator requeues it.",
		},
		[]string{"cause", "mode"},
	)
)

// Outbox dead-letter causes.
const (
	deadLetterPermanent   = "permanent"
	deadLetterMaxAttempts = "max_attempts"
)

func init() {
	for _, mode := range []string{"live", "test"} {
		emailOutboxDeadLettered.WithLabelValues(deadLetterPermanent, mode)
		emailOutboxDeadLettered.WithLabelValues(deadLetterMaxAttempts, mode)
	}
}

func modeLabel(live bool) string {
	if live {
		return "live"
	}
	return "test"
}

// recordOutboxDeadLettered counts one email moved to 'failed'. Call it
// only when the mark applied.
func recordOutboxDeadLettered(cause string, live bool) {
	emailOutboxDeadLettered.WithLabelValues(cause, modeLabel(live)).Inc()
}
