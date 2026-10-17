package email

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/mail"
	"net/textproto"
	"regexp"
	"strings"
	"testing"

	"github.com/sagarsuperuser/velox/internal/domain"
)

// fixedSettings returns the same tenant settings for every tenant, so a
// test can put hostile or non-ASCII branding into the From header.
type fixedSettings struct{ ts domain.TenantSettings }

func (f fixedSettings) Get(context.Context, string) (domain.TenantSettings, error) {
	return f.ts, nil
}

// wirePart is one decoded leaf of a sent message.
type wirePart struct {
	mediaType string
	params    map[string]string
	header    textproto.MIMEHeader
	body      []byte // after Content-Transfer-Encoding decoding
}

// readWire parses the bytes the SMTP server received the way a mail
// client would: the RFC 5322 header block, then every MIME leaf decoded
// by its Content-Transfer-Encoding. Each leaf must declare
// quoted-printable or base64, because every body we send can carry UTF-8.
func readWire(t *testing.T, data string) (*mail.Message, []wirePart) {
	t.Helper()
	msg, err := mail.ReadMessage(strings.NewReader(data))
	if err != nil {
		t.Fatalf("message does not parse: %v\n%s", err, data)
	}
	var parts []wirePart
	walkWireParts(t, textproto.MIMEHeader(msg.Header), msg.Body, &parts)
	return msg, parts
}

func walkWireParts(t *testing.T, h textproto.MIMEHeader, body io.Reader, out *[]wirePart) {
	t.Helper()
	mediaType, params, err := mime.ParseMediaType(h.Get("Content-Type"))
	if err != nil {
		t.Fatalf("bad Content-Type %q: %v", h.Get("Content-Type"), err)
	}
	if strings.HasPrefix(mediaType, "multipart/") {
		mr := multipart.NewReader(body, params["boundary"])
		for {
			// NextRawPart, not NextPart: NextPart silently decodes
			// quoted-printable and deletes the header we are checking.
			p, err := mr.NextRawPart()
			if err == io.EOF {
				return
			}
			if err != nil {
				t.Fatalf("multipart %s: %v", mediaType, err)
			}
			walkWireParts(t, textproto.MIMEHeader(p.Header), p, out)
		}
	}
	raw, err := io.ReadAll(body)
	if err != nil {
		t.Fatalf("read %s part: %v", mediaType, err)
	}
	var decoded []byte
	switch cte := strings.ToLower(h.Get("Content-Transfer-Encoding")); cte {
	case "quoted-printable":
		decoded, err = io.ReadAll(quotedprintable.NewReader(bytes.NewReader(raw)))
	case "base64":
		decoded, err = io.ReadAll(base64.NewDecoder(base64.StdEncoding, bytes.NewReader(raw)))
	default:
		t.Errorf("%s part declares Content-Transfer-Encoding %q; want quoted-printable or base64", mediaType, cte)
		decoded = raw
	}
	if err != nil {
		t.Fatalf("decode %s part: %v", mediaType, err)
	}
	*out = append(*out, wirePart{mediaType: mediaType, params: params, header: h, body: decoded})
}

// partText returns the decoded text of the first leaf with mediaType,
// line endings normalised to \n.
func partText(parts []wirePart, mediaType string) (string, bool) {
	for _, p := range parts {
		if p.mediaType == mediaType {
			return strings.ReplaceAll(string(p.body), "\r\n", "\n"), true
		}
	}
	return "", false
}

var messageIDOnFromDomain = regexp.MustCompile(`^<[^<>@\s]+@velox\.test>$`)

// checkWireFormat asserts the standards a receiving server checks:
// RFC 5322 §3.6 requires Date; §3.6.4 says every message SHOULD have a
// Message-ID (spam filters score its absence). RFC 5321 §4.5.3.1.6 caps a
// line at 1,000 octets with CRLF, and RFC 2045 §6.7/§6.8 keep encoded
// lines to 76. A byte above 127 in the body means the 7bit/8bit label
// question even arises; we send none.
func checkWireFormat(t *testing.T, name, data string) (*mail.Message, []wirePart) {
	t.Helper()
	if data == "" {
		t.Fatalf("%s: nothing reached the SMTP server", name)
	}
	msg, parts := readWire(t, data)

	if _, err := msg.Header.Date(); err != nil {
		t.Errorf("%s: Date header missing or invalid (%q): %v", name, msg.Header.Get("Date"), err)
	}
	if id := msg.Header.Get("Message-ID"); !messageIDOnFromDomain.MatchString(id) {
		t.Errorf("%s: Message-ID %q; want <random@velox.test> on the From domain", name, id)
	}
	if v := msg.Header.Get("MIME-Version"); v != "1.0" {
		t.Errorf("%s: MIME-Version %q, want 1.0", name, v)
	}
	for _, h := range []string{"User-Agent", "X-Mailer"} {
		if v := msg.Header.Get(h); v != "" {
			t.Errorf("%s: unexpected %s header %q — the library's default must stay off", name, h, v)
		}
	}
	for i := 0; i < len(data); i++ {
		if data[i] > 127 {
			t.Errorf("%s: octet 0x%02x at offset %d — every header and body must be 7-bit on the wire", name, data[i], i)
			break
		}
	}
	for n, line := range strings.Split(data, "\r\n") {
		if len(line) > 76 {
			t.Errorf("%s: line %d is %d octets (limit 76 for encoded lines, 998 hard): %.90q", name, n+1, len(line), line)
		}
	}
	if len(parts) == 0 {
		t.Errorf("%s: no MIME leaf parts", name)
	}
	return msg, parts
}

// TestWireFormat_EverySendPath sends every Send* path through a real SMTP
// exchange and checks the bytes the server received. Before the go-mail
// builder: no Date, no Message-ID, UTF-8 bodies labelled 7bit, and a long
// operator note produced a single 1,807-octet line.
func TestWireFormat_EverySendPath(t *testing.T) {
	srv := newScriptedSMTP(t)
	s := newCCTestSender(t, srv)
	s.hostedInvoiceBaseURL = "https://pay.example.com"
	s.SetSettingsGetter(fixedSettings{ts: domain.TenantSettings{CompanyName: "Müller Großhandel GmbH & Co. KG"}})
	ctx := context.Background()

	cust := "Jürgen Weiß — Kreditorenbuchhaltung, Abteilung Süd"
	cc := []string{"finance@acme.test", "eng-lead@acme.test", "controller@acme.test", "audit@acme.test"}
	pdf := bytes.Repeat([]byte("%PDF-1.4 binary \x00\xff\x10 "), 200)
	longNote := strings.Repeat("Please add a card before the next billing run so service continues. ", 22)

	cases := []struct {
		name     string
		send     func() error
		wantText []string // in the decoded text/plain part
		wantCc   bool
		wantPDF  bool
	}{
		{"invoice", func() error {
			return s.SendInvoice(ctx, "t1", "ap@acme.test", cc, cust, "INV-000123", 123456, "eur", pdf, "tok_abc")
		}, []string{cust, "INV-000123", "EUR 1234.56"}, true, true},
		{"invoice-zero", func() error {
			return s.SendInvoice(ctx, "t1", "ap@acme.test", nil, cust, "INV-000124", 0, "eur", pdf, "tok_abc")
		}, []string{cust, "settled"}, false, true},
		{"receipt", func() error {
			return s.SendPaymentReceipt(ctx, "t1", "ap@acme.test", cc, cust, "INV-000123", 123456, "eur", "tok_abc")
		}, []string{cust, "EUR 1234.56"}, true, false},
		{"dunning-warning", func() error {
			return s.SendDunningWarning(ctx, "t1", "ap@acme.test", cc, cust, "INV-000123", 1, 4, "2026-10-20", "insufficient_funds", "tok_abc")
		}, []string{cust, "Attempt 1 of 4"}, true, false},
		{"dunning-escalation", func() error {
			return s.SendDunningEscalation(ctx, "t1", "ap@acme.test", cc, cust, "INV-000123", domain.DunningEscalationOutcome{}, "tok_abc")
		}, []string{cust, "INV-000123"}, true, false},
		{"payment-failed", func() error {
			return s.SendPaymentFailed(ctx, "t1", "ap@acme.test", cc, cust, "INV-000123", "card_declined", "tok_abc")
		}, []string{cust, "card_declined"}, true, false},
		{"setup-link-long-note", func() error {
			return s.SendPaymentSetupLink(ctx, "t1", "ap@acme.test", cust, longNote, "https://pay.example.com/setup/abc")
		}, []string{cust, strings.TrimSpace(longNote), "https://pay.example.com/setup/abc"}, false, false},
		{"setup-request", func() error {
			return s.SendPaymentSetupRequest(ctx, "t1", "ap@acme.test", cust, "INV-000123", 123456, "eur", "https://pay.example.com/u/abc", false, "")
		}, []string{cust, "https://pay.example.com/u/abc"}, false, false},
		{"credit-note", func() error {
			return s.SendCreditNote(ctx, "t1", "ap@acme.test", cc, cust, "CN-0001", "INV-000123", 5000, "eur", pdf)
		}, []string{cust, "CN-0001", "EUR 50.00"}, true, true},
		{"password-reset", func() error {
			return s.SendPasswordReset(ctx, "t1", "ap@acme.test", "Zoë", "https://app.example.com/reset?t=abc")
		}, []string{"Hi Zoë", "https://app.example.com/reset?t=abc", "— Velox"}, false, false},
		{"member-invite", func() error {
			return s.SendMemberInvite(ctx, "t1", "ap@acme.test", "owner@acme.test", "Müller Großhandel", "https://app.example.com/invite?t=abc")
		}, []string{"join Müller Großhandel on Velox", "https://app.example.com/invite?t=abc"}, false, false},
		{"test-email", func() error { return s.SendTestEmail(ctx, "t1", "ap@acme.test") }, []string{"test email from Velox", "— Velox"}, false, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := srv.lastData()
			if err := tc.send(); err != nil {
				t.Fatalf("send: %v", err)
			}
			data := srv.lastData()
			if data == before {
				t.Fatal("no new message reached the SMTP server")
			}
			msg, parts := checkWireFormat(t, tc.name, data)

			text, ok := partText(parts, "text/plain")
			if !ok {
				t.Fatal("no text/plain part")
			}
			for _, want := range tc.wantText {
				if !strings.Contains(text, want) {
					t.Errorf("decoded text/plain lacks %q:\n%s", want, text)
				}
			}

			to, err := msg.Header.AddressList("To")
			if err != nil || len(to) != 1 || to[0].Address != "ap@acme.test" {
				t.Errorf("To: got %v (%v), want ap@acme.test", to, err)
			}
			gotCc, _ := msg.Header.AddressList("Cc")
			if tc.wantCc {
				if len(gotCc) != len(cc) {
					t.Errorf("Cc: got %v, want %v", gotCc, cc)
				}
				for i := range gotCc {
					if i < len(cc) && gotCc[i].Address != cc[i] {
						t.Errorf("Cc[%d]: got %q, want %q", i, gotCc[i].Address, cc[i])
					}
				}
			} else if len(gotCc) != 0 {
				t.Errorf("Cc: got %v, want none", gotCc)
			}

			var gotPDF []byte
			for _, p := range parts {
				if p.mediaType == "application/pdf" {
					gotPDF = p.body
				}
			}
			if tc.wantPDF && !bytes.Equal(gotPDF, pdf) {
				t.Errorf("PDF attachment did not round-trip (%d bytes, want %d)", len(gotPDF), len(pdf))
			}
			if !tc.wantPDF && gotPDF != nil {
				t.Error("unexpected PDF attachment")
			}
		})
	}
}

// TestWireFormat_RichAndPlainRoundTrip: what we hand the builder is what
// a mail client shows after decoding — text and HTML alike, UTF-8 and long
// lines included — and the subject and From name decode back exactly.
func TestWireFormat_RichAndPlainRoundTrip(t *testing.T) {
	srv := newScriptedSMTP(t)
	s := newCCTestSender(t, srv)
	ctx := context.Background()

	text := "Hi Jürgen,\n\n" + strings.Repeat("Ünïcödé line that runs well past seventy-six octets — on purpose. ", 30) + "\n\n.leading dot line\n"
	html := "<p>" + strings.Repeat("Größe &amp; Maß — ", 120) + "</p>\n"
	subject := "Rechnung INV-1 — 1.234,56 € für Müller Großhandel GmbH & Co. KG, Abteilung Süd"

	if err := s.sendRich(ctx, richMessage{
		TenantID: "t1", To: "ap@acme.test", Subject: subject,
		TextBody: text, HTMLBody: html, FromName: "Müller Großhandel",
	}); err != nil {
		t.Fatalf("sendRich: %v", err)
	}
	msg, parts := checkWireFormat(t, "rich", srv.lastData())
	if got, _ := partText(parts, "text/plain"); got != text {
		t.Errorf("text/plain did not round-trip:\n got %q\nwant %q", got, text)
	}
	if got, _ := partText(parts, "text/html"); got != html {
		t.Errorf("text/html did not round-trip:\n got %q\nwant %q", got, html)
	}
	if got, err := new(mime.WordDecoder).DecodeHeader(msg.Header.Get("Subject")); err != nil || got != subject {
		t.Errorf("Subject: got %q (%v), want %q", got, err, subject)
	}
	from, err := msg.Header.AddressList("From")
	if err != nil || len(from) != 1 || from[0].Name != "Müller Großhandel" || from[0].Address != "billing@velox.test" {
		t.Errorf("From: got %v (%v)", from, err)
	}

	if err := s.sendPlain(ctx, "t1", "op@acme.test", "", "Réinitialiser", text); err != nil {
		t.Fatalf("sendPlain: %v", err)
	}
	_, parts = checkWireFormat(t, "plain", srv.lastData())
	if len(parts) != 1 || parts[0].mediaType != "text/plain" {
		t.Fatalf("plain email: want exactly one text/plain part, got %d", len(parts))
	}
	if got, _ := partText(parts, "text/plain"); got != text {
		t.Errorf("plain text did not round-trip:\n got %q\nwant %q", got, text)
	}
	if cs := strings.ToLower(parts[0].params["charset"]); cs != "utf-8" {
		t.Errorf("plain charset %q, want utf-8", cs)
	}
}

// TestWireFormat_MessageIDUnique: two sends of the same email carry
// different Message-IDs, so a receiver never folds them into one.
func TestWireFormat_MessageIDUnique(t *testing.T) {
	srv := newScriptedSMTP(t)
	s := newCCTestSender(t, srv)
	seen := map[string]bool{}
	for i := 0; i < 3; i++ {
		if err := s.SendTestEmail(context.Background(), "t1", "ap@acme.test"); err != nil {
			t.Fatalf("send %d: %v", i, err)
		}
		msg, _ := checkWireFormat(t, fmt.Sprintf("send-%d", i), srv.lastData())
		id := msg.Header.Get("Message-ID")
		if seen[id] {
			t.Fatalf("Message-ID %q repeated", id)
		}
		seen[id] = true
	}
}
