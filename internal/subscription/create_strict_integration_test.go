package subscription

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/api/middleware"
	"github.com/gethardcount/hardcount/internal/auth"
	"github.com/gethardcount/hardcount/internal/customer"
	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/platform/clock"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/pricing"
	"github.com/gethardcount/hardcount/internal/testutil"
)

// createBillerSpy records whether the day-1 bill ran. Only BillOnCreateTx is
// reached on create; it reports "no invoice" so the post-commit finalize
// (the embedded nil interface) is never called.
type createBillerSpy struct {
	Biller
	calls int
}

func (b *createBillerSpy) BillOnCreateTx(context.Context, *sql.Tx, domain.Subscription, time.Time) (domain.Invoice, bool, error) {
	b.calls++
	return domain.Invoice{}, false, nil
}

type fixedZoneSettings struct{ tz string }

func (f fixedZoneSettings) Get(context.Context, string) (domain.TenantSettings, error) {
	return domain.TenantSettings{Timezone: f.tz}, nil
}

// strictCreateFixture is POST /v1/subscriptions through the real router, the
// idempotency middleware and Postgres, for an in_advance plan (a create bills
// day 1).
type strictCreateFixture struct {
	t      *testing.T
	db     *postgres.DB
	ctx    context.Context
	tenant string
	custID string
	planID string
	biller *createBillerSpy
	srv    http.Handler
	now    time.Time
}

func newStrictCreateFixture(t *testing.T, name string) *strictCreateFixture {
	t.Helper()
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, name)

	custs := customer.NewPostgresStore(db)
	cust, err := custs.Create(ctx, tenantID, domain.Customer{ExternalID: "cus_strict", DisplayName: "Strict"})
	if err != nil {
		t.Fatalf("create customer: %v", err)
	}
	ps := pricing.NewPostgresStore(db)
	plan, err := ps.CreatePlan(ctx, tenantID, domain.Plan{
		Code: "strict-adv", Name: "Strict", Currency: "USD",
		BillingInterval: domain.BillingMonthly, BaseBillTiming: domain.BillInAdvance,
		BaseAmountCents: 5000, Status: domain.PlanActive,
	})
	if err != nil {
		t.Fatalf("create plan: %v", err)
	}

	svc := NewService(NewPostgresStore(db), nil)
	svc.SetPlanReader(ps)
	svc.SetCustomerReader(custs)
	svc.SetSettingsReader(fixedZoneSettings{tz: "America/New_York"})
	biller := &createBillerSpy{}
	svc.SetBiller(biller)
	h := NewHandler(svc)

	return &strictCreateFixture{
		t: t, db: db, ctx: ctx, tenant: tenantID, custID: cust.ID, planID: plan.ID,
		biller: biller, srv: middleware.Idempotency(db, nil)(h.Routes()),
		now: time.Date(2026, 3, 5, 15, 0, 0, 0, time.UTC),
	}
}

func (f *strictCreateFixture) post(body, idemKey string) *httptest.ResponseRecorder {
	f.t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if idemKey != "" {
		req.Header.Set("Idempotency-Key", idemKey)
	}
	reqCtx := clock.WithEffectiveNow(auth.WithCaller(f.ctx, auth.Caller{TenantID: f.tenant, KeyID: "key_test"}), f.now)
	rr := httptest.NewRecorder()
	f.srv.ServeHTTP(rr, req.WithContext(reqCtx))
	return rr
}

func (f *strictCreateFixture) subCount() int {
	f.t.Helper()
	tx, err := f.db.BeginTx(f.ctx, postgres.TxTenant, f.tenant)
	if err != nil {
		f.t.Fatalf("begin: %v", err)
	}
	defer postgres.Rollback(tx)
	var n int
	if err := tx.QueryRowContext(f.ctx, `SELECT count(*) FROM subscriptions WHERE customer_id = $1`, f.custID).Scan(&n); err != nil {
		f.t.Fatalf("count subscriptions: %v", err)
	}
	return n
}

// body builds a create body with the known keys plus extra raw JSON members.
func (f *strictCreateFixture) body(code, extra string) string {
	b := `{"code":"` + code + `","display_name":"Strict","customer_id":"` + f.custID + `","items":[{"plan_id":"` + f.planID + `"}]`
	if extra != "" {
		b += "," + extra
	}
	return b + "}"
}

func decodeSub(t *testing.T, rr *httptest.ResponseRecorder) domain.Subscription {
	t.Helper()
	var s domain.Subscription
	if err := json.Unmarshal(rr.Body.Bytes(), &s); err != nil {
		t.Fatalf("decode subscription %q: %v", rr.Body.String(), err)
	}
	return s
}

// TestCreateSubscription_UnknownFieldRefused is D-05: a key the create body
// does not define, such as `billing_cycle_anchor` (the Stripe field), is a
// 422 naming it, not dropped while the caller gets a 201 for a subscription
// that ignores it. Nothing is written, and the 422 is not cached under the
// Idempotency-Key, so the corrected retry with the same key creates the
// subscription.
func TestCreateSubscription_UnknownFieldRefused(t *testing.T) {
	f := newStrictCreateFixture(t, "Strict Create Unknown Field")

	rr := f.post(f.body("strict-1", `"billing_cycle_anchor":1772668800`), "idem-strict-1")
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("billing_cycle_anchor: status %d, want 422; body %s", rr.Code, rr.Body.String())
	}
	if e := decodeEnvelope(t, rr); e.Error.Param != "billing_cycle_anchor" {
		t.Errorf("param: got %q, want billing_cycle_anchor", e.Error.Param)
	}
	if n := f.subCount(); n != 0 {
		t.Fatalf("a refused create wrote %d subscription rows", n)
	}
	if f.biller.calls != 0 {
		t.Fatalf("a refused create ran the day-1 bill %d times", f.biller.calls)
	}

	// Nested: an unknown key inside items[] is caught too.
	rr = f.post(`{"code":"strict-2","display_name":"Strict","customer_id":"`+f.custID+`","items":[{"plan_id":"`+f.planID+`","price":"x"}]}`, "")
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("items[].price: status %d, want 422; body %s", rr.Code, rr.Body.String())
	}
	if e := decodeEnvelope(t, rr); e.Error.Param != "price" {
		t.Errorf("nested param: got %q, want price", e.Error.Param)
	}
	if n := f.subCount(); n != 0 {
		t.Fatalf("a refused nested create wrote %d subscription rows", n)
	}

	// Corrected body under the SAME key: the 422 was released, not cached.
	rr = f.post(f.body("strict-1", ""), "idem-strict-1")
	if rr.Code != http.StatusCreated {
		t.Fatalf("corrected retry with the same key: status %d, want 201; body %s", rr.Code, rr.Body.String())
	}
	if rr.Header().Get("Idempotent-Replayed") != "" {
		t.Fatal("corrected retry was a replay of the refusal")
	}
	if s := decodeSub(t, rr); s.Status != domain.SubscriptionActive {
		t.Fatalf("corrected retry: status %q, want active", s.Status)
	}
	if f.biller.calls != 1 {
		t.Fatalf("the corrected create ran the day-1 bill %d times, want 1", f.biller.calls)
	}
}

// TestCreateSubscription_UsageCapRefused locks the cut of the block usage cap
// (#973): usage_cap_units / overage_action are no longer fields, so a caller
// still sending them gets a 422 naming the key instead of a subscription that
// silently bills all usage.
func TestCreateSubscription_UsageCapRefused(t *testing.T) {
	f := newStrictCreateFixture(t, "Strict Create Usage Cap")

	rr := f.post(f.body("cap-1", `"usage_cap_units":100,"overage_action":"block"`), "")
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("usage_cap_units: status %d, want 422; body %s", rr.Code, rr.Body.String())
	}
	if e := decodeEnvelope(t, rr); e.Error.Param != "usage_cap_units" {
		t.Errorf("param: got %q, want usage_cap_units", e.Error.Param)
	}
	if n := f.subCount(); n != 0 {
		t.Fatalf("a refused cap create wrote %d subscription rows", n)
	}
}

// TestGetSubscription_NoUsageCapKeys: responses (and the lifecycle webhooks,
// which carry the same domain.Subscription) no longer carry the cap fields.
func TestGetSubscription_NoUsageCapKeys(t *testing.T) {
	f := newStrictCreateFixture(t, "Strict Get No Cap Keys")

	rr := f.post(f.body("shape-1", ""), "")
	if rr.Code != http.StatusCreated {
		t.Fatalf("create: status %d; body %s", rr.Code, rr.Body.String())
	}
	id := decodeSub(t, rr).ID

	req := httptest.NewRequest(http.MethodGet, "/"+id, nil)
	reqCtx := clock.WithEffectiveNow(auth.WithTenantID(f.ctx, f.tenant), f.now)
	get := httptest.NewRecorder()
	f.srv.ServeHTTP(get, req.WithContext(reqCtx))
	if get.Code != http.StatusOK {
		t.Fatalf("get: status %d; body %s", get.Code, get.Body.String())
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(get.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode get: %v", err)
	}
	for _, key := range []string{"usage_cap_units", "overage_action"} {
		if v, ok := raw[key]; ok {
			t.Errorf("GET /v1/subscriptions/{id} still carries %s=%s", key, v)
		}
	}
}
