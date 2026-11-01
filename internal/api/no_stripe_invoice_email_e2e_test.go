package api

import (
	"bufio"
	"context"
	"encoding/base64"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net"
	"net/mail"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/email"
	"github.com/gethardcount/hardcount/internal/platform/leader"
)

// smtpSink is a minimal SMTP server that keeps every message it is sent, so
// a test can read the email a customer would get from the server's own
// sender.
type smtpSink struct {
	ln   net.Listener
	mu   sync.Mutex
	msgs []string
}

func newSMTPSink(t *testing.T) *smtpSink {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	s := &smtpSink{ln: ln}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go s.session(conn)
		}
	}()
	return s
}

func (s *smtpSink) addr() (host, port string) {
	h, p, _ := net.SplitHostPort(s.ln.Addr().String())
	return h, p
}

func (s *smtpSink) messages() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.msgs...)
}

func (s *smtpSink) session(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	r := bufio.NewReader(conn)
	write := func(line string) { _, _ = conn.Write([]byte(line + "\r\n")) }
	write("220 sink.test ESMTP")
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		switch cmd := strings.ToUpper(strings.TrimSpace(line)); {
		case strings.HasPrefix(cmd, "EHLO"):
			write("250-sink.test")
			write("250 SIZE 35882577")
		case strings.HasPrefix(cmd, "DATA"):
			write("354 go ahead")
			var b strings.Builder
			for {
				dl, err := r.ReadString('\n')
				if err != nil {
					return
				}
				if strings.TrimRight(dl, "\r\n") == "." {
					break
				}
				b.WriteString(strings.TrimPrefix(dl, ".")) // undo dot-stuffing
			}
			s.mu.Lock()
			s.msgs = append(s.msgs, b.String())
			s.mu.Unlock()
			write("250 queued")
		case strings.HasPrefix(cmd, "QUIT"):
			write("221 bye")
			return
		default:
			write("250 ok")
		}
	}
}

// emailParts decodes one received message's text parts by media type.
func emailParts(t *testing.T, raw string) map[string]string {
	t.Helper()
	msg, err := mail.ReadMessage(strings.NewReader(raw))
	if err != nil {
		t.Fatalf("parse message: %v", err)
	}
	parts := map[string]string{}
	var walk func(h mail.Header, body io.Reader)
	walk = func(h mail.Header, body io.Reader) {
		mediaType, params, err := mime.ParseMediaType(h.Get("Content-Type"))
		if err != nil {
			t.Fatalf("Content-Type %q: %v", h.Get("Content-Type"), err)
		}
		if strings.HasPrefix(mediaType, "multipart/") {
			mr := multipart.NewReader(body, params["boundary"])
			for {
				p, err := mr.NextRawPart()
				if err == io.EOF {
					return
				}
				if err != nil {
					t.Fatalf("next part: %v", err)
				}
				walk(mail.Header(p.Header), p)
			}
		}
		if !strings.HasPrefix(mediaType, "text/") {
			return
		}
		dec := body
		switch strings.ToLower(h.Get("Content-Transfer-Encoding")) {
		case "quoted-printable":
			dec = quotedprintable.NewReader(body)
		case "base64":
			dec = base64.NewDecoder(base64.StdEncoding, body)
		}
		b, err := io.ReadAll(dec)
		if err != nil {
			t.Fatalf("decode %s: %v", mediaType, err)
		}
		parts[mediaType] = strings.ReplaceAll(string(b), "\r\n", "\n")
	}
	walk(msg.Header, msg.Body)
	return parts
}

// runEmailDispatcher runs the email dispatcher as cmd/hardcount runs it,
// behind the leader lease, on the server's own sender, until the test ends.
func (r *noStripeRig) runEmailDispatcher() {
	r.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	d := email.NewDispatcher(r.srv.EmailOutboxStore, r.srv.EmailSender, email.DispatcherConfig{Interval: 50 * time.Millisecond}, leader.New(r.db.Pool, nil))
	d.SetSettledChecker(r.srv.EmailSettledChecker)
	stopped := make(chan struct{})
	go func() { defer close(stopped); d.Start(ctx) }()
	r.t.Cleanup(func() { cancel(); <-stopped })
}

// waitForEmails waits until sink holds want messages, and returns them.
func waitForEmails(t *testing.T, sink *smtpSink, want int) []string {
	t.Helper()
	for deadline := time.Now().Add(20 * time.Second); len(sink.messages()) < want && time.Now().Before(deadline); {
		time.Sleep(50 * time.Millisecond)
	}
	got := sink.messages()
	if len(got) != want {
		t.Fatalf("delivered %d emails, want %d", len(got), want)
	}
	return got
}

// TestE2E_NoStripe_InvoiceEmailOffersViewNotPay reads the invoice email the
// customer gets with no Stripe connected for the invoice's mode, as the
// server sends it: the collector queues it at finalize, and the dispatcher
// renders it with the server's own sender, which asks the hosted page's own
// Stripe check. The hosted page says online payment isn't set up, so the
// email offers "View invoice" and asks the customer to pay as arranged, with
// no offer to pay online. Connect Stripe and the same email, sent by the
// operator's Email invoice, offers "View & pay invoice".
//
// Mutation: remove the router's SetStripeReadiness (the send fails with
// email.ErrNoStripeCheck and nothing is delivered).
func TestE2E_NoStripe_InvoiceEmailOffersViewNotPay(t *testing.T) {
	sink := newSMTPSink(t)
	host, port := sink.addr()
	const hostedBase = "https://billing.no-stripe.test"
	r := newNoStripeRig(t, "No Stripe Email Co",
		"SMTP_HOST", host, "SMTP_PORT", port, "SMTP_TLS", "none",
		"HOSTED_INVOICE_BASE_URL", hostedBase)
	inv := r.finalizedInvoice("email")
	if got := r.emailsOf(inv); !sameEmails(got, queuedEmail{kind: "invoice"}) {
		t.Fatalf("emails after finalize: %+v, want only the invoice email", got)
	}

	r.runEmailDispatcher()
	sent := waitForEmails(t, sink, 1)
	parts := emailParts(t, sent[0])
	html, text := parts["text/html"], parts["text/plain"]
	const payAsArranged = "Please pay as arranged with us, or contact us to arrange payment."
	if !strings.Contains(html, ">View invoice<") || strings.Contains(html, "View &amp; pay invoice") {
		t.Errorf("HTML part: want the View invoice button and no View & pay:\n%s", html)
	}
	if !strings.Contains(text, "View online: "+hostedBase+"/invoice/") || strings.Contains(text, "View or pay online") {
		t.Errorf("text part: want View online and no View or pay online:\n%s", text)
	}
	for part, body := range map[string]string{"html": html, "text": text} {
		if !strings.Contains(body, payAsArranged) {
			t.Errorf("%s part does not ask to pay as arranged:\n%s", part, body)
		}
		if strings.Contains(strings.ToLower(body), "pay your invoice") {
			t.Errorf("%s part offers to pay online:\n%s", part, body)
		}
	}

	// Control: with Stripe connected the hosted page offers Pay, and so does
	// the email.
	r.connectStripe()
	r.call("POST", "/v1/invoices/"+inv+"/send", flowReq{body: map[string]any{"email": "email@no-stripe.test"}}).want(t, 200, "email invoice")
	sent = waitForEmails(t, sink, 2)
	parts = emailParts(t, sent[1])
	if html := parts["text/html"]; !strings.Contains(html, "View &amp; pay invoice") || strings.Contains(html, payAsArranged) {
		t.Errorf("CONTROL with Stripe: HTML part wants View & pay invoice:\n%s", html)
	}
	if text := parts["text/plain"]; !strings.Contains(text, "View or pay online: "+hostedBase+"/invoice/") {
		t.Errorf("CONTROL with Stripe: text part wants View or pay online:\n%s", text)
	}
}
