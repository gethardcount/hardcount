package pricing

import "testing"

// TestLastPriceMessage locks the refusal for removing a meter's last price:
// plan codes stay in the text (the integration tests find plans by them),
// counts read in plain words, and "plan … bills" agrees with one or many.
func TestLastPriceMessage(t *testing.T) {
	cases := []struct {
		name string
		live []string
		want string
	}{
		{"one plan", []string{"pro (" + subscriptionCount(1) + ")"},
			`This is the last price on meter "tokens", and plan pro (1 subscription) bills it, so that usage would go unbilled. Add another price first, or move those subscriptions to a plan without this meter.`},
		{"two plans", []string{"pro (" + subscriptionCount(3) + ")", "team (" + subscriptionCount(2) + ")"},
			`This is the last price on meter "tokens", and plans pro (3 subscriptions), team (2 subscriptions) bill it, so that usage would go unbilled. Add another price first, or move those subscriptions to a plan without this meter.`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := lastPriceMessage("tokens", tc.live); got != tc.want {
				t.Errorf("got  %q\nwant %q", got, tc.want)
			}
		})
	}
}
