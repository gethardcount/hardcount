package invoice

import (
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/domain"
)

// TestRenderChargeAttempts locks the ADR-103 single-source rule: payment
// rows come from charge attempts alone, with exactly two exact-keyed
// suppressions — a dunning row carrying the same PaymentIntent absorbs
// the attempt (and inherits its provider facts), and a succeeded attempt
// defers to the "Invoice paid" lifecycle row, which is the superset
// (credits / offline / $0 pay an invoice with no charge).
func TestRenderChargeAttempts(t *testing.T) {
	wallT := time.Date(2026, 7, 28, 14, 12, 0, 0, time.UTC)
	simT := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	failed := func(pi string, sim bool) domain.InvoiceChargeAttempt {
		a := domain.InvoiceChargeAttempt{
			StripePaymentIntentID: pi, Outcome: domain.ChargeAttemptFailed,
			ProviderReason: "Your card was declined.", AmountCents: 440, OccurredAt: wallT,
		}
		if sim {
			a.SimEffectiveAt = &simT
		}
		return a
	}
	dunningRow := func(pi string) timelineEvent {
		return timelineEvent{Source: "dunning", EventType: "retry_attempted", PaymentIntentID: pi,
			Line: dunningLine(domain.InvoiceDunningEvent{EventType: domain.DunningEventRetryAttempted, AttemptCount: 1, Reason: "payment failed: Your card was declined."}, dunningContext{})}
	}
	count := func(evts []timelineEvent, src string) int {
		n := 0
		for _, e := range evts {
			if e.Source == src {
				n++
			}
		}
		return n
	}

	// Dunning owns the PI → attempt absorbed, and its provider facts
	// land on the dunning row: the decline code in words about the
	// cardholder, else the provider's own text, and the amount.
	out := renderChargeAttempts([]timelineEvent{dunningRow("pi_1")},
		[]domain.InvoiceChargeAttempt{failed("pi_1", true)}, domain.Invoice{Currency: "USD"})
	if len(out) != 1 || count(out, "payment") != 0 {
		t.Fatalf("dunning-owned PI: got %d rows (%d payment), want 1 dunning row only", len(out), count(out, "payment"))
	}
	if out[0].Line.Problem != "Your card was declined." || out[0].Line.Amount == nil || out[0].Line.Amount.Cents != 440 {
		t.Fatalf("dunning row must inherit the attempt's provider facts: %+v", out[0].Line)
	}
	coded := failed("pi_1", true)
	coded.DeclineCode = "insufficient_funds"
	out = renderChargeAttempts([]timelineEvent{dunningRow("pi_1")}, []domain.InvoiceChargeAttempt{coded}, domain.Invoice{Currency: "USD"})
	if out[0].Line.Problem != "Card declined: insufficient funds" {
		t.Fatalf("a decline code wins over the provider's text: %q", out[0].Line.Problem)
	}

	// No dunning row → the attempt renders itself, on the billing axis
	// when it carries a sim anchor.
	out = renderChargeAttempts(nil, []domain.InvoiceChargeAttempt{failed("pi_2", true)}, domain.Invoice{})
	if count(out, "payment") != 1 {
		t.Fatalf("unowned attempt must render: %+v", out)
	}
	if !out[0].IsSimulated || !out[0].sortAt.Equal(simT) {
		t.Fatalf("sim-anchored attempt must render on the billing axis: sim=%v at %v", out[0].IsSimulated, out[0].sortAt)
	}
	// A wall-stamped attempt keeps wall time.
	out = renderChargeAttempts(nil, []domain.InvoiceChargeAttempt{failed("pi_3", false)}, domain.Invoice{})
	if out[0].IsSimulated || !out[0].sortAt.Equal(wallT) {
		t.Fatalf("wall attempt must keep wall time: sim=%v at %v", out[0].IsSimulated, out[0].sortAt)
	}
	// Empty-PI attempts (the PI create itself failed) still render —
	// they can never have a dunning twin to absorb them.
	out = renderChargeAttempts([]timelineEvent{dunningRow("pi_9")},
		[]domain.InvoiceChargeAttempt{failed("", true)}, domain.Invoice{})
	if count(out, "payment") != 1 {
		t.Fatalf("empty-PI attempt must render: %+v", out)
	}

	// Succeeded: deferred on a paid invoice, rendered on an unpaid one.
	paidAt := wallT
	succ := domain.InvoiceChargeAttempt{StripePaymentIntentID: "pi_4", Outcome: domain.ChargeAttemptSucceeded, OccurredAt: wallT}
	out = renderChargeAttempts(nil, []domain.InvoiceChargeAttempt{succ}, domain.Invoice{PaidAt: &paidAt, StripePaymentIntentID: "pi_4"})
	if count(out, "payment") != 0 {
		t.Fatalf("succeeded+paid must defer to the lifecycle row: %+v", out)
	}
	out = renderChargeAttempts(nil, []domain.InvoiceChargeAttempt{succ}, domain.Invoice{})
	if count(out, "payment") != 1 || out[0].Line.Title != "Payment succeeded" {
		t.Fatalf("succeeded+unpaid is an anomaly and must render: %+v", out)
	}

	// Pending never renders — the attention banner owns in-flight.
	out = renderChargeAttempts(nil,
		[]domain.InvoiceChargeAttempt{{StripePaymentIntentID: "pi_5", Outcome: domain.ChargeAttemptPending, OccurredAt: wallT}}, domain.Invoice{})
	if len(out) != 0 {
		t.Fatalf("pending must not render: %+v", out)
	}
}
