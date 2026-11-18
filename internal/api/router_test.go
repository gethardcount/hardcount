package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	mw "github.com/gethardcount/hardcount/internal/api/middleware"
	"github.com/gethardcount/hardcount/internal/auth"
	"github.com/gethardcount/hardcount/internal/config"
	"github.com/gethardcount/hardcount/internal/integrations/litellm"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/platform/request"
	"github.com/gethardcount/hardcount/internal/platform/request/requesttest"
	"github.com/gethardcount/hardcount/internal/session"
)

// newTestServer builds the production Server on a zero DB, so a test may send
// only requests answered without the database, such as /health or one with no
// credentials. NewServer runs engine.MustValidate, so every test that calls
// this also proves no engine collaborator is missing.
func newTestServer() *Server {
	return NewServer(&postgres.DB{}, nil, nil)
}

// fakeLinkToken stands in for the link token a public link carries in its
// path. It is plainly fake, so a failure that prints it is easy to read.
const fakeLinkToken = "FAKE-LINK-TOKEN-DO-NOT-LOG"

// chainStub is the handler the tests apply middleware to. It is a pointer,
// so two handlers made around it compare equal when their fields do.
var chainStub = new(struct{ http.Handler })

// handlersOf applies each middleware to the stub on its own and returns the
// handlers they make: chi.Walk reports middleware as funcs, and the handler a
// middleware returns has the type that says what it does.
func handlersOf(mws []func(http.Handler) http.Handler) []http.Handler {
	out := make([]http.Handler, len(mws))
	for i, m := range mws {
		out[i] = m(chainStub)
	}
	return out
}

// makesSame reports whether m makes the same handler as want: the same type
// with the same fields, such as an mw.RateLimited on the same limiter.
func makesSame(m, want func(http.Handler) http.Handler) bool {
	return m(chainStub) == want(chainStub)
}

func is[T any](h http.Handler) bool {
	_, ok := h.(T)
	return ok
}

// chainStep names a middleware by the handler type it makes.
type chainStep struct {
	name string
	is   func(http.Handler) bool
}

var (
	csrfMW        = chainStep{"the CSRF guard", is[mw.CrossSiteCheck]}
	authMW        = chainStep{"authentication", is[session.CallerRequired]}
	permissionMW  = chainStep{"the permission check", is[auth.PermissionRequired]}
	timeoutMW     = chainStep{"the timeout", is[mw.TimeLimit]}
	idempotencyMW = chainStep{"Idempotency", is[mw.IdempotencyGuard]}
	limiterMW     = chainStep{"the rate limit", is[mw.RateLimited]}
	doorMW        = chainStep{"the door", is[doorHandler]}
)

// chainIndex is the index of the first middleware in mws that makes step, or
// -1.
func chainIndex(mws []func(http.Handler) http.Handler, step chainStep) int {
	for i, h := range handlersOf(mws) {
		if step.is(h) {
			return i
		}
	}
	return -1
}

// A /v1 request with no credentials answers 401 before the rate limit runs,
// so the answer carries no rate-limit headers, on a route and on a path no
// route matches.
func TestRouter_AV1RequestWithNoCredentialsIs401BeforeTheRateLimit(t *testing.T) {
	srv := newTestServer()
	for _, r := range []struct{ method, path string }{
		{"GET", "/v1/customers"},
		{"POST", "/v1/customers"},
		{"GET", "/v1/meters"},
		{"GET", "/v1/plans"},
		{"GET", "/v1/plans/pln_123/prices"},
		{"GET", "/v1/subscriptions"},
		{"GET", "/v1/usage-events"},
		{"GET", "/v1/invoices"},
		{"GET", "/v1/api-keys"},
		{"GET", "/v1/dunning/policy"},
		{"GET", "/v1/credits/balance/cus_123"},
	} {
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, httptest.NewRequest(r.method, r.path, nil))
		var body struct{ Error struct{ Type string } }
		_ = json.NewDecoder(rec.Body).Decode(&body)
		if rec.Code != http.StatusUnauthorized || body.Error.Type != "authentication_error" {
			t.Errorf("%s %s: %d %s, want 401 authentication_error", r.method, r.path, rec.Code, body.Error.Type)
		}
		if got := rec.Header().Get("X-RateLimit-Limit"); got != "" {
			t.Errorf("%s %s: X-RateLimit-Limit = %q on an unauthenticated request, want none", r.method, r.path, got)
		}
	}
}

// On every route the chain runs in this order, each step where the route has
// it: the CSRF guard, authentication, the rate limit, Idempotency, the
// timeout, then the clock door. Each authenticated route is in one of the four
// /v1 groups, read from its limiter, its Idempotency and its time limit: the
// ingest, export and stream groups hold exactly the routes listed here, and
// every other authenticated route is an ordinary one.
func TestRouter_ChainsRunInOrderAndEachRouteIsInOneGroup(t *testing.T) {
	srv := newTestServer()
	type group struct {
		name        string
		limiter     *mw.RateLimiter
		idempotency bool
		timeout     time.Duration // 0: none
	}
	ordinary := group{"ordinary", srv.rateLimiter, true, 30 * time.Second}
	ingest := group{"ingest", srv.ingestLimiter, false, 30 * time.Second}
	exports := group{"exports", srv.rateLimiter, false, exportTimeout}
	stream := group{"stream", srv.rateLimiter, false, 0}
	want := map[group][]string{
		ingest: {
			"GET /v1/usage-events", "GET /v1/usage-events/aggregate", "GET /v1/usage-events/held",
			"POST /v1/integrations/litellm/spend", "POST /v1/usage-events", "POST /v1/usage-events/backfill",
			"POST /v1/usage-events/batch", "POST /v1/usage-events/held/discard", "POST /v1/usage-events/held/release",
		},
		exports: {
			"GET /v1/exports/audit-log.csv", "GET /v1/exports/customers.csv", "GET /v1/exports/invoices.csv",
			"GET /v1/exports/subscriptions.csv", "GET /v1/exports/usage-events.csv",
		},
		stream: {"GET /v1/webhook_events/stream"},
	}
	steps := []chainStep{csrfMW, authMW, permissionMW, limiterMW, idempotencyMW, timeoutMW, doorMW}
	const csrf, authn, perm, limit, idem, timeout, door = 0, 1, 2, 3, 4, 5, 6
	got := map[group][]string{}
	if err := chi.Walk(srv.router, func(method, route string, _ http.Handler, mws ...func(http.Handler) http.Handler) error {
		key := method + " " + canonicalRoute(route)
		at := make([]int, len(steps))
		last, lastName := -1, ""
		for i, step := range steps {
			if at[i] = chainIndex(mws, step); at[i] < 0 {
				continue
			}
			if at[i] < last {
				t.Errorf("%s: %s comes before %s", key, step.name, lastName)
			}
			last, lastName = at[i], step.name
		}
		if at[authn] < 0 {
			return nil // a public route: only the order applies
		}
		for _, i := range []int{csrf, perm, limit, door} {
			if at[i] < 0 {
				t.Errorf("%s: authenticated but without %s", key, steps[i].name)
				return nil
			}
		}
		if at[perm] != at[authn]+1 {
			t.Errorf("%s: the permission check does not come right after authentication", key)
		}
		g := group{idempotency: at[idem] >= 0}
		for _, l := range []*mw.RateLimiter{srv.rateLimiter, srv.ingestLimiter} {
			if makesSame(mws[at[limit]], l.Middleware()) {
				g.limiter = l
			}
		}
		if at[timeout] >= 0 {
			for _, d := range []time.Duration{30 * time.Second, 5 * time.Minute} {
				if makesSame(mws[at[timeout]], mw.Timeout(d)) {
					g.timeout = d
				}
			}
		}
		for _, known := range []group{ordinary, ingest, exports, stream} {
			if g.limiter == known.limiter && g.idempotency == known.idempotency && g.timeout == known.timeout {
				got[known] = append(got[known], key)
				return nil
			}
		}
		t.Errorf("%s: in none of the four groups: its limiter, Idempotency %v and time limit %v match none", key, g.idempotency, g.timeout)
		return nil
	}); err != nil {
		t.Fatalf("walk: %v", err)
	}
	for g, routes := range want {
		sort.Strings(got[g])
		if strings.Join(got[g], "\n") != strings.Join(routes, "\n") {
			t.Errorf("the %s group holds:\n  %s\nwant:\n  %s", g.name, strings.Join(got[g], "\n  "), strings.Join(routes, "\n  "))
		}
	}
	if len(got[ordinary]) < 100 {
		t.Errorf("only %d ordinary routes: did the walk break?", len(got[ordinary]))
	}
}

// A cross-site write with the session cookie answers 403 on the event stream
// and the exports, as on every other signed-in /v1 route.
func TestRouter_CrossSiteWriteToTheStreamOrAnExportIsRefused(t *testing.T) {
	srv := newTestServer()
	for _, path := range []string{"/v1/webhook_events/stream", "/v1/exports/customers.csv"} {
		req := httptest.NewRequest(http.MethodPost, path, nil)
		req.Header.Set("Origin", "https://attacker.example")
		req.AddCookie(&http.Cookie{Name: session.CookieName, Value: "a-session"})
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("cross-site POST %s with a session cookie = %d, want 403 (CSRF)", path, rec.Code)
		}
	}
}

// A cross-site sign-in answers 403 before the login handler runs: sign-in
// sets the session cookie, so another site could sign the visitor's browser
// in to an account of its choosing.
func TestRouter_CrossSiteSignInIsRefused(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/v1/auth/login", strings.NewReader("{"))
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	req.Header.Set("Origin", "https://attacker.example")
	rec := httptest.NewRecorder()
	newTestServer().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "cross-site request blocked") {
		t.Fatalf("cross-site POST /v1/auth/login = %d %s, want 403 cross-site request blocked", rec.Code, rec.Body.String())
	}
}

// An unknown route and a method the route does not take answer the JSON
// error envelope every other error uses, and a 405 lists the path's methods.
func TestRouter_UnknownRouteAndWrongMethodAnswerTheEnvelope(t *testing.T) {
	srv := newTestServer()
	for _, c := range []struct {
		method, path string
		status       int
		code, allow  string
	}{
		{"GET", "/no-such-route", 404, "route_not_found", ""},
		{"GET", "/no/such/route", 404, "route_not_found", ""},
		{"POST", "/health", 405, "method_not_allowed", "GET"},
		{"GET", "/v1/auth/login", 405, "method_not_allowed", "POST"},
	} {
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, httptest.NewRequest(c.method, c.path, nil))
		if rec.Code != c.status {
			t.Errorf("%s %s = %d, want %d; body %q", c.method, c.path, rec.Code, c.status, rec.Body.String())
			continue
		}
		if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
			t.Errorf("%s %s Content-Type = %q, want application/json", c.method, c.path, ct)
		}
		var body struct {
			Error struct {
				Type, Code, Message string
				RequestID           string `json:"request_id"`
			} `json:"error"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Errorf("%s %s body is not JSON: %q", c.method, c.path, rec.Body.String())
			continue
		}
		e := body.Error
		if e.Type != "invalid_request_error" || e.Code != c.code || e.Message == "" || e.RequestID == "" {
			t.Errorf("%s %s error = %+v, want type invalid_request_error, code %s, a message and a request_id", c.method, c.path, e, c.code)
		}
		if got := rec.Header().Get("Allow"); got != c.allow {
			t.Errorf("%s %s Allow = %q, want %q", c.method, c.path, got, c.allow)
		}
	}
}

// edgeRouter mounts the production request edge (useRequestEdge) on a chi
// router with one public token route. Spans go to the returned recorder, and
// the route records the client address it sees.
func edgeRouter(t *testing.T, trustProxy string) (http.Handler, *tracetest.SpanRecorder, *string) {
	t.Helper()
	rec := recordSpans(t)
	var seenIP string
	r := chi.NewRouter()
	route := func(req *http.Request) request.Route {
		return request.Route{Method: req.Method, Pattern: r.Find(chi.NewRouteContext(), req.Method, req.URL.Path)}
	}
	useRequestEdge(r, trustProxy, config.Server{TrustProxy: trustProxy}.TrustedProxies(), request.Begin(route))
	r.Get("/v1/public/invoices/{token}", func(w http.ResponseWriter, req *http.Request) {
		seenIP = request.Of(req.Context()).ClientIP()
		w.WriteHeader(http.StatusOK)
	})
	return r, rec, &seenIP
}

// recordSpans sends every span to the returned recorder until the test ends.
func recordSpans(t *testing.T) *tracetest.SpanRecorder {
	t.Helper()
	rec := tracetest.NewSpanRecorder()
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec)))
	t.Cleanup(func() { otel.SetTracerProvider(prev) })
	return rec
}

func spanAttrs(s sdktrace.ReadOnlySpan) map[attribute.Key]string {
	m := map[attribute.Key]string{}
	for _, kv := range s.Attributes() {
		m[kv.Key] = kv.Value.String()
	}
	return m
}

// The client address reads X-Forwarded-For only from a proxy TRUST_PROXY
// lists, and never X-Real-IP; otherwise it is the TCP peer.
func TestRequestEdge_ClientIPReadsForwardingOnlyFromATrustedProxy(t *testing.T) {
	for _, tc := range []struct {
		name       string
		trustProxy string
		remoteAddr string
		xff        []string // one entry per header line
		xRealIP    string
		want       string
	}{
		{"trusted proxy: right-most untrusted XFF entry", "10.0.0.0/8", "10.0.0.2:443", []string{"198.51.100.7, 10.0.0.5"}, "", "198.51.100.7"},
		{"trusted proxy: XFF split over two header lines merges every line", "10.0.0.0/8", "10.0.0.2:443", []string{"203.0.113.66", "198.51.100.7"}, "", "198.51.100.7"},
		{"trusted proxy: an entry that is not a bare IP stops the walk at the proxy", "10.0.0.0/8", "10.0.0.2:443", []string{"203.0.113.66, 198.51.100.7:4711"}, "", "10.0.0.2"},
		{"trusted proxy: X-Real-IP is not read", "10.0.0.0/8", "10.0.0.2:443", nil, "203.0.113.66", "10.0.0.2"},
		{"untrusted peer: forwarding headers are not read", "10.0.0.0/8", "203.0.113.9:5555", []string{"198.51.100.7"}, "198.51.100.8", "203.0.113.9"},
		{"TRUST_PROXY unset: the TCP peer", "", "10.0.0.2:443", []string{"198.51.100.7"}, "", "10.0.0.2"},
		{"TRUST_PROXY=none: the TCP peer", "none", "203.0.113.9:5555", []string{"198.51.100.7"}, "", "203.0.113.9"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, _, seenIP := edgeRouter(t, tc.trustProxy)
			req := httptest.NewRequest(http.MethodGet, "/v1/public/invoices/"+fakeLinkToken, nil)
			req.RemoteAddr = tc.remoteAddr
			for _, v := range tc.xff {
				req.Header.Add("X-Forwarded-For", v)
			}
			if tc.xRealIP != "" {
				req.Header.Set("X-Real-IP", tc.xRealIP)
			}
			h.ServeHTTP(httptest.NewRecorder(), req)
			if *seenIP != tc.want {
				t.Fatalf("client IP = %q, want %q", *seenIP, tc.want)
			}
		})
	}
}

// A routed request's span is named by its route and carries the trusted
// client address, and no attribute holds the link token.
func TestRequestEdge_SpanCarriesRouteAndTrustedIP_NotToken(t *testing.T) {
	h, rec, _ := edgeRouter(t, "10.0.0.0/8")
	req := httptest.NewRequest(http.MethodGet, "/v1/public/invoices/"+fakeLinkToken, nil)
	req.RemoteAddr = "10.0.0.2:443"
	req.Header.Set("X-Forwarded-For", "203.0.113.66, 198.51.100.7")
	h.ServeHTTP(httptest.NewRecorder(), req)

	spans := rec.Ended()
	if len(spans) != 1 {
		t.Fatalf("got %d spans, want 1", len(spans))
	}
	if got, want := spans[0].Name(), "GET /v1/public/invoices/{token}"; got != want {
		t.Errorf("span name = %q, want %q", got, want)
	}
	attrs := spanAttrs(spans[0])
	for k, want := range map[attribute.Key]string{
		"http.route":     "/v1/public/invoices/{token}",
		"url.path":       "/v1/public/invoices/{token}",
		"client.address": "198.51.100.7",
	} {
		if attrs[k] != want {
			t.Errorf("%s = %q, want %q", k, attrs[k], want)
		}
	}
	for k, v := range attrs {
		if strings.Contains(v, fakeLinkToken) {
			t.Errorf("attribute %s carries the path token: %q", k, v)
		}
	}
}

// A CORS preflight is answered before any route runs: on the production
// router, its span on every link-token address keeps only the path's first
// segment, carries the TCP peer as its client, and holds no token.
func TestRequestEdge_PreflightSpanKeepsTheFirstSegment(t *testing.T) {
	srv := newTestServer()
	for _, path := range []string{
		"/v1/public/invoices/" + fakeLinkToken + "/pdf",
		"/v1/public/payment-updates/" + fakeLinkToken,
		"/v1/public/cost-dashboard/" + fakeLinkToken,
		"/v1/auth/invite/" + fakeLinkToken,
	} {
		rec := recordSpans(t)
		req := httptest.NewRequest(http.MethodOptions, path, nil)
		req.RemoteAddr = "203.0.113.9:5555"
		req.Header.Set("X-Forwarded-For", "198.51.100.7")
		req.Header.Set("Origin", "http://localhost:5173")
		req.Header.Set("Access-Control-Request-Method", http.MethodGet)
		srv.ServeHTTP(httptest.NewRecorder(), req)

		spans := rec.Ended()
		if len(spans) != 1 {
			t.Fatalf("%s: got %d spans, want 1", path, len(spans))
		}
		attrs := spanAttrs(spans[0])
		if got := attrs["url.path"]; got != "/v1/…" {
			t.Errorf("%s: url.path = %q, want /v1/…", path, got)
		}
		if got := attrs["client.address"]; got != "203.0.113.9" {
			t.Errorf("%s: client.address = %q, want the TCP peer; no proxy is trusted", path, got)
		}
		if strings.Contains(spans[0].Name(), fakeLinkToken) {
			t.Errorf("%s: span name carries the path token: %q", path, spans[0].Name())
		}
		for k, v := range attrs {
			if strings.Contains(v, fakeLinkToken) {
				t.Errorf("%s: attribute %s holds the link token: %q", path, k, v)
			}
		}
	}
}

// With no route matched, loggablePath keeps the first segment as the path
// splits it, and a path of one segment or fewer whole.
// TestAccessLog_PathIsAPatternOrTheFirstSegment holds the rest of the rule
// through the production router.
func TestLoggablePath_KeepsTheFirstSegmentOfAnUnmatchedPath(t *testing.T) {
	for _, tc := range []struct{ name, path, want string }{
		{"a path starting with two slashes keeps its empty first segment", "//a/b/c", "//…"},
		{"an encoded slash is cut after it decodes", "/a/b%2Fc/d", "/a/…"},
		{"a one-segment path with a trailing slash is logged whole", "/a/", "/a/"},
		{"the root is logged whole", "/", "/"},
		{"an empty path is logged whole", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest("GET", "/", nil)
			if tc.path != "" {
				req = httptest.NewRequest("GET", tc.path, nil)
			} else {
				req.URL.Path = ""
			}
			var got string
			requesttest.Root(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { got = loggablePath(r) })).
				ServeHTTP(httptest.NewRecorder(), req)
			if got != tc.want {
				t.Errorf("loggablePath(%q) = %q, want %q", tc.path, got, tc.want)
			}
		})
	}
}

// captureLogs sends every log line to the returned buffer until the test ends.
func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &logs
}

// panickingWriter is a ResponseWriter whose first status write panics, so
// any answer the router gives panics inside the Recoverer.
type panickingWriter struct{ *httptest.ResponseRecorder }

func (panickingWriter) WriteHeader(int) { panic("writing the status") }

// The production router logs a panic on a link-token address with a path
// that holds no token. The panic comes from writing the 404, the one answer
// a router on an empty database gives on these paths.
func TestPanicLog_RealRouterLogsNoLinkToken(t *testing.T) {
	for _, tc := range []struct{ name, path, want string }{
		{"a miss under the hosted invoice mount keeps its first segment", "/v1/public/invoices/" + fakeLinkToken + "/no-such-page", "/v1/…"},
		{"the hosted invoice page's address keeps its first segment", "/invoice/" + fakeLinkToken, "/invoice/…"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := newTestServer()
			logs := captureLogs(t)
			srv.ServeHTTP(panickingWriter{httptest.NewRecorder()}, httptest.NewRequest(http.MethodGet, tc.path, nil))
			if strings.Contains(logs.String(), fakeLinkToken) {
				t.Errorf("the log holds the link token:\n%s", logs.String())
			}
			var line string
			for _, l := range strings.Split(logs.String(), "\n") {
				if strings.Contains(l, `msg="handler panic"`) {
					line, _, _ = strings.Cut(l, " stack=")
				}
			}
			if !strings.HasSuffix(line, " path="+tc.want) {
				t.Errorf("panic line path is not %q:\n%s", tc.want, logs.String())
			}
		})
	}
}

// The LiteLLM adapter reads bodies up to litellm.MaxBodyBytes, a full batch
// being about 4 MB, and every other route stays capped at 1 MB.
func TestLimitRequestBody_LiteLLMRouteHasItsOwnCap(t *testing.T) {
	// The stub reads the whole body and answers how much it read, or the cap
	// it hit.
	h := limitRequestBody(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n, err := io.Copy(io.Discard, r.Body)
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			w.WriteHeader(http.StatusRequestEntityTooLarge)
			_, _ = fmt.Fprint(w, tooBig.Limit)
			return
		}
		_, _ = fmt.Fprint(w, n)
	}))
	const batch = 4 << 20
	for _, c := range []struct {
		name, path string
		size       int
		wantCode   int
		wantBody   string
	}{
		{"LiteLLM spend reads a full batch", "/v1/integrations/litellm/spend", batch, http.StatusOK, fmt.Sprint(batch)},
		{"LiteLLM spend reads up to its cap", "/v1/integrations/litellm/spend", litellm.MaxBodyBytes, http.StatusOK, fmt.Sprint(litellm.MaxBodyBytes)},
		{"LiteLLM spend stops past its cap", "/v1/integrations/litellm/spend", litellm.MaxBodyBytes + 1, http.StatusRequestEntityTooLarge, fmt.Sprint(litellm.MaxBodyBytes)},
		{"usage batch keeps 1 MB", "/v1/usage-events/batch", batch, http.StatusRequestEntityTooLarge, fmt.Sprint(maxRequestBodyBytes)},
		{"other routes keep 1 MB", "/v1/customers", maxRequestBodyBytes + 1, http.StatusRequestEntityTooLarge, fmt.Sprint(maxRequestBodyBytes)},
		{"1 MB still reads", "/v1/customers", maxRequestBodyBytes, http.StatusOK, fmt.Sprint(maxRequestBodyBytes)},
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, c.path, bytes.NewReader(make([]byte, c.size))))
		if rec.Code != c.wantCode || rec.Body.String() != c.wantBody {
			t.Errorf("%s: POST %s with %d bytes = %d %q, want %d %q", c.name, c.path, c.size, rec.Code, rec.Body.String(), c.wantCode, c.wantBody)
		}
	}
}
