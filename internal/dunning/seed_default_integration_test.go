package dunning_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/dunning"
	"github.com/gethardcount/hardcount/internal/errs"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/testutil"
)

// seedAccount runs the seed writer on a
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

// modePolicy is the account's policy in one mode, or ErrNotFound.
func modePolicy(svc *dunning.Service, tenantID string, livemode bool) (domain.DunningPolicy, error) {
	return svc.GetPolicy(postgres.WithLivemode(context.Background(), livemode), tenantID)
}

// TestSeedDefaultPolicies_OnePerMode pins what a new account starts with
// (ADR-123): one enabled policy per mode, with the exact seed values, and
// each mode sees only its own row.
//
// Mutation checks: drop one mode from the seed loop (that mode has no
// policy); seed an empty final action (it normalizes to pause and the value
// check fails).
func TestSeedDefaultPolicies_OnePerMode(t *testing.T) {
	db := testutil.SetupTestDB(t)
	svc := dunning.NewService(dunning.NewPostgresStore(db), nil, nil)
	tenantID := testutil.CreateTestTenant(t, db, "Seed One Per Mode")
	seedAccount(t, db, svc, tenantID)

	var ids []string
	for _, live := range []bool{false, true} {
		p, err := modePolicy(svc, tenantID, live)
		if err != nil {
			t.Fatalf("livemode=%v: %v", live, err)
		}
		ids = append(ids, p.ID)
		if !p.Enabled || fmt.Sprint(p.RetryDays) != "[3 4 7]" {
			t.Errorf("%s: enabled=%v retry_days=%v, want enabled [3 4 7]", p.ID, p.Enabled, p.RetryDays)
		}
		if p.FinalSubscriptionAction != domain.SubActionNone || p.FinalInvoiceAction != domain.InvActionNone {
			t.Errorf("%s: final actions %q/%q, want none/none — the seed must not pause, cancel or write off on the account's behalf",
				p.ID, p.FinalSubscriptionAction, p.FinalInvoiceAction)
		}
	}
	if ids[0] == ids[1] {
		t.Fatal("test and live mode must each have their own row")
	}
}

// TestSeedDefaultPolicies_ExistingAccountsUntouched: an account made without
// the seed (before ADR-123) has no policy, and nothing adds one. A failed
// payment there is a quiet skip, not a run.
func TestSeedDefaultPolicies_ExistingAccountsUntouched(t *testing.T) {
	db := testutil.SetupTestDB(t)
	svc := dunning.NewService(dunning.NewPostgresStore(db), nil, nil)
	tenantID := testutil.CreateTestTenant(t, db, "Pre-existing Account")

	for _, live := range []bool{false, true} {
		if p, err := modePolicy(svc, tenantID, live); !errors.Is(err, errs.ErrNotFound) {
			t.Fatalf("livemode=%v: got %+v (%v), want no policy", live, p, err)
		}
	}
}
