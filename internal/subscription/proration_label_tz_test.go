package subscription

import (
	"strings"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/domain"
)

// TestProrationLabels_RenderInBillingTZ locks the ADR-075 audit fix for the
// proration invoice-line labels: the boundary date is rendered in the passed
// billing-timezone location, NOT the raw time.Time's zone. These labels are
// persisted as the customer-facing credit/charge line Description, so a
// non-UTC-host render would print a day-shifted calendar date on a money doc.
//
// The instant is "Jun 1 2026 00:00 IST" (= May 31 18:30 UTC): in Kolkata it is
// Jun 1, in UTC it is May 31 — so the two locs give different dates and the
// assertion can't pass vacuously.
//
// Mutation-verify: drop `.In(loc)` from any label (format the raw `at`) — the
// value carries its construction zone (IST here), so the UTC case keeps saying
// "Jun 1" and fails.
func TestProrationLabels_RenderInBillingTZ(t *testing.T) {
	ist, err := time.LoadLocation("Asia/Kolkata")
	if err != nil {
		t.Fatalf("load IST: %v", err)
	}
	at := time.Date(2026, 6, 1, 0, 0, 0, 0, ist) // May 31 18:30 UTC
	plan := domain.Plan{Name: "Pro"}

	cases := []struct {
		name string
		loc  *time.Location
		want string
	}{
		{"billing TZ (Kolkata)", ist, "Jun 1, 2026"},
		{"UTC", time.UTC, "May 31, 2026"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			labels := map[string]string{
				"upgradeCredit": upgradeCreditLabel(plan, at, tc.loc),
				"upgradeCharge": upgradeChargeLabel(plan, at, tc.loc),
				"qtyCredit":     qtyChangeCreditLabel(plan, 2, at, tc.loc),
				"qtyCharge":     qtyChangeChargeLabel(plan, 2, at, tc.loc),
			}
			for which, got := range labels {
				if !strings.Contains(got, tc.want) {
					t.Errorf("%s label = %q, want it to contain %q (must render the boundary in the passed loc)", which, got, tc.want)
				}
			}
		})
	}
}
