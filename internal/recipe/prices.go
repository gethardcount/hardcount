package recipe

import (
	"fmt"

	"github.com/sagarsuperuser/velox/internal/domain"
)

// recipePrices translates a recipe's rating rules and pricing rules into the
// prices its install puts on each plan (ADR-130 §15, PR-4): for each meter a
// plan names, each pricing rule of that meter becomes one price, with the
// rate of the rule's rating rule. Priority is gone: the most specific price
// wins, and every recipe row has priority 100, so the conformance test runs
// the tie check on each recipe instead (TestEmbeddedRecipes_PricesHaveAWinner).
// A rating rule's currency is the recipe's own; prices take the plan's. The
// prices come out in plan order, then pricing-rule order.
func recipePrices(r domain.Recipe) ([]domain.RecipePrice, error) {
	rules := make(map[string]domain.RecipeRatingRule, len(r.RatingRules))
	for _, rr := range r.RatingRules {
		rules[rr.Key] = rr
	}
	var out []domain.RecipePrice
	for _, p := range r.Plans {
		onPlan := make(map[string]bool, len(p.EventNames))
		for _, e := range p.EventNames {
			onPlan[e] = true
		}
		for i, pr := range r.PricingRules {
			if !onPlan[pr.EventName] {
				continue
			}
			rule, ok := rules[pr.RatingRuleKey]
			if !ok {
				return nil, fmt.Errorf("recipe %q: pricing_rules[%d] references unknown rating_rule %q", r.Key, i, pr.RatingRuleKey)
			}
			match := make(map[string]string, len(pr.DimensionMatch))
			for k, v := range pr.DimensionMatch {
				s, ok := v.(string)
				if !ok {
					return nil, fmt.Errorf("recipe %q: pricing_rules[%d] dimension_match[%q] must be a string, got %T", r.Key, i, k, v)
				}
				match[k] = s
			}
			out = append(out, domain.RecipePrice{
				PlanCode:       p.Code,
				EventName:      pr.EventName,
				DimensionMatch: match,
				PriceShape: domain.PriceShape{
					Mode:               rule.Mode,
					UnitAmountCents:    rule.UnitAmountCents,
					Tiers:              rule.Tiers,
					PackageSize:        rule.PackageSize,
					PackageAmountCents: rule.PackageAmountCents,
				},
			})
		}
	}
	return out, nil
}
