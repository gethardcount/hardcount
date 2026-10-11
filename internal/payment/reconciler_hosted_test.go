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
	r.SetHostedSessions(ledger, reader)

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
	r.SetHostedSessions(ledger, fakeSessionReader{"cs_1": {Status: "complete", PaymentStatus: "paid", PaymentIntentID: "pi_hosted"}})

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
	r.SetHostedSessions(ledger, fakeSessionReader{"cs_1": {Status: "expired"}})
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
