package payment

import (
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
)

// A park that ends with a payment records it on the parked attempt's own row
// (ADR-127 amendment 2026-10-29). The park's row names no PaymentIntent,
// because Stripe's answer named none; the write that records the payment
// later is keyed on the PaymentIntent, so it used to add a second row and
// leave the parked one "Payment result unknown" on the Activity card of an
// invoice already paid or failed. One attempt is one row.

// attemptRow is one invoice_charge_attempts row, as these tests read it.
type attemptRow struct {
	pi, trigger, outcome, reason string
}

func (r *shRig) attemptRows(invoiceID string) []attemptRow {
	r.t.Helper()
	tx, err := r.db.BeginTx(r.ctx, postgres.TxBypass, "")
	if err != nil {
		r.t.Fatal(err)
	}
	defer postgres.Rollback(tx)
	rows, err := tx.QueryContext(r.ctx, `SELECT stripe_payment_intent_id, trigger_source, outcome, provider_reason
		FROM invoice_charge_attempts WHERE invoice_id = $1 ORDER BY created_at, id`, invoiceID)
	if err != nil {
		r.t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var out []attemptRow
	for rows.Next() {
		var a attemptRow
		if err := rows.Scan(&a.pi, &a.trigger, &a.outcome, &a.reason); err != nil {
			r.t.Fatal(err)
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		r.t.Fatal(err)
	}
	return out
}

// deliver hands the Stripe webhook for pi on inv to the payment handler.
func (r *shRig) deliver(eventType string, inv domain.Invoice, pi, status, failure string) {
	r.t.Helper()
	amt := shDue
	if err := r.stripe.HandleWebhook(r.ctx, r.tenantID, domain.StripeWebhookEvent{
		StripeEventID: "evt_" + eventType + "_" + pi, EventType: eventType, ObjectType: "payment_intent",
		InvoiceID: inv.ID, PaymentIntentID: pi, PaymentStatus: status, FailureMessage: failure,
		AmountCents: &amt, Currency: "usd", Payload: map[string]any{},
		ReceivedAt: time.Now().UTC(), OccurredAt: time.Now().UTC(),
	}); err != nil {
		r.t.Fatalf("webhook %s for %s: %v", eventType, pi, err)
	}
}

func TestParked_EndsWithAPayment_TheParkedAttemptRecordsIt(t *testing.T) {
	const declined = "Your card was declined."
	for _, tc := range []struct {
		name    string
		end     func(r *shRig, inv domain.Invoice) string // ends the park; returns its PaymentIntent
		status  domain.InvoicePaymentStatus
		outcome domain.ChargeAttemptOutcome
		reason  string
	}{
		{
			name: "the stale-hold sweep finds it succeeded",
			end: func(r *shRig, inv domain.Invoice) string {
				pi := r.fake.addPI(stripeCustomerOf(inv), inv.ID, "", "succeeded", shDue)
				r.ageHold(inv.ID, staleSweepAge)
				r.runClean()
				return pi.id
			},
			status: domain.PaymentSucceeded, outcome: domain.ChargeAttemptSucceeded,
		},
		{
			name: "the stale-hold sweep finds it processing",
			end: func(r *shRig, inv domain.Invoice) string {
				pi := r.fake.addPI(stripeCustomerOf(inv), inv.ID, "", "processing", shDue)
				r.ageHold(inv.ID, staleSweepAge)
				r.runClean()
				return pi.id
			},
			status: domain.PaymentProcessing, outcome: domain.ChargeAttemptPending,
		},
		{
			name: "its succeeded webhook",
			end: func(r *shRig, inv domain.Invoice) string {
				pi := r.fake.addPI(stripeCustomerOf(inv), inv.ID, "", "succeeded", shDue)
				r.deliver("payment_intent.succeeded", inv, pi.id, "succeeded", "")
				return pi.id
			},
			status: domain.PaymentSucceeded, outcome: domain.ChargeAttemptSucceeded,
		},
		{
			name: "its decline webhook",
			end: func(r *shRig, inv domain.Invoice) string {
				pi := r.fake.addPI(stripeCustomerOf(inv), inv.ID, "", "requires_payment_method", shDue)
				r.deliver("payment_intent.payment_failed", inv, pi.id, "failed", declined)
				return pi.id
			},
			status: domain.PaymentFailed, outcome: domain.ChargeAttemptFailed, reason: declined,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newSHRig(t, "Park Ends One Attempt")
			inv := r.seed("INV-PK-ONE")
			r.park(inv, autoCharge, park500)

			pi := tc.end(r, inv)

			got := r.get(inv.ID)
			if got.PaymentStatus != tc.status || got.StripePaymentIntentID != pi {
				t.Fatalf("invoice: %s with PI %q, want %s with %q", got.PaymentStatus, got.StripePaymentIntentID, tc.status, pi)
			}
			rows := r.attemptRows(inv.ID)
			if len(rows) != 1 {
				t.Fatalf("%d attempt rows %+v, want 1: the parked attempt and its payment are one attempt", len(rows), rows)
			}
			want := attemptRow{pi: pi, trigger: string(domain.ChargeTriggerAutoCharge), outcome: string(tc.outcome), reason: tc.reason}
			if rows[0] != want {
				t.Fatalf("attempt row %+v, want %+v: the parked row takes the payment, keeps who made it, and drops Stripe's 500 text", rows[0], want)
			}
			if n := r.attempts(inv.ID, domain.ChargeAttemptUnknown); n != 0 {
				t.Errorf("%d attempt rows still 'unknown': the Activity card would say the result is unknown", n)
			}
		})
	}
}

// A row for the park's payment can exist before the park ends: its decline
// webhook arrived while the invoice still waited on an older payment, so the
// report added a row without moving the invoice. Linking the parked row then
// would break the unique index on the PaymentIntent and roll back every later
// report of it. The report that ends the park must still be recorded; the
// existing row stays the payment's record (ADR-127 amendment 2026-10-29).
func TestParked_PaymentAlreadyHasARow_ParkStillEnds(t *testing.T) {
	r := newSHRig(t, "Park Ends Existing Row")
	inv := r.seed("INV-PK-EXISTING")
	r.park(inv, autoCharge, park500)
	pi := r.fake.addPI(stripeCustomerOf(inv), inv.ID, "", "requires_payment_method", shDue)
	if err := r.invoices.RecordChargeAttempt(r.ctx, r.tenantID, domain.InvoiceChargeAttempt{
		InvoiceID: inv.ID, StripePaymentIntentID: pi.id, Trigger: domain.ChargeTriggerExternal, Outcome: domain.ChargeAttemptFailed,
		ProviderReason: "Your card was declined.", AmountCents: shDue, OccurredAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("seed the payment's earlier row: %v", err)
	}

	r.deliver("payment_intent.payment_failed", inv, pi.id, "failed", "Your card was declined.")

	got := r.get(inv.ID)
	if got.PaymentStatus != domain.PaymentFailed || got.StripePaymentIntentID != pi.id {
		t.Fatalf("invoice: %s with PI %q, want failed with %q: the decline that ends the park was not recorded",
			got.PaymentStatus, got.StripePaymentIntentID, pi.id)
	}
	n := 0
	for _, a := range r.attemptRows(inv.ID) {
		if a.pi == pi.id {
			n++
			if a.outcome != string(domain.ChargeAttemptFailed) {
				t.Errorf("the payment's row is %s, want failed", a.outcome)
			}
		}
	}
	if n != 1 {
		t.Fatalf("%d rows name the payment, want exactly 1", n)
	}
}
