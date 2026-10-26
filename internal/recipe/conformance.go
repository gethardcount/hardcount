package recipe

import (
	"fmt"
	"strings"

	"github.com/sagarsuperuser/velox/internal/domain"
)

// This file implements the recipe's meter ADOPTION GUARD. When Instantiate
// finds an existing meter with a recipe meter's event name, it may adopt it
// only if its aggregation matches what the recipe declares; a divergent
// match is REFUSED (409), never silently adopted. Aggregation is the sole
// billing-consulted field of a meter: usage rolls up under it, so adopting a
// meter with another aggregation would silently mis-bill.
//
// There is deliberately NO live-subscription clause: adoption only READS
// the meter, and the recipe's prices go on a fresh plan (ADR-085, ADR-130
// §15), so no other plan's prices change. Names and units are display
// labels, not billing-consulted.

// fieldDiff is one billing-affecting field where an existing object diverges
// from what the recipe declares. Want is the recipe's value, Got the existing.
type fieldDiff struct {
	Field string
	Want  string
	Got   string
}

// meterConformanceDiff returns billing-affecting divergence between an existing
// meter (matched by key) and the recipe's declared meter. Only Aggregation is
// compared — the sole billing-consulted field.
func meterConformanceDiff(existing domain.Meter, rm domain.RecipeMeter) []fieldDiff {
	var diffs []fieldDiff
	if existing.Aggregation != rm.Aggregation {
		diffs = append(diffs, fieldDiff{"usage aggregation", rm.Aggregation, existing.Aggregation})
	}
	return diffs
}

// formatDiffs renders operator-facing "field: recipe wants X, existing is Y" copy.
func formatDiffs(diffs []fieldDiff) string {
	parts := make([]string, 0, len(diffs))
	for _, d := range diffs {
		parts = append(parts, fmt.Sprintf("%s: recipe wants %s, existing is %s", d.Field, d.Want, d.Got))
	}
	return strings.Join(parts, "; ")
}

// meterConflictMessage is the refusal when a meter the recipe would create
// already exists and adds up usage another way. Aggregation is the only
// field compared (meterConformanceDiff). A meter's aggregation can change
// (the meter page or PATCH /v1/meters/{id}) until it has a usage event; then
// it is fixed, 409 meter_has_usage (ADR-130 §2). So the message names that
// condition instead of offering a step the operator can't take.
func meterConflictMessage(eventName string, diffs []fieldDiff) string {
	return fmt.Sprintf("Meter %q already exists and adds up usage another way (%s), so the recipe can't use it. "+
		"Its aggregation can change only until the meter has a usage event; after that it is fixed.", eventName, formatDiffs(diffs))
}
