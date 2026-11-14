package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/gethardcount/hardcount/internal/auth"
	"github.com/gethardcount/hardcount/internal/errs"
	"github.com/gethardcount/hardcount/internal/platform/clock"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
)

// spyResolver answers every record on clock clk_1 at doorFrozen, or err, and
// records what it was asked.
type spyResolver struct {
	asked []string
	err   error
}

var doorFrozen = time.Date(2025, 3, 1, 12, 0, 0, 0, time.UTC)

func (s *spyResolver) answer(kind, id string) (clock.Sim, error) {
	s.asked = append(s.asked, kind+":"+id)
	if s.err != nil {
		return clock.Sim{}, s.err
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

// doorRig is the door on the built router, with fake record lookups.
func doorRig(t *testing.T, res *spyResolver) *clockDoor {
	t.Helper()
	return &clockDoor{
		root:              newTestServer().router,
		resolver:          res,
		creditNoteInvoice: func(context.Context, string, string) (string, error) { return "inv_of_note", nil },
		dunningRunInvoice: func(context.Context, string, string) (string, error) { return "inv_of_run", nil },
	}
}

// through runs a request through the door and returns what the handler saw:
// the decision on its ctx, or that it was not reached, and the answer.
func through(d *clockDoor, ctx context.Context, method, path string) (clock.Sim, bool, bool, int) {
	var sim clock.Sim
	var decided, reached bool
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
		sim, decided = clock.Decision(r.Context())
		w.WriteHeader(http.StatusNoContent)
	})
	rec := httptest.NewRecorder()
	d.Middleware(next).ServeHTTP(rec, httptest.NewRequest(method, path, nil).WithContext(ctx))
	return sim, decided, reached, rec.Code
}

// In test mode the door decides each request's time from the route's
// declaration: the clock of the record the path names (directly, or through
// a credit note's or a dunning run's invoice), else real time. In live mode
// it does nothing: no decision, no lookup.
func TestClockDoor_DecidesFromTheRouteTable(t *testing.T) {
	test := auth.WithTenantID(postgres.WithLivemode(context.Background(), false), "t1")
	onClock := clock.Sim{At: doorFrozen, TestClockID: "clk_1"}
	for _, c := range []struct {
		name, method, path string
		want               clock.Sim // the zero Sim is real time
		asked              string
	}{
		{"a customer's route", "GET", "/v1/customers/cus_1", onClock, "customer:cus_1"},
		{"a customer's route by customer_id", "GET", "/v1/credits/balance/cus_2/live", onClock, "customer:cus_2"},
		{"a subscription's route", "POST", "/v1/subscriptions/sub_1/cancel", onClock, "subscription:sub_1"},
		{"an invoice's route", "GET", "/v1/invoices/inv_1/pdf", onClock, "invoice:inv_1"},
		{"a credit note's route, by its invoice", "GET", "/v1/credit-notes/cn_1", onClock, "invoice:inv_of_note"},
		{"a dunning run's route, by its invoice", "POST", "/v1/dunning/runs/run_1/resolve", onClock, "invoice:inv_of_run"},
		{"a route that names no record", "GET", "/v1/whoami", clock.Sim{}, ""},
		{"a route its function decides", "POST", "/v1/customers", clock.Sim{}, ""},
		{"a route the router does not have", "GET", "/v1/nothing-here", clock.Sim{}, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			res := &spyResolver{}
			sim, decided, reached, _ := through(doorRig(t, res), test, c.method, c.path)
			if !reached || !decided || sim != c.want {
				t.Errorf("reached %v, decided %v, %+v; want decided %+v", reached, decided, sim, c.want)
			}
			if got := strings.Join(res.asked, ","); got != c.asked {
				t.Errorf("asked %q, want %q", got, c.asked)
			}
		})
	}

	t.Run("live mode: nothing decided, nothing read", func(t *testing.T) {
		res := &spyResolver{}
		live := auth.WithTenantID(postgres.WithLivemode(context.Background(), true), "t1")
		_, decided, reached, _ := through(doorRig(t, res), live, "GET", "/v1/customers/cus_1")
		if !reached || decided || len(res.asked) != 0 {
			t.Errorf("reached %v, decided %v, asked %v; want reached, nothing decided or asked", reached, decided, res.asked)
		}
	})

	t.Run("no mode yet (a public page): real time", func(t *testing.T) {
		res := &spyResolver{}
		sim, decided, reached, _ := through(doorRig(t, res), context.Background(), "GET", "/v1/public/invoices/tok_1")
		if !reached || !decided || sim != (clock.Sim{}) || len(res.asked) != 0 {
			t.Errorf("reached %v, decided %v, %+v, asked %v; want real time", reached, decided, sim, res.asked)
		}
	})

	t.Run("a record that does not exist: the handler answers it, at real time", func(t *testing.T) {
		res := &spyResolver{err: errs.ErrNotFound}
		sim, decided, reached, _ := through(doorRig(t, res), test, "GET", "/v1/customers/cus_gone")
		if !reached || !decided || sim != (clock.Sim{}) {
			t.Errorf("reached %v, decided %v, %+v; want the handler reached at real time", reached, decided, sim)
		}
	})

	t.Run("a record whose clock cannot be read: answered, not served", func(t *testing.T) {
		res := &spyResolver{err: errors.New("db down")}
		_, _, reached, code := through(doorRig(t, res), test, "GET", "/v1/customers/cus_1")
		if reached || code < 500 {
			t.Errorf("reached %v, code %d; want not reached, a 5xx", reached, code)
		}
	})
}

// isClockDoor reports whether m is the door's Middleware method value.
func isClockDoor(m func(http.Handler) http.Handler) bool {
	fn := runtime.FuncForPC(reflect.ValueOf(m).Pointer())
	return fn != nil && strings.Contains(fn.Name(), "clockDoor).Middleware")
}

// Every route passes through the door, so no handler runs at a time nobody
// decided.
func TestClockDoor_EveryRoutePassesThroughIt(t *testing.T) {
	srv := newTestServer()
	var missing []string
	checked := 0
	if err := chi.Walk(srv.router, func(method, route string, _ http.Handler, mws ...func(http.Handler) http.Handler) error {
		checked++
		for _, m := range mws {
			if isClockDoor(m) {
				return nil
			}
		}
		missing = append(missing, method+" "+canonicalRoute(route))
		return nil
	}); err != nil {
		t.Fatalf("walk: %v", err)
	}
	if len(missing) > 0 {
		t.Errorf("%d of %d routes do not pass through the clock door:\n  %s", len(missing), checked, strings.Join(missing, "\n  "))
	}
}
