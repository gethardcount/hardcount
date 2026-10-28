package api

import (
	"fmt"
	"os"
	"sort"
	"testing"

	mw "github.com/gethardcount/hardcount/internal/api/middleware"
)

// TestMain asserts that the WHOLE package-api test suite — every e2e test that
// drives the real router (TestE2E_FullBillingCycle, the members-invite flow, the
// test-mode flow, the self-host bootstrap flow) — produced ZERO uncovered
// mutations.
//
// This is the RUNTIME half of ADR-090's coverage model, and it is only possible
// because these tests go through NewServer: the AuditCoverage detector is mounted
// at the root there, so every mutating request they make is classified for real
// (2xx + no audit row + not declared exempt ⇒ reported). The BUILD-TIME half —
// the route-audit registry's two-way diff against the live chi route table
// (audit_routes_test.go) — proves every mutating route is DECLARED. This proves
// the declarations are not lies on the paths the suite actually exercises: a
// route declared `explicit` whose emission silently stopped firing fails here,
// naming itself.
//
// It lives in TestMain rather than in one test so it cannot be forgotten when a
// new e2e test is added, and so it sees the union of every route the suite
// touches.
//
// If this fails: the named route committed a state change and left no audit
// evidence. Emit the row on the business transaction (audit.Logger.LogInTx) — or,
// if the request genuinely mutates nothing, declare that (audit.MarkSkip). Do not
// silence it by widening the registry's exempt set: exempt means "this route
// deliberately writes no audit row," which is a claim a reviewer must agree with.
func TestMain(m *testing.M) {
	before := mw.AuditUncoveredMutationsByRoute()

	code := m.Run()
	if code != 0 {
		os.Exit(code) // a real test already failed; don't pile on
	}

	if leaked := increasedRoutes(before, mw.AuditUncoveredMutationsByRoute()); len(leaked) > 0 {
		fmt.Fprint(os.Stderr,
			"\nUNCOVERED MUTATIONS during the api suite — these routes answered 2xx, changed state, and wrote NO audit row (ADR-090):\n")
		for _, l := range leaked {
			fmt.Fprintf(os.Stderr, "  %s  (+%v)\n", l.route, l.delta)
		}
		fmt.Fprint(os.Stderr,
			"Emit the row on the business transaction (audit.Logger.LogInTx). If the request truly mutates nothing, declare audit.MarkSkip.\n\n")
		os.Exit(1)
	}

	os.Exit(code)
}

type routeDelta struct {
	route string
	delta float64
}

// increasedRoutes returns the routes whose uncovered-mutation counter grew.
func increasedRoutes(before, after map[string]float64) []routeDelta {
	var out []routeDelta
	for route, n := range after {
		if d := n - before[route]; d > 0 {
			out = append(out, routeDelta{route: route, delta: d})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].route < out[j].route })
	return out
}
