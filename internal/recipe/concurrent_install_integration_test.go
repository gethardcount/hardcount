package recipe

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/errs"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

// racingPricingWriter is a concurrent-resolver fake. On the install's first
// meter read it runs a second, complete install (race) and lets it commit,
// so the second install lands after this install read the badge and before
// it writes anything. With beforeRead the race commits before the meter read
// (this install then adopts the racer's meter); without it, after the read
// and before the meter insert.
type racingPricingWriter struct {
	PricingWriter
	beforeRead bool
	race       func()
	fired      bool
}

func (r *racingPricingWriter) GetMeterByEventNameTx(ctx context.Context, tx *sql.Tx, eventName string) (domain.Meter, error) {
	if r.fired {
		return r.PricingWriter.GetMeterByEventNameTx(ctx, tx, eventName)
	}
	r.fired = true
	if r.beforeRead {
		r.race()
		return r.PricingWriter.GetMeterByEventNameTx(ctx, tx, eventName)
	}
	m, err := r.PricingWriter.GetMeterByEventNameTx(ctx, tx, eventName)
	r.race()
	return m, err
}

// TestService_Instantiate_ConcurrentInstall pins what the API says about two
// installs that run at the same time: the second one fails with
// ErrAlreadyExists (409), installs nothing, and a retry returns the install
// the first one made, or installs on top of it.
func TestService_Instantiate_ConcurrentInstall(t *testing.T) {
	cases := []struct {
		name       string
		racer      string
		loser      string
		beforeRead bool
	}{
		// The racer commits the meter, plan and badge; this install adopts
		// the meter, takes the next plan code and then meets the badge.
		{"same recipe, race before the meter read", "anthropic_style", "anthropic_style", true},
		// This install read no meter and then meets the racer's meter.
		{"same recipe, race after the meter read", "anthropic_style", "anthropic_style", false},
		// Two AI recipes share the tokens meter.
		{"other recipe, race after the meter read", "openai_style", "anthropic_style", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newRecipeFixture(t)
			tenantID := testutil.CreateTestTenant(t, f.db, "concurrent install")
			ctx := postgres.WithLivemode(context.Background(), false)
			registry, err := Load()
			if err != nil {
				t.Fatalf("load registry: %v", err)
			}

			var racerInst domain.RecipeInstance
			racing := &racingPricingWriter{
				PricingWriter: f.pricingSvc,
				beforeRead:    tc.beforeRead,
				race: func() {
					inst, err := f.svc.Instantiate(ctx, tenantID, tc.racer, nil, InstantiateOptions{})
					if err != nil {
						t.Fatalf("racing install: %v", err)
					}
					racerInst = inst
				},
			}
			svc := NewService(f.db, NewPostgresStore(f.db), registry, racing)

			counts := func() map[string]int {
				out := map[string]int{}
				for _, tbl := range []string{"meters", "plans", "plan_prices", "recipe_instances"} {
					out[tbl] = countRows(t, f.db, tenantID, tbl)
				}
				return out
			}

			_, err = svc.Instantiate(ctx, tenantID, tc.loser, nil, InstantiateOptions{})
			if !racing.fired {
				t.Fatal("the race never ran")
			}
			if !errors.Is(err, errs.ErrAlreadyExists) {
				t.Fatalf("second install: got %v, want ErrAlreadyExists (409)", err)
			}

			// Nothing of the second install is left: only the racer's rows.
			afterRace := counts()
			want := map[string]int{"meters": 1, "plans": 1, "recipe_instances": 1}
			for tbl, n := range want {
				if afterRace[tbl] != n {
					t.Errorf("%s after the failed install: got %d, want %d", tbl, afterRace[tbl], n)
				}
			}

			retry, err := f.svc.Instantiate(ctx, tenantID, tc.loser, nil, InstantiateOptions{})
			if err != nil {
				t.Fatalf("retry: %v", err)
			}
			if tc.loser == tc.racer {
				if retry.ID != racerInst.ID {
					t.Errorf("retry returned instance %q, want the racer's %q", retry.ID, racerInst.ID)
				}
				if got := counts(); got["plans"] != afterRace["plans"] || got["recipe_instances"] != 1 {
					t.Errorf("retry of an installed recipe wrote rows: %v, want %v", got, afterRace)
				}
				return
			}
			// Another recipe: the retry installs, adopting the racer's meter.
			if got := retry.CreatedObjects.MeterIDs; len(got) != 1 || got[0] != racerInst.CreatedObjects.MeterIDs[0] {
				t.Errorf("retry meters: got %v, want the racer's %v", got, racerInst.CreatedObjects.MeterIDs)
			}
			if got := counts(); got["meters"] != 1 || got["plans"] != 2 || got["recipe_instances"] != 2 {
				t.Errorf("after the retry: got %v, want 1 meter, 2 plans, 2 installs", got)
			}
		})
	}
}
