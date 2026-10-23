package domain

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// Which decline codes ask what of the customer, and which stop a dunning
// retry: a hard decline asks for a different card, authentication_required
// for the customer's confirmation, and every other code for nothing beyond
// paying (the ordinary retry schedule).
func TestDeclineAskOf(t *testing.T) {
	cases := []struct {
		code  string
		ask   DeclineAsk
		block error
	}{
		{"lost_card", DeclineAskNewCard, ErrCardNeedsReplacing},
		{"incorrect_number", DeclineAskNewCard, ErrCardNeedsReplacing},
		{"authentication_required", DeclineAskConfirm, ErrCardNeedsConfirmation},
		// Related, but not on Stripe Billing's no-retry list.
		{"authentication_not_handled", DeclineAskNone, nil},
		{"insufficient_funds", DeclineAskNone, nil},
		{"generic_decline", DeclineAskNone, nil},
		{"", DeclineAskNone, nil},
	}
	for _, tc := range cases {
		if got := DeclineAskOf(tc.code); got != tc.ask {
			t.Errorf("DeclineAskOf(%q) = %q, want %q", tc.code, got, tc.ask)
		}
		if got := RetryBlock(tc.code); !errors.Is(got, tc.block) || (tc.block == nil) != (got == nil) {
			t.Errorf("RetryBlock(%q) = %v, want %v", tc.code, got, tc.block)
		}
	}
	if IsHardDecline(AuthenticationRequiredCode) {
		t.Error("authentication_required is a hard decline: its copy would ask for a different card")
	}
}

// P4 of the Stripe card-flow review (2026-10-22): an off-session charge the
// bank wants the customer to confirm read "The customer's card was declined"
// and offered Retry payment. The notice says what the bank asked, offers the
// payment link (the hosted page runs 3-D Secure), never Retry or Update
// payment method, and promises retries only while one is scheduled.
func TestNeedsConfirmationAttention(t *testing.T) {
	failed := Invoice{
		ID: "inv_1", Status: InvoiceFinalized, PaymentStatus: PaymentFailed,
		LastPaymentError: "This payment requires authentication. If the customer is off session, ask them to return to your application and complete the payment.",
		UpdatedAt:        time.Date(2026, 10, 22, 21, 0, 0, 0, time.UTC),
	}
	for _, retrying := range []bool{true, false} {
		att := ClassifyInvoiceAttention(failed, AttentionContext{
			HasPaymentMethod: true, RetryBlockingDeclineCode: AuthenticationRequiredCode, DunningRetrying: retrying,
		})
		if att == nil || att.Code != "payment.authentication_required" || att.Reason != AttentionReasonPaymentFailed {
			t.Fatalf("retrying=%v: attention %+v, want payment_failed / payment.authentication_required", retrying, att)
		}
		checkShape(t, att.Message)
		if !strings.Contains(att.Message, "bank needs them to confirm") || strings.Contains(strings.ToLower(att.Message), "declin") {
			t.Errorf("retrying=%v: message %q, want the bank's request and no decline", retrying, att.Message)
		}
		if !sameActions(actionCodes(att), AttentionActionSendReminder) {
			t.Errorf("retrying=%v: actions %v, want only send_reminder", retrying, actionCodes(att))
		}
		if att.Severity != AttentionSeverityWarning {
			t.Errorf("retrying=%v: severity %s, want warning", retrying, att.Severity)
		}
		// Stripe's developer text stays labelled as the provider's, never
		// Velox's own words.
		if att.ProviderResponse != failed.LastPaymentError || strings.Contains(att.Detail, "off session") {
			t.Errorf("retrying=%v: provider response %q / detail %q", retrying, att.ProviderResponse, att.Detail)
		}
		// 3-D Secure can pass without a challenge, so the bank may not ask.
		if strings.Contains(att.Detail, "bank asks them") || !strings.Contains(att.Detail, "bank may ask them to confirm") {
			t.Errorf("retrying=%v: detail must say the bank may ask, not that it will:\n%s", retrying, att.Detail)
		}
		promises := strings.Contains(att.Detail, "keeps its schedule")
		if promises != retrying {
			t.Errorf("retrying=%v: detail promises retries = %v:\n%s", retrying, promises, att.Detail)
		}
		if !retrying && !strings.Contains(att.Detail, "No automatic retry is scheduled") {
			t.Errorf("retrying=false: detail does not say no retry is scheduled:\n%s", att.Detail)
		}
	}

	// Controls: a hard decline is still "needs a different card", and an
	// ordinary decline still offers Retry.
	if att := ClassifyInvoiceAttention(failed, AttentionContext{HasPaymentMethod: true, RetryBlockingDeclineCode: "stolen_card"}); att == nil || att.Code != "payment.card_refused" {
		t.Errorf("hard decline: attention %+v, want payment.card_refused", att)
	}
	att := ClassifyInvoiceAttention(failed, AttentionContext{HasPaymentMethod: true})
	if att == nil || att.Code != "payment.declined" || !sameActions(actionCodes(att), AttentionActionUpdatePaymentMethod, AttentionActionRetryPayment) {
		t.Errorf("ordinary decline: attention %+v, want payment.declined with Update payment method and Retry payment", att)
	}
}
