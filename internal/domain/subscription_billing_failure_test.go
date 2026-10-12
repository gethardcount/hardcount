package domain

import (
	"testing"
	"time"
)

// TestDueInstant pins the Go form of the due-instant key against the SQL
// LEAST(next_billing_at, CASE WHEN status = 'active' THEN cancel_at END):
// a missing side is ignored, and cancel_at counts only on an active row.
func TestDueInstant(t *testing.T) {
	nb := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	before, after := nb.Add(-time.Hour), nb.Add(time.Hour)
	cases := []struct {
		name string
		sub  Subscription
		want *time.Time
	}{
		{"close only", Subscription{Status: SubscriptionActive, NextBillingAt: &nb}, &nb},
		{"mid-period cancel first", Subscription{Status: SubscriptionActive, NextBillingAt: &nb, CancelAt: &before}, &before},
		{"cancel after the close", Subscription{Status: SubscriptionActive, NextBillingAt: &nb, CancelAt: &after}, &nb},
		{"cancel at the close", Subscription{Status: SubscriptionActive, NextBillingAt: &nb, CancelAt: &nb}, &nb},
		{"trialing ignores cancel_at", Subscription{Status: SubscriptionTrialing, NextBillingAt: &nb, CancelAt: &before}, &nb},
		{"no next_billing_at, active cancel", Subscription{Status: SubscriptionActive, CancelAt: &before}, &before},
		{"no next_billing_at, trialing cancel", Subscription{Status: SubscriptionTrialing, CancelAt: &before}, nil},
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
}
