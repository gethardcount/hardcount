package recipe

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"testing"

	"github.com/shopspring/decimal"
	"gopkg.in/yaml.v3"

	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/testutil"
)

// TestService_Instantiate_WritesTheRecipesPrices installs every built-in
// recipe in one account, each in its own currency, and checks each installed
// plan against its YAML file: the currency, and every price's meter, match,
// mode and amounts. The YAML is read here with its own decoder, not the
// recipe parser, so a fault in the parser or in the install shows as a
// wrong price.
func TestService_Instantiate_WritesTheRecipesPrices(t *testing.T) {
	f := newRecipeFixture(t)
	tenantID := testutil.CreateTestTenant(t, f.db, "installed prices")
	ctx := postgres.WithLivemode(context.Background(), false)

	installs := []struct{ key, currency, code string }{
		{"anthropic_style", "GBP", "ai_gbp"},
		{"openai_style", "EUR", "openai_eur"},
		{"replicate_style", "USD", "gpu_usd"},
	}
	for _, in := range installs {
		t.Run(in.key, func(t *testing.T) {
			want := yamlPrices(t, in.key)
			if len(want) == 0 {
				t.Fatalf("%s.yaml: read no prices", in.key)
			}

			inst, err := f.svc.Instantiate(ctx, tenantID, in.key,
				map[string]any{"currency": in.currency, "plan_code": in.code}, InstantiateOptions{})
			if err != nil {
				t.Fatalf("Instantiate: %v", err)
			}
			if len(inst.CreatedObjects.PlanIDs) != 1 {
				t.Fatalf("plan ids: %v, want one", inst.CreatedObjects.PlanIDs)
			}
			plan, err := f.pricingSvc.GetPlan(ctx, tenantID, inst.CreatedObjects.PlanIDs[0])
			if err != nil {
				t.Fatalf("GetPlan: %v", err)
			}
			if plan.Code != in.code || plan.Currency != in.currency {
				t.Errorf("plan: code %q currency %q, want %q %q", plan.Code, plan.Currency, in.code, in.currency)
			}

			meters, err := f.pricingSvc.ListMeters(ctx, tenantID)
			if err != nil {
				t.Fatalf("ListMeters: %v", err)
			}
			eventName := make(map[string]string, len(meters))
			for _, m := range meters {
				eventName[m.ID] = m.EventName
			}

			stored, err := f.pricingSvc.ListPlanPrices(ctx, tenantID, plan.ID)
			if err != nil {
				t.Fatalf("ListPlanPrices: %v", err)
			}
			got := make(map[string]domain.PlanPrice, len(stored))
			for _, p := range stored {
				got[priceKey(t, eventName[p.MeterID], p.DimensionMatch)] = p
			}
			if len(stored) != len(want) || len(got) != len(want) {
				t.Errorf("prices: %d stored (%d distinct), want %d", len(stored), len(got), len(want))
			}
			for _, key := range slices.Sorted(maps.Keys(want)) {
				w := want[key]
				g, ok := got[key]
				if !ok {
					t.Errorf("%s: not installed", key)
					continue
				}
				if diff := shapeDiff(t, w, g.PriceShape); diff != "" {
					t.Errorf("%s: %s", key, diff)
				}
			}
		})
	}
}

// yamlPrice is one price as the recipe file writes it.
type yamlPrice struct {
	Meter              string            `yaml:"meter"`
	DimensionMatch     map[string]string `yaml:"dimension_match"`
	Mode               string            `yaml:"mode"`
	UnitAmountCents    string            `yaml:"unit_amount_cents"`
	PackageSize        int64             `yaml:"package_size"`
	PackageAmountCents string            `yaml:"package_amount_cents"`
	Tiers              []struct {
		UpTo            *int64 `yaml:"up_to"`
		UnitAmountCents string `yaml:"unit_amount_cents"`
	} `yaml:"tiers"`
}

// yamlPrices reads the recipe file's one plan and returns its prices by
// meter and match.
func yamlPrices(t *testing.T, key string) map[string]yamlPrice {
	t.Helper()
	raw, err := recipeFS.ReadFile("recipes/" + key + ".yaml")
	if err != nil {
		t.Fatalf("read %s: %v", key, err)
	}
	var doc struct {
		Plans []struct {
			Prices []yamlPrice `yaml:"prices"`
		} `yaml:"plans"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse %s: %v", key, err)
	}
	if len(doc.Plans) != 1 {
		t.Fatalf("%s: %d plans, this test reads one", key, len(doc.Plans))
	}
	out := make(map[string]yamlPrice, len(doc.Plans[0].Prices))
	for _, p := range doc.Plans[0].Prices {
		k := priceKey(t, p.Meter, p.DimensionMatch)
		if _, dup := out[k]; dup {
			t.Fatalf("%s: two prices for %s", key, k)
		}
		out[k] = p
	}
	return out
}

func priceKey(t *testing.T, meter string, match map[string]string) string {
	t.Helper()
	if match == nil {
		match = map[string]string{}
	}
	b, err := json.Marshal(match) // map keys marshal sorted
	if err != nil {
		t.Fatal(err)
	}
	return meter + " " + string(b)
}

// shapeDiff says how the stored shape differs from the file's, or "".
func shapeDiff(t *testing.T, w yamlPrice, g domain.PriceShape) string {
	t.Helper()
	dec := func(s string) decimal.Decimal {
		if s == "" {
			return decimal.Zero
		}
		d, err := decimal.NewFromString(s)
		if err != nil {
			t.Fatalf("amount %q: %v", s, err)
		}
		return d
	}
	if string(g.Mode) != w.Mode {
		return fmt.Sprintf("mode %q, file says %q", g.Mode, w.Mode)
	}
	if !g.UnitAmountCents.Equal(dec(w.UnitAmountCents)) {
		return fmt.Sprintf("unit_amount_cents %s, file says %s", g.UnitAmountCents, w.UnitAmountCents)
	}
	if g.PackageSize != w.PackageSize || !g.PackageAmountCents.Equal(dec(w.PackageAmountCents)) {
		return fmt.Sprintf("package %d for %s, file says %d for %s", g.PackageSize, g.PackageAmountCents, w.PackageSize, w.PackageAmountCents)
	}
	if len(g.Tiers) != len(w.Tiers) {
		return fmt.Sprintf("%d tiers, file says %d", len(g.Tiers), len(w.Tiers))
	}
	for i, wt := range w.Tiers {
		gt := g.Tiers[i]
		sameBound := (gt.UpTo == nil) == (wt.UpTo == nil) && (gt.UpTo == nil || *gt.UpTo == *wt.UpTo)
		if !sameBound || !gt.UnitAmountCents.Equal(dec(wt.UnitAmountCents)) {
			return fmt.Sprintf("tier %d differs from the file", i)
		}
	}
	return ""
}
