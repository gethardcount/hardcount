package litellm

import (
	"os"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// noCachePublished lists model families whose provider publishes NO
// cached-input price (verified against the official model pages
// 2026-07-26): OpenAI's pro-tier reasoning models follow that pattern, and
// gpt-4-turbo / gpt-3.5-turbo predate prompt caching entirely. For these,
// a recipe cache_read price would carry an invented rate — so the coherence
// requirement drops to input+output. If a provider starts publishing a
// cached price for one of these, remove it here and the test will demand
// the recipe price.
var noCachePublished = map[string]bool{
	"gpt-4-turbo":   true,
	"gpt-3.5-turbo": true,
	"o1-pro":        true,
	"o3-pro":        true,
	"gpt-5.4-pro":   true,
	"gpt-5.5-pro":   true,
}

// TestModelFamilies_EveryTokenPricedByARecipe pins mapper↔recipe coherence
// on BOTH axes: every recipe token the mapper can emit on the `model`
// dimension must have a price for every token_type the mapper can
// emit for it (input, output, cache_read, and for Anthropic families
// cache_write_5m and cache_write_1h) — or traffic on that (family, role)
// ingests fine and then silently doesn't bill at cycle close. The original version of this test checked
// only the model axis, which let a family with an input price but no output
// price pass while half its revenue leaked.
//
// A price with no token_type in its dimension_match (the embedding prices)
// is a wildcard: it matches any role, so it covers the family completely.
//
// Direction matters: mapper tokens ⊆ recipe prices. The reverse (recipe
// prices a family the mapper doesn't detect, or a role it doesn't emit) is
// harmless — direct API ingest can still use it. The write roles are
// required for Anthropic families only: an OpenAI record carries no
// cache_creation split, so the mapper never emits them for OpenAI traffic.
func TestModelFamilies_EveryTokenPricedByARecipe(t *testing.T) {
	// recipeRoles parses a recipe YAML into model → set of priced
	// "context_tier|token_type" pairs, with an empty part standing for a
	// price that does not name it (a wildcard on that dimension).
	recipeRoles := func(path string) map[string]map[string]bool {
		t.Helper()
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		var doc recipeDoc
		if err := yaml.Unmarshal(raw, &doc); err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		roles := map[string]map[string]bool{}
		for _, r := range doc.prices() {
			m := r.DimensionMatch["model"]
			if m == "" {
				continue
			}
			if roles[m] == nil {
				roles[m] = map[string]bool{}
			}
			roles[m][r.DimensionMatch[DimContextTier]+"|"+r.DimensionMatch["token_type"]] = true
		}
		return roles
	}

	anthropic := recipeRoles("../../recipe/recipes/anthropic_style.yaml")
	openai := recipeRoles("../../recipe/recipes/openai_style.yaml")

	// The roles MapPayload can emit — keep in sync with the TokenType*
	// constants and the emission sites in mapper.go.
	mapperRoles := []string{TokenTypeInput, TokenTypeOutput, TokenTypeCacheRead}
	anthropicWriteRoles := []string{TokenTypeCacheWrite5m, TokenTypeCacheWrite1h}

	for _, f := range modelFamilies {
		var roles map[string]map[string]bool
		var where string
		familyRoles := mapperRoles
		switch {
		case strings.HasPrefix(f.recipeToken, "claude-"):
			roles, where = anthropic, "anthropic_style.yaml"
			familyRoles = append(append([]string{}, mapperRoles...), anthropicWriteRoles...)
		case strings.HasPrefix(f.recipeToken, "gpt-"),
			strings.HasPrefix(f.recipeToken, "o1"),
			strings.HasPrefix(f.recipeToken, "o3"),
			strings.HasPrefix(f.recipeToken, "o4"),
			strings.HasPrefix(f.recipeToken, "text-embedding-"):
			roles, where = openai, "openai_style.yaml"
		default:
			t.Errorf("model family %q has no recipe mapping in this test — extend the switch when adding a provider", f.recipeToken)
			continue
		}
		priced := roles[f.recipeToken]
		if len(priced) == 0 {
			t.Errorf("mapper emits model=%q (id %q) but %s has NO price for it — that traffic ingests and then silently doesn't bill at cycle close", f.recipeToken, f.id, where)
			continue
		}
		// A model priced by prompt length needs a price for every (tier,
		// role), and no price without context_tier: such a price would price
		// both tiers at one rate. Every other model's prices name no tier.
		tier, tiered := promptTiers[f.recipeToken]
		tiers := []string{""}
		if tiered {
			tiers = []string{tier.upTo, tier.above}
		}
		for key := range priced {
			tier := strings.SplitN(key, "|", 2)[0]
			if tiered && tier == "" {
				t.Errorf("%s prices model=%q with a price that names no %s (%q): it would price both prompt tiers at one rate", where, f.recipeToken, DimContextTier, key)
			}
			if !tiered && tier != "" {
				t.Errorf("%s prices model=%q by %s=%q, but the mapper never sets %s for it, so that price never matches", where, f.recipeToken, DimContextTier, tier, DimContextTier)
			}
		}
		for _, tier := range tiers {
			if priced[tier+"|"] {
				continue // wildcard price (no token_type) covers every role
			}
			for _, role := range familyRoles {
				if role == TokenTypeCacheRead && noCachePublished[f.recipeToken] {
					continue
				}
				if !priced[tier+"|"+role] {
					t.Errorf("mapper can emit (model=%q, %s=%q, token_type=%q) but %s has no price for it — that role's traffic silently doesn't bill at cycle close", f.recipeToken, DimContextTier, tier, role, where)
				}
			}
		}
	}
}
