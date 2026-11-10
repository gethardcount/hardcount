package payment

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
)

// paidMidCharge seeds a store whose row was paid offline while the charge was
// in flight — which bumped charge_attempt_seq — and returns it with the
// pre-call snapshot the charge started from (seq 0). The mock store models the
// real attempt-seq CAS; the SQL is proven in
// internal/invoice/charge_outcome_guard_integration_test.go.
func paidMidCharge() (*mockInvoiceUpdater, domain.Invoice) {
	base := newMockInvoiceUpdater()
	paidAt := time.Date(2026, 10, 10, 9, 0, 0, 0, time.UTC)
	base.invoices["inv_1"] = domain.Invoice{
		ID: "inv_1", TenantID: "t1", CustomerID: "cus_1",
		Status: domain.InvoicePaid, PaymentStatus: domain.PaymentSucceeded,
		PaidOutOfBand: true, PaidAt: &paidAt,
		Currency: "USD", ChargeAttemptSeq: 1,
	}
	return base, pendingSnapshot()
}

// declinedMidCharge: this attempt's own decline webhook recorded failed/pi_X
// (bumping the seq) and sent its notice before the charge call returned — the
// store sets failure_notified_pi in the same write, so the fixture does too.
func declinedMidCharge() (*mockInvoiceUpdater, domain.Invoice) {
	base := newMockInvoiceUpdater()
	row := pendingSnapshot()
	row.PaymentStatus = domain.PaymentFailed
	row.StripePaymentIntentID = "pi_X"
	row.ChargeAttemptSeq = 1
	base.invoices["inv_1"] = row
	base.byPI["pi_X"] = "inv_1"
	base.failNotedPI["inv_1"] = "pi_X"
	base.failedEventEnqueues = 1
	return base, pendingSnapshot()
}

func pendingSnapshot() domain.Invoice {
	return domain.Invoice{
		ID: "inv_1", TenantID: "t1", CustomerID: "cus_1",
		Status: domain.InvoiceFinalized, PaymentStatus: domain.PaymentPending,
		AmountDueCents: 5000, Currency: "USD",
	}
}

// A decline whose invoice was paid during the call must not start dunning:
// StartDunning has no paid check, so gating on the pre-call snapshot dunned a
// paid invoice — retries, warning emails, possibly a cancel final action.
func TestChargeInvoice_DeclineAfterMidChargePayment_NoDunning(t *testing.T) {
	invoices, snapshot := paidMidCharge()
	client := &mockStripeClient{shouldFail: true, failErr: &PaymentError{
		Message: "card_declined", PaymentIntentID: "pi_decline", Unknown: false,
	}}
	dunning := &recordingDunningStarter{}
	stripe := NewStripe(client, invoices, newMockWebhookStore(), nil, dunning)

	if _, err := stripe.ChargeInvoice(context.Background(), "t1", snapshot, testHold, "cus_stripe", "pm_test"); err == nil {
		t.Fatal("want the decline returned: it is the truth about this attempt")
	}
	if got := invoices.invoices["inv_1"]; got.Status != domain.InvoicePaid || got.StripePaymentIntentID != "" || !got.PaidOutOfBand {
		t.Fatalf("paid row overwritten: %s/%s/%q", got.Status, got.PaymentStatus, got.StripePaymentIntentID)
	}
	if len(dunning.calls) != 0 {
		t.Fatalf("StartDunning called %d times on an invoice paid during the charge; want 0", len(dunning.calls))
	}
}

// An invoice paid after the charge read it is never confirmed: the stamp
// before the confirm is a compare-and-set on the invoice, so it does not
// apply, and no money moves. (The one-call charge took the money and then
// found the invoice paid.)
func TestChargeInvoice_PaidBeforeTheStamp_ConfirmsNothing(t *testing.T) {
	invoices, snapshot := paidMidCharge()
	client := &mockStripeClient{piID: "pi_async", chargeStatus: "processing"}
	stripe := NewStripe(client, invoices, newMockWebhookStore(), nil, &recordingDunningStarter{})

	_, err := stripe.ChargeInvoice(postgres.WithLivemode(context.Background(), true), "t1", snapshot, testHold, "cus_stripe", "pm_test")
	if !errors.Is(err, ErrPaymentTransient) {
		t.Fatalf("err = %v, want ErrPaymentTransient (nothing confirmed)", err)
	}
	if client.confirmCalls != 0 {
		t.Fatalf("confirms = %d on an invoice paid before the stamp, want 0", client.confirmCalls)
	}
	if got := invoices.invoices["inv_1"]; got.Status != domain.InvoicePaid || got.StripePaymentIntentID != "" || !got.PaidOutOfBand {
		t.Fatalf("paid row changed: %s/%s/%q", got.Status, got.PaymentStatus, got.StripePaymentIntentID)
	}
}

// lockedBuf is a goroutine-safe log sink.
type lockedBuf struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuf) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuf) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func captureLogs(t *testing.T) *lockedBuf {
	t.Helper()
	out := &lockedBuf{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(out, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return out
}

// A confirm that timed out is an unclear outcome on the PaymentIntent the
// charge saved. It never overwrites an outcome recorded during the call (P13),
// and with nothing recorded it leaves the invoice unknown on that
// PaymentIntent, which the reconciler reads by id: nothing is parked.
func TestChargeInvoice_ConfirmTimeout_NeverOverwritesARecordedOutcome(t *testing.T) {
	timeout := &PaymentError{Message: "context deadline exceeded", Unknown: true}

	t.Run("paid during the call: stays paid", func(t *testing.T) {
		invoices, snapshot := paidMidCharge()
		stripe := NewStripe(&mockStripeClient{piID: "pi_T", shouldFail: true, failErr: timeout}, invoices, newMockWebhookStore(), nil, nil)
		_, _ = stripe.ChargeInvoice(postgres.WithLivemode(context.Background(), true), "t1", snapshot, testHold, "cus_stripe", "pm_test")
		if got := invoices.invoices["inv_1"]; got.PaymentStatus != domain.PaymentSucceeded {
			t.Fatalf("paid invoice overwritten: %s, want succeeded", got.PaymentStatus)
		}
	})

	t.Run("own decline recorded during the call: not overwritten", func(t *testing.T) {
		invoices, snapshot := declinedMidCharge()
		stripe := NewStripe(&mockStripeClient{piID: "pi_T", shouldFail: true, failErr: timeout}, invoices, newMockWebhookStore(), nil, nil)
		_, _ = stripe.ChargeInvoice(postgres.WithLivemode(context.Background(), true), "t1", snapshot, testHold, "cus_stripe", "pm_test")
		if got := invoices.invoices["inv_1"]; got.PaymentStatus != domain.PaymentFailed || got.StripePaymentIntentID != "pi_X" {
			t.Fatalf("recorded decline overwritten: %s/%q, want failed/pi_X (still retryable by dunning)", got.PaymentStatus, got.StripePaymentIntentID)
		}
	})

	t.Run("nothing recorded during the call: unknown on the saved PaymentIntent", func(t *testing.T) {
		invoices := newMockInvoiceUpdater()
		snapshot := pendingSnapshot()
		invoices.invoices["inv_1"] = snapshot
		stripe := NewStripe(&mockStripeClient{piID: "pi_T", shouldFail: true, failErr: timeout}, invoices, newMockWebhookStore(), nil, nil)
		_, _ = stripe.ChargeInvoice(postgres.WithLivemode(context.Background(), true), "t1", snapshot, testHold, "cus_stripe", "pm_test")
		if got := invoices.invoices["inv_1"]; got.PaymentStatus != domain.PaymentUnknown || got.StripePaymentIntentID != "pi_T" || got.Parked() {
			t.Fatalf("after the timeout: %s on %q, want unknown on pi_T, not parked", got.PaymentStatus, got.StripePaymentIntentID)
		}
	})
}

// The stamp must carry the seq the attempt's idempotency key was built from,
// not a constant: a dunning retry starts from an invoice that already records
// earlier attempts. With nothing recorded during the call, its decline lands.
func TestChargeInvoice_RetryAfterEarlierAttempts_StampLands(t *testing.T) {
	invoices := newMockInvoiceUpdater()
	snapshot := pendingSnapshot()
	snapshot.PaymentStatus = domain.PaymentFailed
	snapshot.StripePaymentIntentID = "pi_W"
	snapshot.ChargeAttemptSeq = 3
	invoices.invoices["inv_1"] = snapshot
	client := &mockStripeClient{shouldFail: true, failErr: &PaymentError{
		Message: "card_declined", PaymentIntentID: "pi_X", Unknown: false,
	}}
	stripe := NewStripe(client, invoices, newMockWebhookStore(), nil, &recordingDunningStarter{})

	_, _ = stripe.ChargeInvoice(postgres.WithLivemode(context.Background(), true), "t1", snapshot, testHold, "cus_stripe", "pm_test")
	if got := invoices.invoices["inv_1"]; got.StripePaymentIntentID != "pi_X" || got.ChargeAttemptSeq != 5 {
		t.Fatalf("row %s/%q/seq %d, want failed/pi_X/seq 5 (the stamp, then the decline) — the retry's decline was not recorded", got.PaymentStatus, got.StripePaymentIntentID, got.ChargeAttemptSeq)
	}
}

// takenOverStore answers the processing stamp as the real store does when
// another writer moved the invoice first: refused, with the row as it stands.
// then is that row's state and PaymentIntent.
type takenOverStore struct {
	*mockInvoiceUpdater
	then func(inv *domain.Invoice)
}

func (s *takenOverStore) StampChargeOutcome(ctx context.Context, tenantID, id string, attemptSeq int64, ps domain.InvoicePaymentStatus, piID, errMsg string) (domain.Invoice, bool, error) {
	if ps != domain.PaymentProcessing {
		return s.mockInvoiceUpdater.StampChargeOutcome(ctx, tenantID, id, attemptSeq, ps, piID, errMsg)
	}
	inv := s.invoices[id]
	s.then(&inv)
	inv.ChargeAttemptSeq = attemptSeq + 2
	s.invoices[id] = inv
	return inv, false, nil
}

// A charge's payment comes back processing. Before its stamp lands, the
// payment's failure is settled by its webhook, which frees the charge hold,
// and another attempt puts its own PaymentIntent on the invoice. The refused
// stamp then finds the invoice naming that other PaymentIntent: in flight on
// it, or already failed on it. That row is not this charge's outcome:
// returned as such, a dunning retry would wait on a payment it did not make,
// or count that payment's decline as its own. A row the other writer paid is
// the invoice's outcome and is returned.
func TestChargeInvoice_ProcessingStampRefused_InvoiceNamesAnotherAttempt(t *testing.T) {
	for _, tc := range []struct {
		name    string
		then    func(inv *domain.Invoice)
		wantErr bool
	}{
		{"in flight on another PaymentIntent: an error", func(inv *domain.Invoice) {
			inv.PaymentStatus, inv.StripePaymentIntentID = domain.PaymentUnknown, "pi_other_attempt"
		}, true},
		{"failed on another PaymentIntent: an error", func(inv *domain.Invoice) {
			inv.PaymentStatus, inv.StripePaymentIntentID = domain.PaymentFailed, "pi_other_attempt"
		}, true},
		{"paid on another PaymentIntent: the row is returned", func(inv *domain.Invoice) {
			inv.Status, inv.PaymentStatus, inv.StripePaymentIntentID = domain.InvoicePaid, domain.PaymentSucceeded, "pi_other_attempt"
		}, false},
		{"failed on this charge's own PaymentIntent: the row is returned", func(inv *domain.Invoice) {
			inv.PaymentStatus, inv.StripePaymentIntentID = domain.PaymentFailed, "pi_mine"
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := newMockInvoiceUpdater()
			inv := domain.Invoice{
				ID: "inv_1", TenantID: "t1", CustomerID: "cus_1", Status: domain.InvoiceFinalized,
				PaymentStatus: domain.PaymentPending, AmountDueCents: 5000, Currency: "USD",
			}
			base.invoices["inv_1"] = inv
			client := &mockStripeClient{piID: "pi_mine", chargeStatus: "processing"}
			stripe := NewStripe(client, &takenOverStore{mockInvoiceUpdater: base, then: tc.then}, newMockWebhookStore(), nil, &recordingDunningStarter{})

			got, err := stripe.ChargeInvoice(postgres.WithLivemode(context.Background(), true), "t1", inv, testHold, "cus_stripe", "pm_test")
			if tc.wantErr && err == nil {
				t.Fatalf("ChargeInvoice returned the other attempt's row (%s on %q) as this charge's outcome; want an error", got.PaymentStatus, got.StripePaymentIntentID)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("ChargeInvoice: %v, want the row as it stands", err)
			}
			if client.confirmCalls != 1 {
				t.Fatalf("confirms = %d, want 1", client.confirmCalls)
			}
		})
	}
}
