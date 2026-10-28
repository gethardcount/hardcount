package api

import (
	"slices"
	"testing"
	"time"

	"github.com/stripe/stripe-go/v82/form"

	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/payment"
)

func hostedCheckoutTestInputs() (payment.CheckoutClaim, domain.Invoice) {
	claim := payment.CheckoutClaim{
		ID:          "hc_cks_1",
		TenantID:    "t1",
		AmountCents: 1029,
		Currency:    "USD",
		CreatedAt:   time.Date(2026, 10, 22, 9, 0, 0, 0, time.UTC),
	}
	inv := domain.Invoice{ID: "inv_1", InvoiceNumber: "ACME-000135", CustomerID: "cus_1"}
	return claim, inv
}

func encodeHostedCheckoutParams(claim payment.CheckoutClaim, inv domain.Invoice) *form.Values {
	v := &form.Values{}
	form.AppendTo(v, hostedInvoiceCheckoutParams(claim, "cus_stripe_1", inv,
		"https://pay.example/success", "https://pay.example/cancel"))
	return v
}

// TestHostedInvoiceCheckoutParams_ChargesInInvoiceCurrency: the hosted Pay
// button charges exactly the invoice's amount in the invoice's currency. With
// adaptive_pricing unset, Stripe applies the account's Dashboard setting and
// may charge the buyer in a converted local currency with a conversion fee
// (a USD 10.29 invoice charged as INR 1,010.68). The assertion reads the wire
// form, so a nil field (nothing sent, the Dashboard default applies) fails as
// surely as enabled=true.
func TestHostedInvoiceCheckoutParams_ChargesInInvoiceCurrency(t *testing.T) {
	claim, inv := hostedCheckoutTestInputs()
	v := encodeHostedCheckoutParams(claim, inv)

	for key, want := range map[string]string{
		"adaptive_pricing[enabled]":                           "false",
		"mode":                                                "payment",
		"line_items[0][price_data][currency]":                 "usd",
		"line_items[0][price_data][unit_amount]":              "1029",
		"payment_intent_data[metadata][hardcount_purpose]":    payment.PurposeHostedInvoicePay,
		"payment_intent_data[metadata][hardcount_invoice_id]": "inv_1",
	} {
		if got := v.Get(key); !slices.Equal(got, []string{want}) {
			t.Errorf("%s = %q, want [%q]", key, got, want)
		}
	}
}

// TestHostedInvoiceCheckoutParams_StableAcrossReDrives: every re-drive of a
// claim replays the claim id as the Stripe idempotency key, and Stripe rejects
// a replayed key whose params changed. Two builds for the same claim at
// different wall-clock times must therefore send the identical request.
func TestHostedInvoiceCheckoutParams_StableAcrossReDrives(t *testing.T) {
	claim, inv := hostedCheckoutTestInputs()
	first := encodeHostedCheckoutParams(claim, inv).Encode()
	firstKey := *hostedInvoiceCheckoutParams(claim, "cus_stripe_1", inv, "s", "c").IdempotencyKey

	// Cross a Unix-second boundary so any clock-relative param would differ.
	time.Sleep(time.Until(time.Now().Truncate(time.Second).Add(time.Second + 10*time.Millisecond)))

	second := encodeHostedCheckoutParams(claim, inv).Encode()
	secondKey := *hostedInvoiceCheckoutParams(claim, "cus_stripe_1", inv, "s", "c").IdempotencyKey
	if first != second {
		t.Fatalf("re-drive params differ:\nfirst:  %s\nsecond: %s", first, second)
	}
	if firstKey != claim.ID || secondKey != claim.ID {
		t.Fatalf("idempotency keys = %q, %q, want the claim id %q", firstKey, secondKey, claim.ID)
	}
}
