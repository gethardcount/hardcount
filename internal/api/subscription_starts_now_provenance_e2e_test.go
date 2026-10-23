package api

import (
	"net/http"
	"testing"
)

// TestCreateSubscription_StartNowTrueMatchesGolden is the provenance of
// testdata/starts_now: on the code before D2, a create with "start_now": true
// produces exactly the golden bytes. Run with -update-starts-now-golden to
// write them. It goes away with start_now in the next commit; the golden files
// stay as the record of what start_now=true produced.
func TestCreateSubscription_StartNowTrueMatchesGolden(t *testing.T) {
	for _, billingTime := range []string{"calendar", "anniversary"} {
		t.Run(billingTime, func(t *testing.T) {
			r := newLagRigBare(t, "in_advance", 0)
			out := r.must(http.MethodPost, "/v1/subscriptions", startsNowAt, r.startsNowBody("starts-now", billingTime, map[string]any{"start_now": true}), http.StatusCreated)
			checkStartsNowGolden(t, billingTime, r.startsNowSnapshot(flowStr(out, "id")))
		})
	}
}
