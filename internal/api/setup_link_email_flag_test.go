package api

import (
	"context"
	"testing"

	"github.com/sagarsuperuser/velox/internal/domain"
)

// TestCanEmailSetupLinks: the invoice notices say "a setup link is emailed"
// and offer Resend only when this flag is true, so it must be false in every
// case where the setup-link sender refuses. A compose install with SMTP set
// up but PAYMENT_UPDATE_URL empty used to get the "emailed once" line and a
// Resend button that failed.
func TestCanEmailSetupLinks(t *testing.T) {
	cases := []struct {
		smtp bool
		url  string
		want bool
	}{
		{true, "https://pay.example.com/update-payment", true},
		{true, "", false},
		{false, "https://pay.example.com/update-payment", false},
		{false, "", false},
	}
	for _, tc := range cases {
		if got := canEmailSetupLinks(tc.smtp, tc.url); got != tc.want {
			t.Errorf("canEmailSetupLinks(%v, %q) = %v, want %v", tc.smtp, tc.url, got, tc.want)
		}
	}
}

// TestCanEmailSetupLinks_AgreesWithSender: the sender itself refuses when the
// URL is empty, even with SMTP working. If the sender ever stops needing the
// URL, this and the flag change together.
func TestCanEmailSetupLinks_AgreesWithSender(t *testing.T) {
	a := &noPaymentMethodNotifierAdapter{paymentUpdateURL: ""}
	if _, err := a.NotifyNoPaymentMethod(context.Background(), "t", domain.Invoice{}, "operator_resend"); err == nil {
		t.Fatal("sender with no PAYMENT_UPDATE_URL sent; expected a refusal")
	}
	if canEmailSetupLinks(true, "") {
		t.Fatal("flag says setup links can be emailed while the sender refuses")
	}
}
