package recipe

import (
	"fmt"
	"strings"

	"github.com/gethardcount/hardcount/internal/domain"
)

// This file holds the recipe's meter adoption guard. When Instantiate finds
// an existing meter with a recipe meter's event name, it may adopt that
// meter only if its aggregation matches what the recipe declares. A meter
// that differs is refused (409), never adopted without a word. Aggregation
// is the only field of a meter that billing reads. Usage rolls up under it,
// so adopting a meter with another aggregation would bill wrongly without
// a word.
//
// There is no clause for live subscriptions, on purpose. Adoption only
// reads the meter, and the recipe's prices go on a fresh plan (ADR-085,
// ADR-130 §15), so no other plan's prices change. Names and units are
// display labels, which billing does not read.

// fieldDiff is one billing-affecting field where an existing object differs
// from what the recipe declares. Want is the recipe's value, and Got is the
// existing one.
type fieldDiff struct {
	Field string
	Want  string
	Got   string
}

// meterConformanceDiff returns the billing-affecting differences between an
// existing meter (matched by key) and the recipe's declared meter. Only
// Aggregation is compared, because it is the only field billing reads.
func meterConformanceDiff(existing domain.Meter, rm domain.RecipeMeter) []fieldDiff {
	var diffs []fieldDiff
	if existing.Aggregation != rm.Aggregation {
		diffs = append(diffs, fieldDiff{"usage aggregation", rm.Aggregation, existing.Aggregation})
	}
	return diffs
}

// formatDiffs renders the operator-facing copy "field: recipe wants X,
// existing is Y".
func formatDiffs(diffs []fieldDiff) string {
	parts := make([]string, 0, len(diffs))
	for _, d := range diffs {
		parts = append(parts, fmt.Sprintf("%s: recipe wants %s, existing is %s", d.Field, d.Want, d.Got))
	}
	return strings.Join(parts, "; ")
}

// meterConflictMessage is the refusal when a meter the recipe would create
// already exists and adds up usage another way. Aggregation is the only
// field compared (meterConformanceDiff). A meter's aggregation can change,
// on the meter page or with PATCH /v1/meters/{id}, until the meter has a
// usage event. After that it is fixed, and a change gets 409
// meter_has_usage (ADR-130 §2). So the message names that condition,
// instead of offering a step the operator cannot take.
func meterConflictMessage(eventName string, diffs []fieldDiff) string {
	return fmt.Sprintf("Meter %q already exists and adds up usage another way (%s), so the recipe can't use it. "+
		"Its aggregation can change only until the meter has a usage event; after that it is fixed.", eventName, formatDiffs(diffs))
}
