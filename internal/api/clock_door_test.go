package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/gethardcount/hardcount/internal/auth"
	"github.com/gethardcount/hardcount/internal/errs"
	"github.com/gethardcount/hardcount/internal/platform/clock"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
)

// spyResolver answers every record on clock clk_1 at doorFrozen (or, with
// noClock, on no clock, as the engine answers: the machine's time and no
// clock id), or err, and records what it was asked.
type spyResolver struct {
	asked   []string
	err     error
	noClock bool
}

var doorFrozen = time.Date(2025, 3, 1, 12, 0, 0, 0, time.UTC)

func (s *spyResolver) answer(kind, id string) (clock.Sim, error) {
	s.asked = append(s.asked, kind+":"+id)
	if s.err != nil {
		return clock.Sim{}, s.err
	}
	if s.noClock {
		return clock.Sim{At: time.Now().UTC()}, nil
	}
	return clock.Sim{At: doorFrozen, TestClockID: "clk_1"}, nil
}
func (s *spyResolver) SimForCustomer(_ context.Context, _, id string) (clock.Sim, error) {
	return s.answer("customer", id)
}
func (s *spyResolver) SimForSubscription(_ context.Context, _, id string) (clock.Sim, error) {
	return s.answer("subscription", id)
}
func (s *spyResolver) SimForInvoice(_ context.Context, _, id string) (clock.Sim, error) {
	return s.answer("invoice", id)
}

// doorServer is the server's clock in the door's tests: a fixed time,
// counting the reads it answers itself (a bound instant is not a read).
type doorServer struct{ reads int }

var doorServerAt = time.Date(2026, 8, 9, 10, 11, 12, 0, time.UTC)

func (c *doorServer) Now(ctx context.Context) time.Time {
	if at, ok := clock.EffectiveNow(ctx); ok {
		return at
	}
	c.reads++
	return doorServerAt
}

// doorRig is the door on the built router, with fake record lookups and a
// counting server clock (serverOf).
func doorRig(t *testing.T, res *spyResolver) *clockDoor {
	t.Helper()
	return &clockDoor{
		root:              newTestServer().router,
		resolver:          res,
		clock:             &doorServer{},
		creditNoteInvoice: func(context.Context, string, string) (string, error) { return "inv_of_note", nil },
		dunningRunInvoice: func(context.Context, string, string) (string, error) { return "inv_of_run", nil },
	}
}

// serverOf is the rig's server clock.
func serverOf(d *clockDoor) *doorServer { return d.clock.(*doorServer) }

// seen is what a request through the door came to: the decision the handler
// saw on its ctx (decided false when it saw none), whether the handler was
// reached, and the answer.
type seen struct {
	decision clock.Sim
	decided  bool
	reached  bool
	code     int
	body     string
	header   http.Header
}

// through runs a request through the door to a handler that records what it
// saw.
func through(d *clockDoor, ctx context.Context, method, path string) seen {
	var got seen
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.reached = true
		got.decision, got.decided = clock.Decision(r.Context())
		w.WriteHeader(http.StatusNoContent)
	})
	rec := httptest.NewRecorder()
	d.Middleware(next).ServeHTTP(rec, httptest.NewRequest(method, path, nil).WithContext(ctx))
	got.code, got.body, got.header = rec.Code, rec.Body.String(), rec.Header()
	return got
}

// The door decides each request's time from the route's entry. Test mode:
// the clock of the record the path names (directly, or through a credit
// note's or a dunning run's invoice) and its frozen time, with no read of
// the server's clock; a record on no clock, and a route that names no
// record, one read of the server's clock. A decidedIn route and a route the
// router does not have: real time with no instant, and no read. Live: no
// record is read; a real-time or a record route gets one read of the
// server's clock, and a decidedIn route or an unknown one nothing.
func TestClockDoor_DecidesFromTheRouteTable(t *testing.T) {
	test := auth.WithTenantID(postgres.WithLivemode(context.Background(), false), "t1")
	onClock := clock.Sim{At: doorFrozen, TestClockID: "clk_1"}
	realInstant := clock.Sim{At: doorServerAt}
	for _, c := range []struct {
		name, method, path string
		want               clock.Sim // the zero Sim is real time with no instant
		asked              string
		serverReads        int
		noClock            bool
	}{
		{"a customer's route", "GET", "/v1/customers/cus_1", onClock, "customer:cus_1", 0, false},
		{"a customer on no clock", "GET", "/v1/customers/cus_1", realInstant, "customer:cus_1", 1, true},
		{"a customer's route by customer_id", "GET", "/v1/credits/balance/cus_2/live", onClock, "customer:cus_2", 0, false},
		{"a subscription's route", "POST", "/v1/subscriptions/sub_1/cancel", onClock, "subscription:sub_1", 0, false},
		{"an invoice's route", "GET", "/v1/invoices/inv_1/pdf", onClock, "invoice:inv_1", 0, false},
		{"a credit note's route, by its invoice", "GET", "/v1/credit-notes/cn_1", onClock, "invoice:inv_of_note", 0, false},
		{"a dunning run's route, by its invoice", "GET", "/v1/dunning/runs/run_1", onClock, "invoice:inv_of_run", 0, false},
		{"a route that names no record", "GET", "/v1/whoami", realInstant, "", 1, false},
		{"a route its function decides", "POST", "/v1/customers", clock.Sim{}, "", 0, false},
		// Usage's entries decide and bind themselves; usage's build refuses a
		// context with no instant, which the door must leave to fire.
		{"usage ingest", "POST", "/v1/usage-events", clock.Sim{}, "", 0, false},
		{"usage batch", "POST", "/v1/usage-events/batch", clock.Sim{}, "", 0, false},
		{"usage backfill", "POST", "/v1/usage-events/backfill", clock.Sim{}, "", 0, false},
		{"usage held release", "POST", "/v1/usage-events/held/release", clock.Sim{}, "", 0, false},
		{"a LiteLLM spend call", "POST", "/v1/integrations/litellm/spend", clock.Sim{}, "", 0, false},
		{"a route the router does not have", "GET", "/v1/nothing-here", clock.Sim{}, "", 0, false},
	} {
		t.Run("test mode, "+c.name, func(t *testing.T) {
			res := &spyResolver{noClock: c.noClock}
			d := doorRig(t, res)
			got := through(d, test, c.method, c.path)
			if !got.reached || !got.decided || got.decision != c.want || serverOf(d).reads != c.serverReads {
				t.Errorf("reached %v, decided %v, %+v after %d server reads; want decided %+v after %d",
					got.reached, got.decided, got.decision, serverOf(d).reads, c.want, c.serverReads)
			}
			if asked := strings.Join(res.asked, ","); asked != c.asked {
				t.Errorf("asked %q, want %q", asked, c.asked)
			}
		})
	}

	live := auth.WithTenantID(postgres.WithLivemode(context.Background(), true), "t1")
	for _, c := range []struct{ name, method, path string }{
		{"a customer's route", "GET", "/v1/customers/cus_1"},
		{"a subscription's route", "POST", "/v1/subscriptions/sub_1/cancel"},
		{"an invoice's route", "GET", "/v1/invoices/inv_1/pdf"},
		{"a credit note's route", "GET", "/v1/credit-notes/cn_1"},
		{"a dunning run's route", "GET", "/v1/dunning/runs/run_1"},
		{"a route that names no record", "GET", "/v1/whoami"},
	} {
		t.Run("live mode, "+c.name+": one read of the server's clock, nothing looked up", func(t *testing.T) {
			res := &spyResolver{}
			d := doorRig(t, res)
			looked := 0
			d.creditNoteInvoice = func(context.Context, string, string) (string, error) { looked++; return "inv_of_note", nil }
			d.dunningRunInvoice = func(context.Context, string, string) (string, error) { looked++; return "inv_of_run", nil }
			got := through(d, live, c.method, c.path)
			if !got.reached || got.decision != realInstant || serverOf(d).reads != 1 || len(res.asked) != 0 || looked != 0 {
				t.Errorf("reached %v, %+v after %d server reads, asked %v, %d record lookups; want %+v after one, nothing asked or looked up",
					got.reached, got.decision, serverOf(d).reads, res.asked, looked, realInstant)
			}
		})
	}
	for _, path := range []string{"/v1/customers", "/v1/usage-events", "/v1/integrations/litellm/spend", "/v1/nothing-here"} {
		t.Run("live mode, a decidedIn or an unknown route ("+path+"): nothing decided, nothing read", func(t *testing.T) {
			res := &spyResolver{}
			d := doorRig(t, res)
			got := through(d, live, "POST", path)
			if !got.reached || got.decided || len(res.asked) != 0 || serverOf(d).reads != 0 {
				t.Errorf("reached %v, decided %v, asked %v, %d server reads; want reached, nothing decided, asked or read",
					got.reached, got.decided, res.asked, serverOf(d).reads)
			}
		})
	}

	t.Run("no mode yet, a route before authentication (POST /v1/auth/login): one read of the server's clock", func(t *testing.T) {
		res := &spyResolver{}
		d := doorRig(t, res)
		got := through(d, context.Background(), "POST", "/v1/auth/login")
		if !got.reached || got.decision != realInstant || len(res.asked) != 0 || serverOf(d).reads != 1 {
			t.Errorf("reached %v, %+v, asked %v, %d server reads; want %+v after one, nothing asked",
				got.reached, got.decision, res.asked, serverOf(d).reads, realInstant)
		}
	})

	t.Run("no mode yet, a decidedIn route (a public page): real time with no instant, nothing read", func(t *testing.T) {
		res := &spyResolver{}
		d := doorRig(t, res)
		got := through(d, context.Background(), "GET", "/v1/public/invoices/tok_1")
		if !got.reached || !got.decided || got.decision != (clock.Sim{}) || len(res.asked) != 0 || serverOf(d).reads != 0 {
			t.Errorf("reached %v, decided %v, %+v, asked %v, %d server reads; want real time, nothing read",
				got.reached, got.decided, got.decision, res.asked, serverOf(d).reads)
		}
	})

	// chi routes on the escaped path when there is one: a parameter holding
	// an escaped slash finds the same route, and the same parameter, as the
	// handler gets. Matched on the unescaped path, it would find no route and
	// decide real time with no lookup.
	t.Run("test mode, an escaped slash in a path parameter: the route chi routes on", func(t *testing.T) {
		res := &spyResolver{}
		got := through(doorRig(t, res), test, "GET", "/v1/customers/cus%2F1")
		if !got.reached || got.decision.TestClockID != "clk_1" || strings.Join(res.asked, ",") != "customer:cus%2F1" {
			t.Errorf("decided %+v, asked %v; want the clock of customer cus%%2F1", got.decision, res.asked)
		}
	})

	// An empty id, or one Postgres cannot read, would fail its lookup with an
	// error that is not a not-found: a 503 a client retries for ever. The
	// door decides real time without a lookup, and the handler answers it as
	// it would without the door.
	for _, c := range []struct{ name, method, path string }{
		{"an empty id in the middle of the path", "POST", "/v1/subscriptions//cancel"},
		{"an id with a NUL", "GET", "/v1/customers/cus%001"},
		{"an id that is not UTF-8", "GET", "/v1/invoices/%FF/pdf"},
	} {
		t.Run("test mode, "+c.name+": the handler answers it, at one read of the server's clock, with no lookup", func(t *testing.T) {
			res := &spyResolver{err: errors.New("invalid byte sequence for encoding UTF8")}
			d := doorRig(t, res)
			got := through(d, test, c.method, c.path)
			if !got.reached || got.decision != (clock.Sim{At: doorServerAt}) || len(res.asked) != 0 || serverOf(d).reads != 1 {
				t.Errorf("reached %v, %+v, asked %v, %d server reads; want the handler reached at one read of the server's clock, nothing asked",
					got.reached, got.decision, res.asked, serverOf(d).reads)
			}
		})
	}

	for _, c := range []struct{ name, method, path string }{
		{"a customer", "GET", "/v1/customers/cus_gone"},
		{"a credit note", "GET", "/v1/credit-notes/cn_gone"},
		{"a dunning run", "GET", "/v1/dunning/runs/run_gone"},
	} {
		t.Run("test mode, "+c.name+" that does not exist: the handler answers it, at one read of the server's clock", func(t *testing.T) {
			d := doorRig(t, &spyResolver{err: fmt.Errorf("read: %w", errs.ErrNotFound)})
			gone := func(context.Context, string, string) (string, error) {
				return "", fmt.Errorf("read: %w", errs.ErrNotFound)
			}
			d.creditNoteInvoice, d.dunningRunInvoice = gone, gone
			got := through(d, test, c.method, c.path)
			if !got.reached || got.decision != (clock.Sim{At: doorServerAt}) || serverOf(d).reads != 1 {
				t.Errorf("reached %v, %+v after %d server reads; want the handler reached at one read of the server's clock", got.reached, got.decision, serverOf(d).reads)
			}
		})
	}

	// A clock that cannot be read is answered 503, retryable and never
	// stored by the idempotency layer, naming the record; the handler does
	// not run.
	for _, c := range []struct{ name, method, path, record string }{
		{"a customer", "GET", "/v1/customers/cus_1", "customer cus_1"},
		{"a credit note", "GET", "/v1/credit-notes/cn_1", "credit note cn_1"},
		{"a dunning run", "GET", "/v1/dunning/runs/run_1", "dunning run run_1"},
	} {
		t.Run("test mode, "+c.name+" whose clock cannot be read: 503, not served", func(t *testing.T) {
			d := doorRig(t, &spyResolver{err: errors.New("db down")})
			got := through(d, test, c.method, c.path)
			if serverOf(d).reads != 0 {
				t.Errorf("%d server reads; a refused request reads none", serverOf(d).reads)
			}
			if got.reached || got.code != http.StatusServiceUnavailable || got.header.Get("Retry-After") == "" {
				t.Fatalf("reached %v, code %d, Retry-After %q; want not reached, 503 with Retry-After", got.reached, got.code, got.header.Get("Retry-After"))
			}
			want := "The test clock of " + c.record + " could not be read. Nothing was done; send the request again."
			if !strings.Contains(got.body, want) || !strings.Contains(got.body, "test_clock_unavailable") || strings.Contains(got.body, "db down") {
				t.Errorf("body %s; want %q, code test_clock_unavailable, and not the cause", got.body, want)
			}
		})
	}
}

// Every route passes through the door, so no handler runs at a time nobody
// decided.
func TestClockDoor_EveryRoutePassesThroughIt(t *testing.T) {
	srv := newTestServer()
	var missing []string
	checked := 0
	if err := chi.Walk(srv.router, func(method, route string, _ http.Handler, mws ...func(http.Handler) http.Handler) error {
		checked++
		if chainIndex(mws, doorMW) < 0 {
			missing = append(missing, method+" "+canonicalRoute(route))
		}
		return nil
	}); err != nil {
		t.Fatalf("walk: %v", err)
	}
	if len(missing) > 0 {
		t.Errorf("%d of %d routes do not pass through the clock door:\n  %s", len(missing), checked, strings.Join(missing, "\n  "))
	}
}

// The door is the last of its group's middleware: after authentication, so
// it reads the named record as the caller's tenant and mode; after the
// timeout, so its lookups are bounded; and after Idempotency, so a replay
// costs no lookup and the door's 503 is not stored. On the /v1 group all
// four are there, in that order.
func TestClockDoor_ComesAfterAuthenticationIdempotencyAndTheTimeout(t *testing.T) {
	var wrong []string
	v1 := false
	if err := chi.Walk(newTestServer().router, func(method, route string, _ http.Handler, mws ...func(http.Handler) http.Handler) error {
		door := chainIndex(mws, doorMW)
		for _, before := range []chainStep{authMW, timeoutMW, idempotencyMW} {
			if i := chainIndex(mws, before); i >= 0 && i > door {
				wrong = append(wrong, method+" "+route+": the door comes before "+before.name)
			}
		}
		if method == "POST" && canonicalRoute(route) == "/v1/subscriptions/{id}/cancel" {
			v1 = true
			a, i, to := chainIndex(mws, authMW), chainIndex(mws, idempotencyMW), chainIndex(mws, timeoutMW)
			if a < 0 || i < 0 || to < 0 || door < 0 || a >= i || i >= to || to >= door {
				wrong = append(wrong, fmt.Sprintf("%s %s: authentication %d, Idempotency %d, timeout %d, door %d; want them in that order", method, route, a, i, to, door))
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("walk: %v", err)
	}
	if !v1 {
		t.Fatal("POST /v1/subscriptions/{id}/cancel not found: the /v1 group was not checked")
	}
	if len(wrong) > 0 {
		t.Errorf("the door is out of place:\n  %s", strings.Join(wrong, "\n  "))
	}
}

// Every route whose entry reads a record is authenticated, so the door reads
// it as the caller's tenant and mode. A route with no authentication and an
// entry that reads a record would read with no tenant and no mode.
func TestClockDoor_EveryRouteThatReadsARecordIsAuthenticated(t *testing.T) {
	var open []string
	reading := 0
	if err := chi.Walk(newTestServer().router, func(method, route string, _ http.Handler, mws ...func(http.Handler) http.Handler) error {
		switch clockRoutes[routeKey{Method: method, Pattern: canonicalRoute(route)}].Source {
		case clockOfCustomer, clockOfSubscription, clockOfInvoice, clockOfCreditNote, clockOfDunningRun:
			reading++
			if chainIndex(mws, authMW) < 0 {
				open = append(open, method+" "+canonicalRoute(route))
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("walk: %v", err)
	}
	if reading == 0 {
		t.Fatal("no route reads a record: the walk found none of the table's entries")
	}
	if len(open) > 0 {
		t.Errorf("%d routes read a record with no authentication:\n  %s", len(open), strings.Join(open, "\n  "))
	}
}

// Every entry that reads a record names a parameter its route's pattern has.
// One that did not would read an empty id, which the door decides as real
// time: a record on a clock would silently run at real time.
func TestClockDoor_EveryRecordEntryNamesAParameterOfItsRoute(t *testing.T) {
	var wrong []string
	reading := 0
	if err := chi.Walk(newTestServer().router, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		pattern := canonicalRoute(route)
		entry := clockRoutes[routeKey{Method: method, Pattern: pattern}]
		switch entry.Source {
		case clockOfCustomer, clockOfSubscription, clockOfInvoice, clockOfCreditNote, clockOfDunningRun:
			reading++
			if !strings.Contains(pattern, "{"+entry.Param+"}") {
				wrong = append(wrong, method+" "+pattern+": no {"+entry.Param+"}")
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("walk: %v", err)
	}
	if reading == 0 {
		t.Fatal("no route reads a record: the walk found none of the table's entries")
	}
	if len(wrong) > 0 {
		t.Errorf("%d entries name a parameter their route does not have:\n  %s", len(wrong), strings.Join(wrong, "\n  "))
	}
}

// A route table entry whose source the door does not know is a fault in the
// table or the door: pinFor says so, and the door answers 500, never the 503
// that says a clock could not be read, and binds nothing.
func TestClockDoor_AnUnknownClockSourceIsAServerFault(t *testing.T) {
	d := doorRig(t, &spyResolver{})
	if _, _, err := d.pinFor(context.Background(), clockDecl{Source: clockSource(99)}, chi.NewRouteContext()); !errors.Is(err, errUnknownClockSource) {
		t.Fatalf("pinFor: %v; want errUnknownClockSource", err)
	}
	key := routeKey{Method: "GET", Pattern: "/v1/whoami"}
	saved := clockRoutes[key]
	clockRoutes[key] = clockDecl{Source: clockSource(99)}
	t.Cleanup(func() { clockRoutes[key] = saved })
	got := through(d, auth.WithTenantID(postgres.WithLivemode(context.Background(), false), "t1"), "GET", "/v1/whoami")
	if got.reached || got.code != http.StatusInternalServerError || strings.Contains(got.body, "test_clock_unavailable") || serverOf(d).reads != 0 {
		t.Errorf("reached %v, code %d, body %s, %d server reads; want not reached, 500, no clock refusal, no read",
			got.reached, got.code, got.body, serverOf(d).reads)
	}
}
