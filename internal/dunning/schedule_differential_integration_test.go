package dunning_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/audit"
	"github.com/gethardcount/hardcount/internal/customer"
	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/dunning"
	"github.com/gethardcount/hardcount/internal/email"
	"github.com/gethardcount/hardcount/internal/invoice"
	"github.com/gethardcount/hardcount/internal/platform/clock"
	"github.com/gethardcount/hardcount/internal/platform/leader"
	"github.com/gethardcount/hardcount/internal/platform/leader/leadertest"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/pricing"
	"github.com/gethardcount/hardcount/internal/subscription"
	"github.com/gethardcount/hardcount/internal/subscription/subscriptiontest"
	"github.com/gethardcount/hardcount/internal/tenant"
	"github.com/gethardcount/hardcount/internal/testutil"
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

// scheduleCases covers the schedules written today: a new account's policy, the
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
			name: "new account",
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
// escalation email, and the final actions on the invoice and its
// subscription, for each schedule in scheduleCases, with a fake clock ticked
// a day at a time.
//
// The goldens were recorded with today's policy shape. Changing how the
// schedule is written (#B8) must leave every one of them unchanged.
func TestSchedule_Differential(t *testing.T) {
	for _, tc := range scheduleCases() {
		t.Run(tc.name, func(t *testing.T) {
			runScheduleCase(t, tc)
		})
	}
}

func runScheduleCase(t *testing.T, tc scheduleCase) {
	db := testutil.SetupTestDB(t)
	admin := testutil.AdminPool(t)
	ctx := leadertest.Token(t, admin, postgres.WithLivemode(context.Background(), false), leader.RoleDunning)
	tenantID := testutil.CreateTestTenant(t, db, "Schedule Differential")
	wall := clock.NewFake(diffFailedAt)

	cust, err := customer.NewPostgresStore(db).Create(ctx, tenantID, domain.Customer{
		ExternalID: "cus_sched_diff", DisplayName: "Schedule Differential",
	})
	if err != nil {
		t.Fatalf("create customer: %v", err)
	}
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
	subStore := subscription.NewPostgresStore(db)
	sub, err := subStore.Create(ctx, tenantID, domain.Subscription{
		Code: "sub-sched-diff", DisplayName: "Schedule Differential", CustomerID: cust.ID,
		Items:  []domain.SubscriptionItem{{PlanID: plan.ID, Quantity: 1}},
		Status: domain.SubscriptionActive, BillingTime: domain.BillingTimeCalendar,
		StartedAt: &periodStart, CurrentBillingPeriodStart: &periodStart, CurrentBillingPeriodEnd: &periodEnd,
	})
	if err != nil {
		t.Fatalf("create subscription: %v", err)
	}
	subscriptiontest.SetBillingCycle(t, ctx, db, tenantID, sub.ID, periodStart, periodEnd, periodEnd, 0)
	invoices := invoice.NewPostgresStore(db)
	inv, err := invoices.CreateWithLineItems(ctx, tenantID, domain.Invoice{
		CustomerID: cust.ID, SubscriptionID: sub.ID, InvoiceNumber: "INV-SCHED-DIFF",
		Status: domain.InvoiceFinalized, PaymentStatus: domain.PaymentFailed, Currency: "USD",
		SubtotalCents: 4900, TotalAmountCents: 4900, AmountDueCents: 4900,
		BillingPeriodStart: periodStart, BillingPeriodEnd: periodEnd, IssuedAt: &diffFailedAt,
	}, nil)
	if err != nil {
		t.Fatalf("create invoice: %v", err)
	}

	// Every service runs on the fake clock, so the final actions are dated at
	// the tick that runs them.
	runs := dunning.NewPostgresStore(db)
	retrier := &declinedRetry{invoices: invoices}
	svc := dunning.NewService(runs, retrier, wall)
	subSvc := subscription.NewService(subStore, wall)
	invSvc := invoice.NewService(invoices, wall, tenant.NewSettingsStore(db))
	subSvc.SetAuditLogger(audit.NewLogger(db))
	svc.SetSubscriptionPauser(dunningMovers{svc: subSvc}, invoices)
	svc.SetSubscriptionCanceler(dunningMovers{svc: subSvc})
	svc.SetSubscriptionStateReader(dunningMovers{svc: subSvc})
	svc.SetInvoiceUncollectibleMarker(writeOff{svc: invSvc})
	svc.SetChargeHoldClaimer(invoices)
	svc.SetEmailNotifier(email.NewOutboxSender(email.NewOutboxStore(db)))
	svc.SetCustomerEmailFetcher(fixedEmailFetcher{email: "debtor@example.test", name: "Schedule Differential"})

	tc.policy.Enabled = true
	_, err = svc.PutPolicy(ctx, tenantID, tc.policy)
	if err != nil {
		t.Fatalf("upsert policy: %v", err)
	}

	// The failure that starts dunning carries no instant, so it reads the
	// fake clock.
	run, err := svc.StartDunning(clock.WithoutInstant(ctx), tenantID, inv.ID, cust.ID, domain.DunningCausePaymentFailed)
	if err != nil {
		t.Fatalf("start dunning: %v", err)
	}

	advance := func(to time.Time) {
		t.Helper()
		wall.Set(to)
		if _, errs := svc.ProcessDueRuns(ctx, tenantID, 20); len(errs) != 0 {
			t.Fatalf("dunning tick at %s: %v", to, errs)
		}
	}
	for d := 1; d <= diffHorizonDays; d++ {
		advance(diffFailedAt.Add(time.Duration(d) * 24 * time.Hour))
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
			if e.Reason != tc.wantOutcome {
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
	}
	var wantEmails []sent
	for i := range want[:len(want)-1] {
		wantEmails = append(wantEmails, sent{Type: email.TypeDunningWarning, Attempt: i + 1, Max: len(want),
			NextRetryDate: want[i+1].Format("January 2, 2006")})
	}
	wantEmails = append(wantEmails, sent{Type: email.TypeDunningEscalation, Outcome: outcomeOf(tc.wantOutcome)})

	rows, err := admin.Query(`SELECT email_type, payload FROM email_outbox
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
		if err := rows.Scan(&typ, &payload); err != nil {
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
		gotEmails = append(gotEmails, sent{Type: typ, Attempt: p.AttemptNumber, Max: p.MaxAttempts, NextRetryDate: p.NextRetryDate, Outcome: p.EscalationOutcome})
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
	if wantWrittenOff && (gotInv.UncollectibleAt == nil || !gotInv.UncollectibleAt.Equal(last)) {
		t.Errorf("uncollectible_at = %v, want %v", gotInv.UncollectibleAt, last)
	}
	gotSub, err := subStore.Get(ctx, tenantID, sub.ID)
	if err != nil {
		t.Fatalf("read subscription: %v", err)
	}
	switch tc.policy.FinalSubscriptionAction {
	case domain.SubActionCancel:
		if gotSub.Status != domain.SubscriptionCanceled || gotSub.CanceledAt == nil || !gotSub.CanceledAt.Equal(last) {
			t.Errorf("subscription %s canceled_at %v, want canceled at %v", gotSub.Status, gotSub.CanceledAt, last)
		}
	case domain.SubActionPause:
		if gotSub.PauseCollection == nil {
			t.Errorf("subscription collection not paused")
		}
	case domain.SubActionNone:
		if gotSub.Status != domain.SubscriptionActive || gotSub.PauseCollection != nil {
			t.Errorf("subscription %s pause=%v, want untouched", gotSub.Status, gotSub.PauseCollection)
		}
	}
}

// outcomeOf is the escalation email's outcome for an escalated reason.
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

func instants(ts []time.Time) []string {
	out := make([]string, len(ts))
	for i, t := range ts {
		out[i] = t.UTC().Format(time.RFC3339)
	}
	return out
}
