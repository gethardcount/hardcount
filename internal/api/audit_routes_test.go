package api

import (
	"fmt"
	"net/http"
	"sort"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
)

// walkMutatingRoutes enumerates every mutating route in the LIVE router, keyed
// canonically. chi.Walk descends into nested r.Route / r.Mount / r.With
// subrouters and yields fully-qualified patterns, so this is the whole surface —
// including the blocks the deleted catch-all never saw (/v1/auth,
// /v1/public/*, /v1/webhooks, /v1/bootstrap).
//
// The router builds with a zero-value *postgres.DB (no database): NewServer only
// wires stores/handlers here, and chi.Walk touches no handler.
func walkMutatingRoutes(t *testing.T) map[routeKey]bool {
	t.Helper()

	srv := newTestServer()
	found := make(map[routeKey]bool)
	err := chi.Walk(srv.router, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		if !isMutatingMethod(method) {
			return nil
		}
		found[routeKey{Method: method, Pattern: canonicalRoute(route)}] = true
		return nil
	})
	if err != nil {
		t.Fatalf("chi.Walk over the live router: %v", err)
	}
	if len(found) == 0 {
		t.Fatal("walked 0 mutating routes — the walk is broken, not the registry")
	}
	return found
}

// TestAuditRouteRegistry_MatchesLiveRoutes is the drift gate for every future
// route in this codebase. It two-way-diffs the live chi route table against the
// registry, so neither side can move without the other.
//
// This test replaces a middleware that used to guess. Do not "fix" a failure by
// deleting the entry or loosening the walk — declare the route.
func TestAuditRouteRegistry_MatchesLiveRoutes(t *testing.T) {
	walked := walkMutatingRoutes(t)

	// (1) Every mutating route must be declared. An undeclared route is a
	//     route nobody decided the audit story for — which is exactly how the
	//     old catch-all's fabricated rows got shipped.
	var undeclared []string
	for k := range walked {
		if _, ok := auditRouteRegistry[k]; !ok {
			undeclared = append(undeclared, fmt.Sprintf("%s %s", k.Method, k.Pattern))
		}
	}
	if len(undeclared) > 0 {
		sort.Strings(undeclared)
		t.Errorf(`%d mutating route(s) are NOT declared in the audit registry:

%s

You added a route that can change state. Decide its audit story and declare it in
internal/api/audit_routes.go (auditRouteRegistry):

  • explicit(note) — the route emits a real audit row. Emit it IN the business
    transaction (audit.Logger.LogInTx on the caller's *sql.Tx) so the mutation and
    its evidence commit or roll back together (ADR-090), and name the emission
    site in the note.

  • exempt(reason, note) — the route deliberately writes no audit row. reason must
    be one of the closed enum (non_mutating_preview, machine_ingest,
    system_endpoint, webhook_owned), and the note must say WHY and
    record any accepted loss.

An exempt() you don't mean un-audits a live route FOREVER — in an append-only
compliance log, the row you didn't write is not recoverable later.`,
			len(undeclared), "  "+strings.Join(undeclared, "\n  "))
	}

	// (2) Every registry entry must match a real route. A stale entry is a lie
	//     about a surface that no longer exists — and it silently exempts
	//     whatever route later reclaims that pattern.
	var stale []string
	for k, d := range auditRouteRegistry {
		if !walked[k] {
			kind := "explicit"
			if d.Coverage == auditExempt {
				kind = "exempt(" + string(d.Reason) + ")"
			}
			stale = append(stale, fmt.Sprintf("%s %s  [%s]", k.Method, k.Pattern, kind))
		}
	}
	if len(stale) > 0 {
		sort.Strings(stale)
		t.Errorf(`%d audit registry entr(y/ies) match NO live route:

%s

The route was renamed or removed but its registry entry was not. Delete the entry
(or update its pattern to the new one). Registry keys are CANONICAL chi route
patterns — the pattern chi.Walk reports, put through canonicalRoute(): use the
declared pattern with its {placeholders}, never a concrete path.`,
			len(stale), "  "+strings.Join(stale, "\n  "))
	}
}

// TestAuditRouteRegistry_EntriesAreWellFormed pins the shape of the table
// itself: an exemption with no reason (or no justification) is how a route
// quietly stops being audited.
func TestAuditRouteRegistry_EntriesAreWellFormed(t *testing.T) {
	validReasons := map[exemptReason]bool{
		reasonNonMutatingPreview: true,
		reasonMachineIngest:      true,
		reasonSystemEndpoint:     true,
		reasonWebhookOwned:       true,
	}

	for k, d := range auditRouteRegistry {
		where := k.Method + " " + k.Pattern

		if !isMutatingMethod(k.Method) {
			t.Errorf("%s: registry declares a non-mutating method; the registry covers state-changing routes only", where)
		}
		if got := canonicalRoute(k.Pattern); got != k.Pattern {
			t.Errorf("%s: key is not canonical (canonicalRoute gives %q) — the runtime detector keys on the canonical form and would never match this entry", where, got)
		}
		if strings.TrimSpace(d.Note) == "" {
			t.Errorf("%s: empty note. An explicit entry must name its emission site; an exempt entry must justify itself.", where)
		}

		switch d.Coverage {
		case auditExplicit:
			if d.Reason != "" {
				t.Errorf("%s: explicit entry carries exempt reason %q", where, d.Reason)
			}
		case auditExempt:
			if !validReasons[d.Reason] {
				t.Errorf("%s: exempt reason %q is not in the closed enum. Widening what 'un-audited' may mean is a deliberate decision — add the reason constant with a comment, in review.", where, d.Reason)
			}
		default:
			t.Errorf("%s: entry declares no coverage (use explicit(...) or exempt(...))", where)
		}
	}
}

// TestAuditRouteRegistry_ExemptionsStaySmall makes the exempt list a thing you
// have to look at. It is not a cap on correctness — it is a tripwire: if this
// number grows, someone is exempting routes to make the gate quiet.
func TestAuditRouteRegistry_ExemptionsStaySmall(t *testing.T) {
	byReason := map[exemptReason][]string{}
	explicitN := 0
	for k, d := range auditRouteRegistry {
		if d.Coverage == auditExempt {
			byReason[d.Reason] = append(byReason[d.Reason], k.Method+" "+k.Pattern)
			continue
		}
		explicitN++
	}

	// The population as of the uninstall (ADR-090). Bump DELIBERATELY.
	want := map[exemptReason]int{
		reasonSystemEndpoint: 4, // /metrics × POST/PUT/PATCH/DELETE (r.Handle expands to all 9)
		// usage-events {single, batch} + litellm spend. NOT backfill: an
		// operator inserting BACKDATED usage changes what a customer is billed
		// for a period that may already have closed — a money-path action, and
		// it emits (usage.Service.Backfill → IngestAudited).
		reasonMachineIngest:      3,
		reasonNonMutatingPreview: 2, // invoice create_preview, recipe preview
		// NOTE: there is no `bootstrap` reason at all any more — the constant is
		// deleted, so exempt("bootstrap") no longer compiles. POST /v1/bootstrap
		// is explicit: it mints a LIVE secret key and the owner account, and emits
		// its provisioning rows on its own tx.
		// Inbound provider webhooks: Stripe payments + Postmark email
		// delivery reports (ADR-098). Both machine-driven with no
		// operator to attribute; their effects live in provider-truth
		// state + outbox events, with the accepted losses recorded on
		// each registry entry.
		reasonWebhookOwned: 2,
	}
	for reason, n := range want {
		if got := len(byReason[reason]); got != n {
			sort.Strings(byReason[reason])
			t.Errorf("exempt(%s): %d route(s), expected %d.\n  %s\n\nIf you meant to add an exemption, bump the count here and say why in the registry note. If a route lost its audit emission and was exempted to keep CI green, that is the bug.",
				reason, got, n, strings.Join(byReason[reason], "\n  "))
		}
	}
	for reason, routes := range byReason {
		if _, known := want[reason]; !known {
			sort.Strings(routes)
			t.Errorf("exempt(%s) is not accounted for in this test: %s", reason, strings.Join(routes, ", "))
		}
	}

	t.Logf("audit registry: %d explicit, %d exempt (%d total mutating routes)",
		explicitN, len(auditRouteRegistry)-explicitN, len(auditRouteRegistry))
}

// TestCanonicalRoute pins the two normalizations that let chi.Walk's patterns and
// the runtime RoutePattern() meet on one key — and the one normalization that
// must NOT happen.
func TestCanonicalRoute(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"plain", "/v1/customers/{id}", "/v1/customers/{id}"},
		{"walk leaves a residual /*/ for Mount(\"/\") inside Route()", "/v1/auth/*/login", "/v1/auth/login"},
		{"repeated /*/ collapses (Walk does a single pass; RoutePattern loops)", "/v1/a/*/b/*/c", "/v1/a/b/c"},
		{"walk does not trim a subrouter's trailing slash", "/v1/customers/", "/v1/customers"},
		{"mounted root", "/v1/public/invoices/*/{token}/checkout", "/v1/public/invoices/{token}/checkout"},
		{"root stays root", "/", "/"},

		// The load-bearing negative. A 404 inside a mounted subtree yields
		// RoutePattern() == "/v1/customers/*". Trimming the trailing "/*" would
		// collapse it onto the real "/v1/customers" key — a MISS masquerading as
		// a covered route (and, for an exempt key, a silent free pass for
		// anything unmatched under that subtree). Leaving it intact means it
		// matches no key, which is the honest answer.
		{"trailing /* is NOT trimmed (an unmatched subtree must not collapse onto a real key)", "/v1/customers/*", "/v1/customers/*"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := canonicalRoute(tc.in); got != tc.want {
				t.Errorf("canonicalRoute(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}

	// And the property that motivates the negative above: an unmatched subtree
	// pattern must not be exempt just because its parent prefix is.
	if auditRouteExempt("POST", "/v1/usage-events/*") {
		t.Error("an unmatched path under an exempt subtree resolved as exempt — canonicalRoute is collapsing /*")
	}
}

// TestAuditRouteExempt_LooksUpCanonically proves the detector's lookup meets the
// registry on the canonical key even when handed a runtime RoutePattern() with
// chi's residual artifacts.
func TestAuditRouteExempt_LooksUpCanonically(t *testing.T) {
	if !auditRouteExempt("POST", "/v1/usage-events") {
		t.Error("POST /v1/usage-events should be exempt(machine_ingest)")
	}
	// Trailing slash, as chi.Walk reports a subrouter's r.Post("/") leaf.
	if !auditRouteExempt("POST", "/v1/invoices/create_preview/") {
		t.Error("POST /v1/invoices/create_preview/ should canonicalize to /v1/invoices/create_preview and be exempt(non_mutating_preview)")
	}
	// Bootstrap is EXPLICIT (it mints a live secret key and emits for it), so
	// it must NOT resolve as exempt — a regression here would silence the
	// detector on the one route that provisions a money-moving credential.
	if auditRouteExempt("POST", "/v1/bootstrap") {
		t.Error("POST /v1/bootstrap is explicit (it audits its own provisioning), not exempt")
	}
	if auditRouteExempt("POST", "/v1/credits/grant") {
		t.Error("POST /v1/credits/grant is explicit, not exempt")
	}
	if auditRouteExempt("POST", "/v1/not/a/route") {
		t.Error("an unknown route must not resolve as exempt — unknown means uncovered, loudly")
	}
}
