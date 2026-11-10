package creditnote

import (
	"testing"
	"time"
)

// ShortenStepsBudget sets stepsBudget to d for one test, so a test can run a
// refund past its budget without waiting 5 minutes.
func ShortenStepsBudget(t *testing.T, d time.Duration) {
	t.Helper()
	prev := stepsBudget
	stepsBudget = d
	t.Cleanup(func() { stepsBudget = prev })
}
