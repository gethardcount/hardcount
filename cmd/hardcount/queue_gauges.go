package main

import (
	"fmt"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// registerQueueDepthGauges exports the queue-depth gauges that the shipped
// Grafana dashboard and the runbook alert on.
//
// Each gauge runs one COUNT at scrape time. The queries are cheap, because
// they use partial or status indexes, and each is capped at 3s. A query
// failure reports -1, which is visibly wrong, instead of a 0 that looks
// healthy.
func registerQueueDepthGauges(reg prometheus.Registerer, invoiceGrace time.Duration, count func(query string) (float64, error)) {
	auto := promauto.With(reg)
	gauge := func(name, help, query string) {
		auto.NewGaugeFunc(prometheus.GaugeOpts{Name: name, Help: help}, func() float64 {
			n, err := count(query)
			if err != nil {
				return -1
			}
			return n
		})
	}
	// Billing backlog (ADR-006, amendment P27). The scheduler drains every due
	// subscription in one tick, so on a busy day a tick can run for hours. On
	// hardcount_leader_last_tick_age_seconds that looks like a wedged tick.
	// These two gauges tell the cases apart: a draining tick shrinks the count,
	// and a wedged one does not. The predicate is GetDueBilling's.
	//
	// A subscription with a saved billing failure (ADR-119) is left out of both
	// and counted by hardcount_billing_failing_subscriptions below. So each
	// owed subscription is in exactly one of the two: "due and not
	// failing" or "failing". What still grows the age here is billing that is
	// behind, and a subscription whose attempts fail without being saved (an
	// interrupted run, a lost connection). Its id is in the "bill subscription
	// failed" log.
	//
	// "Due" is the billing runs' own rule, invoice grace included (ADR-120). A
	// subscription inside its grace is not counted, and the age counts from the
	// end of the grace. Otherwise every period end would show as backlog for
	// the length of the grace. The age alert (2x the billing interval) would
	// then fire on a healthy install with a long grace.
	//
	// billingFailing is a hand copy of domain.SubscriptionBillingFailingSQL, as
	// duePredicate is of the scan's, because this package imports no domain
	// package. TestBillingGauges_DueAndFailingPartition pins the two copies to
	// each other on real rows.
	const billingFailing = `status = 'active'
		AND billing_failed_for IS NOT NULL
		AND billing_failed_for IS NOT DISTINCT FROM LEAST(next_billing_at, CASE WHEN status = 'active' THEN cancel_at END)`
	dueHelp := "Subscriptions due for billing now whose billing is not failing. A subscription is due once INVOICE_GRACE_PERIOD has passed after its period end or mid-period cancel. Spikes on busy billing days and falls as the scheduler drains it. Subscriptions with a saved billing failure are in hardcount_billing_failing_subscriptions. -1 = metric query failed."
	ageHelp := "Seconds since the oldest due subscription whose billing is not failing became due (its period end or mid-period cancel plus INVOICE_GRACE_PERIOD); 0 when there is none. Alert when it exceeds 2x the billing interval: billing is behind, or a subscription's attempts are interrupted or hit connection errors on every tick. -1 = metric query failed."
	// dueBy is now() less the grace, in whole microseconds (Postgres's
	// timestamp resolution).
	dueBy := fmt.Sprintf("(now() - interval '%d microseconds')", invoiceGrace.Microseconds())
	duePredicate := `status = 'active' AND livemode = %t
		AND (next_billing_at <= ` + dueBy + ` OR (cancel_at IS NOT NULL AND cancel_at <= ` + dueBy + `))
		AND NOT (` + billingFailing + `)`
	for _, m := range []struct {
		label string
		live  bool
	}{{"live", true}, {"test", false}} {
		pred := fmt.Sprintf(duePredicate, m.live)
		for _, g := range []struct{ name, help, query string }{
			{"hardcount_billing_due_subscriptions", dueHelp, `SELECT COUNT(*) FROM subscriptions WHERE ` + pred},
			// The due instant is the earlier of the arms that admit the row
			// (LEAST skips the NULL of an arm that doesn't).
			{"hardcount_billing_oldest_due_age_seconds", ageHelp, `SELECT COALESCE(EXTRACT(EPOCH FROM ` + dueBy + ` - MIN(LEAST(
				CASE WHEN next_billing_at <= ` + dueBy + ` THEN next_billing_at END,
				CASE WHEN cancel_at <= ` + dueBy + ` THEN cancel_at END))), 0)
				FROM subscriptions WHERE ` + pred},
		} {
			q := g.query
			auto.NewGaugeFunc(prometheus.GaugeOpts{Name: g.name, Help: g.help, ConstLabels: prometheus.Labels{"mode": m.label}}, func() float64 {
				n, err := count(q)
				if err != nil {
					return -1
				}
				return n
			})
		}
	}
	// Subscriptions whose billing is failing (ADR-119), by state. retrying
	// means the billing run will try it again on its own. parked means eight
	// attempts failed, and nothing retries it until an operator calls
	// POST /v1/subscriptions/{id}/retry-billing. It is a cluster fact, so every
	// replica computes it from the database on each scrape. The livemode
	// literal is explicit, because the scrape's bypass transaction sets no
	// app.livemode.
	failingHelp := "Subscriptions with a saved billing failure on the work they have due, by state. retrying = waiting for the next automatic attempt; parked = automatic attempts have stopped and an invoice is not being issued until an operator retries billing. Alert on parked > 0, and on the sum rising: one cause is failing many subscriptions. -1 = metric query failed."
	for _, m := range []struct {
		label string
		live  bool
	}{{"live", true}, {"test", false}} {
		for _, st := range []struct{ label, nextAttempt string }{{"retrying", "IS NOT NULL"}, {"parked", "IS NULL"}} {
			q := fmt.Sprintf(`SELECT COUNT(*) FROM subscriptions WHERE %s AND livemode = %t AND billing_next_attempt_at %s`, billingFailing, m.live, st.nextAttempt)
			auto.NewGaugeFunc(prometheus.GaugeOpts{Name: "hardcount_billing_failing_subscriptions", Help: failingHelp, ConstLabels: prometheus.Labels{"mode": m.label, "state": st.label}}, func() float64 {
				n, err := count(q)
				if err != nil {
					return -1
				}
				return n
			})
		}
	}
	// Auto-charge queue depth. The predicate is ListAutoChargePending's, so
	// this counts exactly what the sweep visits each tick. Invoices with no
	// card stay queued on purpose, to be charged when a card is attached
	// (charge-on-attach), so a steady baseline is normal. Under an ADR-116
	// amendment, a sustained count above 5,000 is the trigger to park them
	// instead.
	for _, m := range []struct {
		label string
		live  bool
	}{{"live", true}, {"test", false}} {
		q := fmt.Sprintf(`SELECT COUNT(*) FROM invoices i
			WHERE i.auto_charge_pending AND i.payment_status = 'pending' AND i.status = 'finalized'
			  AND i.livemode = %t
			  AND NOT EXISTS (SELECT 1 FROM subscriptions ps WHERE ps.id = i.subscription_id AND ps.pause_collection_behavior IS NOT NULL AND (ps.pause_collection_resumes_at IS NULL OR ps.pause_collection_resumes_at > now()))`, m.live)
		auto.NewGaugeFunc(prometheus.GaugeOpts{
			Name:        "hardcount_auto_charge_queued_invoices",
			Help:        "Invoices the auto-charge sweep visits each tick, including card-less ones waiting for a payment method. Sustained > 5000 is the ADR-116 amendment trigger. For charges that keep failing, watch hardcount_auto_charge_retries_total{result=\"failed\"}. -1 = metric query failed.",
			ConstLabels: prometheus.Labels{"mode": m.label},
		}, func() float64 {
			n, err := count(q)
			if err != nil {
				return -1
			}
			return n
		})
	}
	gauge("hardcount_email_outbox_pending",
		"Emails waiting for the outbox dispatcher (status='pending'). -1 = metric query failed.",
		`SELECT COUNT(*) FROM email_outbox WHERE status = 'pending'`)
	gauge("hardcount_webhook_outbox_pending",
		"Outbound webhook events waiting for the outbox dispatcher (status='pending'). -1 = metric query failed.",
		`SELECT COUNT(*) FROM webhook_outbox WHERE status = 'pending'`)
	// Outbox stall and relay signals, by mode; page on live. A pending count
	// never pages a small install. An oldest-pending age would page for three
	// days over one email on the backoff ramp. So the email stall is measured
	// from when a row came due. The livemode literal is explicit, because the
	// scrape's bypass transaction sets no app.livemode.
	for _, m := range []struct {
		label string
		live  bool
	}{{"live", true}, {"test", false}} {
		for _, g := range []struct{ name, help, query string }{
			{"hardcount_email_outbox_oldest_due_age_seconds",
				"Seconds the longest-waiting due email has been due (next_attempt_at passed) without a dispatcher taking it; 0 when none is due. Emails waiting out a retry are not due. Grows when the email_outbox role is stalled, or when more emails come due than it sends (about 20-30 a minute on one replica). -1 = metric query failed.",
				`SELECT COALESCE(EXTRACT(EPOCH FROM now() - MIN(next_attempt_at)), 0) FROM email_outbox
					WHERE status = 'pending' AND next_attempt_at <= now() AND livemode = %t`},
			{"hardcount_email_outbox_failing_emails",
				"Emails still pending after 3 or more tries and queued more than 30 minutes ago: their sends keep failing and they are retrying. Usually the SMTP relay refuses the sender or the credentials, or SMTP_HOST is unset. Left alone they are dead-lettered after about 52 hours. -1 = metric query failed.",
				`SELECT COUNT(*) FROM email_outbox
					WHERE status = 'pending' AND attempts >= 3 AND created_at < now() - interval '30 minutes' AND livemode = %t`},
			// Its rows are marked dispatched in the transaction that writes the
			// event and its deliveries, before any POST, so a failing endpoint
			// never keeps a row pending here.
			{"hardcount_webhook_outbox_oldest_pending_age_seconds",
				"Seconds since the oldest pending webhook outbox row was queued; 0 when none is pending. A row stays pending until its event and deliveries are written, so this grows when the webhook_outbox role is stalled or those writes keep failing. A failing endpoint does not show here. -1 = metric query failed.",
				`SELECT COALESCE(EXTRACT(EPOCH FROM now() - MIN(created_at)), 0) FROM webhook_outbox
					WHERE status = 'pending' AND livemode = %t`},
		} {
			q := fmt.Sprintf(g.query, m.live)
			auto.NewGaugeFunc(prometheus.GaugeOpts{Name: g.name, Help: g.help, ConstLabels: prometheus.Labels{"mode": m.label}}, func() float64 {
				n, err := count(q)
				if err != nil {
					return -1
				}
				return n
			})
		}
	}
	gauge("hardcount_dunning_active_runs",
		"Dunning runs currently active. -1 = metric query failed.",
		`SELECT COUNT(*) FROM invoice_dunning_runs WHERE state = 'active'`)
	gauge("hardcount_creditnote_pending_issue_drafts",
		"Clawback credit-note drafts awaiting issue (issue_pending, status='draft') — includes drafts legitimately deferred behind an in-flight source payment (ADR-059); alert on sustained growth/age, not presence. -1 = metric query failed.",
		`SELECT COUNT(*) FROM credit_notes WHERE issue_pending AND status = 'draft'`)
	gauge("hardcount_creditnote_pending_tax_reversals",
		"Issued credit notes whose upstream tax reversal is owed and can be made now (tax_reversal_pending, and the invoice's tax transaction is recorded). Presence is normal for a tick or two; a row here for >24h means the provider keeps rejecting it — runbook tier-2. A marked note whose invoice has no tax transaction yet waits on the invoice's tax commit and is not counted. -1 = metric query failed.",
		`SELECT COUNT(*) FROM credit_notes cn WHERE cn.tax_reversal_pending AND cn.status = 'issued' AND EXISTS (SELECT 1 FROM invoices i WHERE i.id = cn.invoice_id AND i.tenant_id = cn.tenant_id AND COALESCE(i.tax_transaction_id, '') <> '')`)
	gauge("hardcount_invoice_pending_tax_reversals",
		"Voided stripe_tax invoices with a committed upstream tax transaction and no confirmed reversal (tax_reversed_at IS NULL) — the invoice-side owed reversals the #310 sweep re-drives. Alert on age, not presence. -1 = metric query failed.",
		`SELECT COUNT(*) FROM invoices WHERE status = 'voided' AND tax_provider = 'stripe_tax' AND COALESCE(tax_transaction_id,'') <> '' AND tax_reversed_at IS NULL`)
}
