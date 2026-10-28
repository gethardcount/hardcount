package recipe

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"

	"github.com/shopspring/decimal"

	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/errs"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/pricing"
	"github.com/gethardcount/hardcount/internal/testutil"
)

// recipeIntegrationFixture wires the real per-domain stores and services
// against a clean test database. Returned pieces are everything the tests
// need to instantiate a recipe and then count what landed in each table.
type recipeIntegrationFixture struct {
	db         *postgres.DB
	svc        *Service
	store      *PostgresStore
	pricingSvc *pricing.Service
}

func newRecipeFixture(t *testing.T) *recipeIntegrationFixture {
	t.Helper()
	db := testutil.SetupTestDB(t)
	registry, err := Load()
	if err != nil {
		t.Fatalf("load recipe registry: %v", err)
	}
	pricingSvc := pricing.NewService(pricing.NewPostgresStore(db))
	store := NewPostgresStore(db)
	svc := NewService(db, store, registry, pricingSvc)
	return &recipeIntegrationFixture{
		db:         db,
		svc:        svc,
		store:      store,
		pricingSvc: pricingSvc,
	}
}

// countRows runs a SELECT count(*) inside a tenant-scoped tx so RLS
// permits the read. Returns whatever the row count is for that tenant —
// other tenants' rows are filtered by RLS anyway.
func countRows(t *testing.T, db *postgres.DB, tenantID, table string) int {
	t.Helper()
	tx, err := db.BeginTx(postgres.WithLivemode(context.Background(), false), postgres.TxTenant, tenantID)
	if err != nil {
		t.Fatalf("begin count tx: %v", err)
	}
	defer postgres.Rollback(tx)
	var n int
	q := fmt.Sprintf("SELECT count(*) FROM %s", table)
	if err := tx.QueryRowContext(context.Background(), q).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

// TestService_Instantiate_BuildsFullGraph mirrors the design-doc acceptance
// test: anthropic_style produces 1 meter and 1 plan with its prices, and
// nothing else (ADR-130 §15). Counts come from direct
// SQL so there's no test-side double-counting via the same Store path the
// production code wrote through.
func TestService_Instantiate_BuildsFullGraph(t *testing.T) {
	f := newRecipeFixture(t)
	tenantID := testutil.CreateTestTenant(t, f.db, "anthropic install")
	ctx := postgres.WithLivemode(context.Background(), false)

	inst, err := f.svc.Instantiate(ctx, tenantID, "anthropic_style", nil, InstantiateOptions{
		CreatedBy: "operator@example.com",
	})
	if err != nil {
		t.Fatalf("Instantiate: %v", err)
	}
	if inst.ID == "" {
		t.Fatal("expected RecipeInstance ID")
	}
	if inst.RecipeKey != "anthropic_style" {
		t.Errorf("RecipeKey: got %q", inst.RecipeKey)
	}
	if got, want := len(inst.CreatedObjects.MeterIDs), 1; got != want {
		t.Errorf("CreatedObjects.MeterIDs: got %d, want %d", got, want)
	}
	// anthropic_style (ADR-044 matrix): 25 price sets × 5 token roles —
	// 7 from the v2/v3 catalog, 12 added by the 2026-07-26 refresh, and 6
	// added on 2026-10-22 (Fable 5.1, Mythos 5.1, Opus 5.5, Sonnet 5.5, and
	// Haiku 5.5's two prompt tiers).
	if got, want := len(inst.CreatedObjects.PriceIDs), 125; got != want {
		t.Errorf("CreatedObjects.PriceIDs: got %d, want %d", got, want)
	}
	if got, want := len(inst.CreatedObjects.PlanIDs), 1; got != want {
		t.Errorf("CreatedObjects.PlanIDs: got %d, want %d", got, want)
	}

	// Verify against the actual rows persisted in each domain's table.
	if n := countRows(t, f.db, tenantID, "meters"); n != 1 {
		t.Errorf("meters row count: got %d, want 1", n)
	}
	if n := countRows(t, f.db, tenantID, "plan_prices"); n != 125 {
		t.Errorf("plan_prices row count: got %d, want 125", n)
	}
	if n := countRows(t, f.db, tenantID, "plan_price_versions"); n != 125 {
		t.Errorf("plan_price_versions row count: got %d, want 125 (one each: the plan is new)", n)
	}
	if n := countRows(t, f.db, tenantID, "plans"); n != 1 {
		t.Errorf("plans row count: got %d, want 1", n)
	}
	if n := countRows(t, f.db, tenantID, "dunning_policies"); n != 0 {
		t.Errorf("dunning_policies row count: got %d, want 0", n)
	}
	if n := countRows(t, f.db, tenantID, "webhook_endpoints"); n != 0 {
		t.Errorf("webhook_endpoints row count: got %d, want 0", n)
	}
	if n := countRows(t, f.db, tenantID, "recipe_instances"); n != 1 {
		t.Errorf("recipe_instances row count: got %d, want 1", n)
	}
}

// failingPricingWriter wraps the real pricing writer and fails right after
// the plan and its prices are written on the install's transaction. Used to
// inject a mid-graph failure so the rollback path is exercised against real
// DB rows.
type failingPricingWriter struct {
	inner PricingWriter
}

func (f *failingPricingWriter) CreateMeterTx(ctx context.Context, tx *sql.Tx, tenantID string, m domain.Meter) (domain.Meter, error) {
	return f.inner.CreateMeterTx(ctx, tx, tenantID, m)
}
func (f *failingPricingWriter) GetMeterByEventNameTx(ctx context.Context, tx *sql.Tx, eventName string) (domain.Meter, error) {
	return f.inner.GetMeterByEventNameTx(ctx, tx, eventName)
}
func (f *failingPricingWriter) CreatePlanWithPricesTx(ctx context.Context, tx *sql.Tx, tenantID string, p domain.Plan, prices []domain.PlanPrice) (domain.Plan, []domain.PlanPrice, error) {
	if _, _, err := f.inner.CreatePlanWithPricesTx(ctx, tx, tenantID, p, prices); err != nil {
		return domain.Plan{}, nil, err
	}
	return domain.Plan{}, nil, errors.New("synthetic failure after the plan and its prices")
}
func (f *failingPricingWriter) ListPlans(ctx context.Context, tenantID string) ([]domain.Plan, error) {
	return f.inner.ListPlans(ctx, tenantID)
}

// TestService_Instantiate_AtomicityRollback fails partway through the
// graph build and verifies zero rows survive — the contract is "fully
// installed or not installed at all", with no orphan meters or prices to
// clean up by hand.
func TestService_Instantiate_AtomicityRollback(t *testing.T) {
	f := newRecipeFixture(t)
	tenantID := testutil.CreateTestTenant(t, f.db, "atomicity rollback")
	ctx := postgres.WithLivemode(context.Background(), false)

	failing := &failingPricingWriter{inner: f.pricingSvc}
	registry, err := Load()
	if err != nil {
		t.Fatalf("load registry: %v", err)
	}
	svc := NewService(f.db, NewPostgresStore(f.db), registry, failing)

	_, err = svc.Instantiate(ctx, tenantID, "anthropic_style", nil, InstantiateOptions{})
	if err == nil {
		t.Fatal("expected synthetic failure to surface")
	}

	for _, tbl := range []string{
		"meters", "plans", "plan_prices", "plan_price_versions", "recipe_instances",
	} {
		if n := countRows(t, f.db, tenantID, tbl); n != 0 {
			t.Errorf("%s after rollback: got %d, want 0", tbl, n)
		}
	}
}

// TestService_Instantiate_RLSIsolation installs anthropic_style for tenant
// A and verifies tenant B sees no instance — both via the recipe store
// (GetByKey returns ErrNotFound) and via ListRecipes (Instantiated is nil
// for the same recipe key).
func TestService_Instantiate_RLSIsolation(t *testing.T) {
	f := newRecipeFixture(t)
	tenantA := testutil.CreateTestTenant(t, f.db, "tenant A")
	tenantB := testutil.CreateTestTenant(t, f.db, "tenant B")
	ctx := postgres.WithLivemode(context.Background(), false)

	if _, err := f.svc.Instantiate(ctx, tenantA, "anthropic_style", nil, InstantiateOptions{}); err != nil {
		t.Fatalf("Instantiate for A: %v", err)
	}

	if _, err := f.store.GetByKey(ctx, tenantB, "anthropic_style"); !errors.Is(err, errs.ErrNotFound) {
		t.Fatalf("tenant B should not see A's instance: got %v", err)
	}

	items, err := f.svc.ListRecipes(ctx, tenantB)
	if err != nil {
		t.Fatalf("ListRecipes(B): %v", err)
	}
	for _, item := range items {
		if item.Key == "anthropic_style" && item.Instantiated != nil {
			t.Errorf("tenant B sees A's anthropic_style installed: %+v", item.Instantiated)
		}
	}
}

// TestService_Instantiate_PreviewParity verifies Preview and Instantiate
// render the same logical graph (same counts, same plan code). IDs naturally
// differ — Preview has none.
func TestService_Instantiate_PreviewParity(t *testing.T) {
	f := newRecipeFixture(t)
	tenantID := testutil.CreateTestTenant(t, f.db, "preview parity")
	ctx := postgres.WithLivemode(context.Background(), false)

	overrides := map[string]any{"currency": "EUR", "plan_code": "ai_eur", "plan_name": "AI EUR"}

	previewed, err := f.svc.Preview(ctx, "anthropic_style", overrides)
	if err != nil {
		t.Fatalf("Preview: %v", err)
	}
	inst, err := f.svc.Instantiate(ctx, tenantID, "anthropic_style", overrides, InstantiateOptions{})
	if err != nil {
		t.Fatalf("Instantiate: %v", err)
	}

	if got, want := len(inst.CreatedObjects.PriceIDs), len(previewed.Objects.Plans[0].Prices); got != want || got == 0 {
		t.Errorf("price count drift: instantiate=%d, preview=%d", got, want)
	}
	plans, err := f.pricingSvc.ListPlans(ctx, tenantID)
	if err != nil {
		t.Fatalf("ListPlans: %v", err)
	}
	if len(plans) != 1 {
		t.Fatalf("plans: got %d, want 1", len(plans))
	}
	if plans[0].Code != "ai_eur" {
		t.Errorf("plan code: got %q, want ai_eur", plans[0].Code)
	}
	if plans[0].Currency != "EUR" {
		t.Errorf("plan currency: got %q, want EUR", plans[0].Currency)
	}
}

// TestService_Instantiate_Idempotent proves apply is an idempotent EVENT
// (ADR-085): a second apply on an already-installed recipe is a no-op that
// returns the EXISTING instance — never a 409, never a duplicate plan. The
// badge is the idempotency gate; it short-circuits before any write.
func TestService_Instantiate_Idempotent(t *testing.T) {
	f := newRecipeFixture(t)
	tenantID := testutil.CreateTestTenant(t, f.db, "idempotent apply")
	ctx := postgres.WithLivemode(context.Background(), false)

	first, err := f.svc.Instantiate(ctx, tenantID, "anthropic_style", nil, InstantiateOptions{})
	if err != nil {
		t.Fatalf("first apply: %v", err)
	}
	plansAfterFirst := countRows(t, f.db, tenantID, "plans")
	metersAfterFirst := countRows(t, f.db, tenantID, "meters")
	pricesAfterFirst := countRows(t, f.db, tenantID, "plan_prices")

	second, err := f.svc.Instantiate(ctx, tenantID, "anthropic_style", nil, InstantiateOptions{})
	if err != nil {
		t.Fatalf("re-apply must be a no-op, not an error: %v", err)
	}
	if second.ID != first.ID {
		t.Errorf("re-apply returned a new instance id %q, want the existing %q (idempotent)", second.ID, first.ID)
	}

	// No duplicate objects — the badge gated the re-apply before any write.
	if n := countRows(t, f.db, tenantID, "recipe_instances"); n != 1 {
		t.Errorf("recipe_instances after re-apply: got %d, want 1", n)
	}
	if n := countRows(t, f.db, tenantID, "plans"); n != plansAfterFirst {
		t.Errorf("plans after re-apply: got %d, want %d (no duplicate plan)", n, plansAfterFirst)
	}
	if n := countRows(t, f.db, tenantID, "meters"); n != metersAfterFirst {
		t.Errorf("meters after re-apply: got %d, want %d", n, metersAfterFirst)
	}
	if n := countRows(t, f.db, tenantID, "plan_prices"); n != pricesAfterFirst {
		t.Errorf("plan_prices after re-apply: got %d, want %d", n, pricesAfterFirst)
	}
}

// TestService_Instantiate_GeneratesFreshPlanOnCollision proves the born-unique
// plan model (ADR-085): a plan already holding the recipe's default plan_code —
// with billing config the recipe never declares — does NOT block apply and is
// NEVER adopted or mutated. The recipe generates a fresh plan under a uniquified
// code (ai_api_pro_2), wired to the tokens meter (so usage bills), and leaves
// the operator's plan untouched. Mutation-verify: reinstate adopt-by-code and
// this fails — the recipe would 409 or wire its graph onto the divergent plan.
func TestService_Instantiate_GeneratesFreshPlanOnCollision(t *testing.T) {
	f := newRecipeFixture(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, f.db, "born-unique plan")

	// Operator already has a foreign plan under the recipe's default plan_code.
	pre, _, err := f.pricingSvc.CreatePlan(ctx, tenantID, pricing.CreatePlanInput{
		Code: "ai_api_pro", Name: "Operator Pro", Currency: "USD",
		BillingInterval: domain.BillingMonthly, BaseAmountCents: 9900,
		BaseBillTiming: domain.BillInAdvance,
	})
	if err != nil {
		t.Fatalf("pre-create plan: %v", err)
	}

	if _, err := f.svc.Instantiate(ctx, tenantID, "anthropic_style", nil, InstantiateOptions{}); err != nil {
		t.Fatalf("apply must succeed by generating a fresh plan, not refuse: %v", err)
	}

	// Two plans now: the operator's ai_api_pro + the recipe's uniquified one.
	plans, err := f.pricingSvc.ListPlans(ctx, tenantID)
	if err != nil {
		t.Fatalf("ListPlans: %v", err)
	}
	if len(plans) != 2 {
		t.Fatalf("plans: got %d, want 2 (operator's + recipe's fresh)", len(plans))
	}
	var recipePlan domain.Plan
	for _, p := range plans {
		if p.ID != pre.ID {
			recipePlan = p
		}
	}
	if recipePlan.Code != "ai_api_pro_2" {
		t.Errorf("generated plan code: got %q, want ai_api_pro_2 (uniquified, incumbent never renamed)", recipePlan.Code)
	}
	if meters, err := f.pricingSvc.PlanMeterIDs(ctx, tenantID, recipePlan.ID); err != nil || len(meters) == 0 {
		t.Errorf("generated plan must price the tokens meter — else usage silently bills $0 (meters %v, %v)", meters, err)
	}

	// The operator's plan is untouched.
	got, err := f.pricingSvc.GetPlan(ctx, tenantID, pre.ID)
	if err != nil {
		t.Fatalf("GetPlan: %v", err)
	}
	if got.Code != "ai_api_pro" || got.BaseBillTiming != domain.BillInAdvance || got.BaseAmountCents != 9900 {
		t.Errorf("operator's plan was mutated: code=%s timing=%s base=%d, want ai_api_pro/in_advance/9900 (never touched)", got.Code, got.BaseBillTiming, got.BaseAmountCents)
	}
}

// TestService_Instantiate_RefusesDivergentMeter proves the one loud guard
// (ADR-085): a same-key meter whose AGGREGATION contradicts the recipe (max vs
// the recipe's sum) is refused — adopting it would silently mis-roll-up usage.
// The whole tx rolls back; the operator's meter is untouched and nothing the
// recipe would create persists. Mutation-verify: drop meterConformanceDiff and
// this fails — the recipe prices the mis-aggregating meter.
func TestService_Instantiate_RefusesDivergentMeter(t *testing.T) {
	f := newRecipeFixture(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, f.db, "divergent meter refuse")

	// Operator already has a `tokens` meter — but counting max, not the
	// recipe's sum.
	if _, err := f.pricingSvc.CreateMeter(ctx, tenantID, pricing.CreateMeterInput{
		EventName: "tokens", Name: "Tokens", Unit: "tokens", Aggregation: "max",
	}); err != nil {
		t.Fatalf("pre-create meter: %v", err)
	}

	_, err := f.svc.Instantiate(ctx, tenantID, "anthropic_style", nil, InstantiateOptions{})
	if !errors.Is(err, errs.ErrAlreadyExists) {
		t.Fatalf("expected refusal (ErrAlreadyExists) adopting a divergent-aggregation meter, got %v", err)
	}

	// Mutation-verify: the entire tx rolled back — nothing the recipe would
	// have created persisted.
	for _, tbl := range []string{"recipe_instances", "plans", "plan_prices"} {
		if n := countRows(t, f.db, tenantID, tbl); n != 0 {
			t.Errorf("%s after refusal: got %d, want 0 (tx must roll back)", tbl, n)
		}
	}
	// Only the operator's pre-existing meter remains, unchanged.
	if n := countRows(t, f.db, tenantID, "meters"); n != 1 {
		t.Errorf("meters after refusal: got %d, want 1 (operator's meter only)", n)
	}
}

// countRowsMode is countRows with an explicit mode — RLS scopes the count to
// that mode's rows (post-0157 recipe_instances included).
func countRowsMode(t *testing.T, db *postgres.DB, tenantID, table string, live bool) int {
	t.Helper()
	tx, err := db.BeginTx(postgres.WithLivemode(context.Background(), live), postgres.TxTenant, tenantID)
	if err != nil {
		t.Fatalf("begin count tx (live=%v): %v", live, err)
	}
	defer postgres.Rollback(tx)
	var n int
	q := fmt.Sprintf("SELECT count(*) FROM %s", table)
	if err := tx.QueryRowContext(context.Background(), q).Scan(&n); err != nil {
		t.Fatalf("count %s (live=%v): %v", table, live, err)
	}
	return n
}

// TestService_Instantiate_LivemodePartitioned is the regression test for the
// livemode-blind badge (migration 0157). Before the fix recipe_instances had
// no livemode column, so a test-mode install's badge gated the live-mode
// install of the same recipe: the live apply returned the TEST instance as a
// "no-op" and Live showed "Installed" with zero live pricing objects. Each
// mode must install independently and stay idempotent within itself.
func TestService_Instantiate_LivemodePartitioned(t *testing.T) {
	f := newRecipeFixture(t)
	tenantID := testutil.CreateTestTenant(t, f.db, "livemode partitioned install")
	testCtx := postgres.WithLivemode(context.Background(), false)
	liveCtx := postgres.WithLivemode(context.Background(), true)

	testInst, err := f.svc.Instantiate(testCtx, tenantID, "anthropic_style", nil, InstantiateOptions{})
	if err != nil {
		t.Fatalf("test-mode apply: %v", err)
	}

	// The live-mode apply must be a REAL install, not a no-op against the
	// test badge: fresh instance, fresh live objects.
	liveInst, err := f.svc.Instantiate(liveCtx, tenantID, "anthropic_style", nil, InstantiateOptions{})
	if err != nil {
		t.Fatalf("live-mode apply after test-mode apply: %v", err)
	}
	if liveInst.ID == testInst.ID {
		t.Fatalf("live-mode apply no-op'd against the test-mode badge (returned instance %s) — the livemode-blind bug is back", testInst.ID)
	}
	for _, tbl := range []string{"recipe_instances", "plans", "meters", "plan_prices"} {
		if n := countRowsMode(t, f.db, tenantID, tbl, true); n == 0 {
			t.Errorf("%s: live mode has 0 rows after live apply — install landed in the wrong mode", tbl)
		}
	}

	// Badges are mode-scoped in both directions.
	got, err := f.store.GetByKey(testCtx, tenantID, "anthropic_style")
	if err != nil || got.ID != testInst.ID {
		t.Errorf("test-mode badge: got (%v, %v), want instance %s", got.ID, err, testInst.ID)
	}
	got, err = f.store.GetByKey(liveCtx, tenantID, "anthropic_style")
	if err != nil || got.ID != liveInst.ID {
		t.Errorf("live-mode badge: got (%v, %v), want instance %s", got.ID, err, liveInst.ID)
	}

	// Idempotency still holds WITHIN each mode: re-apply is a no-op.
	again, err := f.svc.Instantiate(liveCtx, tenantID, "anthropic_style", nil, InstantiateOptions{})
	if err != nil {
		t.Fatalf("live re-apply: %v", err)
	}
	if again.ID != liveInst.ID {
		t.Errorf("live re-apply minted a new instance %s, want existing %s", again.ID, liveInst.ID)
	}
	if n := countRowsMode(t, f.db, tenantID, "plans", true); n != 1 {
		t.Errorf("live plans after re-apply: got %d, want 1 (no duplicate)", n)
	}
}

// TestService_Instantiate_AdoptsAMeterOtherPlansPrice: a meter only counts
// and its prices are on the plans (ADR-130 §1, §15), so an install that
// adopts a meter another plan prices puts its own prices on its own new plan
// and changes nothing on the other plan. The install's currency is the new
// plan's; the other plan's currency does not matter, because a price takes
// its plan's currency. (Before ADR-130 the recipe added rules to the shared
// meter, which repriced every plan that billed it, so installs were refused
// on a meter in use or in another currency.)
func TestService_Instantiate_AdoptsAMeterOtherPlansPrice(t *testing.T) {
	f := newRecipeFixture(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, f.db, "adopt a meter in use")

	tokens, err := f.pricingSvc.CreateMeter(ctx, tenantID, pricing.CreateMeterInput{
		EventName: "tokens", Name: "Tokens", Unit: "tokens", Aggregation: "sum",
	})
	if err != nil {
		t.Fatalf("pre-create meter: %v", err)
	}
	custom := pricing.PriceInput{MeterID: tokens.ID, DimensionMatch: map[string]string{"model": "gpt-4o", "token_type": "input"},
		PriceShape: domain.PriceShape{Mode: domain.PricingPerUnit, UnitAmountCents: decimal.RequireFromString("0.0004")}}
	operator, _, err := f.pricingSvc.CreatePlan(ctx, tenantID, pricing.CreatePlanInput{
		Code: "operator_usd", Name: "Operator USD", Currency: "USD", BillingInterval: domain.BillingMonthly,
		Prices: []pricing.PriceInput{custom},
	})
	if err != nil {
		t.Fatalf("pre-create plan: %v", err)
	}
	if _, err := f.pricingSvc.AddPrice(ctx, tenantID, operator.ID, pricing.PriceInput{
		MeterID: tokens.ID, PriceShape: domain.PriceShape{Mode: domain.PricingPerUnit, UnitAmountCents: decimal.RequireFromString("0.0001")},
	}); err != nil {
		t.Fatalf("pre-create catch-all: %v", err)
	}
	before, err := f.pricingSvc.ListPlanPrices(ctx, tenantID, operator.ID)
	if err != nil {
		t.Fatalf("list prices: %v", err)
	}

	inst, err := f.svc.Instantiate(ctx, tenantID, "openai_style", map[string]any{"currency": "EUR"}, InstantiateOptions{})
	if err != nil {
		t.Fatalf("an EUR install over a meter a USD plan prices: %v", err)
	}
	if len(inst.CreatedObjects.MeterIDs) != 1 || inst.CreatedObjects.MeterIDs[0] != tokens.ID {
		t.Errorf("created meters = %v, want the adopted %s", inst.CreatedObjects.MeterIDs, tokens.ID)
	}
	if len(inst.CreatedObjects.PlanIDs) != 1 {
		t.Fatalf("created plans = %v, want one", inst.CreatedObjects.PlanIDs)
	}
	recipePlan, err := f.pricingSvc.GetPlan(ctx, tenantID, inst.CreatedObjects.PlanIDs[0])
	if err != nil || recipePlan.Currency != "EUR" {
		t.Errorf("recipe plan = %+v (%v), want EUR", recipePlan, err)
	}
	after, err := f.pricingSvc.ListPlanPrices(ctx, tenantID, operator.ID)
	if err != nil {
		t.Fatalf("list prices: %v", err)
	}
	if len(after) != len(before) {
		t.Fatalf("operator plan prices: %d after the install, %d before", len(after), len(before))
	}
	for i := range before {
		if after[i].ID != before[i].ID || !after[i].UnitAmountCents.Equal(before[i].UnitAmountCents) || after[i].UpdatedAt != before[i].UpdatedAt {
			t.Errorf("operator price %s changed: %+v → %+v", before[i].ID, before[i], after[i])
		}
	}
}
