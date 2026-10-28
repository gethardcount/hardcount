package dunning_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/audit"
	"github.com/sagarsuperuser/velox/internal/customer"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/dunning"
	"github.com/sagarsuperuser/velox/internal/email"
	"github.com/sagarsuperuser/velox/internal/invoice"
	"github.com/sagarsuperuser/velox/internal/platform/clock"
	"github.com/sagarsuperuser/velox/internal/platform/leader"
	"github.com/sagarsuperuser/velox/internal/platform/leader/leadertest"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/pricing"
	"github.com/sagarsuperuser/velox/internal/subscription"
	"github.com/sagarsuperuser/velox/internal/subscription/subscriptiontest"
	"github.com/sagarsuperuser/velox/internal/tenant"
	"github.com/sagarsuperuser/velox/internal/testclock"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

// scheduleCase is one dunning schedule and what it must produce. The golden
// half (wantDays, wantOutcome) is written as days after the failed payment,
// in neither the old nor the new policy shape, so the same golden checks the
// schedule however the policy is written.
type scheduleCase struct {
	name   string
	policy domain.DunningPolicy
	// wantDays: the day, counted from the failed payment, of each retry.
	wantDays []int
	// wantOutcome: the escalated event's reason (what the final actions did).
	wantOutcome string
}

// scheduleCases covers the schedules written today: the seeded default, the
// built-in recipes' policy, one retry, a schedule with entries past its last
// retry (never used), the longest allowed, and uneven waits. The goldens
// were recorded on main before the schedule became one list (#B8), with
// the policy written as grace_period_days + retry_schedule +
// max_retry_attempts (commit 29bbec74). Each policy is now that same
// schedule as retry_days, converted as migration 00206 converts a stored
// policy: the first wait, then the waits the retries use, in days.
func scheduleCases() []scheduleCase {
	return []scheduleCase{
		{
			name: "seeded default",
			policy: domain.DunningPolicy{RetryDays: []int{3, 4, 7},
				FinalSubscriptionAction: domain.SubActionNone, FinalInvoiceAction: domain.InvActionNone},
			wantDays: []int{3, 7, 14}, wantOutcome: "",
		},
		{
			name: "recipe policy",
			policy: domain.DunningPolicy{RetryDays: []int{3, 1, 3, 7},
				FinalSubscriptionAction: domain.SubActionPause, FinalInvoiceAction: domain.InvActionMarkUncollectible},
			wantDays: []int{3, 4, 7, 14}, wantOutcome: "subscription_paused+invoice_marked_uncollectible",
		},
		{
			name: "one retry",
			policy: domain.DunningPolicy{RetryDays: []int{1},
				FinalSubscriptionAction: domain.SubActionCancel, FinalInvoiceAction: domain.InvActionNone},
			wantDays: []int{1}, wantOutcome: "subscription_canceled",
		},
		{
			name: "entries past the last retry",
			policy: domain.DunningPolicy{RetryDays: []int{2, 2},
				FinalSubscriptionAction: domain.SubActionNone, FinalInvoiceAction: domain.InvActionMarkUncollectible},
			wantDays: []int{2, 4}, wantOutcome: "invoice_marked_uncollectible",
		},
		{
			name: "fifteen retries",
			policy: domain.DunningPolicy{RetryDays: []int{30, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1},
				FinalSubscriptionAction: domain.SubActionNone, FinalInvoiceAction: domain.InvActionNone},
			wantDays: []int{30, 31, 32, 33, 34, 35, 36, 37, 38, 39, 40, 41, 42, 43, 44}, wantOutcome: "",
		},
		{
			name: "uneven waits",
			policy: domain.DunningPolicy{RetryDays: []int{5, 5, 1, 10},
				FinalSubscriptionAction: domain.SubActionPause, FinalInvoiceAction: domain.InvActionNone},
			wantDays: []int{5, 10, 11, 21}, wantOutcome: "subscription_paused",
		},
	}
}

// diffFailedAt is when the payment fails. Not midnight, so a wait computed
// in a unit other than whole days shows as a shifted time of day.
var diffFailedAt = time.Date(2027, 3, 10, 9, 30, 0, 0, time.UTC)

const diffHorizonDays = 60 // past every case's last retry

// TestSchedule_Differential pins every retry instant, every reminder and
// escalation email, and the final action, for each schedule in
// scheduleCases, on three timelines:
//
//   - a test clock advanced once across every retry (one advance, many
//     boundaries: each retry still lands on its own instant);
//   - a test clock advanced a day at a time;
//   - the wall clock, ticked a day at a time.
//
// The goldens were recorded with today's policy shape. Changing how the
// schedule is written (#B8) must leave every one of them unchanged.
func TestSchedule_Differential(t *testing.T) {
	for _, tc := range scheduleCases() {
		for _, arm := range []string{"test clock, one advance", "test clock, daily advances", "wall clock, daily ticks"} {
			t.Run(tc.name+"/"+arm, func(t *testing.T) {
				runScheduleCase(t, tc, arm)
			})
		}
	}
}

func runScheduleCase(t *testing.T, tc scheduleCase, arm string) {
	db := testutil.SetupTestDB(t)
	admin := testutil.AdminPool(t)
	ctx := leadertest.Token(t, admin, postgres.WithLivemode(context.Background(), false), leader.RoleDunning)
	tenantID := testutil.CreateTestTenant(t, db, "Schedule Differential")
	onClock := arm != "wall clock, daily ticks"

	var clockID string
	var resolver clock.Resolver
	clocks := testclock.NewPostgresStore(db)
	if onClock {
		clockID = postgres.NewID("hc_tclk")
		if _, err := admin.Exec(`INSERT INTO test_clocks (id, tenant_id, livemode, name, frozen_time, status)
			VALUES ($1, $2, false, 'schedule differential', $3, 'advancing')`, clockID, tenantID, diffFailedAt); err != nil {
			t.Fatalf("insert clock: %v", err)
		}
		resolver = oneClockResolver{clocks: clocks, tenantID: tenantID, clockID: clockID}
	}
	wall := clock.NewFake(diffFailedAt)

	cust, err := customer.NewPostgresStore(db).Create(ctx, tenantID, domain.Customer{
		ExternalID: "cus_sched_diff", DisplayName: "Schedule Differential", TestClockID: clockID,
	})
	if err != nil {
		t.Fatalf("create customer: %v", err)
	}
	invoices := invoice.NewPostgresStore(db)
	draft := domain.Invoice{
		CustomerID: cust.ID, InvoiceNumber: "INV-SCHED-DIFF",
		Status: domain.InvoiceFinalized, PaymentStatus: domain.PaymentFailed, Currency: "USD",
		SubtotalCents: 4900, TotalAmountCents: 4900, AmountDueCents: 4900, IsSimulated: onClock,
		BillingPeriodStart: diffFailedAt.Add(-24 * time.Hour), BillingPeriodEnd: diffFailedAt, IssuedAt: &diffFailedAt,
	}
	// The subscription's final action needs a subscription. On the wall
	// clock the subscription movers date from the real clock, not the fake
	// one, so that arm checks the schedule on a one-off invoice: the
	// subscription half of the outcome is then nothing, by design.
	subStore := subscription.NewPostgresStore(db)
	var subID string
	if onClock {
		plan, err := pricing.NewPostgresStore(db).CreatePlan(ctx, tenantID, domain.Plan{
			Code: "pro", Name: "Pro", Currency: "USD", BillingInterval: domain.BillingMonthly,
			Status: domain.PlanActive, BaseAmountCents: 4900, BaseBillTiming: domain.BillInAdvance,
		})
		if err != nil {
			t.Fatalf("create plan: %v", err)
		}
		// The current period outlasts the horizon, so no final action has a
		// period end to bill first.
		periodStart := diffFailedAt.Add(-24 * time.Hour)
		periodEnd := periodStart.AddDate(0, 3, 0)
		sub, err := subStore.Create(ctx, tenantID, domain.Subscription{
			Code: "sub-sched-diff", DisplayName: "Schedule Differential", CustomerID: cust.ID, TestClockID: clockID,
			Items:  []domain.SubscriptionItem{{PlanID: plan.ID, Quantity: 1}},
			Status: domain.SubscriptionActive, BillingTime: domain.BillingTimeCalendar,
			StartedAt: &periodStart, CurrentBillingPeriodStart: &periodStart, CurrentBillingPeriodEnd: &periodEnd,
		})
		if err != nil {
			t.Fatalf("create subscription: %v", err)
		}
		subscriptiontest.SetBillingCycle(t, ctx, db, tenantID, sub.ID, periodStart, periodEnd, periodEnd, 0)
		subID = sub.ID
		draft.SubscriptionID = sub.ID
		draft.BillingPeriodStart, draft.BillingPeriodEnd = periodStart, periodEnd
	}
	inv, err := invoices.Create(ctx, tenantID, draft)
	if err != nil {
		t.Fatalf("create invoice: %v", err)
	}

	runs := dunning.NewPostgresStore(db)
	retrier := &declinedRetry{invoices: invoices}
	svc := dunning.NewService(runs, retrier, wall)
	svcClock := clock.Clock(clock.Real())
	if onClock {
		svc.SetResolver(resolver)
	} else {
		svcClock = wall
	}
	subSvc := subscription.NewService(subStore, svcClock)
	invSvc := invoice.NewService(invoices, svcClock, tenant.NewSettingsStore(db))
	if onClock {
		subSvc.SetResolver(resolver)
		invSvc.SetResolver(resolver)
	}
	subSvc.SetAuditLogger(audit.NewLogger(db))
	svc.SetSubscriptionPauser(dunningMovers{svc: subSvc}, invoices)
	svc.SetSubscriptionCanceler(dunningMovers{svc: subSvc})
	svc.SetSubscriptionStateReader(dunningMovers{svc: subSvc})
	svc.SetInvoiceUncollectibleMarker(writeOff{svc: invSvc})
	svc.SetChargeHoldClaimer(invoices)
	svc.SetEmailNotifier(email.NewOutboxSender(email.NewOutboxStore(db)))
	svc.SetCustomerEmailFetcher(fixedEmailFetcher{email: "debtor@example.test", name: "Schedule Differential"})

	tc.policy.Name, tc.policy.Enabled = "schedule under test", true
	policy, err := svc.UpsertPolicy(ctx, tenantID, tc.policy)
	if err != nil {
		t.Fatalf("upsert policy: %v", err)
	}
	if err := svc.SetDefaultPolicy(ctx, tenantID, policy.ID); err != nil {
		t.Fatalf("set default policy: %v", err)
	}

	run, err := svc.StartDunning(ctx, tenantID, inv.ID, cust.ID, diffFailedAt, domain.DunningCausePaymentFailed)
	if err != nil {
		t.Fatalf("start dunning: %v", err)
	}
	if run.PolicyID != policy.ID {
		t.Fatalf("run bound to policy %s, want %s", run.PolicyID, policy.ID)
	}

	advance := func(to time.Time) {
		t.Helper()
		if onClock {
			if _, err := admin.Exec(`UPDATE test_clocks SET frozen_time = $1 WHERE id = $2`, to, clockID); err != nil {
				t.Fatalf("advance clock: %v", err)
			}
			simCtx := clock.WithSim(ctx, clock.Sim{At: to, TestClockID: clockID})
			if _, errs := svc.ProcessDueRunsForClock(simCtx, tenantID, clockID, to, 20); len(errs) != 0 {
				t.Fatalf("dunning catchup to %s: %v", to, errs)
			}
			return
		}
		wall.Set(to)
		if _, errs := svc.ProcessDueRuns(ctx, tenantID, 20); len(errs) != 0 {
			t.Fatalf("dunning tick at %s: %v", to, errs)
		}
	}
	if arm == "test clock, one advance" {
		advance(diffFailedAt.AddDate(0, 0, diffHorizonDays))
	} else {
		for d := 1; d <= diffHorizonDays; d++ {
			advance(diffFailedAt.Add(time.Duration(d) * 24 * time.Hour))
		}
	}

	want := make([]time.Time, len(tc.wantDays))
	for i, d := range tc.wantDays {
		want[i] = diffFailedAt.Add(time.Duration(d) * 24 * time.Hour)
	}
	last := want[len(want)-1]

	// The run: every retry used, escalated at the last retry.
	got, err := runs.GetRun(ctx, tenantID, run.ID)
	if err != nil {
		t.Fatalf("read run: %v", err)
	}
	if got.State != domain.DunningEscalated || got.AttemptCount != len(want) {
		t.Fatalf("run state=%s attempts=%d, want escalated after %d", got.State, got.AttemptCount, len(want))
	}
	if got.LastAttemptAt == nil || !got.LastAttemptAt.Equal(last) {
		t.Errorf("last_attempt_at = %v, want %v", got.LastAttemptAt, last)
	}
	if got.ResolvedAt == nil || !got.ResolvedAt.Equal(last) {
		t.Errorf("resolved_at = %v, want %v", got.ResolvedAt, last)
	}
	if len(retrier.calls) != len(want) {
		t.Errorf("%d charges, want %d", len(retrier.calls), len(want))
	}

	// The timeline: one retry row per retry at its instant, then one
	// escalated row at the last retry naming what the final actions did.
	events, err := runs.ListEvents(ctx, tenantID, run.ID)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	var retryAt []time.Time
	escalated := 0
	for _, e := range events {
		switch e.EventType {
		case domain.DunningEventRetryAttempted:
			retryAt = append(retryAt, e.CreatedAt)
		case domain.DunningEventEscalated:
			escalated++
			wantInstant(t, "escalated event", e.CreatedAt, last)
			if e.Reason != tc.wantOutcome && onClock {
				t.Errorf("escalated reason %q, want %q", e.Reason, tc.wantOutcome)
			}
		}
	}
	if fmt.Sprint(instants(retryAt)) != fmt.Sprint(instants(want)) {
		t.Errorf("retries at\n  %v\nwant\n  %v", instants(retryAt), instants(want))
	}
	if escalated != 1 {
		t.Errorf("%d escalated events, want 1", escalated)
	}

	// The emails: a reminder after each retry but the last, naming the
	// attempt, the total and the next retry's date; then one escalation.
	type sent struct {
		Type, NextRetryDate string
		Attempt, Max        int
		Outcome             domain.DunningEscalationOutcome
		SimAt               string
	}
	var wantEmails []sent
	for i := range want[:len(want)-1] {
		wantEmails = append(wantEmails, sent{Type: email.TypeDunningWarning, Attempt: i + 1, Max: len(want),
			NextRetryDate: want[i+1].Format("January 2, 2006"), SimAt: simStamp(onClock, want[i])})
	}
	escalation := sent{Type: email.TypeDunningEscalation, SimAt: simStamp(onClock, last)}
	if onClock {
		escalation.Outcome = outcomeOf(tc.wantOutcome)
	} else {
		escalation.Outcome = domain.DunningEscalationOutcome{InvoiceWrittenOff: tc.policy.FinalInvoiceAction == domain.InvActionMarkUncollectible}
	}
	wantEmails = append(wantEmails, escalation)

	rows, err := admin.Query(`SELECT email_type, payload, sim_effective_at FROM email_outbox
		WHERE tenant_id = $1 AND email_type IN ($2, $3) ORDER BY created_at, id`,
		tenantID, email.TypeDunningWarning, email.TypeDunningEscalation)
	if err != nil {
		t.Fatalf("read emails: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var gotEmails []sent
	for rows.Next() {
		var typ string
		var payload []byte
		var simAt *time.Time
		if err := rows.Scan(&typ, &payload, &simAt); err != nil {
			t.Fatalf("scan email: %v", err)
		}
		var p struct {
			AttemptNumber     int                             `json:"attempt_number"`
			MaxAttempts       int                             `json:"max_attempts"`
			NextRetryDate     string                          `json:"next_retry_date"`
			EscalationOutcome domain.DunningEscalationOutcome `json:"escalation_outcome"`
		}
		if err := json.Unmarshal(payload, &p); err != nil {
			t.Fatalf("decode email payload: %v", err)
		}
		s := sent{Type: typ, Attempt: p.AttemptNumber, Max: p.MaxAttempts, NextRetryDate: p.NextRetryDate, Outcome: p.EscalationOutcome}
		if simAt != nil {
			s.SimAt = simAt.UTC().Format(time.RFC3339)
		}
		gotEmails = append(gotEmails, s)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("emails: %v", err)
	}
	if fmt.Sprintf("%+v", gotEmails) != fmt.Sprintf("%+v", wantEmails) {
		t.Errorf("emails\n  %+v\nwant\n  %+v", gotEmails, wantEmails)
	}

	// The final actions themselves.
	gotInv, err := invoices.Get(ctx, tenantID, inv.ID)
	if err != nil {
		t.Fatalf("read invoice: %v", err)
	}
	wantWrittenOff := tc.policy.FinalInvoiceAction == domain.InvActionMarkUncollectible
	if (gotInv.Status == domain.InvoiceUncollectible) != wantWrittenOff {
		t.Errorf("invoice status %s, want written off: %v", gotInv.Status, wantWrittenOff)
	}
	if wantWrittenOff && onClock {
		if gotInv.UncollectibleAt == nil || !gotInv.UncollectibleAt.Equal(last) {
			t.Errorf("uncollectible_at = %v, want %v", gotInv.UncollectibleAt, last)
		}
	}
	if onClock {
		sub, err := subStore.Get(ctx, tenantID, subID)
		if err != nil {
			t.Fatalf("read subscription: %v", err)
		}
		switch tc.policy.FinalSubscriptionAction {
		case domain.SubActionCancel:
			if sub.Status != domain.SubscriptionCanceled {
				t.Errorf("subscription %s, want canceled", sub.Status)
			}
		case domain.SubActionPause:
			if sub.PauseCollection == nil {
				t.Errorf("subscription collection not paused")
			}
		case domain.SubActionNone:
			if sub.Status != domain.SubscriptionActive || sub.PauseCollection != nil {
				t.Errorf("subscription %s pause=%v, want untouched", sub.Status, sub.PauseCollection)
			}
		}
	}
}

func instants(ts []time.Time) []string {
	out := make([]string, len(ts))
	for i, t := range ts {
		out[i] = t.UTC().Format(time.RFC3339)
	}
	return out
}

// simStamp is the email row's sim_effective_at: the simulated instant on a
// test clock, none on the wall clock.
func simStamp(onClock bool, at time.Time) string {
	if !onClock {
		return ""
	}
	return at.UTC().Format(time.RFC3339)
}

func outcomeOf(reason string) domain.DunningEscalationOutcome {
	var o domain.DunningEscalationOutcome
	for _, part := range strings.Split(reason, "+") {
		switch part {
		case "subscription_paused":
			o.SubscriptionPaused = true
		case "subscription_canceled":
			o.SubscriptionCanceled = true
		case "invoice_marked_uncollectible":
			o.InvoiceWrittenOff = true
		}
	}
	return o
}
