package api

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
)

// TestRouteTable_MatchesLiveRoutes walks the live router and compares it with
// routeTable in both directions, on every axis, and reports every gap in one
// run. chi.Walk descends into every nested Route, Mount and With, so this is
// the whole surface, the blocks outside /v1's authentication included. Do not
// "fix" a failure by deleting an entry or loosening the walk: declare the
// route.
func TestRouteTable_MatchesLiveRoutes(t *testing.T) {
	walked := map[routeKey]bool{}
	if err := chi.Walk(newTestServer().router, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		walked[routeKey{Method: method, Pattern: canonicalRoute(route)}] = true
		return nil
	}); err != nil {
		t.Fatalf("walk: %v", err)
	}
	if len(walked) < 150 {
		t.Fatalf("walked %d routes: the walk is broken, not the table", len(walked))
	}
	var undeclared, stale, unaudited, unauditedExports, strayAudits []string
	for k := range walked {
		entry, ok := routeTable[k]
		name := k.Method + " " + k.Pattern
		if !ok {
			undeclared = append(undeclared, name)
		}
		switch {
		case entry.audit.Coverage != 0:
		case isMutatingMethod(k.Method):
			unaudited = append(unaudited, name)
		case k.Method == http.MethodGet && strings.HasPrefix(k.Pattern, auditEgressPrefix+"/"):
			unauditedExports = append(unauditedExports, name)
		}
	}
	for k, entry := range routeTable {
		name := k.Method + " " + k.Pattern
		if !walked[k] {
			switch entry.audit.Coverage {
			case auditExplicit:
				name += "  [explicit audit]"
			case auditExempt:
				name += "  [exempt(" + string(entry.audit.Reason) + ")]"
			}
			stale = append(stale, name)
		}
		takesOut := k.Method == http.MethodGet && strings.HasPrefix(k.Pattern, auditEgressPrefix+"/")
		if entry.audit.Coverage != 0 && !isMutatingMethod(k.Method) && !takesOut {
			strayAudits = append(strayAudits, name)
		}
	}
	report := func(list []string, msg string) {
		if len(list) > 0 {
			sort.Strings(list)
			t.Errorf("%d %s\n\n  %s", len(list), msg, strings.Join(list, "\n  "))
		}
	}
	report(undeclared, `route(s) have no entry in routeTable (internal/api/route_table.go). Declare
how each one's time is decided: realTime() when the request names no record on
a clock; ofCustomer, ofSubscription, ofInvoice, ofCreditNote or ofDunningRun
when a path parameter names the record; or decidedIn("package.Type.Method")
naming the function that decides after it reads the record the body or a
token names.`)
	report(stale, `routeTable entr(y/ies) match no live route. The route was renamed or removed
but its entry was not: delete the entry or update its pattern. Keys are
canonical chi patterns (canonicalRoute), with their {placeholders}. A stale
audit entry silently exempts whatever route later takes that pattern.`)
	report(unaudited, `mutating route(s) declare no audit. Decide each one's audit story and add it
with .with(...):

  - explicit(note): the route emits a real audit row. Emit it IN the business
    transaction (audit.Logger.LogInTx on the caller's *sql.Tx), so the change
    and its evidence commit or roll back together (ADR-090), and name the
    emission site in the note.
  - exempt(reason, note): the route writes no audit row, on purpose. reason is
    one of the closed enum (non_mutating_preview, machine_ingest,
    system_endpoint, webhook_owned), and the note says why and records any
    accepted loss.

An exempt() you do not mean un-audits a live route for good: in an append-only
compliance log, the row you did not write cannot be recovered later.`)
	report(unauditedExports, `export route(s) under /v1/exports declare no audit. A route that hands an
operator bulk tenant data must emit an action=export audit row before it streams
a byte, and fail closed if that row cannot be written (exportsHandler.auditExport),
then declare it with .with(explicit(...)). A row written when the stream
completes is no substitute: killing the connection mid-stream defeats it.`)
	report(strayAudits, `route(s) that neither change state nor export declare an audit. Only mutating
routes and the GET routes under /v1/exports carry one; remove it.`)
}

// TestRouteTable_DeclarationsAreWhole: each entry says what its parts need,
// and nothing else. A key is canonical. A record's clock parameter is in its
// route's pattern, and a deciding function exists. An audit names its site or
// justifies its exemption with a reason from the closed enum, and an export's
// audit is explicit: there is no honest exempt reason for handing out a whole
// table with no record.
func TestRouteTable_DeclarationsAreWhole(t *testing.T) {
	validReasons := map[exemptReason]bool{
		reasonNonMutatingPreview: true,
		reasonMachineIngest:      true,
		reasonSystemEndpoint:     true,
		reasonWebhookOwned:       true,
	}
	for k, entry := range routeTable {
		name := k.Method + " " + k.Pattern
		if got := canonicalRoute(k.Pattern); got != k.Pattern {
			t.Errorf("%s: key is not canonical (canonicalRoute gives %q); the root would never find it", name, got)
		}
		switch c := entry.clock; c.Source {
		case clockRealTime:
			if c.Param != "" || c.In != "" {
				t.Errorf("%s: real time takes no parameter or function, has %+v", name, c)
			}
		case clockOfCustomer, clockOfSubscription, clockOfInvoice, clockOfCreditNote, clockOfDunningRun:
			if c.Param == "" || c.In != "" || !strings.Contains(k.Pattern, "{"+c.Param+"}") {
				t.Errorf("%s: the record's parameter %q must be in the pattern, and no function named; has %+v", name, c.Param, c)
			}
		case clockDecidedIn:
			if c.In == "" || c.Param != "" {
				t.Errorf("%s: decidedIn names a function and no parameter; has %+v", name, c)
			} else if err := functionExists(c.In); err != nil {
				t.Errorf("%s: %v", name, err)
			}
		default:
			t.Errorf("%s: no clock source", name)
		}
		a := entry.audit
		if a.Coverage == 0 {
			continue
		}
		if strings.TrimSpace(a.Note) == "" {
			t.Errorf("%s: empty audit note. An explicit entry names its emission site; an exempt entry justifies itself.", name)
		}
		switch a.Coverage {
		case auditExplicit:
			if a.Reason != "" {
				t.Errorf("%s: explicit audit carries exempt reason %q", name, a.Reason)
			}
		case auditExempt:
			if !validReasons[a.Reason] {
				t.Errorf("%s: exempt reason %q is not in the closed enum. Widening what un-audited may mean is a deliberate decision: add the reason constant with a comment, in review.", name, a.Reason)
			}
			if !isMutatingMethod(k.Method) {
				t.Errorf("%s: exempt is for mutating routes only; an export must be explicit, since an unrecorded full-table export is the gap ADR-090 §7 closes", name)
			}
		default:
			t.Errorf("%s: audit declares no coverage (use explicit(...) or exempt(...))", name)
		}
	}
}

// TestRouteTable_ExemptionsStaySmall makes the exempt list a thing you
// have to look at. It is not a cap on correctness — it is a tripwire: if this
// number grows, someone is exempting routes to make the gate quiet.
func TestRouteTable_ExemptionsStaySmall(t *testing.T) {
	byReason := map[exemptReason][]string{}
	explicitN := 0
	for k, r := range routeTable {
		d := r.audit
		if d.Coverage == 0 {
			continue
		}
		if d.Coverage == auditExempt {
			byReason[d.Reason] = append(byReason[d.Reason], k.Method+" "+k.Pattern)
			continue
		}
		explicitN++
	}

	// The population as of the uninstall (ADR-090). Bump DELIBERATELY.
	want := map[exemptReason]int{
		reasonSystemEndpoint: 4, // /metrics × POST/PUT/PATCH/DELETE (r.Handle expands to every method)
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
		// each entry.
		reasonWebhookOwned: 2,
	}
	for reason, n := range want {
		if got := len(byReason[reason]); got != n {
			sort.Strings(byReason[reason])
			t.Errorf("exempt(%s): %d route(s), expected %d.\n  %s\n\nIf you meant to add an exemption, bump the count here and say why in the entry's note. If a route lost its audit emission and was exempted to keep CI green, that is the bug.",
				reason, got, n, strings.Join(byReason[reason], "\n  "))
		}
	}
	for reason, routes := range byReason {
		if _, known := want[reason]; !known {
			sort.Strings(routes)
			t.Errorf("exempt(%s) is not accounted for in this test: %s", reason, strings.Join(routes, ", "))
		}
	}

	exemptN := 0
	for _, routes := range byReason {
		exemptN += len(routes)
	}
	t.Logf("route table: %d explicit audits, %d exempt", explicitN, exemptN)
}

// TestCanonicalRoute pins the normalizations that let chi.Walk's patterns and
// Find's meet on one key.
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
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := canonicalRoute(tc.in); got != tc.want {
				t.Errorf("canonicalRoute(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestAuditRoute_ExemptOnlyForDeclaredRoutes: the detector's view of a
// request's route, from its record, is exempt only for a route the table
// declares exempt.
func TestAuditRoute_ExemptOnlyForDeclaredRoutes(t *testing.T) {
	for _, tc := range []struct {
		path string
		want bool
		why  string
	}{
		{"/v1/usage-events", true, "machine_ingest"},
		{"/v1/invoices/create_preview", true, "non_mutating_preview, a subrouter's Post(\"/\") leaf"},
		// Bootstrap mints a live secret key and audits it: a regression here
		// would silence the detector on the one route that provisions a
		// money-moving credential.
		{"/v1/bootstrap", false, "explicit"},
		{"/v1/credits/grant", false, "explicit"},
		{"/v1/usage-events/nope", false, "unmatched, under an exempt subtree"},
		{"/v1/not/a/route", false, "unknown means uncovered, loudly"},
	} {
		if _, exempt := auditRoute(lookedUp(http.MethodPost, tc.path)); exempt != tc.want {
			t.Errorf("POST %s: exempt %v, want %v (%s)", tc.path, exempt, tc.want, tc.why)
		}
	}
}

// functionExists finds the method package.Type.Method in internal/<package>,
// outside its tests.
func functionExists(name string) error {
	parts := strings.Split(name, ".")
	if len(parts) != 3 {
		return fmt.Errorf("%q is not package.Type.Method", name)
	}
	decl := regexp.MustCompile(`(?m)^func \(\w+ \*?` + parts[1] + `\) ` + parts[2] + `\(`)
	files, err := filepath.Glob(filepath.Join("..", parts[0], "*.go"))
	if err != nil || len(files) == 0 {
		return fmt.Errorf("%s: no package internal/%s", name, parts[0])
	}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			return err
		}
		if decl.Match(src) {
			return nil
		}
	}
	return fmt.Errorf("%s: no such method in internal/%s", name, parts[0])
}
