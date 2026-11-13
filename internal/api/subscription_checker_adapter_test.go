package api

import (
	"context"
	"testing"

	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/subscription"
)

// trialEndsBetweenReads is a concurrent-resolver fake for the archive guard's
// subscription reads. It answers each List with the subscription when its
// status matches, and right after the first call it moves the subscription
// from trialing to active: a trial end that commits between two of the
// guard's reads. A trial end does not take the customer-rules lock, so the
// archive check holding that lock does not stop it. The currency read is not
// called here (nil).
type trialEndsBetweenReads struct {
	subscriptionReader
	sub   domain.Subscription
	calls int
}

func (f *trialEndsBetweenReads) List(_ context.Context, filter subscription.ListFilter) ([]domain.Subscription, int, error) {
	var out []domain.Subscription
	if string(f.sub.Status) == filter.Status {
		out = append(out, f.sub)
	}
	f.calls++
	if f.calls == 1 {
		f.sub.Status = domain.SubscriptionActive
	}
	return out, len(out), nil
}

// TestSubscriptionChecker_TrialEndingBetweenReadsIsSeen: the archive guard
// reads one status at a time. A trial that ends between two reads must still
// be found, and listed once. Reading trialing before active does it: the
// subscription is in the first read if the trial has not ended, and in the
// second if it has. Read the other way round, it is in neither, and an
// archive goes through while the subscription bills (ADR-067).
//
// Mutation-verify: read active before trialing in NonTerminalForCustomer →
// no subscription is found.
func TestSubscriptionChecker_TrialEndingBetweenReadsIsSeen(t *testing.T) {
	fake := &trialEndsBetweenReads{sub: domain.Subscription{
		ID: "hc_sub_trial", Code: "trial", DisplayName: "Trial", CustomerID: "hc_cus_1",
		Status: domain.SubscriptionTrialing,
	}}
	a := &subscriptionCheckerAdapter{subs: fake}
	got, err := a.NonTerminalForCustomer(context.Background(), "hc_ten_1", "hc_cus_1")
	if err != nil {
		t.Fatalf("NonTerminalForCustomer: %v", err)
	}
	if len(got) != 1 || got[0].ID != "hc_sub_trial" {
		t.Fatalf("blocking subscriptions = %+v, want hc_sub_trial once: a trial that ended between the reads was missed or counted twice", got)
	}
}
