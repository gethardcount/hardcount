package email

import (
	"context"
	"net"
	"strings"
	"sync/atomic"
	"testing"
)

// TestDeliver_RefusesUnknownTLSMode: a sender whose SMTP_TLS is not one of
// starttls, implicit or none sends nothing and says why. config.Load refuses
// to start on such a value; this is the guard for a sender built without it.
// An unknown mode used to be replaced by starttls in NewSender, and deliver
// itself would have taken the plain-text dial and skipped STARTTLS.
// Mutation-verify: drop the mode switch at the top of deliver → the sender
// connects.
func TestDeliver_RefusesUnknownTLSMode(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = ln.Close() }()
	var dials atomic.Int32
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			dials.Add(1)
			_ = c.Close()
		}
	}()
	host, port, _ := net.SplitHostPort(ln.Addr().String())

	t.Setenv("SMTP_TLS", "tls")
	t.Setenv("SMTP_HOST", host)
	t.Setenv("SMTP_PORT", port)
	built := NewSender("local")
	if built.TLSMode() != "tls" {
		t.Fatalf("NewSender kept TLS mode %q, want the value it was given, not a guess", built.TLSMode())
	}

	for name, s := range map[string]*Sender{
		"NewSender with SMTP_TLS=tls": built,
		"a sender with no mode":       NewTestSender(host, port, "", false),
	} {
		err := s.DeliverForTest(context.Background(), "to@example.com", []byte("Subject: x\r\n\r\nbody"))
		if err == nil || !strings.Contains(err.Error(), "SMTP_TLS") || !strings.Contains(err.Error(), "nothing was sent") {
			t.Errorf("%s: deliver = %v, want a refusal naming SMTP_TLS", name, err)
		}
	}
	if n := dials.Load(); n != 0 {
		t.Fatalf("the sender connected %d times with an unknown TLS mode, want none", n)
	}
}
