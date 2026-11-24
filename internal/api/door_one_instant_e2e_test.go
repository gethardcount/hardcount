package api

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/platform/clock"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/testutil"
)

// runClock is the server's clock in these tests. While the fixtures are
// built it stands still at lagP0, and each fixture request carries lagP0
// bound on its context, which the request keeps, so every fixture write is
// at lagP0. Once stepping, the clock moves a second on every read it answers itself
// and counts those reads, so a request that reads it twice stamps two
// instants.
type runClock struct {
	mu    sync.Mutex
	at    time.Time
	step  bool
	reads int
}

func (c *runClock) Now(ctx context.Context) time.Time {
	if at, ok := clock.EffectiveNow(ctx); ok {
		return at
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.step {
		c.reads++
		c.at = c.at.Add(time.Second)
	}
	return c.at
}

// stepFrom starts stepping from at, with no reads counted yet.
func (c *runClock) stepFrom(at time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.at, c.step, c.reads = at, true, 0
}

func (c *runClock) count() (time.Time, int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at, c.reads
}

// instantRig is a server on a runClock, in one mode, with an in-arrears
// monthly plan whose subscriptions start on lagP0 and end their first period
// on lagB (both from close_lag_e2e_test.go: April 1 and May 1, 2026). No
// invoice grace: a period is due once it has ended.
type instantRig struct {
	t        *testing.T
	db       *postgres.DB
	clk      *runClock
	srv      *Server
	auth     string
	tenantID string
	live     bool
	planID   string
}

func newInstantRig(t *testing.T, live bool) *instantRig {
	t.Helper()
	db := testutil.SetupTestDB(t)
	clk := &runClock{at: lagP0}
	r := &instantRig{t: t, db: db, clk: clk, srv: NewServer(db, clk, nil, WithInvoiceGrace(0)), live: live}
	r.tenantID = testutil.CreateTestTenant(t, db, "door one instant")
	r.auth = "Bearer " + createAPIKeyWithMode(t, db, r.tenantID, live)
	plan := r.must(http.MethodPost, "/v1/plans", lagP0, map[string]any{
		"code": "one-instant", "name": "One instant", "currency": "USD", "billing_interval": "monthly",
		"base_amount_cents": 3000, "base_bill_timing": "in_arrears", "prices": []map[string]any{},
	}, http.StatusCreated)
	r.planID = plan["id"].(string)
	return r
}

// subscribe creates a customer and its subscription, starting on lagP0.
func (r *instantRig) subscribe(name string) string {
	r.t.Helper()
	cust := r.must(http.MethodPost, "/v1/customers", lagP0, map[string]any{
		"external_id": "cus_" + name, "display_name": name, "email": name + "@example.test",
	}, http.StatusCreated)
	sub := r.must(http.MethodPost, "/v1/subscriptions", lagP0, map[string]any{
		"code": "sub-" + name, "display_name": name, "customer_id": cust["id"],
		"items": []map[string]any{{"plan_id": r.planID, "quantity": 1}},
	}, http.StatusCreated)
	return sub["id"].(string)
}

// do serves one request. A non-zero at binds that instant on the request, as
// the fixtures do; the zero time leaves the time to the door.
func (r *instantRig) do(method, path string, at time.Time, body any) (int, []byte) {
	r.t.Helper()
	ctx := context.Background()
	if !at.IsZero() {
		ctx = clock.WithEffectiveNow(ctx, at)
	}
	return serveJSON(r.srv, r.auth, method, path, ctx, body)
}

func (r *instantRig) must(method, path string, at time.Time, body any, want int) map[string]any {
	r.t.Helper()
	code, raw := r.do(method, path, at, body)
	if code != want {
		r.t.Fatalf("%s %s: %d %s, want %d", method, path, code, raw, want)
	}
	out := map[string]any{}
	_ = json.Unmarshal(raw, &out)
	return out
}

// subsUpdated returns each subscription's updated_at, which the period close
// stamps from the context's clock.
func (r *instantRig) subsUpdated(ids ...string) []time.Time {
	r.t.Helper()
	tx, err := r.db.BeginTx(postgres.WithLivemode(context.Background(), r.live), postgres.TxTenant, r.tenantID)
	if err != nil {
		r.t.Fatalf("begin: %v", err)
	}
	defer postgres.Rollback(tx)
	out := make([]time.Time, len(ids))
	for i, id := range ids {
		if err := tx.QueryRow(`SELECT updated_at FROM subscriptions WHERE id = $1`, id).Scan(&out[i]); err != nil {
			r.t.Fatalf("read subscription %s: %v", id, err)
		}
		out[i] = out[i].UTC()
	}
	return out
}

// cycleInvoices returns the created_at and issued_at of the tenant's
// cycle invoices, one pair per invoice.
func (r *instantRig) cycleInvoices() [][2]*time.Time {
	r.t.Helper()
	tx, err := r.db.BeginTx(postgres.WithLivemode(context.Background(), r.live), postgres.TxTenant, r.tenantID)
	if err != nil {
		r.t.Fatalf("begin: %v", err)
	}
	defer postgres.Rollback(tx)
	rows, err := tx.Query(`SELECT created_at, issued_at FROM invoices WHERE billing_reason = 'subscription_cycle' ORDER BY id`)
	if err != nil {
		r.t.Fatalf("read invoices: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var out [][2]*time.Time
	for rows.Next() {
		var created time.Time
		var issued *time.Time
		if err := rows.Scan(&created, &issued); err != nil {
			r.t.Fatalf("scan: %v", err)
		}
		c := created.UTC()
		out = append(out, [2]*time.Time{&c, issued})
	}
	return out
}

// The manual billing run is a job that a request starts, so it runs as the
// scheduled run does: its handler drops the door's instant, and each
// subscription is billed on its own instant, none of them the door's. Four
// reads of the server's clock: the door's (a real-time route), the run's due
// cutoff, then one per subscription. The invoices are dated at their
// period's boundary. In both modes.
func TestBillingRun_EachSubscriptionIsBilledOnItsOwnTime(t *testing.T) {
	for _, live := range []bool{false, true} {
		t.Run(map[bool]string{false: "test mode", true: "live"}[live], func(t *testing.T) {
			r := newInstantRig(t, live)
			a := r.subscribe("run_a")
			b := r.subscribe("run_b")
			start := lagB.Add(time.Hour)
			r.clk.stepFrom(start)
			r.must(http.MethodPost, "/v1/billing/run", time.Time{}, nil, http.StatusOK)
			_, reads := r.clk.count()
			up := r.subsUpdated(a, b)
			rows := map[time.Time]bool{start.Add(3 * time.Second): true, start.Add(4 * time.Second): true}
			if reads != 4 || up[0].Equal(up[1]) || !rows[up[0]] || !rows[up[1]] {
				t.Errorf("%d server reads; closed periods updated %v and %v; want 4 reads (door, cutoff, one per row), each row at its own read (%v)",
					reads, up[0], up[1], rows)
			}
			for i, inv := range r.cycleInvoices() {
				if !inv[0].Equal(lagB) || inv[1] == nil || !inv[1].UTC().Equal(lagB) {
					t.Errorf("invoice %d created %v, issued %v; want both at the period's boundary %v", i, inv[0], inv[1], lagB)
				}
			}
		})
	}
}

// A subscription write closes an ended period first, then plans from the new
// one. On a subscription's route the door binds one instant, and the close
// and the schedule both read it: one read of the server's clock, in both
// modes. The ended period's invoice is dated at its boundary.
func TestScheduleCancel_TheCloseAndTheScheduleShareOneInstant(t *testing.T) {
	for _, live := range []bool{false, true} {
		t.Run(map[bool]string{false: "test mode", true: "live"}[live], func(t *testing.T) {
			r := newInstantRig(t, live)
			subID := r.subscribe("sched")
			r.clk.stepFrom(lagB.Add(time.Hour))
			r.must(http.MethodPost, "/v1/subscriptions/"+subID+"/schedule-cancel", time.Time{}, map[string]any{"at_period_end": true}, http.StatusOK)
			_, reads := r.clk.count()
			invs := r.cycleInvoices()
			if reads != 1 || len(invs) != 1 || !invs[0][0].Equal(lagB) {
				t.Errorf("%d server reads, closing invoices %v; want one read and the ended period's invoice at its boundary %v", reads, invs, lagB)
			}
		})
	}
}

// Finalize collects right after it finalizes. On an invoice's route the door
// binds one instant, and the finalize and the settle both read it, so an
// invoice that credits cover is issued and paid at that one time: one read
// of the server's clock, in both modes.
func TestFinalize_ACreditCoveredInvoiceIsIssuedAndPaidAtOneInstant(t *testing.T) {
	for _, live := range []bool{false, true} {
		t.Run(map[bool]string{false: "test mode", true: "live"}[live], func(t *testing.T) {
			r := newInstantRig(t, live)
			cust := r.must(http.MethodPost, "/v1/customers", lagP0, map[string]any{
				"external_id": "cus_fin", "display_name": "Fin", "email": "fin@example.test",
			}, http.StatusCreated)
			r.must(http.MethodPost, "/v1/credits/grant", lagP0, map[string]any{
				"customer_id": cust["id"], "currency": "USD", "amount_cents": 3000, "description": "covers the invoice",
			}, http.StatusCreated)
			inv := r.must(http.MethodPost, "/v1/invoices", lagP0, map[string]any{
				"customer_id": cust["id"], "currency": "USD",
				"line_items": []map[string]any{{"description": "one-off", "quantity": 1, "unit_amount_cents": 3000}},
			}, http.StatusCreated)
			id := inv["id"].(string)
			start := lagP0.Add(time.Hour)
			r.clk.stepFrom(start)
			r.must(http.MethodPost, "/v1/invoices/"+id+"/finalize", time.Time{}, nil, http.StatusOK)
			_, reads := r.clk.count()
			want := start.Add(time.Second) // the door's read
			got := r.must(http.MethodGet, "/v1/invoices/"+id, lagP0, nil, http.StatusOK)
			issued, _ := time.Parse(time.RFC3339Nano, flowStr(got, "invoice", "issued_at"))
			paid, _ := time.Parse(time.RFC3339Nano, flowStr(got, "invoice", "paid_at"))
			if reads != 1 || flowStr(got, "invoice", "status") != "paid" || !issued.Equal(want) || !paid.Equal(want) {
				t.Errorf("%d server reads; status %v, issued %v, paid %v; want one read, paid, both at %v",
					reads, flowStr(got, "invoice", "status"), issued, paid, want)
			}
		})
	}
}
