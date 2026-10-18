package dunning

import (
	"context"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/domain"
)

// useSeedAsDefault replaces memStore's default with the policy every new
// account is seeded with, after the same normalize+validate the seed writer
// runs.
func useSeedAsDefault(t *testing.T, store *memStore) domain.DunningPolicy {
	t.Helper()
	seed, err := normalizeAndValidatePolicy(DefaultSeedPolicy())
	if err != nil {
		t.Fatalf("the seed policy must pass validation: %v", err)
	}
	seed.ID, seed.TenantID, seed.IsDefault, seed.Seeded = store.defaultID, "t1", true, true
	store.policies[store.defaultID] = seed
	return seed
}

// TestDefaultSeedPolicy_SurvivesNormalize: an empty final action normalizes
// to pause, so the seed must name none explicitly. If it ever relies on the
// defaults, every new account would pause subscriptions it never chose to.
func TestDefaultSeedPolicy_SurvivesNormalize(t *testing.T) {
	got, err := normalizeAndValidatePolicy(DefaultSeedPolicy())
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if got.FinalSubscriptionAction != domain.SubActionNone || got.FinalInvoiceAction != domain.InvActionNone {
		t.Fatalf("final actions %q/%q after normalize, want none/none", got.FinalSubscriptionAction, got.FinalInvoiceAction)
	}
	if !got.Enabled {
		t.Fatal("the seed must be enabled, or StartDunning skips every failed payment")
	}
}

// TestSeedDefaultPoliciesTx_PassesTheSeed: the service hands the store the
// validated seed and forwards the per-insert callback.
func TestSeedDefaultPoliciesTx_PassesTheSeed(t *testing.T) {
	store := &memStore{policies: map[string]domain.DunningPolicy{}, runs: map[string]domain.InvoiceDunningRun{}}
	svc := NewService(store, nil, nil)
	var called int
	if err := svc.SeedDefaultPoliciesTx(context.Background(), nil, "t1", func(p domain.DunningPolicy, _ bool) error {
		called++
		if p.Name != "Default retries" || !p.Seeded || !p.IsDefault {
			t.Errorf("callback got %+v", p)
		}
		return nil
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if called != 1 || len(store.policies) != 1 {
		t.Fatalf("callback calls=%d policies=%d, want 1/1", called, len(store.policies))
	}
}

// TestSeedPolicy_RetriesOnDay3_7_14: a failed payment under the seed is
// retried three days after the failure, then four and seven days after
// that — day 3, 7 and 14.
func TestSeedPolicy_RetriesOnDay3_7_14(t *testing.T) {
	store := newMemStore()
	seed := useSeedAsDefault(t, store)
	svc := NewService(store, &noopRetrier{}, nil)

	before := time.Now().UTC()
	run, err := svc.StartDunning(context.Background(), "t1", "inv_1", "cus_1", time.Time{}, domain.DunningCausePaymentFailed)
	if err != nil {
		t.Fatalf("start dunning: %v", err)
	}
	if run.PolicyID != seed.ID {
		t.Fatalf("run policy = %q, want the seed %q", run.PolicyID, seed.ID)
	}
	if run.NextActionAt == nil {
		t.Fatal("run has no first retry")
	}
	first := run.NextActionAt.Sub(before)
	if first < 72*time.Hour || first > 72*time.Hour+time.Minute {
		t.Fatalf("first retry %v after the failure, want 3 days", first)
	}
	day := 3
	var days []int
	days = append(days, day)
	for _, entry := range seed.RetrySchedule {
		d, _ := time.ParseDuration(entry)
		day += int(d / (24 * time.Hour))
		days = append(days, day)
	}
	if len(days) != 3 || days[0] != 3 || days[1] != 7 || days[2] != 14 {
		t.Fatalf("retry days %v, want [3 7 14]", days)
	}
}

// TestExhaust_SeedPolicy_ChangesNothing: when the seed's retries run out the
// subscription keeps billing and the invoice stays open — no pause, cancel
// or write-off — and the escalation email says nothing changed.
func TestExhaust_SeedPolicy_ChangesNothing(t *testing.T) {
	store := newMemStore()
	useSeedAsDefault(t, store)
	svc := NewService(store, &noopRetrier{}, nil)

	pauser := &recordingPauser{}
	state := &stubSubState{}
	canceler := &flakyCanceler{state: state}
	uncollect := &capturingUncollect{}
	emails := &capturingEscalationEmail{}
	svc.SetSubscriptionPauser(pauser, stubInvGet{inv: unpaidInv()})
	svc.SetSubscriptionCanceler(canceler)
	svc.SetSubscriptionStateReader(state)
	svc.SetInvoiceUncollectibleMarker(uncollect)
	svc.SetEmailNotifier(emails)
	svc.SetCustomerEmailFetcher(stubCustomerEmail{})

	run := exhaustedRun(t, store, svc)
	svc.ProcessDueRuns(context.Background(), "t1", 20)

	if pauser.calls != 0 || canceler.calls != 0 || len(uncollect.calls) != 0 {
		t.Fatalf("pause=%d cancel=%d write-off=%d, want all 0 under the seed", pauser.calls, canceler.calls, len(uncollect.calls))
	}
	got := store.runs[run.ID]
	if got.State != domain.DunningEscalated {
		t.Fatalf("state = %q, want escalated", got.State)
	}
	if len(emails.outcomes) != 1 {
		t.Fatalf("escalation emails: got %d, want 1", len(emails.outcomes))
	}
	if emails.outcomes[0].Any() {
		t.Fatalf("escalation email claims %q; the seed changes nothing", emails.outcomes[0].Reason())
	}
}
