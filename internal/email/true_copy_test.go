package email

import (
	"context"
	stdhtml "html"
	"net/mail"
	"strings"
	"testing"

	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/platform/leader"
)

var (
	noOutcome         = domain.DunningEscalationOutcome{}
	writtenOffOutcome = domain.DunningEscalationOutcome{InvoiceWrittenOff: true}
)

// F-01: a dunning retry that found no card on file charged nothing, yet the
// customer was told "Attempt 1 of 3 to charge invoice X didn't go through"
// and promised a retry date. The no-card warning says why nothing was
// charged, asks the customer to pay, and promises no retry: none can succeed
// until a card is added, and the hosted page it links to cannot save one.
func TestNoCardWarning_SaysNothingWasCharged(t *testing.T) {
	const hosted = "https://pay.example.com/i/tok"
	subject, rawHTML, ctaURL, ctaLabel := renderNoCardWarningHTML("Ada", "INV-0042", hosted)
	html := stdhtml.UnescapeString(rawHTML)
	text := noCardWarningTextBody("Ada", "INV-0042", hosted)

	if !strings.Contains(subject, "INV-0042") || strings.Contains(strings.ToLower(subject), "retry") {
		t.Errorf("subject %q, want the invoice number and no retry", subject)
	}
	for part, body := range map[string]string{"html": html, "text": text} {
		for _, want := range []string{"We couldn't charge invoice INV-0042 because there is no payment method on file.", "Please pay the invoice using the link below."} {
			if !strings.Contains(body, want) {
				t.Errorf("%s part does not say %q:\n%s", part, want, body)
			}
		}
		lower := strings.ToLower(body)
		for _, never := range []string{"didn't go through", "declin", "attempt", "retry", "try again", "unsuccessful"} {
			if strings.Contains(lower, never) {
				t.Errorf("%s part says %q:\n%s", part, never, body)
			}
		}
	}
	if ctaURL != hosted || ctaLabel != "Pay invoice" {
		t.Errorf("CTA %q %q, want Pay invoice on the hosted page", ctaLabel, ctaURL)
	}
	if !strings.Contains(text, "Pay invoice: "+hosted) {
		t.Errorf("text part has no pay link:\n%s", text)
	}

	// No hosted page: no button, and the copy asks them to contact us.
	_, rawNoLink, url, label := renderNoCardWarningHTML("Ada", "INV-0042", "")
	if url != "" || label != "" {
		t.Errorf("CTA %q %q with no hosted page, want none", label, url)
	}
	for part, body := range map[string]string{"html": stdhtml.UnescapeString(rawNoLink), "text": noCardWarningTextBody("Ada", "INV-0042", "")} {
		if strings.Contains(body, "link below") || strings.Contains(body, "Pay invoice:") || !strings.Contains(body, "Please contact us to pay the invoice.") {
			t.Errorf("%s part with no hosted page, want only \"Please contact us to pay the invoice.\":\n%s", part, body)
		}
	}
}

// F-01: the final email opened "All payment attempts for invoice X were
// unsuccessful", false for a customer with no card on file, whose retries
// charged nothing. It says the invoice is unpaid, in both parts, and points
// at us, never "support" or "reach out" (F-13).
func TestEscalation_SaysUnpaidNotThatAttemptsFailed(t *testing.T) {
	_, rawHTML, _, _ := renderDunningEscalationHTML("Ada", "INV-0042", noOutcome, "https://pay.example.com/i/tok")
	text := dunningEscalationTextBody("Ada", "INV-0042", noOutcome, "https://pay.example.com/i/tok")
	for part, body := range map[string]string{"html": stdhtml.UnescapeString(rawHTML), "text": text} {
		if !strings.Contains(body, "Invoice INV-0042 is still unpaid.") {
			t.Errorf("%s part does not say the invoice is unpaid:\n%s", part, body)
		}
		lower := strings.ToLower(body)
		for _, never := range []string{"attempts", "unsuccessful", "support", "reach out"} {
			if strings.Contains(lower, never) {
				t.Errorf("%s part says %q:\n%s", part, never, body)
			}
		}
	}
	_, rawOff, _, label := renderDunningEscalationHTML("Ada", "INV-0042", writtenOffOutcome, "https://pay.example.com/i/tok")
	if !strings.Contains(stdhtml.UnescapeString(rawOff), closedForOnlinePayment) || label != "View invoice" {
		t.Errorf("written-off escalation: CTA %q, want View invoice and %q:\n%s", label, closedForOnlinePayment, rawOff)
	}
}

// F-07: an operator can email a written-off invoice, whose hosted page has no
// Pay button (ADR-110). The email offered "View & pay invoice" and "Tap the
// button below to view or pay". It now says the invoice is closed for online
// payment and offers "View invoice", in both parts.
func TestInvoiceEmail_WrittenOffOffersViewNotPay(t *testing.T) {
	const hosted = "https://pay.example.com/i/tok"
	subject, rawHTML, ctaURL, ctaLabel := renderInvoiceHTML("Ada", "INV-0042", "$60.00", hosted, false, true)
	html := stdhtml.UnescapeString(rawHTML)
	text := invoiceTextBody("Ada", "INV-0042", "$60.00", "usd", hosted, false, true)

	if ctaURL != hosted || ctaLabel != "View invoice" {
		t.Errorf("CTA %q %q, want View invoice on the hosted page", ctaLabel, ctaURL)
	}
	if !strings.Contains(subject, "$60.00") {
		t.Errorf("subject %q, want the amount still owed", subject)
	}
	for part, body := range map[string]string{"html": html, "text": text} {
		if !strings.Contains(body, closedForOnlinePayment) {
			t.Errorf("%s part does not say it is closed for online payment:\n%s", part, body)
		}
		lower := strings.ToLower(body)
		for _, never := range []string{"pay your invoice", "view or pay", "view & pay", "is ready"} {
			if strings.Contains(lower, never) {
				t.Errorf("%s part says %q:\n%s", part, never, body)
			}
		}
	}
	// Control: an open invoice is still payable from the email.
	if _, _, _, label := renderInvoiceHTML("Ada", "INV-0042", "$60.00", hosted, false, false); label != "View & pay invoice" {
		t.Errorf("CONTROL: open invoice CTA %q, want View & pay invoice", label)
	}
}

// The same, through the real sender and an SMTP exchange: the HTML part a
// customer sees carries the "View invoice" button, never "View & pay".
func TestSendInvoice_WrittenOffHTMLPartOffersViewNotPay(t *testing.T) {
	srv := newScriptedSMTP(t)
	s := newCCTestSender(t, srv)
	s.hostedInvoiceBaseURL = "https://pay.example.com"
	for _, tc := range []struct {
		writtenOff bool
		want, not  string
	}{
		{true, "View invoice", "View &amp; pay invoice"},
		{false, "View &amp; pay invoice", closedForOnlinePayment},
	} {
		if err := s.SendInvoice(context.Background(), "t1", "ap@acme.test", nil, "Ada", "INV-0042", 6000, "usd", []byte("%PDF"), "tok_abc", tc.writtenOff); err != nil {
			t.Fatalf("send: %v", err)
		}
		msg, err := mail.ReadMessage(strings.NewReader(srv.lastData()))
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		texts := map[string]string{}
		walkParts(t, msg.Header, msg.Body, texts)
		html := texts["text/html"]
		if !strings.Contains(html, tc.want) || strings.Contains(html, tc.not) {
			t.Errorf("writtenOff=%v: HTML part wants %q and not %q:\n%s", tc.writtenOff, tc.want, tc.not, html)
		}
	}
}

// writtenOffDeliverer records the flag each invoice send carried.
type writtenOffDeliverer struct {
	recordingDeliverer
	writtenOff []bool
}

func (w *writtenOffDeliverer) SendInvoice(_ context.Context, _, _ string, _ []string, _, _ string, _ int64, _ string, _ []byte, _ string, writtenOff bool) error {
	w.writtenOff = append(w.writtenOff, writtenOff)
	return nil
}

// The queued invoice email carries the written-off flag to the sender, and a
// row an older binary queued (no flag) sends the ordinary copy.
func TestDispatcher_InvoiceEmailCarriesWrittenOff(t *testing.T) {
	for _, tc := range []struct {
		name    string
		payload map[string]any
		want    bool
	}{
		{"written off", map[string]any{"invoice_written_off": true}, true},
		{"open, or queued by the older binary", map[string]any{}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sender := &writtenOffDeliverer{}
			d := NewDispatcher(nil, sender, DispatcherConfig{}, leader.AlwaysLead{})
			row := rowOf(TypeInvoice)
			for k, v := range tc.payload {
				row.Payload[k] = v
			}
			if err := d.handle(context.Background(), row); err != nil {
				t.Fatalf("handle: %v", err)
			}
			if len(sender.writtenOff) != 1 || sender.writtenOff[0] != tc.want {
				t.Errorf("sent with writtenOff %v, want [%v]", sender.writtenOff, tc.want)
			}
		})
	}
}
