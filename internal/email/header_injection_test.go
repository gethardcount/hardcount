package email

import (
	"context"
	"mime"
	"net/mail"
	"strings"
	"testing"
)

// These tests read the message bytes the builders produce, the same bytes
// deliver() sends. A tenant CompanyName (the From display name), a subject
// built from customer data, or a bad address must never add a header line:
// a value like "Acme\r\nBcc: victim@x.com" would otherwise smuggle in a
// Bcc header.

const injection = "\r\nBcc: victim@evil.com"

func injectionTestSender() *Sender {
	return &Sender{host: "smtp.example.com", port: "587", from: "billing@velox.dev"}
}

// parseBuilt parses built bytes and fails if any header line is a Bcc.
func parseBuilt(t *testing.T, raw []byte) *mail.Message {
	t.Helper()
	s := string(raw)
	end := strings.Index(s, "\r\n\r\n")
	if end < 0 {
		t.Fatalf("no header/body separator:\n%s", s)
	}
	for _, line := range strings.Split(s[:end], "\r\n") {
		if strings.HasPrefix(strings.ToLower(line), "bcc:") {
			t.Fatalf("header injection: a Bcc line reached the message: %q", line)
		}
	}
	msg, err := mail.ReadMessage(strings.NewReader(s))
	if err != nil {
		t.Fatalf("parse built message: %v", err)
	}
	if bcc := msg.Header.Get("Bcc"); bcc != "" {
		t.Fatalf("header injection: Bcc header %q", bcc)
	}
	return msg
}

func TestBuiltHeaders_FromName(t *testing.T) {
	s := injectionTestSender()
	for name, fromName := range map[string]string{
		"plain":           "Acme Inc",
		"crlf injection":  "Acme" + injection,
		"quotes and \\":   `Acme "Best" \ Co.`,
		"non-ascii":       "Müller & Söhne — Buchhaltung",
		"looks like addr": "billing@evil.com <x@evil.com>",
	} {
		t.Run(name, func(t *testing.T) {
			raw, err := s.buildRich(context.Background(), richMessage{
				To: "ap@acme.test", Subject: "Invoice", TextBody: "b", HTMLBody: "<p>b</p>", FromName: fromName,
			}, nil)
			if err != nil {
				t.Fatalf("buildRich: %v", err)
			}
			msg := parseBuilt(t, raw)
			from, err := mail.ParseAddress(msg.Header.Get("From"))
			if err != nil {
				t.Fatalf("From header %q does not parse: %v", msg.Header.Get("From"), err)
			}
			// The name must reach the reader exactly as the tenant set it:
			// building From from "Name <addr>" text mangles quotes and
			// backslashes.
			if from.Name != fromName || from.Address != "billing@velox.dev" {
				t.Errorf("From = (%q, %q), want (%q, %q)", from.Name, from.Address, fromName, "billing@velox.dev")
			}
		})
	}

	t.Run("empty name yields bare address", func(t *testing.T) {
		raw, err := s.buildPlain(context.Background(), "ap@acme.test", "", "Reset", "body")
		if err != nil {
			t.Fatalf("buildPlain: %v", err)
		}
		from, err := mail.ParseAddress(parseBuilt(t, raw).Header.Get("From"))
		if err != nil || from.Name != "" || from.Address != "billing@velox.dev" {
			t.Errorf("From = %+v (err %v), want the bare SMTP_FROM address", from, err)
		}
	})
}

func TestBuiltHeaders_Subject(t *testing.T) {
	s := injectionTestSender()
	dec := new(mime.WordDecoder)
	for name, subject := range map[string]string{
		"plain ascii":    "Invoice VLX-001 is ready",
		"crlf injection": "Hi" + injection,
		"non-ascii":      "Invoice INV-1 — EUR 12.00 für Müller",
	} {
		t.Run(name, func(t *testing.T) {
			for builder, build := range map[string]func() ([]byte, error){
				"rich": func() ([]byte, error) {
					return s.buildRich(context.Background(), richMessage{To: "ap@acme.test", Subject: subject, TextBody: "b", HTMLBody: "<p>b</p>"}, nil)
				},
				"plain": func() ([]byte, error) {
					return s.buildPlain(context.Background(), "ap@acme.test", "", subject, "body")
				},
			} {
				raw, err := build()
				if err != nil {
					t.Fatalf("%s: build: %v", builder, err)
				}
				got, err := dec.DecodeHeader(parseBuilt(t, raw).Header.Get("Subject"))
				if err != nil {
					t.Fatalf("%s: decode Subject: %v", builder, err)
				}
				if got != subject {
					t.Errorf("%s: Subject = %q, want %q", builder, got, subject)
				}
			}
		})
	}
	// A plain ASCII subject is written as is, not encoded.
	raw, err := s.buildPlain(context.Background(), "ap@acme.test", "", "Invoice VLX-001 is ready", "body")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "\r\nSubject: Invoice VLX-001 is ready\r\n") {
		t.Errorf("plain subject altered:\n%s", raw)
	}
}

// TestBuiltHeaders_AddressRefused: net/mail writes an address's domain part
// as given, so "a@x.test\r\nBcc: v" would put a Bcc line in the To or Cc
// header. The builders refuse any address with a control character.
func TestBuiltHeaders_AddressRefused(t *testing.T) {
	s := injectionTestSender()
	bad := "ap@acme.test" + injection

	if raw, err := s.buildPlain(context.Background(), bad, "", "x", "b"); err == nil {
		t.Errorf("buildPlain with a CRLF To: want an error, got\n%s", raw)
	}
	if raw, err := s.buildRich(context.Background(), richMessage{To: bad, Subject: "x", TextBody: "b", HTMLBody: "b"}, nil); err == nil {
		t.Errorf("buildRich with a CRLF To: want an error, got\n%s", raw)
	}
	if raw, err := s.buildRich(context.Background(), richMessage{To: "ap@acme.test", Subject: "x", TextBody: "b", HTMLBody: "b"},
		[]string{"finance@acme.test", "cc@acme.test" + injection}); err == nil {
		t.Errorf("buildRich with a CRLF Cc: want an error, got\n%s", raw)
	}
	badFrom := &Sender{host: "smtp.example.com", port: "587", from: "billing@velox.dev" + injection}
	if raw, err := badFrom.buildPlain(context.Background(), "ap@acme.test", "", "x", "b"); err == nil {
		t.Errorf("buildPlain with a CRLF SMTP_FROM: want an error, got\n%s", raw)
	}

	// Control: the same addresses without the CRLF build, and every one is
	// in its header.
	raw, err := s.buildRich(context.Background(), richMessage{To: "ap@acme.test", Subject: "x", TextBody: "b", HTMLBody: "b"},
		[]string{"finance@acme.test", "cc@acme.test"})
	if err != nil {
		t.Fatalf("clean addresses: %v", err)
	}
	msg := parseBuilt(t, raw)
	if to := msg.Header.Get("To"); to != "<ap@acme.test>" {
		t.Errorf("To = %q", to)
	}
	if cc := msg.Header.Get("Cc"); cc != "<finance@acme.test>, <cc@acme.test>" {
		t.Errorf("Cc = %q", cc)
	}
}
