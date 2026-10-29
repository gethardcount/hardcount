package webhook

import (
	"testing"
	"time"
)

// The docs and the metric help state how long the outbox keeps retrying a
// row: about 52 hours (docs/ops/runbook/*-outbox-backed-up.md,
// docs/ops/email-setup.md, internal/api/middleware/metrics.go). A row is
// given up on after its MaxOutboxAttempts-th failure, so it waits only the
// first MaxOutboxAttempts-1 entries of the ramp. They said 72 hours for
// months. This fails when the ramp or the attempt count changes, so the
// stated window is updated with it.
func TestOutboxRetryWindowIsAbout52Hours(t *testing.T) {
	var total time.Duration
	for attempt := 1; attempt < MaxOutboxAttempts; attempt++ {
		total += outboxBackoff(attempt)
	}
	if got := total.Round(time.Hour); got != 52*time.Hour {
		t.Fatalf("retry window = %s (about %s); the docs and metric help say about 52 hours: update them", total, got)
	}
}
