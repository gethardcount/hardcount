package api

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/gethardcount/hardcount/internal/auth"
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
token names. Then who may call it: .needs(customerWrite), or another
permission, a session value or anyCaller, or .outsideAuth() for a route outside
/v1's authentication. docs/architecture.md, "Adding a route", has an example.`)
	report(stale, `routeTable entr(y/ies) match no live route. The route was renamed or removed
but its entry was not: delete the entry or update its pattern. Keys are
canonical chi patterns (canonicalRoute), with their {placeholders}. A stale
audit entry silently exempts whatever route later takes that pattern.`)
	report(unaudited, `mutating route(s) declare no audit. Decide each one's audit story and add it
with .withAudit(...):

  - explicit(note): the route emits a real audit row. Emit it IN the business
    transaction (audit.Logger.LogInTx on the caller's *sql.Tx), so the change
    and its evidence commit or roll back together (ADR-090). The note starts
    with the function that writes the row, as package.Type.Method.
  - exempt(reason, note): the route writes no audit row, on purpose. reason is
    one of the closed enum (non_mutating_preview, machine_ingest,
    system_endpoint, webhook_owned), and the note says why and records any
    accepted loss.

An exempt() you do not mean un-audits a live route for good: in an append-only
compliance log, the row you did not write cannot be recovered later.`)
	report(unauditedExports, `export route(s) under /v1/exports declare no audit. A route that hands an
operator bulk tenant data must emit an action=export audit row before it streams
a byte, and fail closed if that row cannot be written (exportsHandler.auditExport),
then declare it with .withAudit(explicit(...)). A row written when the stream
completes is no substitute: killing the connection mid-stream defeats it.`)
	report(strayAudits, `route(s) that neither change state nor export declare an audit. Only mutating
routes and the GET routes under /v1/exports carry one; remove it.`)
}

// TestRouteTable_DeclarationsAreWhole: each entry says what its parts need,
// and nothing else. A key is canonical. A record's clock parameter is in its
// route's pattern, and a deciding function exists. An explicit audit's note
// starts with a function that exists; an exempt one has a reason from the
// closed list, and only a state-changing route is exempt: there is no honest
// reason for handing out a whole table with no record.
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
			if c.In == "" || c.Param != "" || strings.Count(c.In, ".") != 2 {
				t.Errorf("%s: decidedIn names a function as package.Type.Method, and no parameter; has %+v", name, c)
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
			t.Errorf("%s: empty audit note. An explicit entry names the function that writes the row; an exempt entry says why there is none.", name)
		}
		switch a.Coverage {
		case auditExplicit:
			if a.Reason != "" {
				t.Errorf("%s: explicit audit carries exempt reason %q", name, a.Reason)
			}
			if words := strings.Fields(a.Note); len(words) > 0 {
				if err := functionExists(strings.TrimSuffix(words[0], ".")); err != nil {
					t.Errorf("%s: an explicit note starts with the function that writes the row: %v", name, err)
				}
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

// TestRouteTable_AnyCallerStaysSmall lists the routes any API key or session
// may call, so one added to skip a permission shows in review.
func TestRouteTable_AnyCallerStaysSmall(t *testing.T) {
	var got []string
	for k, d := range routeTable {
		if d.access.Any {
			got = append(got, k.Method+" "+k.Pattern)
		}
	}
	sort.Strings(got)
	if want := []string{"GET /v1/whoami"}; !slices.Equal(got, want) {
		t.Errorf("anyCaller routes: %v, want %v. A route that changes or reads an account's data names its permission.", got, want)
	}
}

// TestRouteTable_SessionOnlyRoutesAreListed lists the routes only a dashboard
// session may call, so a key permission given to one shows in review: on a
// members route it would let any secret key change who belongs to the
// account.
func TestRouteTable_SessionOnlyRoutesAreListed(t *testing.T) {
	var got []string
	for k, d := range routeTable {
		if d.access.Session != "" {
			got = append(got, k.Method+" "+k.Pattern)
		}
	}
	sort.Strings(got)
	want := []string{
		"DELETE /v1/members/invitations/{id}",
		"DELETE /v1/members/{userID}",
		"GET /v1/members",
		"POST /v1/members/invite",
		"PUT /v1/settings",
	}
	if !slices.Equal(got, want) {
		t.Errorf("session-only routes: %v, want %v", got, want)
	}
}

// TestRouteTable_WritesNeedAWritePermission: a route behind authentication
// with a POST, PUT, PATCH or DELETE needs a write permission, usage:ingest or
// a dashboard session, so a key that held only read permissions could change
// nothing. The few that need only a read permission are listed, each with
// its reason.
func TestRouteTable_WritesNeedAWritePermission(t *testing.T) {
	var got []string
	for k, d := range routeTable {
		a := d.access
		if !isMutatingMethod(k.Method) || d.outsideAuth || a.Session != "" ||
			a.Perm == auth.PermUsageIngest || a.Perm.Writes() {
			continue
		}
		got = append(got, k.Method+" "+k.Pattern)
	}
	sort.Strings(got)
	want := []string{
		// Works out an invoice and stores nothing.
		"POST /v1/invoices/create_preview",
		// Sends again an event already emitted.
		"POST /v1/webhook_events/{id}/replay",
	}
	if !slices.Equal(got, want) {
		t.Errorf("writes that need no write permission: %v, want %v. A route that changes an account's data needs its write permission.", got, want)
	}
}

// TestRouteTable_OutsideAuthMatchesTheRouter: an entry says outsideAuth
// exactly when its route is outside authentication. One behind it would be
// refused by every caller (its access is zero); one outside it that needed a
// permission would have it checked by nobody.
func TestRouteTable_OutsideAuthMatchesTheRouter(t *testing.T) {
	err := chi.Walk(builtRouter(), func(method, route string, _ http.Handler, mws ...func(http.Handler) http.Handler) error {
		key := routeKey{Method: method, Pattern: canonicalRoute(route)}
		if authed := chainIndex(mws, authMW) >= 0; routeTable[key].outsideAuth == authed {
			t.Errorf("%s %s: outsideAuth is %v, but the route is behind authentication: %v", method, key.Pattern, routeTable[key].outsideAuth, authed)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
}

// TestRouteTable_ExemptionsStaySmall pins how many routes each exempt reason
// covers, so an exemption added to quiet the detector shows in review.
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

	want := map[exemptReason]int{
		reasonSystemEndpoint:     4, // /metrics: POST, PUT, PATCH, DELETE
		reasonMachineIngest:      3, // usage events, their batch, the LiteLLM callback
		reasonNonMutatingPreview: 2, // invoice create_preview, recipe preview
		reasonWebhookOwned:       2, // Stripe, Postmark
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

// canonicalRoute makes chi.Walk's patterns and Find's meet on one key.
func TestCanonicalRoute_MakesWalkAndFindPatternsMeet(t *testing.T) {
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

// On real URLs, as the root's record finds them, only a route marked
// withShowOnce answers with a show-once secret: every marked route with
// concrete ids is, and the near misses around them are not.
func TestIsShowOnce_OnlyOnAMarkedRoute(t *testing.T) {
	for _, tc := range []struct {
		method, path string
		want         bool
	}{
		{"POST", "/v1/api-keys", true},
		{"POST", "/v1/api-keys/", true},
		{"POST", "/v1/api-keys/hc_key_dbs004jmajdrc1h3298g/rotate", true},
		// chi matches an empty id; the handler refuses it, and the refusal
		// is not kept (TestE2E_APIKeyIssue_SecretIsNotStoredForReplay).
		{"POST", "/v1/api-keys//rotate", true},
		// chi routes on the escaped path, so an id holding an escaped slash
		// finds the rotate route, as the handler does.
		{"POST", "/v1/api-keys/a%2Fb/rotate", true},
		{"POST", "/v1/customers/hc_cus_dakhnk2q3gpgqfoq5c0g/rotate-cost-dashboard-token", true},
		{"POST", "/v1/customers/hc_cus_dakhnk2q3gpgqfoq5c0g/payment-methods/setup-session", true},
		{"POST", "/v1/webhook-endpoints/endpoints", true},
		{"POST", "/v1/webhook-endpoints/endpoints/hc_whe_danslg2q3gpgqfoq5c80/rotate-secret", true},

		{"GET", "/v1/api-keys", false},
		{"PATCH", "/v1/api-keys", false},
		{"DELETE", "/v1/api-keys/hc_key_a", false},
		{"POST", "/v1/api-keysx", false},
		{"POST", "/v1/api-keys/hc_key_a", false},
		{"POST", "/v1/api-keys/hc_key_a/rotate/extra", false},
		// The /api-keys mount matches but its sub-router has no such route, so
		// the ordinary group runs and its 404 is kept for a replay; no handler
		// runs, so no secret is involved.
		{"POST", "/v1/api-keys/hc_key_a/rotate/", false},
		{"POST", "/v1/customers", false},
		{"POST", "/v1/customers/hc_cus_a/payment-methods", false},
		{"POST", "/v1/webhook-endpoints/endpoints/hc_whe_a/test", false},
		{"PATCH", "/v1/webhook-endpoints/endpoints/hc_whe_a", false},
		{"POST", "/v2/api-keys", false},
	} {
		if got := isShowOnce(lookedUp(tc.method, tc.path)); got != tc.want {
			t.Errorf("%s %s: %v, want %v", tc.method, tc.path, got, tc.want)
		}
	}
}

// isMutatingMethod reports whether a method can change state.
func isMutatingMethod(m string) bool {
	switch m {
	case "POST", "PUT", "PATCH", "DELETE":
		return true
	}
	return false
}

// auditEgressPrefix is the subtree whose GET routes take data out and so
// declare their audit.
const auditEgressPrefix = "/v1/exports"

// functionExists finds package.Type.Method, or package.Function, in
// internal/<package>, outside its tests.
func functionExists(name string) error {
	parts := strings.Split(name, ".")
	var decl *regexp.Regexp
	switch len(parts) {
	case 2:
		decl = regexp.MustCompile(`(?m)^func ` + parts[1] + `\(`)
	case 3:
		decl = regexp.MustCompile(`(?m)^func \(\w+ \*?` + parts[1] + `\) ` + parts[2] + `\(`)
	default:
		return fmt.Errorf("%q is not package.Type.Method or package.Function", name)
	}
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
	return fmt.Errorf("%s: no such function in internal/%s", name, parts[0])
}
