package api

import (
	"context"
	"fmt"
	"net/http/httptest"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/email"
	"github.com/sagarsuperuser/velox/internal/platform/clock"
	"github.com/sagarsuperuser/velox/internal/platform/leader"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

// TestActivityLines_DunningRunAndSentEmails is the activity line standard's
// test for the Dunning page's recovery timeline and the customer's Sent
// emails card (ADR-128, PR 5 of its plan). Both APIs carry a `line` on every
// row, in the same words the invoice's Activity card uses for the same
// event, and a test-clock line is the real-clock line plus one Recorded
// time.
//
// The story, for a real-clock customer and for one pinned to a test clock:
// a one-off invoice is finalized with no card on file, so payment recovery
// starts; the operator emails the invoice and records an offline payment,
// which ends recovery.
func TestActivityLines_DunningRunAndSentEmails(t *testing.T) {
	db := testutil.SetupTestDB(t)
	// A configured server queues the invoice email; no dispatcher runs in
	// the test, so both arms read "queued".
	t.Setenv("SMTP_HOST", "smtp.activity-lines.invalid")
	ts := httptest.NewServer(NewServer(db, clock.Real(), nil))
	defer ts.Close()

	tenantID := testutil.CreateTestTenant(t, db, "Activity Dunning Emails Co")
	key := createAPIKeyWithMode(t, db, tenantID, false)
	call := func(method, path string, req flowReq) flowResp {
		t.Helper()
		req.bearer = key
		return flowDo(t, ts, method, path, req)
	}

	// With a dunning policy, finalizing an invoice that has no card on file
	// starts payment recovery. The first policy becomes the default.
	call("POST", "/v1/dunning/policies", flowReq{body: map[string]any{
		"name": "Activity lines policy", "enabled": true, "retry_days": []int{3, 3},
	}}).want(t, 201, "create dunning policy")

	const frozen = "2027-02-01T04:30:00Z"
	clockID := flowStr(call("POST", "/v1/test-clocks", flowReq{body: map[string]any{
		"name": "activity lines", "frozen_time": frozen,
	}}).want(t, 201, "create test clock").body, "id")

	type told struct {
		invoiceID string
		invoice   []map[string]any // the invoice's payment timeline rows
		dunning   []map[string]any // the run's events on the Dunning page
		emails    []map[string]any // the customer's Sent emails rows
	}
	story := func(name, clockID string) told {
		t.Helper()
		cust := map[string]any{
			"external_id": "activity-lines-" + name, "display_name": "Activity lines " + name, "email": "billing@activity-lines.test",
		}
		if clockID != "" {
			cust["test_clock_id"] = clockID
		}
		customerID := flowStr(call("POST", "/v1/customers", flowReq{body: cust}).want(t, 201, name+": create customer").body, "id")
		invoiceID := flowStr(call("POST", "/v1/invoices", flowReq{body: map[string]any{
			"customer_id": customerID,
			"line_items":  []map[string]any{{"description": "Activity lines " + name, "quantity": 1, "unit_amount_cents": 4900}},
		}}).want(t, 201, name+": create invoice").body, "id")
		call("POST", "/v1/invoices/"+invoiceID+"/finalize", flowReq{}).want(t, 200, name+": finalize")
		call("POST", "/v1/invoices/"+invoiceID+"/send", flowReq{body: map[string]any{"email": "billing@activity-lines.test"}}).want(t, 200, name+": email invoice")
		call("POST", "/v1/invoices/"+invoiceID+"/record-payment", flowReq{body: map[string]any{"note": "Wire 1042"}}).want(t, 200, name+": record payment")

		timeline := call("GET", "/v1/invoices/"+invoiceID+"/payment-timeline", flowReq{}).want(t, 200, name+": invoice timeline").body
		runs := flowList(call("GET", "/v1/dunning/runs?invoice_id="+invoiceID, flowReq{}).want(t, 200, name+": list runs").body, "data")
		if len(runs) != 1 {
			t.Fatalf("%s: invoice has %d payment-recovery runs, want 1", name, len(runs))
		}
		detail := call("GET", "/v1/dunning/runs/"+flowStr(runs[0], "id"), flowReq{}).want(t, 200, name+": get run").body
		if d, _ := detail["degraded"].([]any); len(d) != 0 {
			t.Errorf("%s: Dunning page degraded lanes %v", name, d)
		}
		sent := call("GET", "/v1/customers/"+customerID+"/sent-emails", flowReq{}).want(t, 200, name+": sent emails").body
		return told{
			invoiceID: invoiceID,
			invoice:   flowList(timeline, "events"),
			dunning:   flowList(detail, "events"),
			emails:    flowList(sent, "sent_emails"),
		}
	}

	real := story("real", "")
	test := story("test", clockID)

	// The Dunning page reads as the standard words it, in the order it
	// happened. Nothing is folded there: the run's end shows.
	realDunning := linesOf(t, real.dunning)
	if got, want := summaries(normalizeLines(realDunning)), []string{
		"Payment recovery started | Automatic",
		"Payment recovery ended | Invoice paid · Automatic",
	}; !reflect.DeepEqual(got, want) {
		t.Errorf("Dunning page lines\n got: %q\nwant: %q", got, want)
	}
	if len(realDunning) == 2 {
		if p := realDunning[0]["problem"]; p != "No payment method on file. Reminders go out until the customer adds one." {
			t.Errorf("payment recovery started: problem %q", p)
		}
		if realDunning[0]["tone"] != "warning" || realDunning[1]["tone"] != "success" {
			t.Errorf("tones: started %v, ended %v; want warning, success", realDunning[0]["tone"], realDunning[1]["tone"])
		}
	}

	// The Sent emails card names the invoice, linked, and who it went to.
	realEmails := linesOf(t, real.emails)
	if got, want := summaries(normalizeLines(realEmails)), []string{
		"Invoice email queued | <number> · To billing@activity-lines.test",
	}; !reflect.DeepEqual(got, want) {
		t.Errorf("Sent emails lines\n got: %q\nwant: %q", got, want)
	}
	for _, l := range realEmails {
		if link := firstLink(l); link == nil || link["kind"] != "invoice" || link["id"] != real.invoiceID {
			t.Errorf("Sent emails line %v: link %v, want the invoice %s", l["title"], link, real.invoiceID)
		}
	}

	// One event, one wording: a row on the Dunning page or the Sent emails
	// card says what the invoice's Activity card says for the same event.
	for _, arm := range []told{real, test} {
		onInvoice := map[string]map[string]any{}
		for _, e := range arm.invoice {
			if l, ok := e["line"].(map[string]any); ok {
				onInvoice[fmt.Sprint(e["id"])] = l
			}
		}
		compared := 0
		for _, e := range arm.dunning {
			il, ok := onInvoice["dunning:"+fmt.Sprint(e["id"])]
			if !ok {
				continue // folded on the invoice page
			}
			compared++
			dl, _ := e["line"].(map[string]any)
			if got, want := normalizeLines([]map[string]any{dl}), normalizeLines([]map[string]any{il}); !reflect.DeepEqual(got, want) {
				t.Errorf("event %v: the Dunning page and the invoice page word it differently\nDunning: %s\ninvoice: %s", e["id"], mustJSON(got), mustJSON(want))
			}
		}
		for _, e := range arm.emails {
			il, ok := onInvoice["email:"+fmt.Sprint(e["id"])]
			if !ok {
				continue
			}
			compared++
			el, _ := e["line"].(map[string]any)
			for _, k := range []string{"kind", "title", "tone", "problem"} {
				if fmt.Sprint(el[k]) != fmt.Sprint(il[k]) {
					t.Errorf("email %v: %s is %v on Sent emails and %v on the invoice page", e["id"], k, el[k], il[k])
				}
			}
		}
		if compared < 2 {
			t.Errorf("only %d rows could be matched with the invoice page; want the recovery start and the invoice email", compared)
		}
	}

	// Same story on both clocks: equal lines, and the test-clock lines are
	// dated on the clock's frozen calendar with a Recorded time. Same-instant
	// events on the Dunning page keep the store's order until the page
	// adopts the shared ordering, so the lines compare as sets.
	for _, c := range []struct {
		name       string
		real, test []map[string]any
	}{{"Dunning page", real.dunning, test.dunning}, {"Sent emails", real.emails, test.emails}} {
		realLines, testLines := linesOf(t, c.real), linesOf(t, c.test)
		for i, l := range testLines {
			if l["recorded_at"] == nil {
				t.Errorf("%s: test-clock row %d (%v) has no recorded_at", c.name, i, l["title"])
			}
			if at, _ := time.Parse(time.RFC3339Nano, fmt.Sprint(l["occurred_at"])); at.UTC().Format(time.RFC3339) != frozen {
				t.Errorf("%s: test-clock row %d (%v) occurred at %v, want the frozen %s", c.name, i, l["title"], l["occurred_at"], frozen)
			}
		}
		for i, l := range realLines {
			if l["recorded_at"] != nil {
				t.Errorf("%s: real-clock row %d (%v) carries recorded_at %v", c.name, i, l["title"], l["recorded_at"])
			}
		}
		if rs, tst := sortedJSON(normalizeLines(realLines)), sortedJSON(normalizeLines(testLines)); !reflect.DeepEqual(rs, tst) {
			t.Errorf("%s: the two clocks tell different stories\nreal: %q\ntest: %q", c.name, rs, tst)
		}
	}
}

// TestActivityLines_SkippedEmailSaysWhy: an invoice is finalized with no
// card on file, so a payment setup email is queued; email delivery is
// stalled (an SMTP outage, G6), the operator records the customer's wire,
// and when the dispatcher reaches the email it skips it, because the
// invoice is paid. Found live on the FLOW E walk, 2026-07-19. Both cards
// that show the email say why: the invoice's Activity card and the
// customer's Sent emails card each read the recorded skip reason through
// their own adapter, so a reason dropped by either one reads "already
// closed" here.
func TestActivityLines_SkippedEmailSaysWhy(t *testing.T) {
	db := testutil.SetupTestDB(t)
	t.Setenv("SMTP_HOST", "smtp.skipped-email.invalid")
	t.Setenv("PAYMENT_UPDATE_URL", "https://billing.skipped-email.test/update-payment")
	srv := NewServer(db, clock.Real(), nil)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	tenantID := testutil.CreateTestTenant(t, db, "Skipped Email Co")
	key := createAPIKeyWithMode(t, db, tenantID, false)
	call := func(method, path string, req flowReq) flowResp {
		t.Helper()
		req.bearer = key
		return flowDo(t, ts, method, path, req)
	}

	customerID := flowStr(call("POST", "/v1/customers", flowReq{body: map[string]any{
		"external_id": "skipped-email", "display_name": "Skipped Email", "email": "billing@skipped-email.test",
	}}).want(t, 201, "create customer").body, "id")
	invoiceID := flowStr(call("POST", "/v1/invoices", flowReq{body: map[string]any{
		"customer_id": customerID,
		"line_items":  []map[string]any{{"description": "Skipped email", "quantity": 1, "unit_amount_cents": 4900}},
	}}).want(t, 201, "create invoice").body, "id")
	call("POST", "/v1/invoices/"+invoiceID+"/finalize", flowReq{}).want(t, 200, "finalize")
	call("POST", "/v1/invoices/"+invoiceID+"/record-payment", flowReq{body: map[string]any{"note": "Wire 2051"}}).want(t, 200, "record payment")

	setupLine := func(rows []map[string]any) string {
		t.Helper()
		lines := normalizeLines(linesOf(t, rows))
		for _, l := range lines {
			if l["kind"] == "email.payment_setup_request" {
				return summaries([]map[string]any{l})[0]
			}
		}
		return ""
	}
	onInvoice := func() string {
		t.Helper()
		return setupLine(flowList(call("GET", "/v1/invoices/"+invoiceID+"/payment-timeline", flowReq{}).want(t, 200, "invoice timeline").body, "events"))
	}
	if got := onInvoice(); !strings.HasPrefix(got, "Payment setup email queued") {
		t.Fatalf("before delivery resumes, the invoice card reads %q; want the setup email queued", got)
	}

	// Delivery resumes: the dispatcher runs as cmd/velox runs it, behind
	// the leader lease, with the invoice store as its staleness check.
	ctx, cancel := context.WithCancel(context.Background())
	d := email.NewDispatcher(srv.EmailOutboxStore, srv.EmailSender, email.DispatcherConfig{Interval: 50 * time.Millisecond}, leader.New(db.Pool, nil))
	d.SetSettledChecker(srv.EmailSettledChecker)
	stopped := make(chan struct{})
	go func() { defer close(stopped); d.Start(ctx) }()
	got := onInvoice()
	for deadline := time.Now().Add(15 * time.Second); strings.HasPrefix(got, "Payment setup email queued") && time.Now().Before(deadline); got = onInvoice() {
		time.Sleep(50 * time.Millisecond)
	}
	cancel()
	<-stopped

	const want = "Payment setup email not sent | The invoice was already paid"
	if !strings.HasPrefix(got, want) {
		t.Errorf("invoice Activity card: %q, want it to start %q", got, want)
	}
	sent := flowList(call("GET", "/v1/customers/"+customerID+"/sent-emails", flowReq{}).want(t, 200, "sent emails").body, "sent_emails")
	if got := setupLine(sent); !strings.HasPrefix(got, want) {
		t.Errorf("Sent emails card: %q, want it to start %q", got, want)
	}
}

// firstLink is the first link part of a line's meta, or nil.
func firstLink(l map[string]any) map[string]any {
	detail, _ := l["detail"].([]any)
	for _, s := range detail {
		parts, _ := s.([]any)
		for _, p := range parts {
			if pm, ok := p.(map[string]any); ok {
				if link, ok := pm["link"].(map[string]any); ok {
					return link
				}
			}
		}
	}
	return nil
}

// sortedJSON renders each normalized line as JSON and sorts them, to compare
// two lists as sets.
func sortedJSON(lines []map[string]any) []string {
	out := make([]string, 0, len(lines))
	for _, l := range lines {
		out = append(out, mustJSON(l))
	}
	sort.Strings(out)
	return out
}
