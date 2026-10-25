package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/sagarsuperuser/velox/internal/platform/clock"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

// Usage prices belong to the plan (ADR-130). These tests drive the real
// server over HTTP against real Postgres, as an integrator would, and check
// the invoices to the cent.
//
// Time. A price version on a used plan takes effect at the database's wall
// clock when it is saved (clock_timestamp()), so the rig works in real time:
// a subscription starts now, its first period P0 started at midnight UTC
// today, and a price written during the test is written inside P0. The
// period closes are run later in simulated wall time (the server's clock and
// the request's effective now), as the scheduler would run them at the end of
// each period. Test-clock subscriptions use their own clocks.

type ppRig struct {
	t        *testing.T
	db       *postgres.DB
	clk      *clock.Fake
	srv      *Server
	auth     string
	tenantID string
	seq      int
}

// newPPRig is a tenant with a test-mode key (test clocks need test mode).
func newPPRig(t *testing.T, name string) *ppRig {
	t.Helper()
	skipShort(t)
	db := testutil.SetupTestDB(t)
	clk := clock.NewFake(time.Now().UTC())
	r := &ppRig{t: t, db: db, clk: clk, srv: NewServer(db, clk, nil)}
	r.tenantID = testutil.CreateTestTenant(t, db, "plan prices "+name)
	r.auth = "Bearer " + createAPIKeyWithMode(t, db, r.tenantID, false)
	return r
}

// do serves one request at `at` (the zero time is real now).
func (r *ppRig) do(at time.Time, method, path string, body any) (int, map[string]any, []byte) {
	r.t.Helper()
	if at.IsZero() {
		at = time.Now().UTC()
	}
	r.clk.Set(at)
	var buf bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&buf).Encode(body)
	}
	req := httptest.NewRequest(method, path, &buf)
	req = req.WithContext(clock.WithEffectiveNow(context.Background(), at))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", r.auth)
	rec := httptest.NewRecorder()
	r.srv.ServeHTTP(rec, req)
	out := map[string]any{}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out, rec.Body.Bytes()
}

func (r *ppRig) must(at time.Time, method, path string, body any, want int) map[string]any {
	r.t.Helper()
	code, out, raw := r.do(at, method, path, body)
	if code != want {
		r.t.Fatalf("%s %s: %d %s, want %d", method, path, code, raw, want)
	}
	return out
}

// refused checks a write is refused with the status and error code, and
// returns the error message.
func (r *ppRig) refused(method, path string, body any, wantStatus int, wantCode string) string {
	r.t.Helper()
	code, out, raw := r.do(time.Time{}, method, path, body)
	if code != wantStatus || flowStr(out, "error", "code") != wantCode {
		r.t.Fatalf("%s %s: %d %s, want %d with code %s", method, path, code, raw, wantStatus, wantCode)
	}
	return flowStr(out, "error", "message")
}

func (r *ppRig) meter(eventName, aggregation string) string {
	r.t.Helper()
	return flowStr(r.must(time.Time{}, http.MethodPost, "/v1/meters", map[string]any{
		"event_name": eventName, "name": eventName, "unit": "units", "aggregation": aggregation,
	}, http.StatusCreated), "id")
}

// perUnit is a per_unit price body.
func perUnit(meterID, cents string, match map[string]string) map[string]any {
	m := map[string]any{}
	for k, v := range match {
		m[k] = v
	}
	return map[string]any{"meter_id": meterID, "dimension_match": m, "mode": "per_unit", "unit_amount_cents": cents}
}

// plan creates a monthly in_arrears plan with no base fee and prices, and
// returns its id and its prices' ids in order.
func (r *ppRig) plan(code string, prices ...map[string]any) (string, []string) {
	r.t.Helper()
	if prices == nil {
		prices = []map[string]any{}
	}
	out := r.must(time.Time{}, http.MethodPost, "/v1/plans", map[string]any{
		"code": code, "name": code, "currency": "USD", "billing_interval": "monthly", "prices": prices,
	}, http.StatusCreated)
	var ids []string
	for _, p := range flowList(out, "prices") {
		ids = append(ids, flowStr(p, "id"))
	}
	if len(ids) != len(prices) {
		r.t.Fatalf("plan %s: %d prices back, want %d: %v", code, len(ids), len(prices), out)
	}
	return flowStr(out, "id"), ids
}

func (r *ppRig) customer(ext, clockID string) string {
	r.t.Helper()
	body := map[string]any{"external_id": ext, "display_name": ext, "email": ext + "@example.test"}
	if clockID != "" {
		body["test_clock_id"] = clockID
	}
	return flowStr(r.must(time.Time{}, http.MethodPost, "/v1/customers", body, http.StatusCreated), "id")
}

// subscribe starts a subscription now (at, or real now) on planID, billed on
// the anniversary of its start. It returns the subscription id and its
// first period.
func (r *ppRig) subscribe(at time.Time, customerID, planID string) (string, time.Time, time.Time) {
	r.t.Helper()
	r.seq++
	out := r.must(at, http.MethodPost, "/v1/subscriptions", map[string]any{
		"code": fmt.Sprintf("pp-sub-%d", r.seq), "display_name": "PP", "customer_id": customerID, "billing_time": "anniversary",
		"items": []map[string]any{{"plan_id": planID, "quantity": 1}},
	}, http.StatusCreated)
	start := ppTime(r.t, flowStr(out, "current_billing_period_start"))
	end := ppTime(r.t, flowStr(out, "current_billing_period_end"))
	return flowStr(out, "id"), start, end
}

func ppTime(t *testing.T, s string) time.Time {
	t.Helper()
	v, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		t.Fatalf("time %q: %v", s, err)
	}
	return v.UTC()
}

// ingest sends one event at ts, served at the instant served (zero: ts).
func (r *ppRig) ingest(served time.Time, ext, event string, qty int64, ts time.Time, dims map[string]string) {
	r.t.Helper()
	r.seq++
	if served.IsZero() {
		served = ts.Add(time.Second)
	}
	body := map[string]any{
		"external_customer_id": ext, "event_name": event, "quantity": qty,
		"timestamp": ts.Format(time.RFC3339Nano), "idempotency_key": fmt.Sprintf("pp-%d", r.seq),
	}
	if len(dims) > 0 {
		d := map[string]any{}
		for k, v := range dims {
			d[k] = v
		}
		body["dimensions"] = d
	}
	r.must(served, http.MethodPost, "/v1/usage-events", body, http.StatusCreated)
}

// run is the billing run at `at`.
func (r *ppRig) run(at time.Time) {
	r.t.Helper()
	r.must(at, http.MethodPost, "/v1/billing/run", nil, http.StatusOK)
}

type ppLine struct {
	typ, priceID, desc string
	amount             int64
	qty                decimal.Decimal
	start, end         *time.Time
}

type ppInvoice struct {
	id, reason string
	start, end time.Time
	lines      []ppLine
}

func (i ppInvoice) usage() []ppLine {
	var out []ppLine
	for _, l := range i.lines {
		if l.typ == "usage" {
			out = append(out, l)
		}
	}
	return out
}

func (i ppInvoice) usageCents() int64 {
	var n int64
	for _, l := range i.usage() {
		n += l.amount
	}
	return n
}

// invoices reads the subscription's live invoices with their lines, oldest
// period first, outside tenant scope.
func (r *ppRig) invoices(subID string) []ppInvoice {
	r.t.Helper()
	ctx := context.Background()
	tx, err := r.db.BeginTx(ctx, postgres.TxBypass, "")
	if err != nil {
		r.t.Fatalf("begin: %v", err)
	}
	defer postgres.Rollback(tx)
	rows, err := tx.QueryContext(ctx, `SELECT id, billing_reason, billing_period_start, billing_period_end
		FROM invoices WHERE subscription_id = $1 AND status <> 'voided' ORDER BY billing_period_start, created_at`, subID)
	if err != nil {
		r.t.Fatalf("list invoices: %v", err)
	}
	var out []ppInvoice
	for rows.Next() {
		var i ppInvoice
		if err := rows.Scan(&i.id, &i.reason, &i.start, &i.end); err != nil {
			r.t.Fatalf("scan invoice: %v", err)
		}
		out = append(out, i)
	}
	_ = rows.Close()
	for k := range out {
		lr, err := tx.QueryContext(ctx, `SELECT line_type, COALESCE(price_id, ''), description, amount_cents, quantity_decimal,
			billing_period_start, billing_period_end FROM invoice_line_items WHERE invoice_id = $1 ORDER BY created_at, id`, out[k].id)
		if err != nil {
			r.t.Fatalf("list lines: %v", err)
		}
		for lr.Next() {
			var l ppLine
			if err := lr.Scan(&l.typ, &l.priceID, &l.desc, &l.amount, &l.qty, &l.start, &l.end); err != nil {
				r.t.Fatalf("scan line: %v", err)
			}
			out[k].lines = append(out[k].lines, l)
		}
		_ = lr.Close()
	}
	return out
}

// cycle returns the one cycle invoice whose period starts at start.
func (r *ppRig) cycle(subID string, start time.Time) ppInvoice {
	r.t.Helper()
	var found []ppInvoice
	for _, i := range r.invoices(subID) {
		if i.reason == "subscription_cycle" && i.start.Equal(start) {
			found = append(found, i)
		}
	}
	if len(found) != 1 {
		r.t.Fatalf("%d cycle invoices for the period starting %v, want 1: %+v", len(found), start, r.invoices(subID))
	}
	return found[0]
}

// wantUsage checks an invoice's usage lines: by price id, the cents billed.
func wantUsage(t *testing.T, what string, inv ppInvoice, want map[string]int64) {
	t.Helper()
	got := map[string]int64{}
	for _, l := range inv.usage() {
		got[l.priceID] += l.amount
	}
	if fmt.Sprint(sortedCents(got)) != fmt.Sprint(sortedCents(want)) {
		t.Errorf("%s: usage cents by price = %v, want %v (lines %+v)", what, got, want, inv.usage())
	}
}

func sortedCents(m map[string]int64) []string {
	out := make([]string, 0, len(m))
	for k, v := range m {
		out = append(out, fmt.Sprintf("%s=%d", k, v))
	}
	sort.Strings(out)
	return out
}

// midpoint is an instant halfway through [a, b).
func midpoint(a, b time.Time) time.Time { return a.Add(b.Sub(a) / 2) }

// recent is an instant between start and now, for usage in the open period.
func recent(start time.Time) time.Time {
	now := time.Now().UTC()
	if !now.After(start.Add(2 * time.Second)) {
		return start.Add(time.Second)
	}
	return midpoint(start, now)
}

// TestE2E_PlanPrices_TwoPlansPriceOneMeterDifferently is pricing shape 3 of
// the design (§4.17): Pro and Enterprise price the same token events at
// their own rates. Each customer's invoice bills its own plan's prices.
//
// Mutation: price the window with every plan's prices, or with the meter's
// first plan, and one of the two invoices bills the other plan's rate.
func TestE2E_PlanPrices_TwoPlansPriceOneMeterDifferently(t *testing.T) {
	r := newPPRig(t, "two plans")
	tokens := r.meter("tokens", "sum")
	pro, proPrices := r.plan("pro",
		perUnit(tokens, "3", map[string]string{"model": "a"}),
		perUnit(tokens, "1", nil))
	ent, entPrices := r.plan("enterprise",
		perUnit(tokens, "2", map[string]string{"model": "a"}),
		perUnit(tokens, "1", nil))
	custA, custB := r.customer("pp-a", ""), r.customer("pp-b", "")
	subA, p0, p1 := r.subscribe(time.Time{}, custA, pro)
	subB, _, _ := r.subscribe(time.Time{}, custB, ent)
	at := recent(p0)
	for _, ext := range []string{"pp-a", "pp-b"} {
		r.ingest(time.Time{}, ext, "tokens", 100, at, map[string]string{"model": "a"})
		r.ingest(time.Time{}, ext, "tokens", 10, at, map[string]string{"model": "b"})
	}
	r.run(p1.Add(time.Hour))
	wantUsage(t, "Pro", r.cycle(subA, p0), map[string]int64{proPrices[0]: 300, proPrices[1]: 10})
	wantUsage(t, "Enterprise", r.cycle(subB, p0), map[string]int64{entPrices[0]: 200, entPrices[1]: 10})
}

// TestE2E_PlanPrices_MostSpecificWins: of the prices whose dimension_match
// an event's labels contain, the one with the most labels takes the event
// (ADR-130 §4), and a {} price takes the rest.
//
// Mutation: claim in creation order instead of by specificity, and the
// 2-label price's events bill at the 1-label rate.
func TestE2E_PlanPrices_MostSpecificWins(t *testing.T) {
	r := newPPRig(t, "most specific")
	tokens := r.meter("tokens", "sum")
	plan, ids := r.plan("pro",
		perUnit(tokens, "1", nil),
		perUnit(tokens, "2", map[string]string{"model": "a"}),
		perUnit(tokens, "5", map[string]string{"model": "a", "token_type": "input"}))
	cust := r.customer("pp-ms", "")
	sub, p0, p1 := r.subscribe(time.Time{}, cust, plan)
	at := recent(p0)
	r.ingest(time.Time{}, "pp-ms", "tokens", 10, at, map[string]string{"model": "a", "token_type": "input"})
	r.ingest(time.Time{}, "pp-ms", "tokens", 10, at, map[string]string{"model": "a", "token_type": "output"})
	r.ingest(time.Time{}, "pp-ms", "tokens", 10, at, map[string]string{"model": "b"})
	r.run(p1.Add(time.Hour))
	inv := r.cycle(sub, p0)
	wantUsage(t, "close", inv, map[string]int64{ids[2]: 50, ids[1]: 20, ids[0]: 10})
	for _, l := range inv.usage() {
		if l.priceID == ids[2] && !strings.Contains(l.desc, "a · input") {
			t.Errorf("line of the 2-label price is described %q, want its labels", l.desc)
		}
	}
}

// TestE2E_PlanPrices_TiesRefusedWhenSaved: two prices of one plan and meter
// that could both take one event with the same number of labels are refused
// when the second is saved (409 price_conflict, naming the other price), an
// identical dimension_match is 409 price_exists, and prices that share a
// label with different values are fine (ADR-130 §4).
//
// Mutation: drop the tie check, and the conflicting add answers 201.
func TestE2E_PlanPrices_TiesRefusedWhenSaved(t *testing.T) {
	r := newPPRig(t, "ties")
	tokens := r.meter("tokens", "sum")
	plan, ids := r.plan("pro", perUnit(tokens, "2", map[string]string{"model": "a"}))
	path := "/v1/plans/" + plan + "/prices"

	msg := r.refused(http.MethodPost, path, perUnit(tokens, "3", map[string]string{"token_type": "input"}), http.StatusConflict, "price_conflict")
	if !strings.Contains(msg, ids[0]) {
		t.Errorf("price_conflict message %q does not name the other price %s", msg, ids[0])
	}
	r.refused(http.MethodPost, path, perUnit(tokens, "3", map[string]string{"model": "a"}), http.StatusConflict, "price_exists")
	r.must(time.Time{}, http.MethodPost, path, perUnit(tokens, "4", map[string]string{"model": "b"}), http.StatusCreated)
	r.must(time.Time{}, http.MethodPost, path, perUnit(tokens, "1", nil), http.StatusCreated)

	// One request with two tying prices: the plan is not created.
	code, out, raw := r.do(time.Time{}, http.MethodPost, "/v1/plans", map[string]any{
		"code": "tied", "name": "tied", "currency": "USD", "billing_interval": "monthly",
		"prices": []map[string]any{
			perUnit(tokens, "1", map[string]string{"model": "a"}),
			perUnit(tokens, "1", map[string]string{"region": "eu"}),
		},
	})
	if code != http.StatusConflict || flowStr(out, "error", "code") != "price_conflict" || flowStr(out, "error", "param") != "prices[1].dimension_match" {
		t.Errorf("plan with two tying prices: %d %s, want 409 price_conflict on prices[1].dimension_match", code, raw)
	}
	if code, _, _ := r.do(time.Time{}, http.MethodGet, "/v1/plans", nil); code != http.StatusOK {
		t.Fatalf("list plans: %d", code)
	}
	if n := r.count(`SELECT count(*) FROM plans WHERE tenant_id = $1 AND code = 'tied'`, r.tenantID); n != 0 {
		t.Errorf("the refused plan was created: %d rows", n)
	}

	// A label value is a string; a number is refused by name.
	code, out, raw = r.do(time.Time{}, http.MethodPost, path, map[string]any{
		"meter_id": tokens, "dimension_match": map[string]any{"tier": 2}, "mode": "per_unit", "unit_amount_cents": "1",
	})
	if code != http.StatusUnprocessableEntity || flowStr(out, "error", "param") != "dimension_match" {
		t.Errorf("numeric label value: %d %s, want 422 on dimension_match", code, raw)
	}
}

func (r *ppRig) count(query string, args ...any) int {
	r.t.Helper()
	ctx := context.Background()
	tx, err := r.db.BeginTx(ctx, postgres.TxBypass, "")
	if err != nil {
		r.t.Fatalf("begin: %v", err)
	}
	defer postgres.Rollback(tx)
	var n int
	if err := tx.QueryRowContext(ctx, query, args...).Scan(&n); err != nil {
		r.t.Fatalf("%s: %v", query, err)
	}
	return n
}

func (r *ppRig) exec(query string, args ...any) {
	r.t.Helper()
	ctx := context.Background()
	tx, err := r.db.BeginTx(ctx, postgres.TxBypass, "")
	if err != nil {
		r.t.Fatalf("begin: %v", err)
	}
	defer postgres.Rollback(tx)
	if _, err := tx.ExecContext(ctx, query, args...); err != nil {
		r.t.Fatalf("%s: %v", query, err)
	}
	if err := tx.Commit(); err != nil {
		r.t.Fatalf("commit: %v", err)
	}
}

// priceOf is the content GET /v1/plans/{id} shows for the price.
func (r *ppRig) priceOf(planID, priceID string) map[string]any {
	r.t.Helper()
	for _, p := range flowList(r.must(time.Time{}, http.MethodGet, "/v1/plans/"+planID, nil, http.StatusOK), "prices") {
		if flowStr(p, "id") == priceID {
			return p
		}
	}
	return nil
}

// TestE2E_PlanPrices_NeverUsedPlanAppliesAtOnce: on a plan no subscription
// has used, a price add, edit or removal applies at once (effective "now"):
// the price keeps one version, from the beginning of time, so the first
// period bills the edited rate. Adding a removed price back keeps its id.
//
// Mutation: append a version on a never-used plan too, and the first period
// (pinned at its start, before the edit) bills the old rate.
func TestE2E_PlanPrices_NeverUsedPlanAppliesAtOnce(t *testing.T) {
	r := newPPRig(t, "never used")
	calls := r.meter("calls", "sum")
	plan, ids := r.plan("basic", perUnit(calls, "1", nil))
	path := "/v1/plans/" + plan + "/prices/" + ids[0]

	edit := r.must(time.Time{}, http.MethodPatch, path, map[string]any{"mode": "per_unit", "unit_amount_cents": "4"}, http.StatusOK)
	if flowStr(edit, "effective") != "now" {
		t.Errorf("edit on a never-used plan: effective %q, want now", flowStr(edit, "effective"))
	}
	if got := r.priceOf(plan, ids[0]); flowStr(got, "unit_amount_cents") != "4" {
		t.Errorf("price after the edit = %v, want unit_amount_cents 4", got)
	}
	if n := r.count(`SELECT count(*) FROM plan_price_versions WHERE price_id = $1`, ids[0]); n != 1 {
		t.Errorf("versions of the price = %d, want 1 (the edit replaces it)", n)
	}
	removed := r.must(time.Time{}, http.MethodDelete, path, nil, http.StatusOK)
	if flowStr(removed, "effective") != "now" || removed["removed"] != true {
		t.Errorf("removal on a never-used plan: %v, want effective now and removed", removed)
	}
	if got := flowList(r.must(time.Time{}, http.MethodGet, "/v1/plans/"+plan+"/prices", nil, http.StatusOK), "data"); len(got) != 0 {
		t.Errorf("prices after the removal = %v, want none", got)
	}
	back := r.must(time.Time{}, http.MethodPost, "/v1/plans/"+plan+"/prices", perUnit(calls, "6", nil), http.StatusCreated)
	if flowStr(back, "id") != ids[0] {
		t.Errorf("the price added back has id %s, want its old id %s", flowStr(back, "id"), ids[0])
	}

	cust := r.customer("pp-nu", "")
	sub, p0, p1 := r.subscribe(time.Time{}, cust, plan)
	r.ingest(time.Time{}, "pp-nu", "calls", 10, recent(p0), nil)
	r.run(p1.Add(time.Hour))
	wantUsage(t, "first period", r.cycle(sub, p0), map[string]int64{ids[0]: 60})
}

// TestE2E_PlanPrices_UsedPlanAppliesNextPeriod: on a plan a subscription has
// used, a price edit, add or removal applies from each subscriber's next
// billing period (effective "next_period"). The open period keeps the prices
// it started with, and its preview says the same as its invoice.
//
// Mutation: resolve prices at the latest version instead of the period
// start, and the first close bills the edited rate.
func TestE2E_PlanPrices_UsedPlanAppliesNextPeriod(t *testing.T) {
	r := newPPRig(t, "used")
	tokens := r.meter("tokens", "sum")
	plan, ids := r.plan("pro", perUnit(tokens, "1", nil))
	catchAll := ids[0]
	cust := r.customer("pp-used", "")
	sub, p0, p1 := r.subscribe(time.Time{}, cust, plan)
	p2 := p1.AddDate(0, 1, 0)
	r.ingest(time.Time{}, "pp-used", "tokens", 10, recent(p0), map[string]string{"model": "a"})

	edit := r.must(time.Time{}, http.MethodPatch, "/v1/plans/"+plan+"/prices/"+catchAll, map[string]any{"mode": "per_unit", "unit_amount_cents": "2"}, http.StatusOK)
	if flowStr(edit, "effective") != "next_period" {
		t.Errorf("edit on a used plan: effective %q, want next_period", flowStr(edit, "effective"))
	}
	add := r.must(time.Time{}, http.MethodPost, "/v1/plans/"+plan+"/prices", perUnit(tokens, "5", map[string]string{"model": "a"}), http.StatusCreated)
	modelA := flowStr(add, "id")
	if flowStr(add, "effective") != "next_period" {
		t.Errorf("add on a used plan: effective %q, want next_period", flowStr(add, "effective"))
	}
	if got := r.priceOf(plan, catchAll); flowStr(got, "unit_amount_cents") != "2" {
		t.Errorf("GET shows %v, want the content from now on (2)", got)
	}
	plan2 := r.must(time.Time{}, http.MethodGet, "/v1/plans/"+plan, nil, http.StatusOK)
	if plan2["used"] != true {
		t.Errorf("used = %v, want true", plan2["used"])
	}

	// The preview of the open period is priced as its close will be.
	preview := r.must(time.Time{}, http.MethodPost, "/v1/invoices/create_preview", map[string]any{"customer_id": cust, "subscription_id": sub}, http.StatusOK)
	var previewUsage int64
	for _, l := range flowList(preview, "lines") {
		if flowStr(l, "line_type") == "usage" {
			previewUsage += flowInt(l, "amount_cents")
		}
	}
	if previewUsage != 10 {
		t.Errorf("preview usage = %dc, want 10 (the old {} rate; the new model a price is next period's)", previewUsage)
	}

	r.run(p1.Add(time.Hour))
	wantUsage(t, "P0: the prices it started with", r.cycle(sub, p0), map[string]int64{catchAll: 10})

	// P1: the edit and the add are in force.
	r.ingest(p1.Add(2*time.Hour), "pp-used", "tokens", 10, p1.Add(time.Hour), map[string]string{"model": "a"})
	r.ingest(p1.Add(2*time.Hour), "pp-used", "tokens", 10, p1.Add(time.Hour), map[string]string{"model": "b"})
	r.run(p2.Add(time.Hour))
	wantUsage(t, "P1: the edit and the add", r.cycle(sub, p1), map[string]int64{modelA: 50, catchAll: 20})
}

// TestE2E_PlanPrices_NewSubscriberOnTheDayOfAChange: a subscription that
// starts on a used plan later on the day of a price change gets the change
// from its second period, not its first. Its first period starts at midnight,
// before the change, and a period is priced by the prices in force at its
// start (ADR-130 §4, Risks). So its first period bills the old rate, still
// bills a price removed that day, and does not bill a price added that day.
// The docs say so (docs/ai-pricing.md, the API reference, the plan page).
//
// Mutation: price a first period at the subscription's start instead of the
// period start, and the first period bills the edit, drops the removed
// price and bills the added one.
func TestE2E_PlanPrices_NewSubscriberOnTheDayOfAChange(t *testing.T) {
	r := newPPRig(t, "same day")
	calls := r.meter("calls", "sum")
	extra := r.meter("extra", "sum")
	tokens := r.meter("tokens", "sum")
	plan, ids := r.plan("pro", perUnit(calls, "1", nil), perUnit(extra, "7", nil))
	callsPrice, extraPrice := ids[0], ids[1]
	r.subscribe(time.Time{}, r.customer("pp-sd-a", ""), plan) // the plan is used

	for _, w := range []map[string]any{
		r.must(time.Time{}, http.MethodPatch, "/v1/plans/"+plan+"/prices/"+callsPrice, map[string]any{"mode": "per_unit", "unit_amount_cents": "5"}, http.StatusOK),
		r.must(time.Time{}, http.MethodDelete, "/v1/plans/"+plan+"/prices/"+extraPrice, nil, http.StatusOK),
	} {
		if flowStr(w, "effective") != "next_period" {
			t.Fatalf("write on a used plan: effective %q, want next_period", flowStr(w, "effective"))
		}
	}
	add := r.must(time.Time{}, http.MethodPost, "/v1/plans/"+plan+"/prices", perUnit(tokens, "2", nil), http.StatusCreated)
	tokensPrice := flowStr(add, "id")

	sub, p0, p1 := r.subscribe(time.Time{}, r.customer("pp-sd-b", ""), plan)
	p2 := p1.AddDate(0, 1, 0)
	for _, ev := range []string{"calls", "extra", "tokens"} {
		r.ingest(time.Time{}, "pp-sd-b", ev, 10, recent(p0), nil)
	}
	r.run(p1.Add(time.Hour))
	wantUsage(t, "first period: the prices in force at its midnight start", r.cycle(sub, p0), map[string]int64{callsPrice: 10, extraPrice: 70})

	for _, ev := range []string{"calls", "extra", "tokens"} {
		r.ingest(p1.Add(2*time.Hour), "pp-sd-b", ev, 10, p1.Add(time.Hour), nil)
	}
	r.run(p2.Add(time.Hour))
	wantUsage(t, "second period: the changes", r.cycle(sub, p1), map[string]int64{callsPrice: 50, tokensPrice: 20})
}

// TestE2E_PlanPrices_RemovalEndsOverrides: removing a price ends its
// customer overrides at the instant the removal takes effect for each
// customer, and the response names them (ADR-130 §6). The open period keeps
// the negotiated rate. Adding the price back keeps its id but not the
// override: the next period bills the list price.
//
// Mutation: end the overrides at the removal's request instead of the
// tombstone, or skip ending them, and the added-back price bills the old
// negotiated rate.
func TestE2E_PlanPrices_RemovalEndsOverrides(t *testing.T) {
	r := newPPRig(t, "removal overrides")
	calls := r.meter("calls", "sum")
	plan, ids := r.plan("pro", perUnit(calls, "10", nil))
	cust := r.customer("pp-ro", "")
	sub, p0, p1 := r.subscribe(time.Time{}, cust, plan)
	p2 := p1.AddDate(0, 1, 0)
	o := r.must(time.Time{}, http.MethodPost, "/v1/price-overrides", map[string]any{
		"customer_id": cust, "price_id": ids[0], "mode": "per_unit", "unit_amount_cents": "4", "reason": "negotiated",
	}, http.StatusCreated)
	// The override was negotiated before this period started.
	r.exec(`UPDATE customer_price_overrides SET created_at = $1 WHERE id = $2`, p0.Add(-time.Hour), flowStr(o, "id"))
	r.ingest(time.Time{}, "pp-ro", "calls", 10, recent(p0), nil)

	rm := r.must(time.Time{}, http.MethodDelete, "/v1/plans/"+plan+"/prices/"+ids[0], nil, http.StatusOK)
	if flowStr(rm, "effective") != "next_period" {
		t.Errorf("removal on a used plan: effective %q, want next_period", flowStr(rm, "effective"))
	}
	ended := flowList(rm, "ended_overrides")
	if len(ended) != 1 || flowStr(ended[0], "id") != flowStr(o, "id") || flowStr(ended[0], "customer_id") != cust {
		t.Errorf("ended_overrides = %v, want the customer's override", rm["ended_overrides"])
	}
	back := r.must(time.Time{}, http.MethodPost, "/v1/plans/"+plan+"/prices", perUnit(calls, "10", nil), http.StatusCreated)
	if flowStr(back, "id") != ids[0] {
		t.Errorf("added-back price id %s, want %s", flowStr(back, "id"), ids[0])
	}
	if n := len(flowList(r.must(time.Time{}, http.MethodGet, "/v1/price-overrides?customer_id="+cust, nil, http.StatusOK), "data")); n != 0 {
		t.Errorf("active overrides after the removal = %d, want 0", n)
	}

	r.run(p1.Add(time.Hour))
	wantUsage(t, "P0 keeps the negotiated rate", r.cycle(sub, p0), map[string]int64{ids[0]: 40})
	r.ingest(p1.Add(2*time.Hour), "pp-ro", "calls", 10, p1.Add(time.Hour), nil)
	r.run(p2.Add(time.Hour))
	wantUsage(t, "P1 bills the added-back price at list", r.cycle(sub, p1), map[string]int64{ids[0]: 100})
}

// TestE2E_PlanPrices_OverrideFromTheNextPeriod: an override create applies
// from the customer's next period (ADR-130 §6). (The delete side is
// TestE2E_PlanPrices_TestClockOverride: wall time cannot be moved past a
// period start in a test, a test clock can.)
//
// Mutation: resolve the override at the close's instant instead of the
// period start, and P0 bills the new override.
func TestE2E_PlanPrices_OverrideFromTheNextPeriod(t *testing.T) {
	r := newPPRig(t, "override next period")
	calls := r.meter("calls", "sum")
	plan, ids := r.plan("pro", perUnit(calls, "10", nil))
	cust := r.customer("pp-onp", "")
	sub, p0, p1 := r.subscribe(time.Time{}, cust, plan)
	p2 := p1.AddDate(0, 1, 0)
	r.must(time.Time{}, http.MethodPost, "/v1/price-overrides", map[string]any{
		"customer_id": cust, "price_id": ids[0], "mode": "per_unit", "unit_amount_cents": "4",
	}, http.StatusCreated)
	r.ingest(time.Time{}, "pp-onp", "calls", 10, recent(p0), nil)
	r.run(p1.Add(time.Hour))
	wantUsage(t, "P0: list price", r.cycle(sub, p0), map[string]int64{ids[0]: 100})

	r.ingest(p1.Add(2*time.Hour), "pp-onp", "calls", 10, p1.Add(time.Hour), nil)
	r.run(p2.Add(time.Hour))
	wantUsage(t, "P1: the override", r.cycle(sub, p1), map[string]int64{ids[0]: 40})
}

// TestE2E_PlanPrices_BaseBillTiming: base_bill_timing is required when the
// base fee is above 0, defaults to in_arrears at 0, and in_advance with a
// base of 0 is refused on create and on PATCH (ADR-130 §14). A PATCH can set
// the base fee to 0 (B9).
func TestE2E_PlanPrices_BaseBillTiming(t *testing.T) {
	r := newPPRig(t, "base bill timing")
	plan := func(base int64, timing string) (int, map[string]any) {
		body := map[string]any{"code": fmt.Sprintf("p%d%s", base, timing), "name": "p", "currency": "USD", "billing_interval": "monthly", "base_amount_cents": base}
		if timing != "" {
			body["base_bill_timing"] = timing
		}
		code, out, _ := r.do(time.Time{}, http.MethodPost, "/v1/plans", body)
		return code, out
	}
	if code, out := plan(500, ""); code != http.StatusUnprocessableEntity || flowStr(out, "error", "param") != "base_bill_timing" {
		t.Errorf("base 500 without a timing: %d %v, want 422 on base_bill_timing", code, out)
	}
	if code, out := plan(0, "in_advance"); code != http.StatusUnprocessableEntity || flowStr(out, "error", "param") != "base_bill_timing" {
		t.Errorf("base 0 in_advance: %d %v, want 422 on base_bill_timing", code, out)
	}
	code, out := plan(0, "")
	if code != http.StatusCreated || flowStr(out, "base_bill_timing") != "in_arrears" {
		t.Errorf("base 0 without a timing: %d %v, want 201 in_arrears", code, out)
	}
	code, adv := plan(500, "in_advance")
	if code != http.StatusCreated {
		t.Fatalf("base 500 in_advance: %d %v", code, adv)
	}
	if code, out, raw := r.do(time.Time{}, http.MethodPatch, "/v1/plans/"+flowStr(adv, "id"), map[string]any{"base_amount_cents": 0}); code != http.StatusUnprocessableEntity || flowStr(out, "error", "param") != "base_amount_cents" {
		t.Errorf("base to 0 on an in_advance plan: %d %s, want 422 on base_amount_cents", code, raw)
	}
	code, arr := plan(700, "in_arrears")
	if code != http.StatusCreated {
		t.Fatalf("base 700 in_arrears: %d %v", code, arr)
	}
	if got := r.must(time.Time{}, http.MethodPatch, "/v1/plans/"+flowStr(arr, "id"), map[string]any{"base_amount_cents": 0}, http.StatusOK); flowInt(got, "base_amount_cents") != 0 {
		t.Errorf("PATCH base 0: %v, want base_amount_cents 0", got)
	}
	if code, out, raw := r.do(time.Time{}, http.MethodPatch, "/v1/plans/"+flowStr(arr, "id"), map[string]any{"base_bill_timing": "in_advance"}); code != http.StatusUnprocessableEntity || flowStr(out, "error", "param") != "base_bill_timing" {
		t.Errorf("in_advance on a base-0 plan: %d %s, want 422 on base_bill_timing", code, raw)
	}
}

// TestE2E_PlanPrices_OldContractRefused: the rating rules and meter pricing
// rules are gone (ADR-130 §16), and so are a meter's default price and a
// plan's meter list.
func TestE2E_PlanPrices_OldContractRefused(t *testing.T) {
	r := newPPRig(t, "old contract")
	calls := r.meter("calls", "sum")
	for _, path := range []string{"/v1/rating-rules", "/v1/meter-pricing-rules", "/v1/meters/" + calls + "/pricing-rules"} {
		if code, _, raw := r.do(time.Time{}, http.MethodGet, path, nil); code != http.StatusNotFound {
			t.Errorf("GET %s: %d %s, want 404", path, code, raw)
		}
	}
	if code, out, raw := r.do(time.Time{}, http.MethodPost, "/v1/meters", map[string]any{
		"event_name": "other", "name": "other", "rating_rule_version_id": "vlx_rrv_x",
	}); code != http.StatusUnprocessableEntity || flowStr(out, "error", "param") != "rating_rule_version_id" {
		t.Errorf("meter with a default price: %d %s, want 422 unknown field", code, raw)
	}
	if code, out, raw := r.do(time.Time{}, http.MethodPost, "/v1/plans", map[string]any{
		"code": "m", "name": "m", "currency": "USD", "billing_interval": "monthly", "meter_ids": []string{calls},
	}); code != http.StatusUnprocessableEntity || flowStr(out, "error", "param") != "meter_ids" {
		t.Errorf("plan with meter_ids: %d %s, want 422 unknown field", code, raw)
	}
	plan, ids := r.plan("pro", perUnit(calls, "1", nil))
	if code, out, raw := r.do(time.Time{}, http.MethodPatch, "/v1/plans/"+plan+"/prices/"+ids[0], map[string]any{
		"dimension_match": map[string]any{"model": "a"}, "mode": "per_unit", "unit_amount_cents": "1",
	}); code != http.StatusUnprocessableEntity || flowStr(out, "error", "param") != "dimension_match" {
		t.Errorf("PATCH a price's dimension_match: %d %s, want 422: it is the identity", code, raw)
	}
}

// TestE2E_PlanPrices_SwapPricesEachWindowByItsPlan: after a mid-period swap
// between two plans that price one meter, each window is priced by its own
// plan, as two lines (ADR-130 §9), on the spend-threshold invoice and on the
// close.
//
// Mutation: merge the windows per meter, or price the threshold invoice's
// one flat window with the current plan, and the pre-swap usage bills at the
// new plan's rate.
func TestE2E_PlanPrices_SwapPricesEachWindowByItsPlan(t *testing.T) {
	r := newPPRig(t, "swap")
	calls := r.meter("calls", "sum")
	planA, aIDs := r.plan("plan_a", perUnit(calls, "1", nil))
	planB, bIDs := r.plan("plan_b", perUnit(calls, "3", nil))
	cust := r.customer("pp-swap", "")
	sub, p0, p1 := r.subscribe(time.Time{}, cust, planA)
	swapAt := time.Now().UTC()
	r.ingest(swapAt, "pp-swap", "calls", 10, recent(p0), nil)
	items := flowList(r.must(swapAt, http.MethodGet, "/v1/subscriptions/"+sub, nil, http.StatusOK), "items")
	if len(items) != 1 {
		t.Fatalf("items = %v", items)
	}
	r.must(swapAt.Add(time.Second), http.MethodPatch, "/v1/subscriptions/"+sub+"/items/"+flowStr(items[0], "id"),
		map[string]any{"new_plan_id": planB, "immediate": true}, http.StatusOK)
	after := swapAt.Add(time.Minute)
	r.ingest(after.Add(time.Second), "pp-swap", "calls", 10, after, nil)

	// A spend threshold fires on the windows: 10c + 30c = 40c crosses 35c.
	r.must(after.Add(time.Minute), http.MethodPut, "/v1/subscriptions/"+sub+"/billing-thresholds", map[string]any{"amount_gte": 35}, http.StatusOK)
	fireAt := after.Add(2 * time.Minute)
	r.clk.Set(fireAt)
	if n, errs := r.srv.BillingEngine.ScanThresholds(postgres.WithLivemode(clock.WithEffectiveNow(context.Background(), fireAt), false), 50); n != 1 || len(errs) != 0 {
		t.Fatalf("threshold scan: fired %d, errors %v; want 1", n, errs)
	}
	var fire ppInvoice
	for _, i := range r.invoices(sub) {
		if i.reason == "threshold" {
			fire = i
		}
	}
	wantUsage(t, "threshold invoice", fire, map[string]int64{aIDs[0]: 10, bIDs[0]: 30})

	r.ingest(fireAt.Add(time.Minute), "pp-swap", "calls", 5, fireAt.Add(30*time.Second), nil)
	r.run(p1.Add(time.Hour))
	wantUsage(t, "close: the rest, on plan B", r.cycle(sub, p0), map[string]int64{bIDs[0]: 15})
}
