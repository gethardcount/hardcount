package arch

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The five billing-failure columns on subscriptions (migration 0176, ADR-119)
// are never cleared when a subscription's due instant moves; they go stale and
// stay on the row. A record counts only while billing_failed_for equals the
// row's due instant, and that rule lives in one predicate
// (domain.SubscriptionBillingFailingSQL) and its Go twin in the row scan.
//
// A reader written as `billing_failure_count > 0` anywhere else would treat
// stale columns as a live failure: a healthy subscription shown as parked, or
// skipped by a new scan. So the column names may appear only in the files that
// own the rule.
var billingFailureColumnOwners = map[string]bool{
	"internal/domain/subscription_billing_failure.go": true, // the predicate
	"internal/subscription/postgres.go":               true, // the scan, the two writers, the gates
	"internal/api/middleware/metrics.go":              true, // the gauges' hand copy, pinned by TestBillingGauges_DueAndFailingPartition
}

func TestBillingFailureColumns_ReadOnlyThroughThePredicate(t *testing.T) {
	root := repoRoot(t)
	column := regexp.MustCompile(`\bbilling_(failed_for|failure_count|failed_at|next_attempt_at|last_error)\b`)
	seenOwners := map[string]bool{}
	for _, dir := range []string{"internal", "cmd"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(p string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
				return err
			}
			src, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			if !column.Match(src) {
				return nil
			}
			rel, _ := filepath.Rel(root, p)
			rel = filepath.ToSlash(rel)
			if billingFailureColumnOwners[rel] {
				seenOwners[rel] = true
				return nil
			}
			for i, line := range strings.Split(string(src), "\n") {
				if column.MatchString(line) {
					t.Errorf("%s:%d names a billing-failure column outside the predicate's owners.\n"+
						"  Read billing-failure state through domain.Subscription.BillingFailure (Go) or\n"+
						"  domain.SubscriptionBillingFailingSQL (SQL): the raw columns go stale and are never cleared (ADR-119).\n"+
						"  %s", rel, i+1, strings.TrimSpace(line))
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	// An owner that no longer names a column has moved; keep the list honest.
	for rel := range billingFailureColumnOwners {
		if !seenOwners[rel] {
			t.Errorf("%s is listed as an owner but names no billing-failure column — if the code moved, move this entry", rel)
		}
	}
}
