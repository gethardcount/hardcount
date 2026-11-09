package payment

import (
	"context"
	"errors"
	"testing"

	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/errs"
)

// recordingAnomalies mirrors invoice.PostgresStore.RecordPaymentAnomaly: it
// records an anomaly, and the event the store queues with it in one
// transaction, once per kind and charge. A second report of the marker's
// kind and charge records nothing and returns false. err, when set, fails
// every call, as a failed write does.
type recordingAnomalies struct {
	kinds  []string
	pis    []string
	byType map[string]int
	last   map[string]map[string]any
	marker [2]string
	err    error
}

func (r *recordingAnomalies) RecordPaymentAnomaly(_ context.Context, _, _, kind, pi string, _ int64, data map[string]any) (bool, error) {
	if r.err != nil {
		return false, r.err
	}
	if r.marker == [2]string{kind, pi} {
		return false, nil
	}
	r.marker = [2]string{kind, pi}
	r.kinds = append(r.kinds, kind)
	r.pis = append(r.pis, pi)
	if r.byType == nil {
		r.byType, r.last = map[string]int{}, map[string]map[string]any{}
	}
	r.byType[kind]++
	r.last[kind] = data
	return true, nil
}

func newAnomalyHarness(inv domain.Invoice) (*Stripe, *mockInvoiceUpdater, *recordingAnomalies) {
	invoices := newMockInvoiceUpdater()
	invoices.invoices[inv.ID] = inv
	if inv.StripePaymentIntentID != "" {
		invoices.byPI[inv.StripePaymentIntentID] = inv.ID
	}
	anomalies := &recordingAnomalies{}
	s := NewStripe(&mockStripeClient{}, invoices, newMockWebhookStore(), nil)
	s.SetAnomalyRecorder(anomalies)
	return s, invoices, anomalies
}

// TestSettleSucceeded_DuplicatePI_FastPath: a SECOND, different PI succeeding
// against an already-paid invoice records payment.duplicate_charge (marker
// and event); a same-PI redelivery stays a silent skip. Mutation seam: drop
// the fast-path comparison → nothing recorded.
func TestSettleSucceeded_DuplicatePI_FastPath(t *testing.T) {
	paid := domain.Invoice{
		ID: "inv_1", TenantID: "t1", Status: domain.InvoicePaid,
		PaymentStatus: domain.PaymentSucceeded, StripePaymentIntentID: "pi_A",
		AmountPaidCents: 1000,
	}
	s, _, anomalies := newAnomalyHarness(paid)

	// Same PI: routine redelivery — silence.
	if err := s.SettleSucceeded(context.Background(), "t1", paid, "pi_A", 1000, SourceWebhook); err != nil {
		t.Fatalf("same-PI: %v", err)
	}
	if anomalies.byType[domain.EventPaymentDuplicateCharge] != 0 {
		t.Fatalf("same-PI redelivery recorded duplicate_charge — false alarm")
	}

	// Different PI: money captured twice.
	if err := s.SettleSucceeded(context.Background(), "t1", paid, "pi_B", 1000, SourceWebhook); err != nil {
		t.Fatalf("different-PI: %v", err)
	}
	if len(anomalies.kinds) != 1 || anomalies.kinds[0] != domain.EventPaymentDuplicateCharge || anomalies.pis[0] != "pi_B" {
		t.Fatalf("recorded anomalies = %v/%v, want duplicate_charge/pi_B", anomalies.kinds, anomalies.pis)
	}
}

// TestSettleSucceeded_DuplicateCharge_PayloadNamesTheOriginalPayment: the
// payment.duplicate_charge payload says how the first payment was made. A
// card payment names its PaymentIntent on the invoice; an offline one has
// none, so the invoice's paid_out_of_band tells it apart. Before #1370 the offline case was told
// apart by an "out_of_band:" prefix in recorded_payment_intent_id; without
// the flag it would read the same as a credits-paid invoice ("").
func TestSettleSucceeded_DuplicateCharge_PayloadNamesTheOriginalPayment(t *testing.T) {
	for _, c := range []struct {
		name    string
		pi      string
		offline bool
	}{
		{"card", "pi_A", false},
		{"offline", "", true},
		{"credits", "", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			paid := domain.Invoice{
				ID: "inv_1", TenantID: "t1", Status: domain.InvoicePaid,
				PaymentStatus: domain.PaymentSucceeded, StripePaymentIntentID: c.pi,
				PaidOutOfBand: c.offline, AmountPaidCents: 1000,
			}
			s, _, anomalies := newAnomalyHarness(paid)
			if err := s.SettleSucceeded(context.Background(), "t1", paid, "pi_B", 1000, SourceWebhook); err != nil {
				t.Fatalf("SettleSucceeded: %v", err)
			}
			got := anomalies.last[domain.EventPaymentDuplicateCharge]
			if got == nil {
				t.Fatal("no payment.duplicate_charge payload")
			}
			if err := domain.CheckWebhookData(domain.EventPaymentDuplicateCharge, got); err != nil {
				t.Fatal(err)
			}
			if got["incoming_payment_intent_id"] != "pi_B" {
				t.Errorf("incoming_payment_intent_id = %v, want pi_B", got["incoming_payment_intent_id"])
			}
			// The invoice names the first payment: its PaymentIntent (left
			// out when it has none), and paid_out_of_band.
			inv := got[domain.WebhookResourceInvoice].(map[string]any)
			if pi, _ := inv["stripe_payment_intent_id"].(string); pi != c.pi {
				t.Errorf("invoice.stripe_payment_intent_id = %q, want %q", pi, c.pi)
			}
			if v, ok := inv["paid_out_of_band"]; !ok || v != c.offline {
				t.Errorf("invoice.paid_out_of_band = %v (present %v), want %v", v, ok, c.offline)
			}
		})
	}
}

// TestSettleSucceeded_DuplicatePI_TransitionSkip: the transitioned=false skip
// point compares against the row RETURNED by the FOR-UPDATE transition — a
// checkout invoice records its PI only at settle, so the caller's stale
// snapshot (empty recorded PI) must NOT be the comparand. Same-PI concurrent
// loser = silence; different-PI = duplicate_charge.
func TestSettleSucceeded_DuplicatePI_TransitionSkip(t *testing.T) {
	// Caller snapshot is PRE-settle (processing, no recorded PI — checkout
	// shape); the stored row is already paid by pi_A (the concurrent winner).
	snapshot := domain.Invoice{
		ID: "inv_1", TenantID: "t1", Status: domain.InvoiceFinalized,
		PaymentStatus: domain.PaymentProcessing, AmountDueCents: 1000,
	}
	stored := snapshot
	stored.Status = domain.InvoicePaid
	stored.PaymentStatus = domain.PaymentSucceeded
	stored.StripePaymentIntentID = "pi_A"
	stored.AmountPaidCents = 1000
	stored.AmountDueCents = 0

	s, _, anomalies := newAnomalyHarness(stored)

	// Same PI as the winner: the loser of a routine concurrent redelivery.
	if err := s.SettleSucceeded(context.Background(), "t1", snapshot, "pi_A", 1000, SourceWebhook); err != nil {
		t.Fatalf("same-PI loser: %v", err)
	}
	if anomalies.byType[domain.EventPaymentDuplicateCharge] != 0 {
		t.Fatalf("same-PI concurrent loser recorded duplicate_charge — the stale-snapshot false alarm the panel predicted")
	}

	// Different PI: genuine double charge racing the settle.
	if err := s.SettleSucceeded(context.Background(), "t1", snapshot, "pi_B", 1000, SourceWebhook); err != nil {
		t.Fatalf("different-PI loser: %v", err)
	}
	if anomalies.byType[domain.EventPaymentDuplicateCharge] != 1 {
		t.Fatalf("duplicate_charge recorded %d times, want 1", anomalies.byType[domain.EventPaymentDuplicateCharge])
	}
}

// TestSettleSucceeded_PaymentOnVoided: the transition's InvalidState (voided
// target) records payment.received_on_voided_invoice and ABSORBS the error
// — the webhook must not retry a terminal invoice forever.
func TestSettleSucceeded_PaymentOnVoided(t *testing.T) {
	voided := domain.Invoice{
		ID: "inv_v", TenantID: "t1", Status: domain.InvoiceVoided,
		PaymentStatus: domain.PaymentPending,
	}
	s, invoices, anomalies := newAnomalyHarness(voided)
	invoices.markPaidErr = errs.InvalidState("cannot mark invoice paid from status \"voided\"")

	if err := s.SettleSucceeded(context.Background(), "t1", voided, "pi_Z", 700, SourceWebhook); err != nil {
		t.Fatalf("voided settle must absorb, got: %v", err)
	}
	if len(anomalies.kinds) != 1 || anomalies.kinds[0] != domain.EventPaymentReceivedOnVoidedInvoice {
		t.Fatalf("recorded anomalies = %v, want received_on_voided", anomalies.kinds)
	}
}

// TestSettleSucceeded_AnomalyWriteFails_ErrorReachesTheCaller: a failed
// anomaly write fails the settle call, so the webhook answers 5xx and
// Stripe delivers the charge again (or the reconciler retries): the marker
// and its event are saved together on that retry, never dropped. Each of the
// three anomalies SettleSucceeded records after its transition.
func TestSettleSucceeded_AnomalyWriteFails_ErrorReachesTheCaller(t *testing.T) {
	errWrite := errors.New("connection reset")
	paid := domain.Invoice{
		ID: "inv_1", TenantID: "t1", Status: domain.InvoicePaid,
		PaymentStatus: domain.PaymentSucceeded, StripePaymentIntentID: "pi_A", AmountPaidCents: 1000,
	}
	unpaid := domain.Invoice{
		ID: "inv_1", TenantID: "t1", Status: domain.InvoiceFinalized,
		PaymentStatus: domain.PaymentProcessing, AmountDueCents: 1000,
	}
	for _, c := range []struct {
		name             string
		stored, snapshot domain.Invoice
		markPaidErr      error
	}{
		{"duplicate on a paid invoice", paid, paid, nil},
		{"duplicate that lost the settle race", paid, unpaid, nil},
		{"payment on a voided invoice", unpaid, unpaid, errs.InvalidState("voided")},
	} {
		t.Run(c.name, func(t *testing.T) {
			s, invoices, anomalies := newAnomalyHarness(c.stored)
			invoices.markPaidErr = c.markPaidErr
			anomalies.err = errWrite
			if err := s.SettleSucceeded(context.Background(), "t1", c.snapshot, "pi_B", 1000, SourceWebhook); !errors.Is(err, errWrite) {
				t.Fatalf("settle: err %v, want the anomaly write's error", err)
			}
		})
	}
}

// TestSettleSucceeded_AmountMismatch: captured != due hands the captured
// amount to the settle transition, which records payment.amount_mismatch in
// it (detection-only; the settle still stands). The exact-match settle
// stays silent. Mutation seam: pass 0 instead of the captured amount.
func TestSettleSucceeded_AmountMismatch(t *testing.T) {
	inv := domain.Invoice{
		ID: "inv_m", TenantID: "t1", Status: domain.InvoiceFinalized,
		PaymentStatus: domain.PaymentProcessing, AmountDueCents: 1000,
	}
	s, invoices, _ := newAnomalyHarness(inv)

	// Captured 1000 == due 1000 → silence.
	if err := s.SettleSucceeded(context.Background(), "t1", inv, "pi_ok", 1000, SourceWebhook); err != nil {
		t.Fatalf("exact settle: %v", err)
	}
	if len(invoices.amountMismatches) != 0 {
		t.Fatal("exact-amount settle recorded amount_mismatch")
	}

	inv2 := domain.Invoice{
		ID: "inv_m2", TenantID: "t1", Status: domain.InvoiceFinalized,
		PaymentStatus: domain.PaymentProcessing, AmountDueCents: 600,
	}
	s2, invoices2, anomalies2 := newAnomalyHarness(inv2)
	// The drifted-session shape: Stripe captured 1000, the invoice books 600.
	if err := s2.SettleSucceeded(context.Background(), "t1", inv2, "pi_drift", 1000, SourceWebhook); err != nil {
		t.Fatalf("drift settle: %v", err)
	}
	if len(invoices2.amountMismatches) != 1 || invoices2.amountMismatches[0] != "pi_drift" {
		t.Fatalf("amount_mismatch recorded for %v, want pi_drift once", invoices2.amountMismatches)
	}
	// The settle transaction records it; no second write after the commit.
	if len(anomalies2.kinds) != 0 {
		t.Fatalf("anomalies recorded after the settle = %v, want none", anomalies2.kinds)
	}
}
