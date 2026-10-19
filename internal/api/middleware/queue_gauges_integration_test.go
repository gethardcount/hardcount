package middleware_test

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	mw "github.com/sagarsuperuser/velox/internal/api/middleware"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	vtestutil "github.com/sagarsuperuser/velox/internal/testutil"
)

// The gauges register on the default Prometheus registry, which refuses a
// second registration. Both tests in this file therefore share one
// registration whose count function reads through gaugeDB, the database of
// whichever test is running.
var (
	gaugeOnce   sync.Once
	gaugeDB     *postgres.DB
	gaugeFailed []string
)

// registerGauges wires the gauges the way router.go does: a BYPASS tx per
// scrape, and a 1h invoice grace (the INVOICE_GRACE_PERIOD default).
func registerGauges(db *postgres.DB) {
	gaugeDB, gaugeFailed = db, nil
	gaugeOnce.Do(func() {
		mw.RegisterQueueDepthGauges(time.Hour, func(query string) (float64, error) {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			tx, err := gaugeDB.BeginTx(ctx, postgres.TxBypass, "")
			if err != nil {
				return 0, err
			}
			defer postgres.Rollback(tx)
			var n float64
			if err := tx.QueryRowContext(ctx, query).Scan(&n); err != nil {
				gaugeFailed = append(gaugeFailed, err.Error()+"\n"+query)
				return 0, err
			}
			return n, nil
		})
	})
}

// TestQueueDepthGauges_EveryQueryRunsOnRealPostgres runs every scrape-time
// gauge's SQL against the real schema. A gauge whose query fails reports -1,
// which a dashboard can show for months before anyone reads it as "broken"
// instead of "busy".
func TestQueueDepthGauges_EveryQueryRunsOnRealPostgres(t *testing.T) {
	registerGauges(vtestutil.SetupTestDB(t))

	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	seen := map[string]bool{}
	for _, f := range families {
		for _, m := range f.GetMetric() {
			if g := m.GetGauge(); g != nil && g.GetValue() == -1 {
				t.Errorf("%s%v reports -1 (query failed)", f.GetName(), m.GetLabel())
			}
		}
		seen[f.GetName()] = true
	}
	for _, name := range []string{"velox_billing_due_subscriptions", "velox_billing_oldest_due_age_seconds", "velox_billing_failing_subscriptions", "velox_auto_charge_queued_invoices", "velox_parked_invoices", "velox_email_outbox_pending",
		"velox_email_outbox_oldest_due_age_seconds", "velox_email_outbox_failing_emails", "velox_webhook_outbox_oldest_pending_age_seconds"} {
		if !seen[name] {
			t.Errorf("gauge %s not registered", name)
		}
	}
	if len(gaugeFailed) > 0 {
		t.Errorf("failing gauge queries:\n%s", strings.Join(gaugeFailed, "\n---\n"))
	}
}

// gaugeValue reads one gauge by name and exact label set.
func gaugeValue(t *testing.T, name string, labels map[string]string) float64 {
	t.Helper()
	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, f := range families {
		if f.GetName() != name {
			continue
		}
	metrics:
		for _, m := range f.GetMetric() {
			if len(m.GetLabel()) != len(labels) {
				continue
			}
			for _, l := range m.GetLabel() {
				if labels[l.GetName()] != l.GetValue() {
					continue metrics
				}
			}
			return m.GetGauge().GetValue()
		}
	}
	t.Fatalf("gauge %s%v not found", name, labels)
	return 0
}

// TestBillingGauges_DueAndFailingPartition: every owed wall-clock subscription
// is in exactly one of two gauges (ADR-119). "Due" counts the ones billing is
// not failing on, including one whose failures are never saved; "failing"
// counts the saved ones by state. Rows that only look like failures (a record
// for another due instant, a record on a clock-pinned or canceled row) count
// as what they really are.
//
// The gauge predicate is a hand copy of domain.SubscriptionBillingFailingSQL.
// The last assertion pins the copy to the original on these rows; drift in
// either (say, dropping the due-instant key from one) breaks it.
func TestBillingGauges_DueAndFailingPartition(t *testing.T) {
	db := vtestutil.SetupTestDB(t)
	registerGauges(db)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)
	due := now.Add(-48 * time.Hour)
	const tenantID, clockID = "vlx_ten_bfgauge", "vlx_tclk_bfgauge"

	tx, err := db.BeginTx(ctx, postgres.TxBypass, "")
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer postgres.Rollback(tx)
	mustExec := func(q string, args ...any) {
		t.Helper()
		if _, err := tx.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	mustExec(`SELECT set_config('app.livemode', 'off', true)`)
	mustExec(`INSERT INTO tenants (id, name, status) VALUES ($1, 'BillingGauges', 'active')`, tenantID)
	mustExec(`INSERT INTO test_clocks (id, tenant_id, name, frozen_time, status, livemode)
		VALUES ($1, $2, 'gauges', $3, 'ready', false)`, clockID, tenantID, now)
	mustExec(`INSERT INTO customers (id, tenant_id, external_id, display_name, email, created_at, updated_at)
		VALUES ('cus_bfgauge', $1, 'g', 'BillingGauges', '', $2, $2)`, tenantID, now)
	seed := func(id, status string, nextBilling time.Time, cancelAt *time.Time, clock any, failedFor, nextAttempt *time.Time) {
		mustExec(`INSERT INTO subscriptions (
			id, tenant_id, code, display_name, customer_id, status, billing_time,
			current_billing_period_start, current_billing_period_end, next_billing_at, cancel_at,
			livemode, test_clock_id, billing_failed_for, billing_failure_count, billing_next_attempt_at,
			created_at, updated_at
		) VALUES ($1, $2, $1, $1, 'cus_bfgauge', $3, 'calendar', $4, $5, $5, $10, false, $6, $7, $8, $9, $4, $4)`,
			id, tenantID, status, nextBilling.Add(-30*24*time.Hour), nextBilling, clock,
			postgres.NullableTime(failedFor), map[bool]int{true: 3, false: 0}[failedFor != nil], postgres.NullableTime(nextAttempt),
			postgres.NullableTime(cancelAt))
	}
	later, stale, notDue := now.Add(time.Hour), due.Add(-30*24*time.Hour), now.Add(72*time.Hour)
	earlier := due.Add(-24 * time.Hour)
	seed("sub_g_healthy", "active", due, nil, nil, nil, nil)
	// Failing on every run with an error that is never saved (a lost
	// connection, an interrupted run): it has no record, so it is "due".
	seed("sub_g_unrecorded", "active", due.Add(time.Hour), nil, nil, nil, nil)
	seed("sub_g_retrying", "active", due, nil, nil, &due, &later)
	seed("sub_g_parked", "active", due, nil, nil, &due, nil)
	// The cancel arm of the key. An active row parked on a failing mid-period
	// cancel: its due instant is cancel_at, not next_billing_at. A copy of
	// the predicate that keyed on next_billing_at alone would call it due.
	seed("sub_g_cancel_parked", "active", notDue, &due, nil, &due, nil)
	// A trialing row ignores cancel_at: its due instant is next_billing_at
	// although an earlier cancel_at is set. A copy that dropped the
	// status = 'active' test would key it on cancel_at and lose it.
	seed("sub_g_trial_retrying", "trialing", due, &earlier, nil, &due, &later)
	// Decoys.
	seed("sub_g_stale", "active", due, nil, nil, &stale, nil)    // record for another due instant: due, not failing
	seed("sub_g_clock", "active", due, nil, clockID, &due, nil)  // clock-pinned: neither
	seed("sub_g_canceled", "canceled", due, nil, nil, &due, nil) // evidence on a canceled row: neither
	seed("sub_g_notdue", "active", notDue, nil, nil, nil, nil)
	// Inside the invoice grace (ADR-120; the gauges are registered with 1h):
	// the billing runs do not close these yet, so they are not due. One per
	// arm of the due instant.
	inGrace := now.Add(-30 * time.Minute)
	seed("sub_g_ingrace", "active", inGrace, nil, nil, nil, nil)
	seed("sub_g_cancel_ingrace", "active", notDue, &inGrace, nil, nil, nil)
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	test := func(extra ...string) map[string]string {
		l := map[string]string{"mode": "test"}
		for i := 0; i+1 < len(extra); i += 2 {
			l[extra[i]] = extra[i+1]
		}
		return l
	}
	if got := gaugeValue(t, "velox_billing_due_subscriptions", test()); got != 3 {
		t.Errorf("due = %v, want 3 (healthy, unrecorded, and the row with a stale record; the two rows inside their grace are not due)", got)
	}
	retrying := gaugeValue(t, "velox_billing_failing_subscriptions", test("state", "retrying"))
	parked := gaugeValue(t, "velox_billing_failing_subscriptions", test("state", "parked"))
	if retrying != 2 || parked != 2 {
		t.Errorf("failing: retrying=%v parked=%v, want 2 and 2 (one of each keyed to next_billing_at, a parked mid-period cancel, a retrying trial)", retrying, parked)
	}
	if got := gaugeValue(t, "velox_billing_failing_subscriptions", map[string]string{"mode": "live", "state": "parked"}); got != 0 {
		t.Errorf("live-mode parked = %v, want 0: the test-mode rows leaked across the livemode partition", got)
	}
	// The oldest due age is the healthy/stale row's, counted from when it
	// became due for the billing run: 48h after its due instant less the 1h
	// grace is 47h.
	if age := gaugeValue(t, "velox_billing_oldest_due_age_seconds", test()); age < 47*3600-300 || age > 47*3600+300 {
		t.Errorf("oldest due age = %vs, want about 47h (48h since the due instant, less the 1h grace)", age)
	}

	// The hand copy agrees with the domain predicate.
	var byDomain float64
	if err := vtestutil.AdminPool(t).QueryRowContext(ctx,
		`SELECT COUNT(*) FROM subscriptions s WHERE `+domain.SubscriptionBillingFailingSQL("s")+` AND s.livemode = false`).Scan(&byDomain); err != nil {
		t.Fatalf("count by domain predicate: %v", err)
	}
	if byDomain != retrying+parked {
		t.Errorf("gauge predicate counts %v failing, domain.SubscriptionBillingFailingSQL counts %v: the hand copy in metrics.go has drifted", retrying+parked, byDomain)
	}
}

// TestOutboxGauges_StallAndRelayBroken pins the three outbox signals an
// operator pages on (G6, loops review 2026-10-19). The real trigger: the SMTP
// relay refuses the sender or the credentials (AWS SES: "554 Message
// rejected: Email address is not verified."; 535 after a credential rotation;
// 454 on the sandbox quota; SMTP_HOST unset). Such an error is retried for
// about 72 hours, so pending > 1000 never fires for a small install and the
// emails are dead-lettered with only an ERROR log.
//
//   - Email stall: how long the longest-waiting due email has been due. It is
//     measured from next_attempt_at, not created_at: an email on the backoff
//     ramp is not due, and one that comes due after three days of retries has
//     waited only since it came due. A gauge on the oldest pending row would
//     page for three days over one email on the ramp.
//   - Relay broken: pending emails tried 3 or more times and queued over
//     30 minutes ago.
//   - Webhook outbox stall: age of the oldest pending row. Its rows are marked
//     dispatched in the same transaction that creates the event and its
//     deliveries, before any POST, so a failing endpoint never keeps a row
//     pending here.
//
// All three split by mode; the bypass scrape sets no app.livemode, so each
// query names it.
func TestOutboxGauges_StallAndRelayBroken(t *testing.T) {
	db := vtestutil.SetupTestDB(t)
	registerGauges(db)
	ctx := context.Background()
	const tenantID = "vlx_ten_obgauge"

	tx, err := db.BeginTx(ctx, postgres.TxBypass, "")
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer postgres.Rollback(tx)
	mustExec := func(q string, args ...any) {
		t.Helper()
		if _, err := tx.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	mustExec(`INSERT INTO tenants (id, name, status) VALUES ($1, 'OutboxGauges', 'active')`, tenantID)
	// The livemode trigger stamps each row from the session setting.
	mode := func(live bool) {
		t.Helper()
		v := "off"
		if live {
			v = "on"
		}
		mustExec(`SELECT set_config('app.livemode', $1, true)`, v)
	}
	// Times are offsets from the database clock, in seconds.
	email := func(id, status string, attempts int, createdAgo, dueIn int, lastError string) {
		t.Helper()
		mustExec(`INSERT INTO email_outbox (id, tenant_id, email_type, payload, status, attempts, next_attempt_at, created_at, last_error)
			VALUES ($1, $2, 'invoice', '{"to":"a@b.test"}', $3, $4, now() + make_interval(secs => $5), now() - make_interval(secs => $6), NULLIF($7, ''))`,
			id, tenantID, status, attempts, dueIn, createdAgo, lastError)
	}
	webhook := func(id, status string, createdAgo int) {
		t.Helper()
		mustExec(`INSERT INTO webhook_outbox (id, tenant_id, event_type, payload, status, attempts, next_attempt_at, created_at)
			VALUES ($1, $2, 'invoice.paid', '{}', $3, 0, now() - make_interval(secs => $4), now() - make_interval(secs => $4))`,
			id, tenantID, status, createdAgo)
	}
	const minute, hour, day = 60, 3600, 86400
	const rejected = "554 Message rejected: Email address is not verified."

	mode(false)
	email("eo_due20m", "pending", 0, 20*minute, -20*minute, "")    // due 20 min and not picked up: the stall
	email("eo_ramp", "pending", 12, 3*day, 2*hour, rejected)       // on the ramp: not due; relay broken
	email("eo_cameDue", "pending", 10, 3*day, -1*minute, rejected) // due 1 min after 3 days of retries; relay broken
	email("eo_young", "pending", 3, 10*minute, minute, rejected)   // 3 tries but queued 10 min ago: not yet
	email("eo_twoTries", "pending", 2, 2*hour, minute, rejected)   // old, but only 2 tries
	email("eo_dlq", "failed", 15, 4*day, -3*day, rejected)         // dead-lettered: neither
	email("eo_sent", "dispatched", 1, 5*day, -5*day, "")           // sent: neither
	webhook("wo_pending45m", "pending", 45*minute)                 // the webhook stall
	webhook("wo_sent", "dispatched", 2*day)                        // dispatched: excluded
	webhook("wo_dlq", "failed", 5*day)                             // dead-lettered: excluded
	mode(true)
	email("eo_live_due2m", "pending", 0, 2*minute, -2*minute, "")
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	near := func(name string, labels map[string]string, want float64) {
		t.Helper()
		if got := gaugeValue(t, name, labels); got < want-60 || got > want+60 {
			t.Errorf("%s%v = %v, want about %v", name, labels, got, want)
		}
	}
	test, live := map[string]string{"mode": "test"}, map[string]string{"mode": "live"}

	near("velox_email_outbox_oldest_due_age_seconds", test, 20*minute)
	near("velox_email_outbox_oldest_due_age_seconds", live, 2*minute)
	if got := gaugeValue(t, "velox_email_outbox_failing_emails", test); got != 2 {
		t.Errorf("failing emails (test) = %v, want 2: the row on the ramp and the one that came due, both tried 3+ times and queued 30+ min ago", got)
	}
	if got := gaugeValue(t, "velox_email_outbox_failing_emails", live); got != 0 {
		t.Errorf("failing emails (live) = %v, want 0: the test-mode rows leaked across the livemode partition", got)
	}
	near("velox_webhook_outbox_oldest_pending_age_seconds", test, 45*minute)
	if got := gaugeValue(t, "velox_webhook_outbox_oldest_pending_age_seconds", live); got != 0 {
		t.Errorf("webhook outbox oldest pending age (live) = %v, want 0 with no live rows", got)
	}
}
