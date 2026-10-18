package email

import (
	"context"
	"encoding/base64"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/mail"
	"strings"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/domain"
)

// fixedSettings hands every tenant the same branding, so a test can put a
// long, non-ASCII company name into the From header.
type fixedSettings struct{ ts domain.TenantSettings }

func (f fixedSettings) Get(context.Context, string) (domain.TenantSettings, error) {
	return f.ts, nil
}

// TestWireFormat_EverySendPath sends every email Velox writes through a
// real SMTP exchange and checks the bytes the server received against the
// mail standards a receiving server or spam filter applies:
//
//   - RFC 5322 §3.6: a Date and a Message-ID header (Message-ID on the
//     SMTP_FROM domain);
//   - RFC 5321 §4.5.3.1.6 and RFC 2045 §6.7: no line over 998 octets, and
//     body lines at most 76, so a long operator note or HTML line cannot
//     break the message in transit;
//   - RFC 2045: no octet above 127 anywhere, so UTF-8 text travels encoded
//     instead of carrying a false "7bit" label;
//   - the decoded parts still hold the exact text we rendered, so the
//     encoding never changes what the customer reads.
func TestWireFormat_EverySendPath(t *testing.T) {
	srv := newScriptedSMTP(t)
	s := newCCTestSender(t, srv)
	s.hostedInvoiceBaseURL = "https://pay.example.com"
	s.SetSettingsGetter(fixedSettings{ts: domain.TenantSettings{
		CompanyName: `Müller "Großhandel" \ Logistik GmbH & Co. KG — Kreditorenbuchhaltung Abteilung Süd`,
	}})
	const outboxID = "vlx_emob_00112233445566778899aabb"
	ctx := WithCorrelation(context.Background(), outboxID)

	cust := "Müller Großhandel und Logistik GmbH & Co. KG — Kreditorenbuchhaltung Abteilung Süd, Werk 3"
	// One unbroken operator note: before go-mail this became a single
	// 1,807-octet line in the text part.
	note := strings.Repeat("Please add a card before the next billing run so service continues. ", 22)
	pdf := []byte("%PDF-1.4 fake invoice bytes — not a real document\n")

	cases := []struct {
		name     string
		send     func() error
		wantText []string // must appear in the decoded text/plain part
	}{
		{"invoice", func() error {
			return s.SendInvoice(ctx, "t1", "ap@acme.test", []string{"finance@acme.test"}, cust, "INV-000123", 123456, "eur", pdf, "tok_abc")
		}, []string{cust, "INV-000123"}},
		{"invoice-zero", func() error {
			return s.SendInvoice(ctx, "t1", "ap@acme.test", nil, cust, "INV-000124", 0, "eur", pdf, "tok_abc")
		}, []string{cust, "settled"}},
		{"receipt", func() error {
			return s.SendPaymentReceipt(ctx, "t1", "ap@acme.test", nil, cust, "INV-000123", 123456, "eur", "tok_abc")
		}, []string{cust}},
		{"dunning-warning", func() error {
			return s.SendDunningWarning(ctx, "t1", "ap@acme.test", nil, cust, "INV-000123", 1, 4, "2026-10-20", "insufficient_funds", "tok_abc", false)
		}, []string{cust, "insufficient_funds"}},
		{"dunning-warning-card-refused", func() error {
			return s.SendDunningWarning(ctx, "t1", "ap@acme.test", nil, cust, "INV-000123", 1, 4, "2026-10-20", "", "tok_abc", true)
		}, []string{cust, "different card"}},
		{"dunning-escalation", func() error {
			return s.SendDunningEscalation(ctx, "t1", "ap@acme.test", nil, cust, "INV-000123", domain.DunningEscalationOutcome{}, "tok_abc")
		}, []string{cust}},
		{"payment-failed", func() error {
			return s.SendPaymentFailed(ctx, "t1", "ap@acme.test", nil, cust, "INV-000123", "card_declined", "tok_abc")
		}, []string{cust, "card_declined"}},
		{"setup-link-long-note", func() error {
			return s.SendPaymentSetupLink(ctx, "t1", "ap@acme.test", cust, note, "https://pay.example.com/setup/abc")
		}, []string{cust, strings.TrimSpace(note)}},
		{"setup-request", func() error {
			return s.SendPaymentSetupRequest(ctx, "t1", "ap@acme.test", cust, "INV-000123", 123456, "eur", "https://pay.example.com/u/abc", false, "")
		}, []string{cust, "https://pay.example.com/u/abc"}},
		{"credit-note", func() error {
			return s.SendCreditNote(ctx, "t1", "ap@acme.test", nil, cust, "CN-0001", "INV-000123", 5000, "eur", pdf)
		}, []string{cust, "CN-0001"}},
		{"password-reset", func() error {
			return s.SendPasswordReset(ctx, "", "ap@acme.test", "Jürgen", "https://app.example.com/reset?t=abc")
		}, []string{"Jürgen", "https://app.example.com/reset?t=abc", "— Velox"}},
		{"member-invite", func() error {
			return s.SendMemberInvite(ctx, "", "ap@acme.test", "owner@acme.test", "Müller GmbH", "https://app.example.com/invite?t=abc")
		}, []string{"Müller GmbH", "https://app.example.com/invite?t=abc"}},
		{"test-email", func() error {
			return s.SendTestEmail(ctx, "", "ap@acme.test")
		}, []string{"This is a test email from Velox.", "— Velox"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.send(); err != nil {
				t.Fatalf("send: %v", err)
			}
			raw := srv.lastData()
			checkWireFormat(t, raw, "velox.test", outboxID, tc.wantText)
		})
	}
}

// checkWireFormat asserts the standards above on one received message.
func checkWireFormat(t *testing.T, raw, fromDomain, outboxID string, wantText []string) {
	t.Helper()

	eightBit := 0
	for i := 0; i < len(raw); i++ {
		if raw[i] > 127 {
			eightBit++
		}
	}
	if eightBit > 0 {
		t.Errorf("%d octets above 127 on the wire; RFC 2045 needs 8-bit text encoded", eightBit)
	}
	if strings.Contains(raw, "Content-Transfer-Encoding: 7bit") {
		t.Error(`a part is labelled "7bit"; text parts must say how they are encoded`)
	}

	hdrEnd := strings.Index(raw, "\r\n\r\n")
	if hdrEnd < 0 {
		t.Fatalf("no header/body separator in:\n%s", raw[:min(400, len(raw))])
	}
	for _, line := range strings.Split(raw[:hdrEnd], "\r\n") {
		if len(line) > 998 {
			t.Errorf("header line of %d octets exceeds RFC 5322's 998: %.80q…", len(line), line)
		}
	}
	for _, line := range strings.Split(raw[hdrEnd+4:], "\r\n") {
		if len(line) > 76 {
			t.Errorf("body line of %d octets exceeds RFC 2045's 76: %.80q…", len(line), line)
		}
	}

	msg, err := mail.ReadMessage(strings.NewReader(raw))
	if err != nil {
		t.Fatalf("parse message: %v", err)
	}
	date, err := msg.Header.Date()
	if err != nil {
		t.Errorf("Date header (RFC 5322 §3.6.1): %v", err)
	} else if d := time.Since(date); d < -time.Minute || d > time.Minute {
		t.Errorf("Date header %v is not the send time", date)
	}
	msgID := msg.Header.Get("Message-ID")
	if !strings.HasPrefix(msgID, "<") || !strings.HasSuffix(msgID, "@"+fromDomain+">") || len(msgID) < len(fromDomain)+12 {
		t.Errorf("Message-ID %q: want <unique@%s> (RFC 5322 §3.6.4, on the SMTP_FROM domain)", msgID, fromDomain)
	}
	if got := msg.Header.Get(correlationHeaderName); got != outboxID {
		t.Errorf("%s = %q, want %q (ADR-098 correlation lost)", correlationHeaderName, got, outboxID)
	}
	if ua := msg.Header.Get("User-Agent") + msg.Header.Get("X-Mailer"); ua != "" {
		t.Errorf("unexpected mailer banner %q", ua)
	}

	texts := map[string]string{}
	walkParts(t, mail.Header(msg.Header), msg.Body, texts)
	plain, ok := texts["text/plain"]
	if !ok {
		t.Fatalf("no text/plain part found")
	}
	for _, want := range wantText {
		if !strings.Contains(plain, want) {
			t.Errorf("decoded text/plain lost %q", want)
		}
	}
}

// walkParts decodes every leaf part by its Content-Transfer-Encoding and
// records the decoded text by media type. It fails on a text part that is
// not quoted-printable or base64: such a part carries raw octets.
func walkParts(t *testing.T, h mail.Header, body io.Reader, texts map[string]string) {
	t.Helper()
	mediaType, params, err := mime.ParseMediaType(h.Get("Content-Type"))
	if err != nil {
		t.Fatalf("Content-Type %q: %v", h.Get("Content-Type"), err)
	}
	if strings.HasPrefix(mediaType, "multipart/") {
		r := multipart.NewReader(body, params["boundary"])
		for {
			p, err := r.NextRawPart()
			if err == io.EOF {
				return
			}
			if err != nil {
				t.Fatalf("next part of %s: %v", mediaType, err)
			}
			walkParts(t, mail.Header(p.Header), p, texts)
		}
	}
	var dec io.Reader
	switch cte := strings.ToLower(h.Get("Content-Transfer-Encoding")); cte {
	case "quoted-printable":
		dec = quotedprintable.NewReader(body)
	case "base64":
		dec = base64.NewDecoder(base64.StdEncoding, body)
	default:
		t.Fatalf("%s part has Content-Transfer-Encoding %q; want quoted-printable or base64", mediaType, cte)
	}
	b, err := io.ReadAll(dec)
	if err != nil {
		t.Fatalf("decode %s part: %v", mediaType, err)
	}
	if strings.HasPrefix(mediaType, "text/") {
		if cs := strings.ToLower(params["charset"]); cs != "utf-8" {
			t.Errorf("%s charset = %q, want utf-8", mediaType, cs)
		}
		texts[mediaType] = strings.ReplaceAll(string(b), "\r\n", "\n")
	}
}
