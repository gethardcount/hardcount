package subscription

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/auth"
	"github.com/gethardcount/hardcount/internal/errs"
	"github.com/gethardcount/hardcount/internal/platform/clock"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
)

// customerAnswer answers every customer with sim, or fails with err, and
// counts the reads.
type customerAnswer struct {
	sim   clock.Sim
	err   error
	reads int
}

func (c *customerAnswer) SimForCustomer(context.Context, string, string) (clock.Sim, error) {
	c.reads++
	return c.sim, c.err
}
func (c *customerAnswer) SimForSubscription(context.Context, string, string) (clock.Sim, error) {
	return clock.Sim{}, errors.New("not used")
}
func (c *customerAnswer) SimForInvoice(context.Context, string, string) (clock.Sim, error) {
	return clock.Sim{}, errors.New("not used")
}

// A new subscription's time is decided from the customer its body names:
// the customer's clock; a not-found is real time; a clock that cannot be
// read refuses the create; live mode, and a body naming no customer, read
// nothing.
func TestDecideForCustomer(t *testing.T) {
	frozen := time.Date(2025, 1, 10, 12, 0, 0, 0, time.UTC)
	testMode := postgres.WithLivemode(context.Background(), false)
	for _, c := range []struct {
		name       string
		ctx        context.Context
		customerID string
		answer     customerAnswer
		wantReads  int
		wantClock  string
		wantReal   bool
		wantErr    bool
	}{
		{"the customer's clock", testMode, "cus_1", customerAnswer{sim: clock.Sim{At: frozen, TestClockID: "clk_cus"}}, 1, "clk_cus", false, false},
		{"a not-found is real time", testMode, "cus_1", customerAnswer{err: fmt.Errorf("read test clock clk_gone: %w", errs.ErrNotFound)}, 1, "", true, false},
		{"an unreadable clock refuses", testMode, "cus_1", customerAnswer{err: errors.New("connection reset by peer")}, 1, "", false, true},
		{"live mode reads nothing", postgres.WithLivemode(context.Background(), true), "cus_1", customerAnswer{err: errors.New("must not be read")}, 0, "", false, false},
		{"no customer id reads nothing", testMode, "", customerAnswer{err: errors.New("must not be read")}, 0, "", false, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			svc := NewService(newMemStore(), nil)
			res := c.answer
			svc.SetResolver(&res)
			ctx, err := svc.DecideForCustomer(c.ctx, "t1", c.customerID)
			if c.wantErr != (err != nil) || (c.wantErr && (!errors.Is(err, errs.ErrUnavailable) || !errors.Is(err, clock.ErrUndecided))) {
				t.Fatalf("err %v; want error %v (unavailable over clock.ErrUndecided)", err, c.wantErr)
			}
			if res.reads != c.wantReads {
				t.Errorf("%d reads; want %d", res.reads, c.wantReads)
			}
			if sim, _ := clock.SimOf(ctx); sim.TestClockID != c.wantClock {
				t.Errorf("clock %q; want %q", sim.TestClockID, c.wantClock)
			}
			if d, ok := clock.Decision(ctx); c.wantReal && (!ok || !d.At.IsZero() || d.TestClockID != "") {
				t.Errorf("decision %+v (%v); want real time", d, ok)
			}
		})
	}
}

// POST /v1/subscriptions decides from the body's customer before the service
// runs: a clock that cannot be read answers 503, and nothing is created.
func TestCreateRoute_DecidesBeforeTheServiceRuns(t *testing.T) {
	store := newMemStore()
	svc := NewService(store, nil)
	svc.SetResolver(&customerAnswer{err: errors.New("connection reset by peer")})
	body := `{"code":"sub-time","display_name":"Time","customer_id":"cus_1","items":[{"plan_id":"pln_1"}]}`
	req := httptest.NewRequest("POST", "/", strings.NewReader(body))
	req = req.WithContext(auth.WithTenantID(postgres.WithLivemode(req.Context(), false), "t1"))
	rec := httptest.NewRecorder()
	NewHandler(svc).create(rec, req)
	if rec.Code != 503 || !strings.Contains(rec.Body.String(), "test_clock_unavailable") {
		t.Errorf("%d %s; want 503 test_clock_unavailable", rec.Code, rec.Body.String())
	}
	if len(store.subs) != 0 {
		t.Errorf("%d subscriptions created; want none", len(store.subs))
	}
}

// countingClocks answers the customer as on clock clk_1 and counts the reads
// of each kind.
type countingClocks struct{ customerReads, subscriptionReads int }

func (c *countingClocks) SimForCustomer(context.Context, string, string) (clock.Sim, error) {
	c.customerReads++
	return clock.Sim{At: time.Date(2025, 1, 10, 12, 0, 0, 0, time.UTC), TestClockID: "clk_1"}, nil
}
func (c *countingClocks) SimForSubscription(context.Context, string, string) (clock.Sim, error) {
	c.subscriptionReads++
	return clock.Sim{At: time.Date(2025, 1, 10, 12, 0, 0, 0, time.UTC), TestClockID: "clk_1"}, nil
}
func (c *countingClocks) SimForInvoice(context.Context, string, string) (clock.Sim, error) {
	return clock.Sim{}, errors.New("not used")
}

// One decision and one read per create: the create and its audit row run on
// the handler's decision, so the audit row carries the customer's clock and
// no second lookup is made.
func TestCreateRoute_OneDecisionForTheCreateAndItsAuditRow(t *testing.T) {
	clocks := &countingClocks{}
	svc := NewService(newMemStore(), nil)
	svc.SetResolver(clocks)
	h := NewHandler(svc)
	h.SetResolver(clocks)
	audits := &capturingAudit{}
	h.SetAuditLogger(audits)
	body := `{"code":"sub-time","display_name":"Time","customer_id":"cus_1","items":[{"plan_id":"pln_1"}]}`
	req := httptest.NewRequest("POST", "/", strings.NewReader(body))
	req = req.WithContext(auth.WithTenantID(postgres.WithLivemode(req.Context(), false), "t1"))
	rec := httptest.NewRecorder()
	h.create(rec, req)
	if rec.Code != 201 {
		t.Fatalf("%d %s; want 201", rec.Code, rec.Body.String())
	}
	var created struct {
		StartedAt time.Time `json:"started_at"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil || !created.StartedAt.Equal(time.Date(2025, 1, 10, 12, 0, 0, 0, time.UTC)) {
		t.Errorf("started_at %v (%v); want the clock's time", created.StartedAt, err)
	}
	if len(audits.entries) != 1 || audits.entries[0].sim.TestClockID != "clk_1" {
		t.Errorf("audit rows %+v; want one, on clk_1", audits.entries)
	}
	if clocks.customerReads != 1 || clocks.subscriptionReads != 0 {
		t.Errorf("reads: customer %d, subscription %d; want 1 and 0", clocks.customerReads, clocks.subscriptionReads)
	}
}
