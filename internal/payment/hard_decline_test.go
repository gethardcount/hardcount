package payment

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stripe/stripe-go/v82"

	"github.com/sagarsuperuser/velox/internal/auth"
	"github.com/sagarsuperuser/velox/internal/domain"
)

// #966: when the bank refuses a card for good (lost, stolen, pick up …), every
// source that reports the decline — the charge call, the webhook and the
// reconciler — puts the decline code and the declined card on record, so the
// next dunning retry can skip that card. The customer is asked for a
// different card instead of being told to try again.
//
// The errors here are built from the JSON Stripe sends, not from Go structs
// filled in by hand: a test that hand-builds the SDK error passes even when
// the field it relies on is never populated from a real response.

// stripeCardErrorJSON is the body of Stripe's 402 card_error for an
// off-session PaymentIntent confirm (the shape test cards such as
// 4000000000009987 return for lost_card): the decline sits both on the error
// and on its payment_intent.last_payment_error, with the card on both.
func stripeCardErrorJSON(declineCode, message, piID, pmID string) string {
	return fmt.Sprintf(`{
  "charge": "ch_3Q0testcharge",
  "code": "card_declined",
  "decline_code": %[1]q,
  "doc_url": "https://stripe.com/docs/error-codes/card-declined",
  "message": %[2]q,
  "payment_intent": {
    "id": %[3]q,
    "object": "payment_intent",
    "amount": 5000,
    "currency": "usd",
    "last_payment_error": {
      "charge": "ch_3Q0testcharge",
      "code": "card_declined",
      "decline_code": %[1]q,
      "doc_url": "https://stripe.com/docs/error-codes/card-declined",
      "message": %[2]q,
      "payment_method": {"id": %[4]q, "object": "payment_method", "type": "card"},
      "type": "card_error"
    },
    "metadata": {"velox_invoice_id": "inv_1"},
    "payment_method": null,
    "status": "requires_payment_method"
  },
  "payment_method": {"id": %[4]q, "object": "payment_method", "type": "card"},
  "request_log_url": "https://dashboard.stripe.com/test/logs/req_test?t=1",
  "type": "card_error"
}`, declineCode, message, piID, pmID)
}

// The charge call below runs through the real LiveStripeClient with Stripe's
// HTTP reply canned (withCannedStripe), so stripe-go itself decodes the 402
// and classifyStripeError sees exactly what a real decline produces.

func TestChargeDecline_RecordsDeclineCodeAndCard(t *testing.T) {
	cases := []struct {
		code, message string
		hard          bool
	}{
		{"lost_card", "Your card was declined.", true},
		{"stolen_card", "Your card was declined.", true},
		{"pickup_card", "Your card was declined.", true},
		{"insufficient_funds", "Your card has insufficient funds.", false},
		{"do_not_honor", "Your card was declined.", false},
	}
	for _, tc := range cases {
		t.Run(tc.code, func(t *testing.T) {
			invoices := newMockInvoiceUpdater()
			snapshot := pendingSnapshot()
			invoices.invoices["inv_1"] = snapshot
			withCannedStripe(t, http.StatusPaymentRequired,
				`{"error": `+stripeCardErrorJSON(tc.code, tc.message, "pi_decl", "pm_on_file")+`}`)
			email := &recordingFailedEmail{}
			dunning := &recordingDunningStarter{}
			s := NewStripe(NewLiveStripeClient(NewStripeClients(fixedCreds{})), invoices, newMockWebhookStore(), nil, dunning)
			s.SetEmailPaymentFailed(email, staticCustomerEmail{})

			_, err := s.ChargeInvoice(auth.WithTenantID(context.Background(), "t1"), "t1", snapshot, "cus_stripe", "pm_on_file")
			var pe *PaymentError
			if !errors.As(err, &pe) || pe.Unknown || pe.DeclineCode != tc.code || pe.PaymentIntentID != "pi_decl" {
				t.Fatalf("charge error = %#v, want a definite %s decline naming pi_decl", err, tc.code)
			}

			if len(invoices.failureReports) != 1 {
				t.Fatalf("failure reports = %d, want 1", len(invoices.failureReports))
			}
			rep := invoices.failureReports[0]
			if rep.DeclineCode != tc.code || rep.PaymentMethodID != "pm_on_file" {
				t.Errorf("report decline %q card %q, want %q on pm_on_file — the attempt row is where the next dunning retry looks",
					rep.DeclineCode, rep.PaymentMethodID, tc.code)
			}
			var attempt *domain.InvoiceChargeAttempt
			for i := range invoices.chargeAttempts {
				if invoices.chargeAttempts[i].StripePaymentIntentID == "pi_decl" {
					attempt = &invoices.chargeAttempts[i]
				}
			}
			if attempt == nil || attempt.DeclineCode != tc.code || attempt.StripePaymentMethodID != "pm_on_file" {
				t.Errorf("charge attempt row = %+v, want decline %q on pm_on_file", attempt, tc.code)
			}

			if email.sends != 1 || len(email.reasons) != 1 {
				t.Fatalf("payment-failed emails = %d, want 1", email.sends)
			}
			wantReason := tc.message
			if tc.hard {
				wantReason = refusedCardCustomerMessage
			}
			if email.reasons[0] != wantReason {
				t.Errorf("customer email reason = %q, want %q", email.reasons[0], wantReason)
			}
			// Dunning still starts: the schedule and its attempt count carry
			// on, as Stripe Billing's do for a hard decline.
			if len(dunning.calls) != 1 {
				t.Errorf("StartDunning calls = %d, want 1", len(dunning.calls))
			}
		})
	}
}

// The customer email for a hard decline never names the reason: Stripe asks
// that lost, stolen and fraud declines reach the customer as a plain decline.
func TestRefusedCardCustomerMessage_DoesNotNameTheReason(t *testing.T) {
	for _, word := range []string{"lost", "stolen", "fraud", "pick"} {
		if strings.Contains(strings.ToLower(refusedCardCustomerMessage), word) {
			t.Errorf("customer message %q names %q", refusedCardCustomerMessage, word)
		}
	}
}

// paymentFailedEventBody is a payment_intent.payment_failed event as Stripe
// posts it: the PaymentIntent with its last_payment_error.
func paymentFailedEventBody(lastPaymentError string) string {
	return `{
  "id": "evt_test_failed",
  "object": "event",
  "type": "payment_intent.payment_failed",
  "data": {"object": {
    "id": "pi_web",
    "object": "payment_intent",
    "amount": 5000,
    "currency": "usd",
    "last_payment_error": ` + lastPaymentError + `,
    "metadata": {"velox_invoice_id": "inv_1", "velox_tenant_id": "t1"},
    "payment_method": null,
    "status": "requires_payment_method"
  }}
}`
}

func TestWebhookPaymentFailed_CarriesDeclineCodeAndCard(t *testing.T) {
	cases := []struct {
		name, lastPaymentError string
		wantCode, wantCard     string
	}{
		{
			name: "stolen card, payment_method object",
			lastPaymentError: `{"code": "card_declined", "decline_code": "stolen_card", "message": "Your card was declined.",
				"payment_method": {"id": "pm_stolen", "object": "payment_method", "type": "card"}, "type": "card_error"}`,
			wantCode: "stolen_card", wantCard: "pm_stolen",
		},
		{
			name:             "payment_method as a bare id",
			lastPaymentError: `{"code": "card_declined", "decline_code": "lost_card", "payment_method": "pm_bare", "type": "card_error"}`,
			wantCode:         "lost_card", wantCard: "pm_bare",
		},
		{
			name:             "no last_payment_error",
			lastPaymentError: `null`,
		},
		{
			// Unreadable decline fields must not fail the settlement: the
			// decline is still recorded, and is retried on schedule.
			name:             "decline_code of the wrong type",
			lastPaymentError: `{"decline_code": 42, "payment_method": {"id": "pm_x"}}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			invoices := newMockInvoiceUpdater()
			invoices.invoices["inv_1"] = pendingSnapshot()
			s, _, _ := newDeclineStripe(&mockStripeClient{}, invoices)

			err := s.HandleWebhook(context.Background(), "t1", domain.StripeWebhookEvent{
				StripeEventID:   "evt_" + tc.name,
				EventType:       "payment_intent.payment_failed",
				PaymentIntentID: "pi_web",
				InvoiceID:       "inv_1",
				FailureMessage:  "Your card was declined.",
				Payload:         map[string]any{"raw": paymentFailedEventBody(tc.lastPaymentError)},
			})
			if err != nil {
				t.Fatalf("HandleWebhook: %v", err)
			}
			if len(invoices.failureReports) != 1 {
				t.Fatalf("failure reports = %d, want 1", len(invoices.failureReports))
			}
			rep := invoices.failureReports[0]
			if rep.DeclineCode != tc.wantCode || rep.PaymentMethodID != tc.wantCard {
				t.Errorf("report decline %q card %q, want %q %q", rep.DeclineCode, rep.PaymentMethodID, tc.wantCode, tc.wantCard)
			}
			if got := invoices.invoices["inv_1"].PaymentStatus; got != domain.PaymentFailed {
				t.Errorf("payment status %s, want failed", got)
			}
		})
	}
}

// The reconciler is the third reporter: a charge whose response was lost and
// whose PaymentIntent the reconciler finds declined. It reads the decline
// from the retrieved PaymentIntent, decoded here from Stripe's JSON.
func TestPaymentIntentResult_ReadsLastPaymentError(t *testing.T) {
	var pi stripe.PaymentIntent
	if err := json.Unmarshal([]byte(`{
  "id": "pi_rec",
  "object": "payment_intent",
  "amount": 5000,
  "amount_received": 0,
  "currency": "usd",
  "last_payment_error": {
    "code": "card_declined",
    "decline_code": "pickup_card",
    "message": "Your card was declined.",
    "payment_method": {"id": "pm_pickup", "object": "payment_method", "type": "card"},
    "type": "card_error"
  },
  "metadata": {"velox_invoice_id": "inv_1", "velox_purpose": "dunning_retry"},
  "payment_method": null,
  "status": "requires_payment_method"
}`), &pi); err != nil {
		t.Fatalf("decode PaymentIntent: %v", err)
	}
	res := paymentIntentResult(&pi)
	if res.DeclineCode != "pickup_card" || res.PaymentMethodID != "pm_pickup" {
		t.Errorf("result decline %q card %q, want pickup_card pm_pickup", res.DeclineCode, res.PaymentMethodID)
	}
	if res.Status != "requires_payment_method" || res.Purpose != "dunning_retry" {
		t.Errorf("result status %q purpose %q, want requires_payment_method dunning_retry", res.Status, res.Purpose)
	}

	var ok stripe.PaymentIntent
	if err := json.Unmarshal([]byte(`{"id": "pi_ok", "object": "payment_intent", "status": "succeeded", "last_payment_error": null}`), &ok); err != nil {
		t.Fatalf("decode PaymentIntent: %v", err)
	}
	if res := paymentIntentResult(&ok); res.DeclineCode != "" || res.PaymentMethodID != "" {
		t.Errorf("succeeded PI result decline %q card %q, want both empty", res.DeclineCode, res.PaymentMethodID)
	}
}

func TestReconciler_DeclinedPI_CarriesDeclineCodeAndCard(t *testing.T) {
	store := newMockReconcileStore(domain.Invoice{
		ID: "inv_1", TenantID: "t1", PaymentStatus: domain.PaymentUnknown,
		StripePaymentIntentID: "pi_rec",
	})
	client := &mockStripeClient{piStates: map[string]PaymentIntentResult{
		"pi_rec": {ID: "pi_rec", Status: "requires_payment_method", DeclineCode: "lost_card", PaymentMethodID: "pm_lost"},
	}}
	rec := &recordingSettler{}
	r := NewReconciler(client, store, time.Second)
	r.SetSettler(rec)

	if _, errs := r.Run(context.Background(), 10); len(errs) != 0 {
		t.Fatalf("errs: %v", errs)
	}
	if len(rec.failed) != 1 {
		t.Fatalf("SettleFailed calls = %d, want 1", len(rec.failed))
	}
	if got := rec.failed[0]; got.declineCode != "lost_card" || got.declinedCard != "pm_lost" {
		t.Errorf("reconciler report decline %q card %q, want lost_card pm_lost", got.declineCode, got.declinedCard)
	}
}
