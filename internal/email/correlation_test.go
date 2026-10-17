package email

import (
	"context"
	"strings"
	"testing"
)

// TestCorrelationHeader_CtxRoundTrip pins the ctx→header contract: the
// header value is set iff the ctx carries a well-formed outbox id, and a
// malformed value (injection attempt, oversize) drops the header rather
// than emitting anything.
func TestCorrelationHeader_CtxRoundTrip(t *testing.T) {
	if got := correlationID(context.Background()); got != "" {
		t.Errorf("no ctx id: want empty header, got %q", got)
	}

	ctx := WithCorrelation(context.Background(), "vlx_emob_00112233445566778899aabb")
	want := "vlx_emob_00112233445566778899aabb"
	if got := correlationID(ctx); got != want {
		t.Errorf("header value: got %q, want %q", got, want)
	}

	for name, bad := range map[string]string{
		"crlf injection": "vlx_emob_1\r\nBcc: attacker@evil.test",
		"space":          "vlx emob 1",
		"oversize":       strings.Repeat("a", 81),
		"empty":          "",
	} {
		if got := correlationID(WithCorrelation(context.Background(), bad)); got != "" {
			t.Errorf("%s: want dropped header, got %q", name, got)
		}
	}
}

// TestCorrelationHeader_OnTheWire proves the stamped header actually
// reaches the SMTP DATA payload — for both MIME builders — exactly once,
// and is absent when the ctx carries no id (direct Sender use outside
// the dispatcher).
func TestCorrelationHeader_OnTheWire(t *testing.T) {
	srv := newScriptedSMTP(t)
	s := newCCTestSender(t, srv)
	ctx := WithCorrelation(context.Background(), "vlx_emob_deadbeefdeadbeefdeadbeef")

	if err := s.sendRich(ctx, richMessage{
		TenantID: "t1", To: "ap@acme.test",
		Subject: "Invoice INV-1", TextBody: "hi", HTMLBody: "<p>hi</p>",
	}); err != nil {
		t.Fatalf("sendRich: %v", err)
	}
	data := srv.lastData()
	if n := strings.Count(data, "X-PM-Metadata-vlx-outbox-id: vlx_emob_deadbeefdeadbeefdeadbeef"); n != 1 {
		t.Errorf("sendRich wire message: want header exactly once, found %d\n%s", n, data[:min(400, len(data))])
	}

	if err := s.sendPlain(ctx, "t1", "op@acme.test", "", "Reset", "body"); err != nil {
		t.Fatalf("sendPlain: %v", err)
	}
	if n := strings.Count(srv.lastData(), "X-PM-Metadata-vlx-outbox-id: vlx_emob_deadbeefdeadbeefdeadbeef"); n != 1 {
		t.Errorf("sendPlain wire message: want header exactly once, found %d", n)
	}

	// No ctx id → no header on the wire.
	if err := s.sendRich(context.Background(), richMessage{
		TenantID: "t1", To: "ap@acme.test",
		Subject: "Invoice INV-2", TextBody: "hi", HTMLBody: "<p>hi</p>",
	}); err != nil {
		t.Fatalf("sendRich (no ctx): %v", err)
	}
	if strings.Contains(srv.lastData(), "X-PM-Metadata") {
		t.Error("header must be absent when ctx carries no outbox id")
	}
}
