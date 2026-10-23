package dunning

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// TestScheduleCopy_FirstWaitAnchor: the copy that says where the first wait
// of retry_days counts from must not say only "the failed payment". A run
// for an invoice with no card on file (cause no_payment_method, ADR-060)
// starts with no payment failed: StartDunning anchors its first wait on the
// collection that found no card.
func TestScheduleCopy_FirstWaitAnchor(t *testing.T) {
	// The anchor phrase and the few characters after it: a phrase that goes
	// on with " or" or "(" names the no-card case too.
	anchor := regexp.MustCompile(`(?i)(counts?|counted|after) (from )?the failed payment.{0,4}`)
	for _, path := range []string{
		"../../api/openapi.yaml",
		"../../web-v2/public/openapi.yaml",
		"../../web-v2/src/pages/DunningPolicies.tsx",
		"../../web-v2/src/lib/dunningSchedule.ts",
		"../../docs/adr/036-dunning-campaigns-model.md",
		"../platform/migrate/sql/00206_dunning_retry_days.sql",
		"../domain/dunning.go",
		"../recipe/recipes/anthropic_style.yaml",
		"../recipe/recipes/openai_style.yaml",
		"../recipe/recipes/replicate_style.yaml",
		"../../web-v2/src/lib/api.ts",
		"handler.go",
		"service.go",
	} {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		// Comments and YAML wrap sentences across lines.
		flat := strings.Join(strings.Fields(strings.NewReplacer("//", " ", "--", " ", "*", " ", "#", " ").Replace(string(b))), " ")
		for _, m := range anchor.FindAllString(flat, -1) {
			if !strings.Contains(m, " or") && !strings.Contains(m, "(") {
				t.Errorf("%s says the first wait counts only from a failed payment: %q", path, m)
			}
		}
	}
}
