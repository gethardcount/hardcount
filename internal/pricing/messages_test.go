package pricing

import "testing"

// TestMeterInUseMessage locks the D05 refusal: it names the count in plain
// words, the meter key, and the way that waits for the next period. A
// refused new rule also says when a new rule is allowed.
func TestMeterInUseMessage(t *testing.T) {
	cases := []struct {
		name    string
		subs    int
		newRule bool
		want    string
	}{
		{"one subscription", 1, false,
			`1 subscription uses meter "tokens", so the prices and aggregation that bill its usage are locked: a change now would change what they are billed for usage already recorded. To change a rate, publish a new version of the price; it applies from each subscription's next period.`},
		{"new rule", 3, true,
			`3 subscriptions use meter "tokens", so the prices and aggregation that bill its usage are locked: a change now would change what they are billed for usage already recorded. To change a rate, publish a new version of the price; it applies from each subscription's next period. A new price can be added only when the meter has no default price and no price with the same or a lower priority matches the same events.`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := meterInUseMessage("tokens", tc.subs, tc.newRule); got != tc.want {
				t.Errorf("got  %q\nwant %q", got, tc.want)
			}
		})
	}
}
