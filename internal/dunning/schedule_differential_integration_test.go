package dunning_test

import (
	"context"
	"encoding/json"
	"fmt"
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
	"github.com/gethardcount/hardcount/internal/subscription"
	"github.com/gethardcount/hardcount/internal/tenant"
	"github.com/gethardcount/hardcount/internal/testutil"
)

// scheduleCase is one dunning schedule and what it must produce. The golden
// half (wantDays) is written as days after the failed payment, in neither the
// old nor the new policy shape, so the same golden checks the schedule
// however the policy is written.
type scheduleCase struct {
	name   string
	policy domain.DunningPolicy
	// wantDays: the day, counted from the failed payment, of each retry.
	wantDays []int
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
			wantDays: []int{3, 7, 14},
		},
		{
			name: "recipe policy",
			policy: domain.DunningPolicy{RetryDays: []int{3, 1, 3, 7},
				FinalSubscriptionAction: domain.SubActionPause, FinalInvoiceAction: domain.InvActionMarkUncollectible},
			wantDays: []int{3, 4, 7, 14},
		},
		{
			name: "one retry",
			policy: domain.DunningPolicy{RetryDays: []int{1},
				FinalSubscriptionAction: domain.SubActionCancel, FinalInvoiceAction: domain.InvActionNone},
			wantDays: []int{1},
		},
		{
			name: "entries past the last retry",
			policy: domain.DunningPolicy{RetryDays: []int{2, 2},
				FinalSubscriptionAction: domain.SubActionNone, FinalInvoiceAction: domain.InvActionMarkUncollectible},
			wantDays: []int{2, 4},
		},
		{
			name: "fifteen retries",
			policy: domain.DunningPolicy{RetryDays: []int{30, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1},
				FinalSubscriptionAction: domain.SubActionNone, FinalInvoiceAction: domain.InvActionNone},
			wantDays: []int{30, 31, 32, 33, 34, 35, 36, 37, 38, 39, 40, 41, 42, 43, 44},
		},
		{
			name: "uneven waits",
			policy: domain.DunningPolicy{RetryDays: []int{5, 5, 1, 10},
				FinalSubscriptionAction: domain.SubActionPause, FinalInvoiceAction: domain.InvActionNone},
			wantDays: []int{5, 10, 11, 21},
		},
	}
}

// diffFailedAt is when the payment fails. Not midnight, so a wait computed
// in a unit other than whole days shows as a shifted time of day.
var diffFailedAt = time.Date(2027, 3, 10, 9, 30, 0, 0, time.UTC)

const diffHorizonDays = 60 // past every case's last retry

// TestSchedule_Differential pins every retry instant, every reminder and
// escalation email, and the final action, for each schedule in
// scheduleCases, with the wall clock ticked a day at a time.
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
	invoices := invoice.NewPostgresStore(db)
	draft := domain.Invoice{
		CustomerID: cust.ID, InvoiceNumber: "INV-SCHED-DIFF",
		Status: domain.InvoiceFinalized, PaymentStatus: domain.PaymentFailed, Currency: "USD",
		SubtotalCents: 4900, TotalAmountCents: 4900, AmountDueCents: 4900,
		BillingPeriodStart: diffFailedAt.Add(-24 * time.Hour), BillingPeriodEnd: diffFailedAt, IssuedAt: &diffFailedAt,
	}
	// The subscription movers date from the real clock, not the fake one, so
	// the schedule is checked on a one-off invoice: the subscription half of
	// the outcome is then nothing, by design.
	subStore := subscription.NewPostgresStore(db)
	inv, err := invoices.CreateWithLineItems(ctx, tenantID, draft, nil)
	if err != nil {
		t.Fatalf("create invoice: %v", err)
	}

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

	tc.policy.Name, tc.policy.Enabled = "schedule under test", true
	policy, err := svc.UpsertPolicy(ctx, tenantID, tc.policy)
	if err != nil {
		t.Fatalf("upsert policy: %v", err)
	}
	if err := svc.SetDefaultPolicy(ctx, tenantID, policy.ID); err != nil {
		t.Fatalf("set default policy: %v", err)
	}

	// The failure that starts dunning is decided at real time.
	run, err := svc.StartDunning(clock.WithRealTime(ctx), tenantID, inv.ID, cust.ID, diffFailedAt, domain.DunningCausePaymentFailed)
	if err != nil {
		t.Fatalf("start dunning: %v", err)
	}
	if run.PolicyID != policy.ID {
		t.Fatalf("run bound to policy %s, want %s", run.PolicyID, policy.ID)
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
			NextRetryDate: want[i+1].Format("January 2, 2006")})
	}
	wantEmails = append(wantEmails, sent{Type: email.TypeDunningEscalation,
		Outcome: domain.DunningEscalationOutcome{InvoiceWrittenOff: tc.policy.FinalInvoiceAction == domain.InvActionMarkUncollectible}})

	rows, err := admin.Query(`SELECT email_type, payload,
			CASE WHEN test_clock_id IS NOT NULL THEN effective_at END FROM email_outbox
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
}

func instants(ts []time.Time) []string {
	out := make([]string, len(ts))
	for i, t := range ts {
		out[i] = t.UTC().Format(time.RFC3339)
	}
	return out
}
