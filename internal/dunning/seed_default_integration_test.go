package dunning_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/sagarsuperuser/velox/internal/auth"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/dunning"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

// seedAccount runs the seed writer the way tenant.Service.Create does: on a
// live-mode tenant tx, so the per-mode GUC switching is exercised against
// real RLS and the 0021 livemode trigger.
func seedAccount(t *testing.T, db *postgres.DB, svc *dunning.Service, tenantID string) {
	t.Helper()
	ctx := postgres.WithLivemode(context.Background(), true)
	tx, err := db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer postgres.Rollback(tx)
	if err := svc.SeedDefaultPoliciesTx(ctx, tx, tenantID, nil); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

func listPolicies(t *testing.T, svc *dunning.Service, tenantID string, livemode bool) []domain.DunningPolicy {
	t.Helper()
	ps, err := svc.ListPolicies(postgres.WithLivemode(context.Background(), livemode), tenantID)
	if err != nil {
		t.Fatalf("list policies (livemode=%v): %v", livemode, err)
	}
	return ps
}

func onlySeed(t *testing.T, svc *dunning.Service, tenantID string, livemode bool) domain.DunningPolicy {
	t.Helper()
	ps := listPolicies(t, svc, tenantID, livemode)
	if len(ps) != 1 {
		t.Fatalf("livemode=%v: want exactly 1 policy after seeding, got %d", livemode, len(ps))
	}
	return ps[0]
}

func defaultOf(t *testing.T, svc *dunning.Service, tenantID string, livemode bool) domain.DunningPolicy {
	t.Helper()
	def, err := svc.GetDefaultPolicy(postgres.WithLivemode(context.Background(), livemode), tenantID)
	if err != nil {
		t.Fatalf("default (livemode=%v): %v", livemode, err)
	}
	return def
}

func operatorPolicy(name string) domain.DunningPolicy {
	return domain.DunningPolicy{
		Name: name, Enabled: true, MaxRetryAttempts: 2, GracePeriodDays: 1,
		RetrySchedule:           []string{"24h"},
		FinalSubscriptionAction: domain.SubActionPause, FinalInvoiceAction: domain.InvActionNone,
	}
}

// TestSeedDefaultPolicies_OnePerMode pins what a new account starts with
// (ADR-123): one enabled default per mode, with the exact seed values, and
// each mode sees only its own row.
//
// Mutation checks: drop one mode from the seed loop (the other mode's list
// is empty); seed an empty final action (it normalizes to pause and the
// value check fails).
func TestSeedDefaultPolicies_OnePerMode(t *testing.T) {
	db := testutil.SetupTestDB(t)
	svc := dunning.NewService(dunning.NewPostgresStore(db), nil, nil)
	tenantID := testutil.CreateTestTenant(t, db, "Seed One Per Mode")
	seedAccount(t, db, svc, tenantID)

	test := onlySeed(t, svc, tenantID, false)
	live := onlySeed(t, svc, tenantID, true)
	if test.ID == live.ID {
		t.Fatal("test and live mode must each have their own row")
	}
	for _, p := range []domain.DunningPolicy{test, live} {
		if !p.IsDefault || !p.Seeded || !p.Enabled {
			t.Errorf("%s: is_default=%v seeded=%v enabled=%v, want all true", p.ID, p.IsDefault, p.Seeded, p.Enabled)
		}
		if p.Name != "Default retries" || p.MaxRetryAttempts != 3 || p.GracePeriodDays != 3 {
			t.Errorf("%s: name=%q max=%d grace=%d", p.ID, p.Name, p.MaxRetryAttempts, p.GracePeriodDays)
		}
		if len(p.RetrySchedule) != 2 || p.RetrySchedule[0] != "96h" || p.RetrySchedule[1] != "168h" {
			t.Errorf("%s: retry_schedule=%v, want [96h 168h]", p.ID, p.RetrySchedule)
		}
		if p.FinalSubscriptionAction != domain.SubActionNone || p.FinalInvoiceAction != domain.InvActionNone {
			t.Errorf("%s: final actions %q/%q, want none/none — the seed must not pause, cancel or write off on the account's behalf",
				p.ID, p.FinalSubscriptionAction, p.FinalInvoiceAction)
		}
	}
}

// TestSeedDefaultPolicies_HandsOverDefault pins the one rule the seed needs
// beyond "exists": it holds the default only until someone creates a policy
// of their own (ADR-123). Otherwise a recipe's policy, or the operator's
// first one, would be created non-default and govern nothing.
//
// Mutation checks: remove the demote statement in upsertPolicyTx (subtest 1
// fails); stop clearing seeded on UPDATE (subtest 2 fails); stop clearing
// seeded in SetDefaultPolicy (subtest 3 fails).
func TestSeedDefaultPolicies_HandsOverDefault(t *testing.T) {
	db := testutil.SetupTestDB(t)
	svc := dunning.NewService(dunning.NewPostgresStore(db), nil, nil)
	testCtx := postgres.WithLivemode(context.Background(), false)

	t.Run("first created policy takes the default in its own mode only", func(t *testing.T) {
		tenantID := testutil.CreateTestTenant(t, db, "Seed Handover")
		seedAccount(t, db, svc, tenantID)
		seed := onlySeed(t, svc, tenantID, false)
		liveSeed := onlySeed(t, svc, tenantID, true)

		mine, err := svc.UpsertPolicy(testCtx, tenantID, operatorPolicy("mine"))
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		if !mine.IsDefault {
			t.Fatal("the first policy created after the seed must become the default")
		}
		if def := defaultOf(t, svc, tenantID, false); def.ID != mine.ID {
			t.Fatalf("test-mode default = %s, want the new policy %s", def.ID, mine.ID)
		}
		// The seed stays, demoted, so anything pointing at it still works.
		got, err := svc.GetPolicyByID(testCtx, tenantID, seed.ID)
		if err != nil {
			t.Fatalf("seed must still exist after hand-over: %v", err)
		}
		if got.IsDefault {
			t.Fatal("seed still default after hand-over")
		}
		// Live mode is untouched.
		if def := defaultOf(t, svc, tenantID, true); def.ID != liveSeed.ID {
			t.Fatalf("live default changed to %s by a test-mode create; want the live seed %s", def.ID, liveSeed.ID)
		}

		// A second create is an ordinary non-default policy again.
		second, err := svc.UpsertPolicy(testCtx, tenantID, operatorPolicy("second"))
		if err != nil {
			t.Fatalf("second create: %v", err)
		}
		if second.IsDefault {
			t.Fatal("only the first create takes over; the second must be non-default")
		}
	})

	// A policy created turned off (the API's enabled defaults to false; the
	// dashboard's switch can be off for a draft) must not take the default:
	// a disabled default starts no dunning, so every unassigned customer
	// would silently stop being retried. The next enabled create still takes
	// over. Mutation check: drop the p.Enabled guard on the demote.
	t.Run("a disabled policy leaves the seed as default", func(t *testing.T) {
		tenantID := testutil.CreateTestTenant(t, db, "Seed Disabled Create")
		seedAccount(t, db, svc, tenantID)
		seed := onlySeed(t, svc, tenantID, false)

		draft := operatorPolicy("draft")
		draft.Enabled = false
		created, err := svc.UpsertPolicy(testCtx, tenantID, draft)
		if err != nil {
			t.Fatalf("create disabled: %v", err)
		}
		if created.IsDefault {
			t.Fatal("a disabled policy must not take the default from the enabled seed")
		}
		if def := defaultOf(t, svc, tenantID, false); def.ID != seed.ID || !def.Enabled || !def.Seeded {
			t.Fatalf("default = %s (enabled=%v seeded=%v), want the enabled seed %s still seeded", def.ID, def.Enabled, def.Seeded, seed.ID)
		}

		mine, err := svc.UpsertPolicy(testCtx, tenantID, operatorPolicy("mine"))
		if err != nil {
			t.Fatalf("create enabled: %v", err)
		}
		if !mine.IsDefault {
			t.Fatal("the first enabled policy must still take over after a disabled one was created")
		}
	})

	t.Run("an edited seed keeps the default", func(t *testing.T) {
		tenantID := testutil.CreateTestTenant(t, db, "Seed Edited")
		seedAccount(t, db, svc, tenantID)
		seed := onlySeed(t, svc, tenantID, false)

		seed.FinalSubscriptionAction = domain.SubActionPause
		edited, err := svc.UpsertPolicy(testCtx, tenantID, seed)
		if err != nil {
			t.Fatalf("edit seed: %v", err)
		}
		if edited.Seeded {
			t.Fatal("editing the seed must clear seeded — it is the operator's policy now")
		}
		other, err := svc.UpsertPolicy(testCtx, tenantID, operatorPolicy("other"))
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		if other.IsDefault {
			t.Fatal("a create must not take the default from an edited seed")
		}
		if def := defaultOf(t, svc, tenantID, false); def.ID != seed.ID {
			t.Fatalf("default = %s, want the edited seed %s", def.ID, seed.ID)
		}
	})

	t.Run("a seed chosen as default keeps it", func(t *testing.T) {
		tenantID := testutil.CreateTestTenant(t, db, "Seed Chosen")
		seedAccount(t, db, svc, tenantID)
		seed := onlySeed(t, svc, tenantID, false)

		if err := svc.SetDefaultPolicy(testCtx, tenantID, seed.ID); err != nil {
			t.Fatalf("set default: %v", err)
		}
		other, err := svc.UpsertPolicy(testCtx, tenantID, operatorPolicy("other"))
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		if other.IsDefault {
			t.Fatal("a create must not take the default from a seed the operator chose")
		}
		if def := defaultOf(t, svc, tenantID, false); def.ID != seed.ID || def.Seeded {
			t.Fatalf("default = %s (seeded=%v), want the chosen seed %s with seeded cleared", def.ID, def.Seeded, seed.ID)
		}
	})

	t.Run("a customer assigned to the seed still resolves to it after hand-over", func(t *testing.T) {
		tenantID := testutil.CreateTestTenant(t, db, "Seed Assigned")
		seedAccount(t, db, svc, tenantID)
		seed := onlySeed(t, svc, tenantID, false)

		assigned := dunning.NewService(dunning.NewPostgresStore(db), nil, nil)
		assigned.SetCustomerPolicyReader(fixedPolicyReader{id: seed.ID})
		if _, err := svc.UpsertPolicy(testCtx, tenantID, operatorPolicy("takes over")); err != nil {
			t.Fatalf("create: %v", err)
		}
		eff, err := assigned.GetEffectivePolicyForCustomer(testCtx, tenantID, "cus_assigned")
		if err != nil {
			t.Fatalf("effective policy: %v", err)
		}
		if eff.ID != seed.ID {
			t.Fatalf("assigned customer resolves to %s, want the seed %s", eff.ID, seed.ID)
		}
	})
}

type fixedPolicyReader struct{ id string }

func (r fixedPolicyReader) GetDunningPolicyID(context.Context, string, string) (string, error) {
	return r.id, nil
}

// TestSeedDefaultPolicies_CreateRacesSeedEdit: a recipe-style create and an
// edit of the seed land at the same time. Whichever commits first, the mode
// ends with exactly one default and neither call fails: the edit's row lock
// makes the create's demote re-check "seeded" after the wait.
func TestSeedDefaultPolicies_CreateRacesSeedEdit(t *testing.T) {
	db := testutil.SetupTestDB(t)
	svc := dunning.NewService(dunning.NewPostgresStore(db), nil, nil)
	testCtx := postgres.WithLivemode(context.Background(), false)

	for i := 0; i < 10; i++ {
		tenantID := testutil.CreateTestTenant(t, db, "Seed Race")
		seedAccount(t, db, svc, tenantID)
		seed := onlySeed(t, svc, tenantID, false)
		seed.GracePeriodDays = 5

		var wg sync.WaitGroup
		var editErr, createErr error
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, editErr = svc.UpsertPolicy(testCtx, tenantID, seed)
		}()
		go func() {
			defer wg.Done()
			_, createErr = svc.UpsertPolicy(testCtx, tenantID, operatorPolicy("recipe"))
		}()
		wg.Wait()
		if editErr != nil || createErr != nil {
			t.Fatalf("iteration %d: edit err=%v create err=%v", i, editErr, createErr)
		}
		defaults := 0
		for _, p := range listPolicies(t, svc, tenantID, false) {
			if p.IsDefault {
				defaults++
			}
		}
		if defaults != 1 {
			t.Fatalf("iteration %d: %d defaults, want exactly 1", i, defaults)
		}
	}
}

// TestSeedDefaultPolicies_ExistingAccountsUntouched: accounts made before
// this change have no policy, and nothing adds one — migration 0180 only
// adds a column. A failed payment there is still a quiet skip, not a run.
func TestSeedDefaultPolicies_ExistingAccountsUntouched(t *testing.T) {
	db := testutil.SetupTestDB(t)
	svc := dunning.NewService(dunning.NewPostgresStore(db), nil, nil)
	tenantID := testutil.CreateTestTenant(t, db, "Pre-existing Account")

	for _, live := range []bool{false, true} {
		if ps := listPolicies(t, svc, tenantID, live); len(ps) != 0 {
			t.Fatalf("livemode=%v: an existing account gained %d policies", live, len(ps))
		}
	}
}

type recordingAudit struct {
	mu   sync.Mutex
	rows []map[string]any
}

func (a *recordingAudit) Log(_ context.Context, _, action, resourceType, resourceID, _ string, metadata map[string]any) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.rows = append(a.rows, map[string]any{"action": action, "type": resourceType, "id": resourceID, "metadata": metadata})
	return nil
}

// TestCreatePolicyRoute_DefaultTakeover drives POST /policies on a seeded
// account the way an integrator does. A body without "enabled" (so false)
// must leave the seed as the default; an enabled create takes the default,
// and its audit row says so, because the seed's demotion writes no row of
// its own and the audit log would otherwise not show why the default moved.
//
// Mutation checks: drop the p.Enabled guard on the demote (the first
// request's policy comes back is_default=true); drop the became_default
// metadata (the audit assertion fails).
func TestCreatePolicyRoute_DefaultTakeover(t *testing.T) {
	db := testutil.SetupTestDB(t)
	svc := dunning.NewService(dunning.NewPostgresStore(db), nil, nil)
	tenantID := testutil.CreateTestTenant(t, db, "Seed Create Route")
	seedAccount(t, db, svc, tenantID)
	seed := onlySeed(t, svc, tenantID, false)

	h := dunning.NewHandler(svc)
	rec := &recordingAudit{}
	h.SetAuditLogger(rec)
	routes := h.Routes()

	post := func(body string) domain.DunningPolicy {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/policies", strings.NewReader(body))
		req = req.WithContext(postgres.WithLivemode(auth.WithTenantID(context.Background(), tenantID), false))
		w := httptest.NewRecorder()
		routes.ServeHTTP(w, req)
		if w.Code != http.StatusCreated {
			t.Fatalf("status = %d, want 201; body: %s", w.Code, w.Body.String())
		}
		var p domain.DunningPolicy
		if err := json.Unmarshal(w.Body.Bytes(), &p); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return p
	}

	// No "enabled" key: Go's zero value makes this a disabled policy.
	draft := post(`{"name":"Enterprise","max_retry_attempts":2,"grace_period_days":1,"retry_schedule":["24h"],"final_subscription_action":"pause","final_invoice_action":"none"}`)
	if draft.Enabled || draft.IsDefault {
		t.Fatalf("draft enabled=%v is_default=%v, want a disabled non-default policy", draft.Enabled, draft.IsDefault)
	}
	if def := defaultOf(t, svc, tenantID, false); def.ID != seed.ID {
		t.Fatalf("default = %s after a disabled create, want the seed %s", def.ID, seed.ID)
	}
	if md, _ := rec.rows[len(rec.rows)-1]["metadata"].(map[string]any); len(md) != 0 {
		t.Errorf("audit metadata for a non-default create = %v, want none", md)
	}

	active := post(`{"name":"Active","enabled":true,"max_retry_attempts":2,"grace_period_days":1,"retry_schedule":["24h"],"final_subscription_action":"pause","final_invoice_action":"none"}`)
	if !active.IsDefault {
		t.Fatal("the first enabled create must take the default from the seed")
	}
	last := rec.rows[len(rec.rows)-1]
	md, _ := last["metadata"].(map[string]any)
	if last["id"] != active.ID || md["became_default"] != true {
		t.Fatalf("audit row = %v, want the create of %s with became_default=true", last, active.ID)
	}
}
