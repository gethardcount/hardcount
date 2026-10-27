package recipe

import "testing"

// TestConflictMessages locks the install refusal: a meter the recipe would
// adopt adds up usage another way. The difference stays in the text and the
// meter's event name stays (operators find it on the Pricing page by it).
func TestConflictMessages(t *testing.T) {
	got := meterConflictMessage("tokens", []fieldDiff{{"usage aggregation", "sum", "max"}})
	want := `Meter "tokens" already exists and adds up usage another way (usage aggregation: recipe wants sum, existing is max), so the recipe can't use it. Its aggregation can change only until the meter has a usage event; after that it is fixed.`
	if got != want {
		t.Errorf("got  %q\nwant %q", got, want)
	}
}
