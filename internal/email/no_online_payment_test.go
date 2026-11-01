package email

import (
	"context"
	"errors"
	stdhtml "html"
	"net/mail"
	"strings"
	"sync"
	"testing"

	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/platform/leader"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
)

// stripeCheck is a fake StripeReadiness. It answers connected, or err, and
// records each tenant and mode it was asked about.
type stripeCheck struct {
	connected bool
	err       error

	mu    sync.Mutex
	asked []askedMode
}

type askedMode struct {
	tenantID string
	livemode bool
}

func (c *stripeCheck) HasFor(_ context.Context, tenantID string, livemode bool) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.asked = append(c.asked, askedMode{tenantID, livemode})
	return c.connected, c.err
}

func (c *stripeCheck) askedModes() []askedMode {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]askedMode(nil), c.asked...)
}

// payWording is what an email must not say when the hosted page it links has
// no Pay button.
var payWording = []string{"pay your invoice", "view or pay", "view & pay", "pay invoice", "settle it using the link", "using the link below"}

func assertNoPayWording(t *testing.T, part, body string) {
	t.Helper()
	lower := strings.ToLower(stdhtml.UnescapeString(body))
	for _, never := range payWording {
		if strings.Contains(lower, never) {
			t.Errorf("%s part says %q, but the hosted page can't take a payment:\n%s", part, never, body)
		}
	}
}

// With no Stripe connected for the invoice's mode, the hosted page says
// "Online payment isn't set up for this invoice. Contact … to arrange
// payment." The invoice email used to say "Tap the button below to view or
// pay your invoice", with a "View & pay invoice" button and "View or pay
// online" in the text part. It now asks the customer to pay as arranged and
// offers "View invoice", in both parts. With Stripe connected it is
// unchanged.
func TestInvoiceEmail_NoOnlinePaymentOffersViewNotPay(t *testing.T) {
	const hosted = "https://pay.example.com/invoice/tok"
	subject, rawHTML, ctaURL, ctaLabel := renderInvoiceHTML("Ada", "INV-0042", "$60.00", hosted, false, false, false)
	text := invoiceTextBody("Ada", "INV-0042", "$60.00", "usd", hosted, false, false, false)

	if ctaURL != hosted || ctaLabel != "View invoice" {
		t.Errorf("CTA %q %q, want View invoice on the hosted page", ctaLabel, ctaURL)
	}
	if subject != "Invoice INV-0042 — $60.00" {
		t.Errorf("subject %q, want the amount still owed", subject)
	}
	html := stdhtml.UnescapeString(rawHTML)
	for part, body := range map[string]string{"html": html, "text": text} {
		if !strings.Contains(body, payAsArranged) {
			t.Errorf("%s part does not ask to pay as arranged (%q):\n%s", part, payAsArranged, body)
		}
		assertNoPayWording(t, part, body)
	}
	if !strings.Contains(html, "Tap the button below to view your invoice.") {
		t.Errorf("html part does not say the button views the invoice:\n%s", html)
	}
	if !strings.Contains(text, "View online: "+hosted) {
		t.Errorf("text part has no view link:\n%s", text)
	}

	// Control: with Stripe connected the email is unchanged.
	_, rawPay, _, payLabel := renderInvoiceHTML("Ada", "INV-0042", "$60.00", hosted, false, false, true)
	if payLabel != "View & pay invoice" || !strings.Contains(rawPay, "Tap the button below to view or pay your invoice.") || strings.Contains(rawPay, escape(payAsArranged)) {
		t.Errorf("CONTROL: with Stripe the CTA is %q and the body:\n%s", payLabel, rawPay)
	}
	if payText := invoiceTextBody("Ada", "INV-0042", "$60.00", "usd", hosted, false, false, true); !strings.Contains(payText, "View or pay online: "+hosted) || strings.Contains(payText, payAsArranged) {
		t.Errorf("CONTROL: with Stripe the text part is:\n%s", payText)
	}
}

// The settled and written-off invoice emails already offer no payment. The
// Stripe connection does not change a word of them.
func TestInvoiceEmail_SettledAndWrittenOffIgnoreTheStripeConnection(t *testing.T) {
	const hosted = "https://pay.example.com/invoice/tok"
	for _, tc := range []struct {
		name                string
		zeroDue, writtenOff bool
	}{
		{"settled", true, false},
		{"written off", false, true},
		{"settled and written off", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s1, h1, u1, l1 := renderInvoiceHTML("Ada", "INV-0042", "$60.00", hosted, tc.zeroDue, tc.writtenOff, true)
			s2, h2, u2, l2 := renderInvoiceHTML("Ada", "INV-0042", "$60.00", hosted, tc.zeroDue, tc.writtenOff, false)
			if s1 != s2 || h1 != h2 || u1 != u2 || l1 != l2 {
				t.Errorf("HTML part changes with the Stripe connection:\nwith:    %q %q\n%s\nwithout: %q %q\n%s", l1, u1, h1, l2, u2, h2)
			}
			t1 := invoiceTextBody("Ada", "INV-0042", "$60.00", "usd", hosted, tc.zeroDue, tc.writtenOff, true)
			t2 := invoiceTextBody("Ada", "INV-0042", "$60.00", "usd", hosted, tc.zeroDue, tc.writtenOff, false)
			if t1 != t2 {
				t.Errorf("text part changes with the Stripe connection:\nwith:\n%s\nwithout:\n%s", t1, t2)
			}
		})
	}
}

// A dunning run with no Stripe connected still ends in the policy's final
// actions (ADR-060 amendment 2026-10-30), and the final notice offered a
// "Pay invoice" button and "settle it using the link below". The hosted page
// it links can't take a payment, so the notice now asks the customer to pay
// as arranged and offers "View invoice", for every outcome that leaves the
// invoice owed. With Stripe connected it is unchanged.
func TestEscalation_NoOnlinePaymentOffersViewNotPay(t *testing.T) {
	const hosted = "https://pay.example.com/invoice/tok"
	for _, o := range []domain.DunningEscalationOutcome{
		{},
		{SubscriptionCanceled: true},
		{SubscriptionPaused: true},
	} {
		_, rawHTML, ctaURL, ctaLabel := renderDunningEscalationHTML("Ada", "INV-0042", o, hosted, false)
		text := dunningEscalationTextBody("Ada", "INV-0042", o, hosted, false)
		if ctaURL != hosted || ctaLabel != "View invoice" {
			t.Errorf("outcome %+v: CTA %q %q, want View invoice on the hosted page", o, ctaLabel, ctaURL)
		}
		for part, body := range map[string]string{"html": stdhtml.UnescapeString(rawHTML), "text": text} {
			if !strings.Contains(body, escalationLead("INV-0042")) || !strings.Contains(body, payAsArranged) {
				t.Errorf("outcome %+v: %s part does not say it is unpaid and to pay as arranged:\n%s", o, part, body)
			}
			assertNoPayWording(t, part, body)
		}
		if !strings.Contains(text, "View invoice: "+hosted) {
			t.Errorf("outcome %+v: text part has no view link:\n%s", o, text)
		}

		// Control: with Stripe connected the notice is unchanged.
		_, _, _, payLabel := renderDunningEscalationHTML("Ada", "INV-0042", o, hosted, true)
		payText := dunningEscalationTextBody("Ada", "INV-0042", o, hosted, true)
		if payLabel != "Pay invoice" || !strings.Contains(payText, "Pay invoice: "+hosted) || strings.Contains(payText, payAsArranged) {
			t.Errorf("CONTROL outcome %+v: with Stripe the CTA is %q and the text part:\n%s", o, payLabel, payText)
		}
	}
	// The subscription clause still leads the sentence.
	if sentence, offerPay := dunningEscalationCopy(domain.DunningEscalationOutcome{SubscriptionCanceled: true}, false); offerPay ||
		sentence != "Your subscription has been canceled. The outstanding balance remains due. "+payAsArranged {
		t.Errorf("canceled, no Stripe: %q offerPay=%v", sentence, offerPay)
	}
	// Written off: already closed for online payment, whatever Stripe says.
	for _, payOnline := range []bool{true, false} {
		if sentence, offerPay := dunningEscalationCopy(writtenOffOutcome, payOnline); offerPay || sentence != closedForOnlinePayment {
			t.Errorf("written off, payOnline=%v: %q offerPay=%v", payOnline, sentence, offerPay)
		}
	}
}

// sentParts is the decoded HTML and text parts of the last message srv took.
func sentParts(t *testing.T, srv *scriptedSMTP) (html, text string) {
	t.Helper()
	msg, err := mail.ReadMessage(strings.NewReader(srv.lastData()))
	if err != nil {
		t.Fatalf("parse sent message: %v", err)
	}
	texts := map[string]string{}
	walkParts(t, msg.Header, msg.Body, texts)
	return texts["text/html"], texts["text/plain"]
}

func messagesSent(srv *scriptedSMTP) int {
	srv.mu.Lock()
	defer srv.mu.Unlock()
	return len(srv.data)
}

// The caller: the outbox dispatcher hands a queued invoice email to the
// Sender, which asks the Stripe check about the row's own mode and renders
// what it answers. Through a real SMTP exchange, the customer's HTML and text
// parts say "View invoice" and pay as arranged when Stripe isn't connected,
// and "View & pay invoice" when it is.
//
// Mutation: make SendInvoice pass payOnline=true without asking (the
// no-Stripe arms fail), or ask about a fixed mode (the mode check fails).
func TestDispatcher_InvoiceEmailSaysWhatTheHostedPageCanDo(t *testing.T) {
	for _, tc := range []struct {
		name      string
		connected bool
		livemode  bool
		wantCTA   string
		wantText  string
	}{
		{"no Stripe, test mode", false, false, ">View invoice<", "View online: https://pay.example.com/invoice/tok_abc"},
		{"no Stripe, live mode", false, true, ">View invoice<", "View online: https://pay.example.com/invoice/tok_abc"},
		{"Stripe connected", true, false, ">View &amp; pay invoice<", "View or pay online: https://pay.example.com/invoice/tok_abc"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := newScriptedSMTP(t)
			s := newCCTestSender(t, srv)
			s.hostedInvoiceBaseURL = "https://pay.example.com"
			check := &stripeCheck{connected: tc.connected}
			s.SetStripeReadiness(check)
			d := NewDispatcher(nil, s, DispatcherConfig{}, leader.AlwaysLead{})

			row := rowOf(TypeInvoice)
			row.TenantID = "t_acme"
			row.Livemode = tc.livemode
			row.Payload = map[string]any{
				"to": "ap@acme.test", "customer_name": "Ada", "invoice_number": "INV-0042",
				"amount_cents": 6000, "currency": "usd", "public_token": "tok_abc",
			}
			if err := d.handle(context.Background(), row); err != nil {
				t.Fatalf("handle: %v", err)
			}

			if got := check.askedModes(); len(got) != 1 || got[0] != (askedMode{"t_acme", tc.livemode}) {
				t.Errorf("Stripe check asked %+v, want once for t_acme in the row's mode (livemode %v)", got, tc.livemode)
			}
			html, text := sentParts(t, srv)
			if !strings.Contains(html, tc.wantCTA) {
				t.Errorf("HTML part has no %q button:\n%s", tc.wantCTA, html)
			}
			if !strings.Contains(text, tc.wantText) {
				t.Errorf("text part has no %q:\n%s", tc.wantText, text)
			}
			if !tc.connected {
				for part, body := range map[string]string{"html": html, "text": text} {
					if !strings.Contains(body, payAsArranged) {
						t.Errorf("%s part does not ask to pay as arranged:\n%s", part, body)
					}
					assertNoPayWording(t, part, body)
				}
			}
		})
	}
}

// The final notice reads the same check on the same path: with no Stripe it
// offers "View invoice" and asks to pay as arranged; with Stripe, "Pay
// invoice". A written-off invoice's notice is already closed for online
// payment, so it does not ask.
func TestDispatcher_EscalationSaysWhatTheHostedPageCanDo(t *testing.T) {
	send := func(t *testing.T, check *stripeCheck, outcome domain.DunningEscalationOutcome) (html, text string) {
		t.Helper()
		srv := newScriptedSMTP(t)
		s := newCCTestSender(t, srv)
		s.hostedInvoiceBaseURL = "https://pay.example.com"
		s.SetStripeReadiness(check)
		d := NewDispatcher(nil, s, DispatcherConfig{}, leader.AlwaysLead{})
		row := rowOf(TypeDunningEscalation)
		row.Livemode = true
		row.Payload = map[string]any{
			"to": "ap@acme.test", "customer_name": "Ada", "invoice_number": "INV-0042",
			"public_token": "tok_abc", "escalation_outcome": outcome,
		}
		if err := d.handle(context.Background(), row); err != nil {
			t.Fatalf("handle: %v", err)
		}
		return sentParts(t, srv)
	}

	noStripe := &stripeCheck{connected: false}
	html, text := send(t, noStripe, domain.DunningEscalationOutcome{SubscriptionCanceled: true})
	if got := noStripe.askedModes(); len(got) != 1 || got[0] != (askedMode{"t1", true}) {
		t.Errorf("Stripe check asked %+v, want once for t1 in live mode", got)
	}
	if !strings.Contains(html, ">View invoice<") || !strings.Contains(text, "View invoice: https://pay.example.com/invoice/tok_abc") {
		t.Errorf("no Stripe: want View invoice in both parts\nhtml:\n%s\ntext:\n%s", html, text)
	}
	for part, body := range map[string]string{"html": html, "text": text} {
		if !strings.Contains(body, payAsArranged) {
			t.Errorf("no Stripe: %s part does not ask to pay as arranged:\n%s", part, body)
		}
		assertNoPayWording(t, part, body)
	}

	html, text = send(t, &stripeCheck{connected: true}, domain.DunningEscalationOutcome{SubscriptionCanceled: true})
	if !strings.Contains(html, ">Pay invoice<") || !strings.Contains(text, "Pay invoice: https://pay.example.com/invoice/tok_abc") {
		t.Errorf("CONTROL with Stripe: want Pay invoice in both parts\nhtml:\n%s\ntext:\n%s", html, text)
	}

	failing := &stripeCheck{err: errors.New("db down")}
	html, _ = send(t, failing, writtenOffOutcome)
	if n := len(failing.askedModes()); n != 0 {
		t.Errorf("written off: Stripe check asked %d times, want 0", n)
	}
	if !strings.Contains(html, ">View invoice<") || !strings.Contains(html, closedForOnlinePayment) {
		t.Errorf("written off: want View invoice and %q:\n%s", closedForOnlinePayment, html)
	}
}

// A failed read is not "not connected" and not "connected": the send fails,
// nothing reaches the customer, and the outbox retries the row. So does a
// Sender with no check wired, and a ctx with no mode.
func TestSendInvoice_StripeCheckFailureSendsNothing(t *testing.T) {
	readErr := errors.New("decrypt stripe key: bad ciphertext")
	live := postgres.WithLivemode(context.Background(), false)
	for _, tc := range []struct {
		name  string
		check StripeReadiness
		ctx   context.Context
		want  error
	}{
		{"read error", &stripeCheck{err: readErr}, live, readErr},
		{"no check wired", nil, live, ErrNoStripeCheck},
		{"no mode on ctx", &stripeCheck{connected: true}, context.Background(), postgres.ErrLivemodeUnset},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := newScriptedSMTP(t)
			s := newCCTestSender(t, srv)
			s.hostedInvoiceBaseURL = "https://pay.example.com"
			if tc.check != nil {
				s.SetStripeReadiness(tc.check)
			}
			err := s.SendInvoice(tc.ctx, "t1", "ap@acme.test", nil, "Ada", "INV-0042", 6000, "usd", nil, "tok_abc", false)
			if !errors.Is(err, tc.want) {
				t.Errorf("invoice send error %v, want %v", err, tc.want)
			}
			err = s.SendDunningEscalation(tc.ctx, "t1", "ap@acme.test", nil, "Ada", "INV-0042", noOutcome, "tok_abc")
			if !errors.Is(err, tc.want) {
				t.Errorf("escalation send error %v, want %v", err, tc.want)
			}
			if n := messagesSent(srv); n != 0 {
				t.Errorf("%d messages reached SMTP, want none", n)
			}
		})
	}
}

// The check is read only when the email would offer to pay online. A settled
// or written-off invoice, or one with no hosted page to link, is sent without
// asking, so a failed read never holds it up.
func TestSendInvoice_AsksTheStripeCheckOnlyWhenItWouldOfferToPay(t *testing.T) {
	ctx := postgres.WithLivemode(context.Background(), false)
	for _, tc := range []struct {
		name       string
		dueCents   int64
		writtenOff bool
		token      string
	}{
		{"settled", 0, false, "tok_abc"},
		{"written off", 6000, true, "tok_abc"},
		{"no hosted page", 6000, false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := newScriptedSMTP(t)
			s := newCCTestSender(t, srv)
			s.hostedInvoiceBaseURL = "https://pay.example.com"
			check := &stripeCheck{err: errors.New("db down")}
			s.SetStripeReadiness(check)
			if err := s.SendInvoice(ctx, "t1", "ap@acme.test", nil, "Ada", "INV-0042", tc.dueCents, "usd", nil, tc.token, tc.writtenOff); err != nil {
				t.Fatalf("send: %v", err)
			}
			if n := len(check.askedModes()); n != 0 {
				t.Errorf("Stripe check asked %d times, want 0", n)
			}
			html, text := sentParts(t, srv)
			for part, body := range map[string]string{"html": html, "text": text} {
				assertNoPayWording(t, part, body)
			}
		})
	}
}
