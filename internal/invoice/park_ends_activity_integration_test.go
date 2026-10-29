package invoice

import (
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/domain"
)

// TestParkEnded_ActivityShowsNoUnknownRow: the Activity card after a park
// ends with a payment (ADR-127 amendment 2026-10-29). The parked attempt's
// row used to stay 'unknown' beside the payment's own row, so a paid invoice
// showed "Payment result unknown: the payment provider has not confirmed
// whether this charge went through" under "Invoice paid", and a failed one
// showed it beside the decline. The rows are written by the real store
// transitions and rendered by renderChargeAttempts, as the Activity endpoint
// does.
func TestParkEnded_ActivityShowsNoUnknownRow(t *testing.T) {
	for _, tc := range []struct {
		name  string
		end   func(r *snapRig, inv domain.Invoice) error
		kinds []string // the attempt lines the card shows, in order
	}{
		{
			name: "its payment succeeded",
			end: func(r *snapRig, inv domain.Invoice) error {
				_, _, err := r.store.MarkPaidCardSettlementTransition(r.ctx, r.tenantID, inv.ID, "pi_park_paid", time.Now().UTC(), nil)
				return err
			},
			kinds: nil, // a success on a paid invoice is told by "Invoice paid"
		},
		{
			name: "its payment was declined",
			end: func(r *snapRig, inv domain.Invoice) error {
				_, _, err := r.store.MarkPaymentFailedReportingTransition(r.ctx, r.tenantID, inv.ID,
					domain.PaymentFailureReport{PaymentIntentID: "pi_park_declined", Message: "Your card was declined."}, nil)
				return err
			},
			kinds: []string{"payment.failed"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newSnapRig(t, "Park Ended Activity")
			inv, err := r.store.Create(r.ctx, r.tenantID, r.protoInvoice("INV-PARK-ACT", domain.InvoiceFinalized, domain.BillingReasonManual))
			if err != nil {
				t.Fatalf("create invoice: %v", err)
			}
			// The charge path's park: the attempt row first (no PaymentIntent),
			// then the unknown stamp.
			if err := r.store.RecordChargeAttempt(r.ctx, r.tenantID, domain.InvoiceChargeAttempt{
				InvoiceID: inv.ID, Trigger: domain.ChargeTriggerAutoCharge, Outcome: domain.ChargeAttemptUnknown,
				ProviderReason: "stripe: 500", AmountCents: inv.AmountDueCents, OccurredAt: time.Now().UTC(),
			}); err != nil {
				t.Fatalf("record the parked attempt: %v", err)
			}
			if _, applied, err := r.store.StampChargeOutcome(r.ctx, r.tenantID, inv.ID, inv.ChargeAttemptSeq, domain.PaymentUnknown, "", "stripe: 500"); err != nil || !applied {
				t.Fatalf("park: applied=%v err=%v", applied, err)
			}

			if err := tc.end(r, inv); err != nil {
				t.Fatalf("end the park: %v", err)
			}

			ended, err := r.store.Get(r.ctx, r.tenantID, inv.ID)
			if err != nil {
				t.Fatal(err)
			}
			attempts, err := r.store.ListChargeAttemptsByInvoice(r.ctx, r.tenantID, inv.ID)
			if err != nil {
				t.Fatal(err)
			}
			var kinds []string
			for _, e := range renderChargeAttempts(nil, attempts, ended) {
				kinds = append(kinds, e.Line.Kind)
			}
			if len(kinds) != len(tc.kinds) {
				t.Fatalf("Activity attempt lines %v, want %v", kinds, tc.kinds)
			}
			for i := range kinds {
				if kinds[i] != tc.kinds[i] {
					t.Fatalf("Activity attempt lines %v, want %v", kinds, tc.kinds)
				}
			}
		})
	}
}
