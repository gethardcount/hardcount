package api

import (
	"fmt"
	"net/http"
	"os"
	"reflect"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"gopkg.in/yaml.v3"
)

// specPath is api/openapi.yaml, relative to this package. `go test` runs with
// the package directory as the working directory.
const specPath = "../../api/openapi.yaml"

// metricsAnyMethod is why the non-GET /metrics routes are not in the spec.
const metricsAnyMethod = "/metrics is registered with r.Handle, which chi expands to every method. " +
	"Prometheus scrapes it with GET, the one method the spec lists. The other methods exist only because of r.Handle."

// routesNotInSpec lists the live routes the spec leaves out on purpose, each
// with the reason. Keys are "METHOD /pattern", the pattern as chi registers it.
// Add an entry only for a route no caller should use. A route that someone
// calls belongs in api/openapi.yaml.
var routesNotInSpec = map[string]string{
	"POST /metrics":    metricsAnyMethod,
	"PUT /metrics":     metricsAnyMethod,
	"PATCH /metrics":   metricsAnyMethod,
	"DELETE /metrics":  metricsAnyMethod,
	"HEAD /metrics":    metricsAnyMethod,
	"OPTIONS /metrics": metricsAnyMethod,
	"CONNECT /metrics": metricsAnyMethod,
	"TRACE /metrics":   metricsAnyMethod,
}

// openAPIMethods are the operation keys an OpenAPI 3.0 path item can hold.
// Other keys on a path item (parameters, summary, servers) are not operations.
var openAPIMethods = map[string]bool{
	"get": true, "put": true, "post": true, "delete": true,
	"options": true, "head": true, "patch": true, "trace": true,
}

var pathParam = regexp.MustCompile(`\{[^}]*\}`)

// routeShape is the route with every path parameter written as {}. Two paths
// with the same shape match the same URLs, whatever their parameters are named.
func routeShape(method, path string) string {
	return method + " " + pathParam.ReplaceAllString(canonicalRoute(path), "{}")
}

// liveRoutes walks the router NewServer builds. It returns every route keyed
// by routeShape, mapped to the canonical pattern chi registered.
func liveRoutes(t *testing.T) map[string]string {
	t.Helper()
	srv := newTestServer()
	out := make(map[string]string)
	err := chi.Walk(srv.router, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		out[routeShape(method, route)] = canonicalRoute(route)
		return nil
	})
	if err != nil {
		t.Fatalf("chi.Walk over the live router: %v", err)
	}
	if len(out) == 0 {
		t.Fatal("walked 0 routes: the walk is broken, not the router")
	}
	return out
}

// specOperations reads api/openapi.yaml. It returns every operation keyed by
// routeShape, mapped to the path the spec declares.
func specOperations(t *testing.T) map[string]string {
	t.Helper()
	raw, err := os.ReadFile(specPath)
	if err != nil {
		t.Fatalf("read %s: %v", specPath, err)
	}
	var doc struct {
		Paths map[string]map[string]yaml.Node `yaml:"paths"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse %s: %v", specPath, err)
	}
	out := make(map[string]string)
	for path, item := range doc.Paths {
		for key := range item {
			if !openAPIMethods[key] {
				continue
			}
			k := routeShape(strings.ToUpper(key), path)
			if prev, dup := out[k]; dup {
				t.Errorf("spec paths %s and %s describe the same route (%s)", prev, path, k)
			}
			out[k] = path
		}
	}
	if len(out) == 0 {
		t.Fatalf("read 0 operations from %s: the parse is broken, not the spec", specPath)
	}
	return out
}

// TestOpenAPISpec_MatchesRouter ties api/openapi.yaml to the chi router in both
// directions. Every route the server answers must be in the spec with the same
// method and the same path parameter names, or be listed in routesNotInSpec
// with a reason. Every operation in the spec must be a route the server
// answers. `make gen`, the dashboard's typed client and anyone generating a
// client read the spec. A route missing from it is invisible to them, and a
// spec path with no route promises something the server does not do.
func TestOpenAPISpec_MatchesRouter(t *testing.T) {
	live := liveRoutes(t)
	spec := specOperations(t)

	liveExact := make(map[string]bool, len(live))
	for k, pattern := range live {
		method, _, _ := strings.Cut(k, " ")
		liveExact[method+" "+pattern] = true
	}

	var missing, notRouted, renamed, badExclusions []string
	for k, pattern := range live {
		method, _, _ := strings.Cut(k, " ")
		path, inSpec := spec[k]
		switch {
		case inSpec && path != pattern:
			renamed = append(renamed, fmt.Sprintf("%s: spec path %s, chi pattern %s", method, path, pattern))
		case inSpec:
		case routesNotInSpec[method+" "+pattern] != "":
		default:
			missing = append(missing, method+" "+pattern)
		}
	}
	for k, path := range spec {
		if _, ok := live[k]; !ok {
			method, _, _ := strings.Cut(k, " ")
			notRouted = append(notRouted, method+" "+path)
		}
	}
	for k, reason := range routesNotInSpec {
		method, pattern, _ := strings.Cut(k, " ")
		switch {
		case strings.TrimSpace(reason) == "":
			badExclusions = append(badExclusions, k+": no reason given")
		case !liveExact[k]:
			badExclusions = append(badExclusions, k+": no such route in the router")
		case spec[routeShape(method, pattern)] != "":
			badExclusions = append(badExclusions, k+": the spec describes it, so remove the exclusion")
		}
	}

	if len(missing) > 0 {
		sort.Strings(missing)
		t.Errorf(`%d route(s) the server answers are not in api/openapi.yaml:

  %s

Describe each one in api/openapi.yaml (path, method, operationId, responses) and
run make gen. Only a route no caller should use goes in routesNotInSpec instead,
with the reason.`, len(missing), strings.Join(missing, "\n  "))
	}
	if len(notRouted) > 0 {
		sort.Strings(notRouted)
		t.Errorf(`%d operation(s) in api/openapi.yaml match no route in the router:

  %s

The route was removed or renamed and the spec was not, or the spec has the wrong
method or path. Fix the spec and run make gen.`, len(notRouted), strings.Join(notRouted, "\n  "))
	}
	if len(renamed) > 0 {
		sort.Strings(renamed)
		t.Errorf(`%d spec path(s) name their path parameters differently from the router:

  %s

Rename the parameters in api/openapi.yaml to match the chi pattern and run make
gen. The generated server reads each parameter by the name the spec gives it.`, len(renamed), strings.Join(renamed, "\n  "))
	}
	if len(badExclusions) > 0 {
		sort.Strings(badExclusions)
		t.Errorf("routesNotInSpec has entries to fix:\n  %s", strings.Join(badExclusions, "\n  "))
	}
}

// idempotencyKeyRef is how a spec operation declares the Idempotency-Key header.
const idempotencyKeyRef = "#/components/parameters/IdempotencyKey"

// specIdempotencyHeader reads api/openapi.yaml. It returns every operation keyed
// by routeShape, mapped to whether the operation (or its path item) lists the
// IdempotencyKey parameter.
func specIdempotencyHeader(t *testing.T) map[string]bool {
	t.Helper()
	raw, err := os.ReadFile(specPath)
	if err != nil {
		t.Fatalf("read %s: %v", specPath, err)
	}
	type param struct {
		Ref string `yaml:"$ref"`
	}
	var doc struct {
		Paths map[string]map[string]yaml.Node `yaml:"paths"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse %s: %v", specPath, err)
	}
	declares := func(n yaml.Node) bool {
		var params []param
		if err := n.Decode(&params); err != nil {
			t.Fatalf("decode parameters in %s: %v", specPath, err)
		}
		for _, p := range params {
			if p.Ref == idempotencyKeyRef {
				return true
			}
		}
		return false
	}
	out := make(map[string]bool)
	for path, item := range doc.Paths {
		onPath := false
		if n, ok := item["parameters"]; ok {
			onPath = declares(n)
		}
		for key, node := range item {
			if !openAPIMethods[key] {
				continue
			}
			var op struct {
				Parameters yaml.Node `yaml:"parameters"`
			}
			if err := node.Decode(&op); err != nil {
				t.Fatalf("decode %s %s: %v", key, path, err)
			}
			out[routeShape(strings.ToUpper(key), path)] = onPath || (op.Parameters.Kind != 0 && declares(op.Parameters))
		}
	}
	return out
}

// isIdempotencyMiddleware reports whether m is the closure mw.Idempotency
// returns. Comparing function pointers does not work: the compiler inlines
// mw.Idempotency into NewServer, so the router's closure is a different
// function from one built here. The closure's name still carries
// ".Idempotency." either way ("middleware.Idempotency.func1" when not inlined,
// "NewServer.func15.Idempotency.4" when inlined). If a rename breaks the match,
// the test finds 0 routes and fails.
func isIdempotencyMiddleware(m func(http.Handler) http.Handler) bool {
	fn := runtime.FuncForPC(reflect.ValueOf(m).Pointer())
	return fn != nil && strings.Contains(fn.Name(), ".Idempotency.")
}

// TestOpenAPISpec_IdempotencyKeyHeader ties the spec's Idempotency-Key header
// to the routes that honour it. mw.Idempotency caches the response of a POST,
// PUT or PATCH that sends the header, and only routes mounted behind it do
// that. Every such route must declare the header in api/openapi.yaml, so
// generated clients can send it, and no other operation may declare it, since
// the server would ignore it there.
func TestOpenAPISpec_IdempotencyKeyHeader(t *testing.T) {
	honours := make(map[string]bool)
	srv := newTestServer()
	err := chi.Walk(srv.router, func(method, route string, _ http.Handler, mws ...func(http.Handler) http.Handler) error {
		if method != http.MethodPost && method != http.MethodPut && method != http.MethodPatch {
			return nil
		}
		for _, m := range mws {
			if isIdempotencyMiddleware(m) {
				honours[routeShape(method, route)] = true
				break
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("chi.Walk over the live router: %v", err)
	}
	if len(honours) == 0 {
		t.Fatal("found 0 routes behind mw.Idempotency: the walk is broken, not the router")
	}

	spec := specIdempotencyHeader(t)
	paths := specOperations(t)
	var undeclared, stray []string
	for k := range honours {
		if declared, inSpec := spec[k]; inSpec && !declared {
			undeclared = append(undeclared, k+" ("+paths[k]+")")
		}
	}
	for k, declared := range spec {
		if declared && !honours[k] {
			stray = append(stray, k+" ("+paths[k]+")")
		}
	}
	if len(undeclared) > 0 {
		sort.Strings(undeclared)
		t.Errorf("%d operation(s) honour Idempotency-Key but do not declare it. Add\n  - $ref: \"%s\"\nto their parameters:\n  %s",
			len(undeclared), idempotencyKeyRef, strings.Join(undeclared, "\n  "))
	}
	if len(stray) > 0 {
		sort.Strings(stray)
		t.Errorf("%d operation(s) declare Idempotency-Key, but mw.Idempotency does not act on them, so the server ignores it:\n  %s",
			len(stray), strings.Join(stray, "\n  "))
	}
}
