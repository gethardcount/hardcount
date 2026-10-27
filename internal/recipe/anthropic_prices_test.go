package recipe

import (
	"fmt"
	"testing"

	"github.com/shopspring/decimal"
	"gopkg.in/yaml.v3"
)

// anthropicPricesChecked is the date anthropicListPrices was last compared
// with Anthropic's pricing page. When you check the page again, update the
// table and this date (docs/dev/release-checklist.md).
const anthropicPricesChecked = "2026-10-22"

// anthropicPricingPage is the page the table is copied from.
const anthropicPricingPage = "https://platform.claude.com/docs/en/about-claude/pricing"

// anthropicListPrice is one model's list price in dollars per million tokens,
// by token type. tier is the context_tier value for a model priced by prompt
// length, else "".
type anthropicListPrice struct {
	model, tier                        string
	input, output, cacheRead, w5m, w1h string
	onPage                             bool
	checked                            string
}

// anthropicListPrices is Anthropic's pricing page as of
// anthropicPricesChecked. Rows with onPage false are models the page no
// longer lists; the recipe keeps their last published price, checked on the
// date given.
var anthropicListPrices = []anthropicListPrice{
	{"claude-fable-5.1", "", "10", "50", "0.25", "12.50", "20", true, anthropicPricesChecked},
	{"claude-mythos-5.1", "", "10", "50", "0.25", "12.50", "20", true, anthropicPricesChecked},
	{"claude-fable-5", "", "10", "50", "1", "12.50", "20", true, anthropicPricesChecked},
	{"claude-mythos-5", "", "10", "50", "1", "12.50", "20", true, anthropicPricesChecked},
	{"claude-opus-5.5", "", "4", "20", "0.20", "5", "8", true, anthropicPricesChecked},
	{"claude-opus-5", "", "5", "25", "0.50", "6.25", "10", true, anthropicPricesChecked},
	{"claude-opus-4.8", "", "5", "25", "0.50", "6.25", "10", true, anthropicPricesChecked},
	{"claude-opus-4.7", "", "5", "25", "0.50", "6.25", "10", true, anthropicPricesChecked},
	{"claude-opus-4.6", "", "5", "25", "0.50", "6.25", "10", true, anthropicPricesChecked},
	{"claude-opus-4.5", "", "5", "25", "0.50", "6.25", "10", true, anthropicPricesChecked},
	{"claude-opus-4.1", "", "15", "75", "1.50", "18.75", "30", true, anthropicPricesChecked},
	{"claude-opus-4", "", "15", "75", "1.50", "18.75", "30", true, anthropicPricesChecked},
	{"claude-sonnet-5.5", "", "2", "10", "0.10", "2.50", "4", true, anthropicPricesChecked},
	{"claude-sonnet-5", "", "2", "10", "0.20", "2.50", "4", true, anthropicPricesChecked},
	{"claude-sonnet-4.6", "", "3", "15", "0.30", "3.75", "6", true, anthropicPricesChecked},
	{"claude-sonnet-4.5", "", "3", "15", "0.30", "3.75", "6", true, anthropicPricesChecked},
	{"claude-sonnet-4", "", "3", "15", "0.30", "3.75", "6", true, anthropicPricesChecked},
	{"claude-haiku-5.5", "prompt_up_to_100k", "0.10", "0.50", "0.01", "0.125", "0.20", true, anthropicPricesChecked},
	{"claude-haiku-5.5", "prompt_over_100k", "0.50", "2.50", "0.05", "0.625", "1", true, anthropicPricesChecked},
	{"claude-haiku-4.5", "", "1", "5", "0.10", "1.25", "2", true, anthropicPricesChecked},
	{"claude-3.5-haiku", "", "0.80", "4", "0.08", "1", "1.60", true, anthropicPricesChecked},
	// No longer on the page; the recipe keeps the last published price.
	{"claude-3.5-sonnet", "", "3", "15", "0.30", "3.75", "6", false, "2026-07-26"},
	{"claude-3-opus", "", "15", "75", "1.50", "18.75", "30", false, "2026-07-26"},
	{"claude-3-sonnet", "", "3", "15", "0.30", "3.75", "6", false, "2026-07-26"},
	{"claude-3-haiku", "", "0.25", "1.25", "0.025", "0.3125", "0.50", false, "2026-07-26"},
}

// TestAnthropicStyle_PricesMatchThePricingPage checks the anthropic_style
// recipe against anthropicListPrices both ways: every price's rate is the
// table's dollars per million tokens × 0.0001 (cents per token), exactly,
// and every table entry has its price. A price change on the page becomes a
// list of the exact recipe prices to fix.
func TestAnthropicStyle_PricesMatchThePricingPage(t *testing.T) {
	raw, err := recipeFS.ReadFile("recipes/anthropic_style.yaml")
	if err != nil {
		t.Fatalf("read recipe: %v", err)
	}
	var doc struct {
		Plans []struct {
			Code   string `yaml:"code"`
			Prices []struct {
				DimensionMatch  map[string]string `yaml:"dimension_match"`
				UnitAmountCents string            `yaml:"unit_amount_cents"`
			} `yaml:"prices"`
		} `yaml:"plans"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse recipe: %v", err)
	}

	perMTokToCents := decimal.RequireFromString("0.0001")
	type wantRate struct {
		cents  decimal.Decimal
		source string
	}
	want := map[string]wantRate{} // "model|tier|token_type" → cents per token
	for _, p := range anthropicListPrices {
		source := "the pricing page (checked " + p.checked + ")"
		if !p.onPage {
			source = "its last published price (checked " + p.checked + "; no longer on the page)"
		}
		for role, dollars := range map[string]string{
			"input": p.input, "output": p.output, "cache_read": p.cacheRead,
			"cache_write_5m": p.w5m, "cache_write_1h": p.w1h,
		} {
			want[p.model+"|"+p.tier+"|"+role] = wantRate{decimal.RequireFromString(dollars).Mul(perMTokToCents), source}
		}
	}

	seen := map[string]bool{}
	for _, plan := range doc.Plans {
		for i, pr := range plan.Prices {
			key := fmt.Sprintf("%s|%s|%s", pr.DimensionMatch["model"], pr.DimensionMatch["context_tier"], pr.DimensionMatch["token_type"])
			where := fmt.Sprintf("plan %s prices[%d]", plan.Code, i)
			seen[key] = true
			w, ok := want[key]
			if !ok {
				t.Errorf("%s (%s) has no row in anthropicListPrices: check the price on %s and add it", where, key, anthropicPricingPage)
				continue
			}
			got, err := decimal.NewFromString(pr.UnitAmountCents)
			if err != nil {
				t.Errorf("%s: rate %q: %v", where, pr.UnitAmountCents, err)
				continue
			}
			if !got.Equal(w.cents) {
				t.Errorf("%s (%s) is %s cents per token; %s is %s", where, key, got, w.source, w.cents)
			}
		}
	}
	for key := range want {
		if !seen[key] {
			t.Errorf("anthropicListPrices has %s but the recipe has no price for it", key)
		}
	}
}
