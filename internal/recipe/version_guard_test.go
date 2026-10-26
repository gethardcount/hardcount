package recipe

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"testing"
)

// pinnedCatalog maps "<key>@<version>" to the SHA-256 of that recipe's YAML
// bytes. A recipe's version string is its cache-buster for operators ("is
// this the template I installed?") — but nothing structural forces a bump
// when the YAML moves, and the openai/anthropic catalogs each shipped
// content changes under an unmoved version before this guard existed
// (found by the 2026-07-26 recipe e2e review). Editing a recipe now fails
// this test until you EITHER bump the version and re-pin, or admit the
// content change under the same version is a bug.
//
// To update after a deliberate change: bump `version:` in the YAML, then
// replace the pin with the value from the test's failure message.
// MAJOR bumps on 2026-08-05: ADR-112 replaced the dunning block's
// `final_action` key with `final_subscription_action` +
// `final_invoice_action`, and parseRecipe REJECTS the old key by name. A
// recipe pinned at the previous version no longer parses, which is a
// breaking DSL change, not an edit.
// anthropic_style 6.0.0 on 2026-10-22: the new lineup's prices, Haiku 5.5's
// two prompt tiers (rules on context_tier), and Sonnet 5 at $2 / $10. A
// MAJOR bump because an install of 5.0.0 does not get these on re-apply.
// MAJOR bumps on 2026-10-23 (all three): the dunning block's max_retries
// and intervals_hours became one list, retry_days, and parseRecipe refuses
// the old keys. The policy a recipe makes retries on the same days.
// Re-pinned under the same versions, unreleased, when only a comment changed.
// MAJOR bumps on 2026-10-24 (anthropic 8.0.0, openai 7.0.0, replicate
// 5.0.0): a meter is named by `event_name`, not `key`, and a pricing rule has
// no `aggregation_mode` (ADR-130 §2). parseRecipe refuses both old keys, so
// a recipe at the previous version no longer parses. Prices are unchanged.
// MAJOR bumps on 2026-10-24 again (anthropic 9.0.0, openai 8.0.0,
// replicate 6.0.0): the price shapes take ADR-130 §3's names, mode per_unit
// and unit_amount_cents, and parseRecipe refuses flat_amount_cents. Prices
// are unchanged.
// MAJOR bumps on 2026-10-26 (anthropic 10.0.0, openai 9.0.0, replicate
// 7.0.0): a recipe holds only meters and plans with their prices (ADR-130
// §15). rating_rules, pricing_rules, dunning and webhook are gone, and
// parseRecipe refuses them. Prices are unchanged; an install no longer
// creates a dunning policy or a webhook endpoint.
var pinnedCatalog = map[string]string{
	"anthropic_style@10.0.0": "fbe0a5693290dd7042a3664eb8ad7cd37dc8c25247f74bd27c4a1403e4aaddbf",
	"openai_style@9.0.0":     "fd2564361133ba391063b343d76f820d2f7dc20f4f9ad2bec8a31fa85342a50a",
	"replicate_style@7.0.0":  "6e32b8e7f3f792754795221ad467bee47174fa3bd6a2ee9183598d92fcfc9302",
}

// TestRecipeCatalog_VersionMovesWithContent asserts every embedded recipe's
// (version, content-hash) pair matches the pin above, in both directions:
// same version + different bytes = a content change hiding under a stale
// version; a version not in the pin map = a bump that forgot to re-pin.
func TestRecipeCatalog_VersionMovesWithContent(t *testing.T) {
	entries, err := recipeFS.ReadDir("recipes")
	if err != nil {
		t.Fatalf("read embedded recipes dir: %v", err)
	}
	seen := map[string]bool{}
	for _, e := range entries {
		raw, err := recipeFS.ReadFile("recipes/" + e.Name())
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		rec, err := parseRecipe(raw)
		if err != nil {
			t.Fatalf("parse %s: %v", e.Name(), err)
		}
		sum := sha256.Sum256(raw)
		got := hex.EncodeToString(sum[:])
		pinKey := fmt.Sprintf("%s@%s", rec.Key, rec.Version)
		seen[pinKey] = true

		want, ok := pinnedCatalog[pinKey]
		if !ok {
			t.Errorf("%s: version %s has no pin — add %q: %q to pinnedCatalog (and make sure the version was bumped on purpose)",
				e.Name(), rec.Version, pinKey, got)
			continue
		}
		if want != got {
			t.Errorf("%s: content changed but version is still %s — bump `version:` and re-pin (new hash %s). A moved template under an unmoved version lies to operators about what they installed.",
				e.Name(), rec.Version, got)
		}
	}
	for pinKey := range pinnedCatalog {
		if !seen[pinKey] {
			t.Errorf("pinnedCatalog entry %q matches no embedded recipe — prune the stale pin", pinKey)
		}
	}
}
