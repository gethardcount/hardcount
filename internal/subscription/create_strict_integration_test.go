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

	"github.com/sagarsuperuser/velox/internal/api/middleware"
	"github.com/sagarsuperuser/velox/internal/auth"
	"github.com/sagarsuperuser/velox/internal/customer"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/platform/clock"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/pricing"
	"github.com/sagarsuperuser/velox/internal/testutil"
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
// idempotency middleware and Postgres, for an in_advance plan (a create that
// starts now bills day 1).
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
	loc    *time.Location
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

	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatalf("load zone: %v", err)
	}
	svc := NewService(NewPostgresStore(db), nil)
	svc.SetPlanReader(ps)
	svc.SetCustomerReader(custs)
	svc.SetSettingsReader(fixedZoneSettings{tz: loc.String()})
	biller := &createBillerSpy{}
	svc.SetBiller(biller)
	h := NewHandler(svc)
	h.SetDB(db)

	return &strictCreateFixture{
		t: t, db: db, ctx: ctx, tenant: tenantID, custID: cust.ID, planID: plan.ID,
		biller: biller, srv: middleware.Idempotency(db, nil)(h.Routes()),
		now: time.Date(2026, 3, 5, 15, 0, 0, 0, time.UTC), loc: loc,
	}
}

func (f *strictCreateFixture) post(body, idemKey string) *httptest.ResponseRecorder {
	f.t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if idemKey != "" {
		req.Header.Set("Idempotency-Key", idemKey)
	}
	reqCtx := clock.WithEffectiveNow(context.WithValue(f.ctx, auth.TestTenantIDKey(), f.tenant), f.now)
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

// TestCreateSubscription_UnknownFieldRefused is D-05: `trial_period_days` (the
// Stripe spelling) instead of `trial_days` was dropped by the decoder, so a
// caller asking for a 14-day trial got an active subscription billed on day 1
// with a 201. The unknown key is now a 422 naming it, nothing is written, and
// the 422 is not cached under the Idempotency-Key, so the corrected retry
// with the same key creates the trial.
func TestCreateSubscription_UnknownFieldRefused(t *testing.T) {
	f := newStrictCreateFixture(t, "Strict Create Unknown Field")

	rr := f.post(f.body("strict-1", `"trial_period_days":14`), "idem-strict-1")
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("trial_period_days: status %d, want 422; body %s", rr.Code, rr.Body.String())
	}
	if e := decodeEnvelope(t, rr); e.Error.Param != "trial_period_days" {
		t.Errorf("param: got %q, want trial_period_days", e.Error.Param)
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
	rr = f.post(f.body("strict-1", `"trial_days":14`), "idem-strict-1")
	if rr.Code != http.StatusCreated {
		t.Fatalf("corrected retry with the same key: status %d, want 201; body %s", rr.Code, rr.Body.String())
	}
	if rr.Header().Get("Idempotent-Replayed") != "" {
		t.Fatal("corrected retry was a replay of the refusal")
	}
	if s := decodeSub(t, rr); s.Status != domain.SubscriptionTrialing {
		t.Fatalf("trial_days:14 status: got %q, want trialing", s.Status)
	}
	if f.biller.calls != 0 {
		t.Fatalf("a trialing create ran the day-1 bill %d times", f.biller.calls)
	}
}

// TestCreateSubscription_TrialDaysBounds is D-03: a negative trial_days was
// read as "no trial" (charged on day 1) and any size was
// accepted. trial_days is 0..730 (Stripe: a trial "can be at most two years").
func TestCreateSubscription_TrialDaysBounds(t *testing.T) {
	f := newStrictCreateFixture(t, "Strict Create Trial Bounds")

	for _, days := range []string{"-1", "731"} {
		rr := f.post(f.body("bound-"+strings.TrimPrefix(days, "-"), `"trial_days":`+days+``), "")
		if rr.Code != http.StatusUnprocessableEntity {
			t.Fatalf("trial_days %s: status %d, want 422; body %s", days, rr.Code, rr.Body.String())
		}
		if e := decodeEnvelope(t, rr); e.Error.Param != "trial_days" {
			t.Errorf("trial_days %s: param %q, want trial_days", days, e.Error.Param)
		}
	}
	if n := f.subCount(); n != 0 {
		t.Fatalf("out-of-range trial_days wrote %d subscription rows", n)
	}
	if f.biller.calls != 0 {
		t.Fatalf("out-of-range trial_days ran the day-1 bill %d times", f.biller.calls)
	}

	rr := f.post(f.body("bound-730", `"trial_days":730`), "")
	if rr.Code != http.StatusCreated {
		t.Fatalf("trial_days 730: status %d, want 201; body %s", rr.Code, rr.Body.String())
	}
	s := decodeSub(t, rr)
	if s.Status != domain.SubscriptionTrialing || s.TrialEndAt == nil {
		t.Fatalf("trial_days 730: status %q trial_end %v, want trialing with an end", s.Status, s.TrialEndAt)
	}
	if want := domain.AddDaysIn(f.now, 730, f.loc); !s.TrialEndAt.Equal(want) {
		t.Errorf("trial_end_at: got %s, want %s", s.TrialEndAt, want)
	}

	rr = f.post(f.body("bound-0", `"trial_days":0`), "")
	if rr.Code != http.StatusCreated {
		t.Fatalf("trial_days 0: status %d, want 201; body %s", rr.Code, rr.Body.String())
	}
	if s := decodeSub(t, rr); s.Status != domain.SubscriptionActive || s.TrialEndAt != nil {
		t.Fatalf("trial_days 0: status %q trial_end %v, want active with no trial", s.Status, s.TrialEndAt)
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
	reqCtx := clock.WithEffectiveNow(context.WithValue(f.ctx, auth.TestTenantIDKey(), f.tenant), f.now)
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
