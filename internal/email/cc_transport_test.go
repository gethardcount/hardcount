package email

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
)

// scriptedSMTP is a minimal single-connection-at-a-time SMTP server for
// exercising deliver()'s per-recipient RCPT policy (ADR-082). Each
// accepted connection walks a real EHLO/MAIL/RCPT/DATA exchange;
// rcptReplies maps an address to a custom reply line (e.g. "550 5.1.1
// no such user"); dropOnRcpt kills the connection when that address is
// RCPT'd (transport-failure simulation).
type scriptedSMTP struct {
	ln          net.Listener
	mu          sync.Mutex
	rcpts       []string
	data        []string
	rcptReplies map[string]string
	dropOnRcpt  string
}

func newScriptedSMTP(t *testing.T) *scriptedSMTP {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	s := &scriptedSMTP{ln: ln, rcptReplies: map[string]string{}}
	t.Cleanup(func() { _ = ln.Close() })
	go s.serve()
	return s
}

func (s *scriptedSMTP) addr() (host, port string) {
	h, p, _ := net.SplitHostPort(s.ln.Addr().String())
	return h, p
}

func (s *scriptedSMTP) recordedRcpts() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.rcpts...)
}

func (s *scriptedSMTP) lastData() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.data) == 0 {
		return ""
	}
	return s.data[len(s.data)-1]
}

func (s *scriptedSMTP) serve() {
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			return
		}
		go s.session(conn)
	}
}

func (s *scriptedSMTP) session(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	r := bufio.NewReader(conn)
	write := func(line string) { _, _ = conn.Write([]byte(line + "\r\n")) }
	write("220 scripted.test ESMTP")
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		cmd := strings.TrimSpace(line)
		upper := strings.ToUpper(cmd)
		switch {
		case strings.HasPrefix(upper, "EHLO"):
			write("250-scripted.test")
			write("250 SIZE 35882577")
		case strings.HasPrefix(upper, "HELO"):
			write("250 scripted.test")
		case strings.HasPrefix(upper, "MAIL FROM"):
			write("250 ok")
		case strings.HasPrefix(upper, "RCPT TO"):
			addr := cmd
			if i := strings.Index(cmd, "<"); i >= 0 {
				if j := strings.Index(cmd[i:], ">"); j > 0 {
					addr = cmd[i+1 : i+j]
				}
			}
			s.mu.Lock()
			drop := s.dropOnRcpt != "" && strings.EqualFold(addr, s.dropOnRcpt)
			reply, scripted := s.rcptReplies[strings.ToLower(addr)]
			if !drop && !scripted {
				s.rcpts = append(s.rcpts, strings.ToLower(addr))
			}
			s.mu.Unlock()
			if drop {
				return // connection dies mid-transaction
			}
			if scripted {
				write(reply)
				continue
			}
			write("250 ok")
		case strings.HasPrefix(upper, "DATA"):
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
				// Undo the sender's dot-stuffing (RFC 5321 §4.5.2) so
				// lastData is the message itself, as a real server stores it.
				b.WriteString(strings.TrimPrefix(dl, "."))
			}
			s.mu.Lock()
			s.data = append(s.data, b.String())
			s.mu.Unlock()
			write("250 queued")
		case strings.HasPrefix(upper, "QUIT"):
			write("221 bye")
			return
		default:
			write("250 ok")
		}
	}
}

// recordingBounce captures ReportBounce calls so tests can pin WHICH
// address a bounce was attributed to — the misattribution guard.
type recordingBounce struct {
	mu    sync.Mutex
	calls []string
}

func (r *recordingBounce) ReportBounce(_ context.Context, _, email, _ string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, email)
}

func (r *recordingBounce) addresses() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.calls...)
}

// staticSuppression suppresses a fixed set of addresses.
type staticSuppression struct{ suppressed map[string]bool }

func (s staticSuppression) IsSuppressed(_ context.Context, _, email string) (bool, string, error) {
	return s.suppressed[strings.ToLower(email)], "bounced", nil
}

func newCCTestSender(t *testing.T, srv *scriptedSMTP) *Sender {
	t.Helper()
	host, port := srv.addr()
	return &Sender{host: host, port: port, from: "billing@velox.test", tlsMode: "none"}
}

// TestCC_HeaderAndEnvelope: one MIME message, one SMTP transaction —
// RCPT for primary + each CC, exactly one visible Cc header listing
// both, and (negative pin) no Cc header at all when the list is empty.
func TestCC_HeaderAndEnvelope(t *testing.T) {
	srv := newScriptedSMTP(t)
	s := newCCTestSender(t, srv)

	err := s.sendRich(context.Background(), richMessage{
		TenantID: "t1", To: "ap@acme.test",
		Cc:      []string{"finance@acme.test", "eng-lead@acme.test"},
		Subject: "Invoice INV-1", TextBody: "hi", HTMLBody: "<p>hi</p>",
	})
	if err != nil {
		t.Fatalf("sendRich: %v", err)
	}
	rcpts := srv.recordedRcpts()
	want := []string{"ap@acme.test", "finance@acme.test", "eng-lead@acme.test"}
	if fmt.Sprint(rcpts) != fmt.Sprint(want) {
		t.Errorf("RCPT set: got %v, want %v", rcpts, want)
	}
	data := srv.lastData()
	if !strings.Contains(data, "Cc: <finance@acme.test>, <eng-lead@acme.test>") {
		t.Errorf("wire message missing the visible Cc header:\n%s", data[:min(400, len(data))])
	}
	if n := strings.Count(data, "\nCc:"); n+strings.Count(data[:3], "Cc:") != 1 {
		t.Errorf("expected exactly one Cc header, found %d", n)
	}

	// Negative: no cc → no Cc header on the wire.
	if err := s.sendRich(context.Background(), richMessage{
		TenantID: "t1", To: "solo@acme.test",
		Subject: "Invoice INV-2", TextBody: "hi", HTMLBody: "<p>hi</p>",
	}); err != nil {
		t.Fatalf("sendRich (no cc): %v", err)
	}
	if strings.Contains(srv.lastData(), "Cc:") {
		t.Error("Cc header must be absent when the list is empty")
	}
}

// TestCC_HeaderInjectionNeutralized: a CRLF-bearing CC value (defense in
// depth — normalization upstream should already reject it) cannot smuggle
// a raw header line: the builder refuses the address, so the send fails
// before any SMTP traffic.
func TestCC_HeaderInjectionNeutralized(t *testing.T) {
	srv := newScriptedSMTP(t)
	s := newCCTestSender(t, srv)

	err := s.sendRich(context.Background(), richMessage{
		TenantID: "t1", To: "ap@acme.test",
		Cc:      []string{"evil@x.test>\r\nBcc: victim@y.test"},
		Subject: "x", TextBody: "b", HTMLBody: "<p>b</p>",
	})
	if err == nil {
		t.Error("a CRLF-bearing CC must fail the send")
	}
	data := srv.lastData()
	for _, line := range strings.Split(data, "\r\n") {
		if strings.HasPrefix(line, "Bcc:") {
			t.Fatalf("header injection: raw Bcc line reached the wire: %q", line)
		}
	}
}

// TestCC_RcptRejectionAttribution is the misattribution pin: a 550 on a
// CC recipient must (a) NOT fail the send, (b) report the bounce for
// the CC address and NEVER for the primary — a leaked CC error would
// flip the primary customer to bounced and suppression-gate their real
// invoices. A 550 on the PRIMARY keeps today's semantics: hard error,
// bounce attributed to the primary.
func TestCC_RcptRejectionAttribution(t *testing.T) {
	srv := newScriptedSMTP(t)
	srv.rcptReplies["dead@acme.test"] = "550 5.1.1 no such user"
	s := newCCTestSender(t, srv)
	bounces := &recordingBounce{}
	s.SetBounceReporter(bounces)

	err := s.sendRich(context.Background(), richMessage{
		TenantID: "t1", To: "ap@acme.test",
		Cc:      []string{"dead@acme.test", "finance@acme.test"},
		Subject: "x", TextBody: "b", HTMLBody: "<p>b</p>",
	})
	if err != nil {
		t.Fatalf("a CC rejection must not fail the send: %v", err)
	}
	rcpts := srv.recordedRcpts()
	if fmt.Sprint(rcpts) != fmt.Sprint([]string{"ap@acme.test", "finance@acme.test"}) {
		t.Errorf("surviving RCPT set: got %v", rcpts)
	}
	if addrs := bounces.addresses(); len(addrs) != 1 || addrs[0] != "dead@acme.test" {
		t.Errorf("bounce attribution: got %v, want exactly [dead@acme.test] — attributing to the primary suppression-gates their real invoices", addrs)
	}

	// Primary rejection: hard error + bounce on the primary.
	srv2 := newScriptedSMTP(t)
	srv2.rcptReplies["gone@acme.test"] = "550 5.1.1 no such user"
	s2 := newCCTestSender(t, srv2)
	bounces2 := &recordingBounce{}
	s2.SetBounceReporter(bounces2)
	err = s2.sendRich(context.Background(), richMessage{
		TenantID: "t1", To: "gone@acme.test", Cc: []string{"finance@acme.test"},
		Subject: "x", TextBody: "b", HTMLBody: "<p>b</p>",
	})
	if err == nil {
		t.Fatal("primary RCPT rejection must fail the send")
	}
	if addrs := bounces2.addresses(); len(addrs) != 1 || addrs[0] != "gone@acme.test" {
		t.Errorf("primary bounce attribution: got %v, want [gone@acme.test]", addrs)
	}
}

// TestCC_TransportErrorAborts: a connection death mid-CC-RCPT is a
// transport failure, not a recipient rejection — the whole send must
// error (retry-safe: DATA never sent) and no bounce may be recorded.
func TestCC_TransportErrorAborts(t *testing.T) {
	srv := newScriptedSMTP(t)
	srv.dropOnRcpt = "finance@acme.test"
	s := newCCTestSender(t, srv)
	bounces := &recordingBounce{}
	s.SetBounceReporter(bounces)

	err := s.sendRich(context.Background(), richMessage{
		TenantID: "t1", To: "ap@acme.test", Cc: []string{"finance@acme.test"},
		Subject: "x", TextBody: "b", HTMLBody: "<p>b</p>",
	})
	if err == nil {
		t.Fatal("transport failure mid-RCPT must abort the whole send — continuing would silently lose the primary's email")
	}
	if len(bounces.addresses()) != 0 {
		t.Errorf("transport failure is not a bounce; got reports for %v", bounces.addresses())
	}
	if srv.lastData() != "" {
		t.Error("nothing may be delivered on a transport abort")
	}
}

// TestCC_SuppressionSemantics: a suppressed PRIMARY blocks the whole
// send with zero SMTP traffic (never downgraded to CC-only); a
// suppressed CC is silently dropped from the recipient set while the
// send proceeds.
func TestCC_SuppressionSemantics(t *testing.T) {
	srv := newScriptedSMTP(t)
	s := newCCTestSender(t, srv)
	s.SetSuppressionChecker(staticSuppression{suppressed: map[string]bool{"bounced@acme.test": true}})

	// Primary suppressed → ErrRecipientSuppressed, no SMTP traffic.
	err := s.sendRich(context.Background(), richMessage{
		TenantID: "t1", To: "bounced@acme.test", Cc: []string{"finance@acme.test"},
		Subject: "x", TextBody: "b", HTMLBody: "<p>b</p>",
	})
	if !errors.Is(err, ErrRecipientSuppressed) {
		t.Fatalf("suppressed primary: got %v, want ErrRecipientSuppressed", err)
	}
	if len(srv.recordedRcpts()) != 0 {
		t.Errorf("suppressed primary must produce ZERO SMTP traffic (no CC-only downgrade); got RCPTs %v", srv.recordedRcpts())
	}

	// CC suppressed → dropped, primary + clean CC still delivered.
	err = s.sendRich(context.Background(), richMessage{
		TenantID: "t1", To: "ap@acme.test",
		Cc:      []string{"bounced@acme.test", "finance@acme.test"},
		Subject: "x", TextBody: "b", HTMLBody: "<p>b</p>",
	})
	if err != nil {
		t.Fatalf("send with one suppressed cc: %v", err)
	}
	rcpts := srv.recordedRcpts()
	if fmt.Sprint(rcpts) != fmt.Sprint([]string{"ap@acme.test", "finance@acme.test"}) {
		t.Errorf("RCPT set with suppressed cc: got %v, want primary + clean cc only", rcpts)
	}
	if strings.Contains(srv.lastData(), "bounced@acme.test") {
		t.Error("suppressed cc must not appear in the Cc header either")
	}
}
