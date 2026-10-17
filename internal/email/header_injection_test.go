package email

import (
	"context"
	"strings"
	"testing"

	"github.com/sagarsuperuser/velox/internal/domain"
)

// headerBlock returns the top-level header lines of a wire message.
func headerBlock(data string) []string {
	if i := strings.Index(data, "\r\n\r\n"); i >= 0 {
		data = data[:i]
	}
	return strings.Split(data, "\r\n")
}

// assertNoSmuggledHeader fails if a Bcc header (or any line starting with
// "Bcc:") exists in the top-level header block or in any MIME part header.
func assertNoSmuggledHeader(t *testing.T, data string) {
	t.Helper()
	msg, parts := readWire(t, data)
	if v, ok := msg.Header["Bcc"]; ok {
		t.Errorf("smuggled Bcc header reached the wire: %q", v)
	}
	for _, line := range headerBlock(data) {
		if strings.HasPrefix(strings.ToLower(line), "bcc:") {
			t.Errorf("raw Bcc line in the header block: %q", line)
		}
	}
	for _, p := range parts {
		if v := p.header.Get("Bcc"); v != "" {
			t.Errorf("smuggled Bcc header in a %s part: %q", p.mediaType, v)
		}
	}
}

// TestHeaderInjection_OnTheWire: every caller-influenced header value —
// the tenant's company name (From), the subject, the tenant name in an
// invite subject, and the attachment file name — carries a CRLF payload.
// The bytes the SMTP server receives must parse with no Bcc header
// anywhere. Checked on the built message, not on a helper's return value,
// so a builder that bypasses the helper is caught too.
func TestHeaderInjection_OnTheWire(t *testing.T) {
	const payload = "Acme\r\nBcc: victim@evil.test"
	ctx := context.Background()

	t.Run("company name in From", func(t *testing.T) {
		srv := newScriptedSMTP(t)
		s := newCCTestSender(t, srv)
		s.SetSettingsGetter(fixedSettings{ts: domain.TenantSettings{CompanyName: payload}})
		if err := s.SendPaymentReceipt(ctx, "t1", "ap@acme.test", nil, "Ann", "INV-1", 100, "usd", ""); err != nil {
			t.Fatalf("send: %v", err)
		}
		data := srv.lastData()
		assertNoSmuggledHeader(t, data)
		msg, _ := readWire(t, data)
		from, err := msg.Header.AddressList("From")
		if err != nil || len(from) != 1 || from[0].Address != "billing@velox.test" {
			t.Fatalf("From: got %v (%v), want the SMTP_FROM address", from, err)
		}
		if from[0].Name != payload {
			t.Errorf("From display name: got %q, want the company name encoded intact", from[0].Name)
		}
	})

	t.Run("subject", func(t *testing.T) {
		srv := newScriptedSMTP(t)
		s := newCCTestSender(t, srv)
		if err := s.sendRich(ctx, richMessage{
			TenantID: "t1", To: "ap@acme.test", Subject: "Hi\r\nBcc: victim@evil.test",
			TextBody: "b", HTMLBody: "<p>b</p>",
		}); err != nil {
			t.Fatalf("send: %v", err)
		}
		assertNoSmuggledHeader(t, srv.lastData())
	})

	t.Run("tenant name in invite subject", func(t *testing.T) {
		srv := newScriptedSMTP(t)
		s := newCCTestSender(t, srv)
		if err := s.SendMemberInvite(ctx, "t1", "new@acme.test", "owner@acme.test", payload, "https://app.example.com/invite?t=abc"); err != nil {
			t.Fatalf("send: %v", err)
		}
		assertNoSmuggledHeader(t, srv.lastData())
	})

	t.Run("attachment file name", func(t *testing.T) {
		srv := newScriptedSMTP(t)
		s := newCCTestSender(t, srv)
		if err := s.SendInvoice(ctx, "t1", "ap@acme.test", nil, "Ann", "INV-1\"\r\nBcc: victim@evil.test", 100, "usd", []byte("%PDF-1.4"), ""); err != nil {
			t.Fatalf("send: %v", err)
		}
		data := srv.lastData()
		assertNoSmuggledHeader(t, data)
		_, parts := readWire(t, data)
		var found bool
		for _, p := range parts {
			if p.mediaType != "application/pdf" {
				continue
			}
			found = true
			if strings.ContainsAny(p.params["name"], "\r\n\"") {
				t.Errorf("attachment name kept CR, LF or a quote: %q", p.params["name"])
			}
		}
		if !found {
			t.Error("no application/pdf part")
		}
	})
}

// TestFromHeader_DisplayNameRoundTrips: a tenant's company name reaches
// the customer's inbox exactly as typed. Quotes, backslashes, commas and
// non-ASCII letters are escaped or encoded, never dropped or doubled — the
// failure mode of building the header as a `"name" <addr>` string.
func TestFromHeader_DisplayNameRoundTrips(t *testing.T) {
	for _, name := range []string{
		"Acme Inc",
		`Acme "Best" Widgets`,
		`Back\slash Ltd`,
		"Acme, Inc.",
		"Müller Großhandel GmbH & Co. KG",
		"株式会社サンプル",
	} {
		t.Run(name, func(t *testing.T) {
			srv := newScriptedSMTP(t)
			s := newCCTestSender(t, srv)
			if err := s.sendRich(context.Background(), richMessage{
				TenantID: "t1", To: "ap@acme.test", Subject: "x",
				TextBody: "b", HTMLBody: "<p>b</p>", FromName: name,
			}); err != nil {
				t.Fatalf("send: %v", err)
			}
			msg, _ := readWire(t, srv.lastData())
			from, err := msg.Header.AddressList("From")
			if err != nil || len(from) != 1 {
				t.Fatalf("From %q does not parse: %v", msg.Header.Get("From"), err)
			}
			if from[0].Name != name || from[0].Address != "billing@velox.test" {
				t.Errorf("From: got name %q addr %q, want %q billing@velox.test", from[0].Name, from[0].Address, name)
			}
		})
	}

	// No display name: the From header is the bare SMTP_FROM address.
	srv := newScriptedSMTP(t)
	s := newCCTestSender(t, srv)
	if err := s.SendTestEmail(context.Background(), "", "op@acme.test"); err != nil {
		t.Fatalf("send: %v", err)
	}
	msg, _ := readWire(t, srv.lastData())
	from, err := msg.Header.AddressList("From")
	if err != nil || len(from) != 1 || from[0].Name != "" || from[0].Address != "billing@velox.test" {
		t.Errorf("From without a name: got %v (%v)", from, err)
	}
}
