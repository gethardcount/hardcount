package email

import (
	"context"
	"errors"
	"fmt"
	"html/template"
	"net"
	"net/textproto"
	"os"
	"strings"
	"testing"
)

func TestSender_NotConfigured(t *testing.T) {
	s := &Sender{} // No SMTP config
	if s.IsConfigured() {
		t.Error("should not be configured without host")
	}

	// Contract: every send path must surface ErrSMTPNotConfigured when
	// SMTP_HOST is unset. Silent stdout fallback was removed so
	// misconfiguration is an operator-visible failure, not invisible
	// drops.
	err := s.SendInvoice(context.Background(), "t1", "test@example.com", nil, "Test Customer", "VLX-000001", 19900, "USD", []byte("%PDF"), "")
	if !errors.Is(err, ErrSMTPNotConfigured) {
		t.Errorf("unconfigured sender should return ErrSMTPNotConfigured, got: %v", err)
	}
}

func TestSender_Configured(t *testing.T) {
	s := &Sender{host: "smtp.example.com", port: "587", from: "billing@velox.dev"}
	if !s.IsConfigured() {
		t.Error("should be configured with host")
	}
}

func TestFormatAmount(t *testing.T) {
	tests := []struct {
		cents    int64
		currency string
		want     string
	}{
		{19900, "usd", "USD 199.00"},
		{500, "eur", "EUR 5.00"},
		{0, "usd", "USD 0.00"},
		{99, "gbp", "GBP 0.99"},
	}

	for _, tt := range tests {
		got := formatAmount(tt.cents, tt.currency)
		if got != tt.want {
			t.Errorf("formatAmount(%d, %q) = %q, want %q", tt.cents, tt.currency, got, tt.want)
		}
	}
}

// TestRenderInvoiceHTML exercises the layout + content render path end-to-
// end so template drift (missing var, escape slip) surfaces here instead
// of during a live send. The full HTML isn't byte-matched — we check the
// key substrings industry-standard verifiers look for.
func TestRenderInvoiceHTML(t *testing.T) {
	subject, content, ctaURL, ctaLabel := renderInvoiceHTML("Acme Corp", "INV-42", "USD 199.00", "https://app.velox.dev/invoice/vlx_pinv_abc", false)
	if subject != "Invoice INV-42 — USD 199.00" {
		t.Errorf("subject = %q", subject)
	}
	if ctaURL == "" || ctaLabel == "" {
		t.Errorf("CTA missing when hosted URL provided: url=%q label=%q", ctaURL, ctaLabel)
	}
	html, err := renderLayout(layoutInputs{
		Subject: subject, CompanyName: "YourCo", LogoURL: "https://cdn.example.com/logo.png",
		BrandColor: "#1f6feb", SupportURL: "https://yourco.com/support",
		Content: template.HTML(content),
		CTAURL:  ctaURL, CTALabel: ctaLabel,
	})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	for _, want := range []string{
		"YourCo",                           // company name in header
		"https://cdn.example.com/logo.png", // logo
		"#1f6feb",                          // brand color accent
		"INV-42",                           // invoice number
		"USD 199.00",                       // amount
		"vlx_pinv_abc",                     // hosted token in CTA URL
		"View &amp; pay invoice",           // CTA label (html-escaped "&")
		"Acme Corp",                        // customer name interpolated in content
		"Powered by Velox Billing",         // footer credit
	} {
		if !strings.Contains(html, want) {
			t.Errorf("rendered HTML missing %q", want)
		}
	}
	// No raw "<script" even if a hostile customer name got in — html-escape
	// happens inside renderInvoiceHTML because we wrap interpolations in
	// html.EscapeString before concatenation.
	hostile, _, _, _ := renderInvoiceHTML("<script>alert(1)</script>", "INV-42", "USD 0.01", "", false)
	if strings.Contains(hostile, "<script>") {
		t.Errorf("renderInvoiceHTML should escape customer name, got %q", hostile)
	}
}

// smtpReply builds the error net/smtp returns for a server reply, wrapped
// the way deliver and sendRich wrap it on the way to the classifier.
func smtpReply(code int, msg string) error {
	return fmt.Errorf("send email: %w", fmt.Errorf("smtp rcpt to: %w", &textproto.Error{Code: code, Msg: msg}))
}

// TestIsPermanentSMTPBounce checks the classifier behind the
// bounce-capture hook and the outbox's dead-letter decision. Only a
// server REPLY can be a bounce: 550-553 or a 5.1.x / 5.2.x status →
// permanent; sender-side 5xx, 4xx and every non-reply error → not.
func TestIsPermanentSMTPBounce(t *testing.T) {
	tests := []struct {
		name      string
		err       error
		permanent bool
	}{
		{"no error", nil, false},
		// Recipient-rejection replies — should classify as bounce
		{"classic 550", smtpReply(550, "5.1.1 User unknown"), true},
		{"551 user not local", smtpReply(551, "user not local"), true},
		{"553 relaying", smtpReply(553, "relaying denied"), true},
		{"552 too big", smtpReply(552, "message size exceeds limits"), true},
		{"550 mailbox unavailable", smtpReply(550, "mailbox unavailable"), true},
		{"unwrapped 550", &textproto.Error{Code: 550, Msg: "mailbox unavailable"}, true},
		{"enhanced 5.1.1 on an ambiguous 554", smtpReply(554, "5.1.1 user unknown"), true},
		{"enhanced 5.2.2 over quota", smtpReply(552, "5.2.2 mailbox full"), true},
		{"multi-line 550", smtpReply(550, "5.1.1 The email account that you tried to reach does not exist.\n5.1.1 Check the address and try again."), true},
		// Sender/server-side 5xx — must NOT classify as bounce (the
		// recipient is fine; OUR side has a problem). Pre-fix every
		// 5xx mapped to bounce; surfaced 2026-05-29 when a wrong
		// Postmark Server Token returned 535 and Velox flagged
		// every recipient as bounced as a result.
		{"535 auth failed (Postmark token mismatch)", smtpReply(535, "5.7.8 Error: authentication failed"), false},
		{"530 access denied", smtpReply(530, "5.7.0 authentication required"), false},
		{"521 server doesn't accept mail", smtpReply(521, "mail not accepted"), false},
		{"534 auth too weak", smtpReply(534, "5.7.9 auth too weak"), false},
		{"538 encryption required", smtpReply(538, "5.7.11 encryption required"), false},
		{"554 transaction failed ambiguous", smtpReply(554, "transaction failed"), false},
		// 5.7.x on a 550-553 reply is a policy rejection of the SENDER
		// (relay denied, SPF/DMARC failure, blocklisted IP): one
		// misconfigured relay would otherwise flag every customer.
		{"550 5.7.1 policy reject", smtpReply(550, "5.7.1 message rejected by policy"), false},
		{"553 5.7.1 sender address rejected", smtpReply(553, "5.7.1 Sender address rejected: not owned by user"), false},
		{"550 5.7.1 quoting 5.1.1 later in the text", smtpReply(550, "5.7.1 blocked by policy, see section 5.1.1 of the postmaster guide"), false},
		// Transient replies — not bounces, whatever their text quotes
		{"421 transient", smtpReply(421, "try again later"), false},
		{"450 soft bounce", smtpReply(450, "mailbox busy"), false},
		{"451 quoting 5.2.1", smtpReply(451, "4.3.0 deferred: upstream answered 550 5.2.1 mailbox disabled"), false},
		{"451 carrying a mismatched 5.2.1 status", smtpReply(451, "5.2.1 mailbox temporarily disabled"), false},
		{"421 quoting 550", smtpReply(421, "4.7.0 try again later (previous reply was 550)"), false},
		// Not a server reply at all — an address, a port or free text
		// that happens to look like a status must never mark a
		// recipient bounced.
		{"dial timeout to an address containing 5.1.2", fmt.Errorf("send email: %w", fmt.Errorf("smtp dial (starttls): %w",
			&net.OpError{Op: "dial", Net: "tcp", Addr: &net.TCPAddr{IP: net.IPv4(10, 5, 1, 2), Port: 587}, Err: os.ErrDeadlineExceeded})), false},
		{"read timeout from an address containing 5.2.7", fmt.Errorf("send email: %w", fmt.Errorf("smtp rcpt to: %w",
			&net.OpError{Op: "read", Net: "tcp", Source: &net.TCPAddr{IP: net.IPv4(10, 0, 0, 9), Port: 41000}, Addr: &net.TCPAddr{IP: net.IPv4(10, 5, 2, 7), Port: 587}, Err: os.ErrDeadlineExceeded})), false},
		{"read timeout from local port 55012", fmt.Errorf("send email: %w", fmt.Errorf("smtp data close: %w",
			&net.OpError{Op: "read", Net: "tcp", Source: &net.TCPAddr{IP: net.IPv4(10, 0, 0, 9), Port: 55012}, Addr: &net.TCPAddr{IP: net.IPv4(203, 0, 113, 7), Port: 587}, Err: os.ErrDeadlineExceeded})), false},
		{"bounce-looking text that is not a reply", errors.New("550 5.1.1 user unknown"), false},
		{"timeout", errors.New("dial tcp: i/o timeout"), false},
		{"zip code false-positive", errors.New("SMTP relay error for customer at 95014"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, _ := isPermanentSMTPBounce(tt.err)
			if got != tt.permanent {
				t.Errorf("isPermanentSMTPBounce(%v) = %v, want %v", tt.err, got, tt.permanent)
			}
		})
	}
}

// TestHostedInvoiceURL covers the base-URL-and-token assembly rules.
func TestHostedInvoiceURL(t *testing.T) {
	tests := []struct {
		name     string
		base     string
		token    string
		expected string
	}{
		{"empty base", "", "vlx_pinv_abc", ""},
		{"empty token", "https://app.example.com", "", ""},
		{"trailing slash trimmed", "https://app.example.com/", "vlx_pinv_abc", "https://app.example.com/invoice/vlx_pinv_abc"},
		{"no trailing slash", "https://app.example.com", "vlx_pinv_abc", "https://app.example.com/invoice/vlx_pinv_abc"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &Sender{hostedInvoiceBaseURL: tt.base}
			if got := s.hostedInvoiceURL(tt.token); got != tt.expected {
				t.Errorf("got %q, want %q", got, tt.expected)
			}
		})
	}
}

// TestInvoiceEmail_ZeroDueCopy locks the 2026-07-19 FLOW E copy finding:
// a settled invoice (amount_due==0 — the plumbed fact, paid or fully
// credit-covered) must not title itself "— USD 0.00" with a "View & pay"
// CTA. Both the HTML and plaintext twins adapt on the same fact.
func TestInvoiceEmail_ZeroDueCopy(t *testing.T) {
	subject, html, _, ctaLabel := renderInvoiceHTML("C", "NIM-7", "USD 0.00", "https://h/x", true)
	if subject != "Invoice NIM-7 — no payment due" {
		t.Errorf("zero-due subject: got %q", subject)
	}
	if !strings.Contains(html, "no payment is needed") {
		t.Error("zero-due HTML must say no payment is needed")
	}
	if ctaLabel != "View invoice" {
		t.Errorf("zero-due CTA: got %q, want 'View invoice' (never 'View & pay')", ctaLabel)
	}

	subject, _, _, ctaLabel = renderInvoiceHTML("C", "NIM-7", "USD 11.00", "https://h/x", false)
	if subject != "Invoice NIM-7 — USD 11.00" {
		t.Errorf("due subject changed: got %q", subject)
	}
	if ctaLabel != "View & pay invoice" {
		t.Errorf("due CTA changed: got %q", ctaLabel)
	}

	text := invoiceTextBody("C", "NIM-7", "USD 0.00", "usd", "https://h/x", true)
	if !strings.Contains(text, "no payment is needed") || strings.Contains(text, "View or pay") {
		t.Errorf("zero-due text twin must match the HTML's promise: %q", text)
	}
}
