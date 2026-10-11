package payment

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/domain"
)

type fakeHostedLedger struct {
	rows      []CheckoutClaim
	expired   []string
	completed []string
	touched   []string
}

func (f *fakeHostedLedger) ListHostedToReconcile(context.Context, time.Time, int) ([]CheckoutClaim, error) {
	return f.rows, nil
}
func (f *fakeHostedLedger) MarkSessionCompleted(_ context.Context, _, id string) error {
	f.completed = append(f.completed, id)
	return nil
}
func (f *fakeHostedLedger) MarkExpired(_ context.Context, _, id string) error {
	f.expired = append(f.expired, id)
	return nil
}
func (f *fakeHostedLedger) TouchReconcile(_ context.Context, _, id string) error {
	f.touched = append(f.touched, id)
	return nil
}

type fakeSessionReader map[string]CheckoutSessionResult

var errSessionRead = errors.New("stripe unavailable")

func (f fakeSessionReader) GetCheckoutSession(_ context.Context, id string) (CheckoutSessionResult, error) {
	r, ok := f[id]
	if !ok {
		return CheckoutSessionResult{}, errSessionRead
	}
	return r, nil
}

// TestHostedSweep_SettlesPaidSessionsEvenOnAPaidInvoice: a hosted payment no
// webhook reported is settled through the shared primitive. It must reach
// SettleSucceeded even when the invoice is already paid by another
// PaymentIntent: that call is what raises payment.duplicate_charge, so a
// sweep that skipped paid invoices (as the PI sweeps' settle helper does)
// would hide the one double charge this sweep exists to surface.
func TestHostedSweep_SettlesPaidSessionsEvenOnAPaidInvoice(t *testing.T) {
	store := newMockReconcileStore(
		domain.Invoice{ID: "inv_paid", TenantID: "t1", Status: domain.InvoicePaid, PaymentStatus: domain.PaymentSucceeded, StripePaymentIntentID: "pi_velox"},
		domain.Invoice{ID: "inv_open", TenantID: "t1", Status: domain.InvoiceFinalized, PaymentStatus: domain.PaymentFailed},
	)
	client := &mockStripeClient{piStates: map[string]PaymentIntentResult{
		"pi_hosted_1": {ID: "pi_hosted_1", Status: "succeeded", AmountReceivedCents: 5000},
		"pi_hosted_2": {ID: "pi_hosted_2", Status: "succeeded", AmountReceivedCents: 5000},
	}}
	ledger := &fakeHostedLedger{rows: []CheckoutClaim{
		{ID: "cks_dup", TenantID: "t1", InvoiceID: "inv_paid", StripeSessionID: "cs_dup", Status: "invoice_settled"},
		{ID: "cks_lost", TenantID: "t1", InvoiceID: "inv_open", StripeSessionID: "cs_lost", Status: "completed"},
		{ID: "cks_exp", TenantID: "t1", InvoiceID: "inv_open", StripeSessionID: "cs_exp", Status: "open"},
		{ID: "cks_live", TenantID: "t1", InvoiceID: "inv_open", StripeSessionID: "cs_live", Status: "open"},
		{ID: "cks_err", TenantID: "t1", InvoiceID: "inv_open", StripeSessionID: "cs_err", Status: "open"},
		{ID: "cks_unpaid", TenantID: "t1", InvoiceID: "inv_open", StripeSessionID: "cs_unpaid", Status: "open"},
	}}
	reader := fakeSessionReader{
		"cs_dup":    {Status: "complete", PaymentStatus: "paid", PaymentIntentID: "pi_hosted_1"},
		"cs_lost":   {Status: "complete", PaymentStatus: "paid", PaymentIntentID: "pi_hosted_2"},
		"cs_exp":    {Status: "expired", PaymentStatus: "unpaid"},
		"cs_live":   {Status: "open", PaymentStatus: "unpaid"},
		"cs_unpaid": {Status: "complete", PaymentStatus: "unpaid"},
	}
	settler := &recordingSettler{}
	r := NewReconciler(client, store, time.Second)
	r.SetSettler(settler)
	r.SetHostedSessions(ledger, reader, nil)

	resolved, errs := r.sweepHostedSessions(context.Background(), 50)

	if len(errs) != 1 || !errors.Is(errs[0], errSessionRead) {
		t.Fatalf("errs = %v, want exactly the cs_err read failure", errs)
	}
	if resolved != 3 {
		t.Errorf("resolved = %d, want 3 (two paid, one expired)", resolved)
	}
	want := map[string]string{"inv_paid": "pi_hosted_1", "inv_open": "pi_hosted_2"}
	if len(settler.succeeded) != 2 {
		t.Fatalf("settled = %+v, want both paid sessions", settler.succeeded)
	}
	for _, c := range settler.succeeded {
		if want[c.invoiceID] != c.piID {
			t.Errorf("settled %s with %s, want %s", c.invoiceID, c.piID, want[c.invoiceID])
		}
	}
	if len(settler.failed) != 0 {
		t.Errorf("an expired or unpaid session must not report a failure: %+v", settler.failed)
	}
	assertIDs(t, "completed", ledger.completed, "cks_dup", "cks_lost")
	assertIDs(t, "expired", ledger.expired, "cks_exp")
	assertIDs(t, "touched", ledger.touched, "cks_live", "cks_err", "cks_unpaid")
}

// TestHostedSweep_OneAlertPerCharge: when the webhook already settled this
// hosted payment as an anomaly (the invoice's marker names its
// PaymentIntent), the sweep records the session and does not settle again,
// which would send a second payment.duplicate_charge for one charge.
func TestHostedSweep_OneAlertPerCharge(t *testing.T) {
	store := newMockReconcileStore(domain.Invoice{
		ID: "inv_paid", TenantID: "t1", Status: domain.InvoicePaid, PaymentStatus: domain.PaymentSucceeded,
		StripePaymentIntentID: "pi_velox", PaymentAnomalyKind: domain.EventPaymentDuplicateCharge,
		PaymentAnomalyPaymentIntentID: "pi_hosted",
	})
	client := &mockStripeClient{piStates: map[string]PaymentIntentResult{
		"pi_hosted": {ID: "pi_hosted", Status: "succeeded", AmountReceivedCents: 5000},
	}}
	ledger := &fakeHostedLedger{rows: []CheckoutClaim{
		{ID: "cks_1", TenantID: "t1", InvoiceID: "inv_paid", StripeSessionID: "cs_1", Status: "invoice_settled"},
	}}
	settler := &recordingSettler{}
	r := NewReconciler(client, store, time.Second)
	r.SetSettler(settler)
	r.SetHostedSessions(ledger, fakeSessionReader{"cs_1": {Status: "complete", PaymentStatus: "paid", PaymentIntentID: "pi_hosted"}}, nil)

	if n, errs := r.sweepHostedSessions(context.Background(), 50); n != 1 || len(errs) != 0 {
		t.Fatalf("n=%d errs=%v", n, errs)
	}
	if len(settler.succeeded) != 0 {
		t.Fatalf("settled again: %+v — a second alert for a payment already escalated", settler.succeeded)
	}
	assertIDs(t, "completed", ledger.completed, "cks_1")
}

// TestHostedSweep_UnwiredDoesNothing: without a settler the sweep cannot
// record a payment correctly, so it must not touch anything.
func TestHostedSweep_UnwiredDoesNothing(t *testing.T) {
	ledger := &fakeHostedLedger{rows: []CheckoutClaim{{ID: "cks_1", TenantID: "t1", StripeSessionID: "cs_1"}}}
	r := NewReconciler(&mockStripeClient{}, newMockReconcileStore(), time.Second)
	r.SetHostedSessions(ledger, fakeSessionReader{"cs_1": {Status: "expired"}}, nil)
	if n, errs := r.sweepHostedSessions(context.Background(), 50); n != 0 || len(errs) != 0 {
		t.Fatalf("n=%d errs=%v", n, errs)
	}
	if len(ledger.expired)+len(ledger.completed)+len(ledger.touched) != 0 {
		t.Fatalf("unwired sweep wrote: %+v", ledger)
	}
}

func assertIDs(t *testing.T, what string, got []string, want ...string) {
	t.Helper()
	if len(got) != len(want) {
		t.Errorf("%s = %v, want %v", what, got, want)
		return
	}
	seen := map[string]bool{}
	for _, g := range got {
		seen[g] = true
	}
	for _, w := range want {
		if !seen[w] {
			t.Errorf("%s = %v, want %v", what, got, want)
			return
		}
	}
}

type cardCall struct{ tenantID, customerID, stripeCustomerID, purpose string }

type recordingCardRecorder struct {
	calls []cardCall
	err   error
}

func (r *recordingCardRecorder) RecordCheckoutCard(_ context.Context, tenantID, customerID, stripeCustomerID, purpose string) error {
	r.calls = append(r.calls, cardCall{tenantID, customerID, stripeCustomerID, purpose})
	return r.err
}

// A paid hosted session whose webhooks were all lost: the sweep records the
// payment AND the card the session saved (ADR-021), for the invoice's
// customer and the session's Stripe customer. Without the card, the next
// invoice went to card-less dunning asking for a card the customer had just
// given. A failed card record keeps the session unresolved for the next tick.
func TestHostedSweep_RecordsTheSessionsCard(t *testing.T) {
	newSweep := func(rec *recordingCardRecorder) (*Reconciler, *fakeHostedLedger) {
		store := newMockReconcileStore(domain.Invoice{ID: "inv_1", TenantID: "t1", CustomerID: "cus_velox", Status: domain.InvoiceFinalized, PaymentStatus: domain.PaymentFailed})
		client := &mockStripeClient{piStates: map[string]PaymentIntentResult{"pi_hosted": {ID: "pi_hosted", Status: "succeeded", AmountReceivedCents: 5000}}}
		ledger := &fakeHostedLedger{rows: []CheckoutClaim{{ID: "cks_1", TenantID: "t1", InvoiceID: "inv_1", StripeSessionID: "cs_1", Status: "open"}}}
		r := NewReconciler(client, store, time.Second)
		r.SetSettler(&recordingSettler{})
		r.SetHostedSessions(ledger, fakeSessionReader{"cs_1": {Status: "complete", PaymentStatus: "paid", PaymentIntentID: "pi_hosted", CustomerID: "cus_stripe"}}, rec)
		return r, ledger
	}

	rec := &recordingCardRecorder{}
	r, ledger := newSweep(rec)
	if n, errs := r.sweepHostedSessions(context.Background(), 50); n != 1 || len(errs) != 0 {
		t.Fatalf("n=%d errs=%v", n, errs)
	}
	if len(rec.calls) != 1 || rec.calls[0] != (cardCall{"t1", "cus_velox", "cus_stripe", PurposeHostedInvoicePay}) {
		t.Fatalf("card calls = %+v, want one for (t1, cus_velox, cus_stripe, hosted pay)", rec.calls)
	}
	assertIDs(t, "completed", ledger.completed, "cks_1")

	failing := &recordingCardRecorder{err: errors.New("db down")}
	r, ledger = newSweep(failing)
	if n, errs := r.sweepHostedSessions(context.Background(), 50); n != 0 || len(errs) != 1 {
		t.Fatalf("card failure: n=%d errs=%v, want unresolved with one error", n, errs)
	}
	if len(ledger.completed) != 0 {
		t.Errorf("a session whose card was not recorded must not be marked completed: %v", ledger.completed)
	}
	assertIDs(t, "touched", ledger.touched, "cks_1")
}
