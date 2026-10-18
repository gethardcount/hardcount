package domain

import (
	"strings"
	"testing"
	"time"
)

// The charge record (ADR-127): a charge sent to Stripe whose result is not
// saved. Its payment status is still pending or failed, so the status alone
// says "nothing in flight" while Stripe may be taking the money.

func recordedInvoice(ps InvoicePaymentStatus) Invoice {
	started := time.Date(2026, 10, 19, 9, 0, 0, 0, time.UTC)
	return Invoice{Status: InvoiceFinalized, PaymentStatus: ps, ChargeStartedAt: &started}
}

func TestPaymentInFlight_TheChargeRecordCounts(t *testing.T) {
	for _, ps := range []InvoicePaymentStatus{PaymentPending, PaymentFailed} {
		if !recordedInvoice(ps).PaymentInFlight() {
			t.Errorf("%s with a charge record: PaymentInFlight = false, want true", ps)
		}
		if (Invoice{Status: InvoiceFinalized, PaymentStatus: ps}).PaymentInFlight() {
			t.Errorf("%s with no record: PaymentInFlight = true, want false (control)", ps)
		}
	}
}

// Every action refuses, the write-off included (a parked invoice's exit does
// not apply: this charge is being confirmed, not unidentifiable), under the
// in-flight code, and the copy says the wait is about an hour.
func TestPaymentBlocksAction_ChargeRecord(t *testing.T) {
	every := []InvoiceAction{
		ActionVoid, ActionMarkUncollectible, ActionRecordOfflinePayment,
		ActionIssueCreditNote, ActionCollectPayment, ActionResendSetupLink, ActionEmailInvoice,
	}
	for _, ps := range []InvoicePaymentStatus{PaymentPending, PaymentFailed} {
		for _, a := range every {
			got := PaymentBlocksAction(recordedInvoice(ps), a)
			if !got.Blocked || got.Code != "payment_in_flight" {
				t.Errorf("%s/%s: blocked=%v code=%q, want blocked with payment_in_flight", ps, a, got.Blocked, got.Code)
			}
			if !strings.Contains(got.Message, "about an hour") || !strings.Contains(got.Message, "Stripe") {
				t.Errorf("%s/%s: message %q must say Stripe is confirming it and that this takes about an hour", ps, a, got.Message)
			}
			if strings.Contains(got.Message, "uncollectible") {
				t.Errorf("%s/%s: message %q advises a write-off for a charge that is being confirmed", ps, a, got.Message)
			}
		}
	}
	// The held variant (void, write-off, offline payment) refuses with no hold.
	for _, a := range []InvoiceAction{ActionVoid, ActionMarkUncollectible, ActionRecordOfflinePayment} {
		if b := PaymentBlocksActionHeld(recordedInvoice(PaymentPending), a, false); !b.Blocked {
			t.Errorf("%s: allowed with a charge record and no hold", a)
		}
	}
}
