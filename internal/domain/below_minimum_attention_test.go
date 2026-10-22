package domain

import (
	"strings"
	"testing"
	"time"
)

func belowMinimumInvoice() Invoice {
	at := time.Date(2026, 10, 22, 6, 46, 57, 0, time.UTC)
	return Invoice{
		ID: "vlx_inv_min", Status: InvoiceFinalized, PaymentStatus: PaymentPending,
		AmountDueCents: 30, Currency: "USD", UpdatedAt: at.Add(time.Hour),
		TaxFacts:         TaxFacts{TaxStatus: InvoiceTaxOK},
		LastPaymentError: "Amount must be at least $0.50 usd",
		BelowMinimumAt:   &at,
	}
}

// An invoice Stripe refused as below its minimum charge reads as that, for
// the operator to act on, and not as a decline, a scheduled charge or a
// missing card: each of those promises a charge that cannot happen.
func TestClassify_AmountBelowMinimum(t *testing.T) {
	inv := belowMinimumInvoice()
	for name, atc := range map[string]AttentionContext{
		"card on file":       {HasPaymentMethod: true},
		"no card on file":    {},
		"collection paused":  {HasPaymentMethod: true, CollectionPaused: true},
		"retry still queued": {HasPaymentMethod: true},
	} {
		t.Run(name, func(t *testing.T) {
			in := inv
			if name == "retry still queued" {
				in.AutoChargePending = true
			}
			att := ClassifyInvoiceAttention(in, atc)
			if att == nil || att.Reason != AttentionReasonAmountBelowMinimum || att.Code != "payment.amount_too_small" ||
				att.Severity != AttentionSeverityWarning {
				t.Fatalf("attention %+v, want warning amount_below_minimum / payment.amount_too_small", att)
			}
			checkShape(t, att.Message)
			if !sameActions(actionCodes(att), AttentionActionVoidInvoice) {
				t.Errorf("actions %v, want only void_invoice", actionCodes(att))
			}
			if att.ProviderResponse != inv.LastPaymentError {
				t.Errorf("provider_response %q, want Stripe's sentence", att.ProviderResponse)
			}
			if att.Since == nil || !att.Since.Equal(*inv.BelowMinimumAt) {
				t.Errorf("since %v, want the refusal's time %v", att.Since, inv.BelowMinimumAt)
			}
			for _, w := range []string{"void", "record a payment"} {
				if !strings.Contains(att.Message, w) {
					t.Errorf("message %q does not name %q", att.Message, w)
				}
			}
			if !strings.Contains(att.Detail, "next invoice") {
				t.Errorf("detail %q does not say how to bill it on the next invoice", att.Detail)
			}
			// Collect payment applies a credit balance before it charges, so
			// it can still pay the invoice: the detail says so.
			if !strings.Contains(att.Detail, "credit balance") {
				t.Errorf("detail %q does not say Collect payment applies a credit balance", att.Detail)
			}
			// The minimum depends on the account's settlement currency, so
			// Velox names no figure; Stripe's sentence in provider_response
			// does.
			if strings.Contains(att.Detail, "$0.50") {
				t.Errorf("detail %q names a minimum; it is true only for an account that settles in USD", att.Detail)
			}
		})
	}

	t.Run("a later decline or a paid invoice reads as itself", func(t *testing.T) {
		failed := belowMinimumInvoice()
		failed.PaymentStatus = PaymentFailed
		if att := ClassifyInvoiceAttention(failed, AttentionContext{HasPaymentMethod: true}); att == nil || att.Reason != AttentionReasonPaymentFailed {
			t.Errorf("failed: %+v, want payment_failed", att)
		}
		paid := belowMinimumInvoice()
		paid.Status, paid.PaymentStatus = InvoicePaid, PaymentSucceeded
		if att := ClassifyInvoiceAttention(paid, AttentionContext{}); att != nil {
			t.Errorf("paid: %+v, want none", att)
		}
	})

	t.Run("with prepaid credit at risk, one Void button", func(t *testing.T) {
		att := ClassifyInvoiceAttention(belowMinimumInvoice(), AttentionContext{HasPaymentMethod: true, CommitGrantDrawableCents: 2000})
		if att == nil || att.Reason != AttentionReasonAmountBelowMinimum || !sameActions(actionCodes(att), AttentionActionVoidInvoice) {
			t.Errorf("attention %+v actions %v, want amount_below_minimum with one void_invoice", att, actionCodes(att))
		}
	})
}
