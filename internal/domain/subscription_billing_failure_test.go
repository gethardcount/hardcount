package domain

import (
	"testing"
	"time"
)

// TestDueInstant pins the Go form of the due-instant key against the SQL
// form, next_billing_at: a subscription scheduled to cancel owes its work at
// the same instant, the close of its period, and a missing next_billing_at
// means nothing is due.
func TestDueInstant(t *testing.T) {
	nb := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	cases := []struct {
		name string
		sub  Subscription
		want *time.Time
	}{
		{"close", Subscription{Status: SubscriptionActive, NextBillingAt: &nb}, &nb},
		{"close that cancels", Subscription{Status: SubscriptionActive, NextBillingAt: &nb, CancelAtPeriodEnd: true}, &nb},
		{"canceled", Subscription{Status: SubscriptionCanceled, NextBillingAt: &nb}, &nb},
		{"nothing due", Subscription{Status: SubscriptionActive}, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := DueInstant(c.sub)
			if (got == nil) != (c.want == nil) || (got != nil && !got.Equal(*c.want)) {
				t.Fatalf("DueInstant = %v, want %v", got, c.want)
			}
		})
	}
	if got := SubscriptionDueInstantSQL("s"); got != "s.next_billing_at" {
		t.Errorf("SubscriptionDueInstantSQL(s) = %q, want s.next_billing_at", got)
	}
}
