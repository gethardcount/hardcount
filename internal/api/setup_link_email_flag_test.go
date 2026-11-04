package api

import (
	"context"
	"strings"
	"testing"

	"github.com/gethardcount/hardcount/internal/domain"
)

// TestCanEmailSetupLinks: the invoice notices say "a setup link is emailed"
// and offer Resend only when this flag is true, so it must be false in every
// case where the setup-link sender refuses. A compose install with SMTP set
// up but PAYMENT_UPDATE_URL empty used to get the "emailed once" line and a
// Resend button that failed. Without PAYMENT_UPDATE_RETURN_URL the link's page
// refuses to add the card, so the email is refused too.
func TestCanEmailSetupLinks(t *testing.T) {
	const page, back = "https://pay.example.com/update-payment", "https://pay.example.com/payment-method-added"
	cases := []struct {
		smtp           bool
		url, returnURL string
		want           bool
	}{
		{true, page, back, true},
		{true, page, "", false},
		{true, "", back, false},
		{true, "", "", false},
		{false, page, back, false},
		{false, "", "", false},
	}
	for _, tc := range cases {
		if got := canEmailSetupLinks(tc.smtp, tc.url, tc.returnURL); got != tc.want {
			t.Errorf("canEmailSetupLinks(%v, %q, %q) = %v, want %v", tc.smtp, tc.url, tc.returnURL, got, tc.want)
		}
	}
}

// TestCanEmailSetupLinks_AgreesWithSender: the sender itself refuses when
// either address is empty, even with SMTP working, and before it mints a
// token. If the sender ever stops needing an address, this and the flag
// change together.
func TestCanEmailSetupLinks_AgreesWithSender(t *testing.T) {
	cases := []struct {
		name, url, returnURL, wantErr string
	}{
		{"no payment-update page", "", "https://pay.example.com/payment-method-added", "PAYMENT_UPDATE_URL"},
		{"no return page", "https://pay.example.com/update-payment", "", "PAYMENT_UPDATE_RETURN_URL"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// tokenSvc and the email sender are nil: a refusal must come
			// before either is used.
			a := &noPaymentMethodNotifierAdapter{paymentUpdateURL: tc.url, paymentUpdateReturnURL: tc.returnURL}
			_, err := a.NotifyNoPaymentMethod(context.Background(), "t", domain.Invoice{}, "operator_resend")
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("sender error = %v, want a refusal naming %s", err, tc.wantErr)
			}
			if canEmailSetupLinks(true, tc.url, tc.returnURL) {
				t.Fatal("flag says setup links can be emailed while the sender refuses")
			}
		})
	}
}
