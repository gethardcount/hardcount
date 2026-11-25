package api

import (
	"context"
	"fmt"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/email"
	"github.com/gethardcount/hardcount/internal/platform/clock"
	"github.com/gethardcount/hardcount/internal/platform/leader"
	"github.com/gethardcount/hardcount/internal/tenantstripe"
	"github.com/gethardcount/hardcount/internal/testutil"
)

// TestActivityLines_DunningRunAndSentEmails is the activity line standard's
// test for the Dunning page's recovery timeline and the customer's Sent
// emails card (ADR-128, PR 5 of its plan). Both APIs carry a `line` on every
// row, in the same words the invoice's Activity card uses for the same
// event, with no Recorded time.
//
// The story: a one-off invoice is finalized with no card on file, so payment
// recovery starts; the operator emails the invoice and records an offline
// payment, which ends recovery.
func TestActivityLines_DunningRunAndSentEmails(t *testing.T) {
	db := testutil.SetupTestDB(t)
	// A configured server queues the invoice email; no dispatcher runs in
	// the test, so the lines read "queued".
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
	call("PUT", "/v1/dunning/policy", flowReq{body: map[string]any{
		"final_subscription_action": "pause", "final_invoice_action": "none",
		"enabled": true, "retry_days": []int{3, 3},
	}}).want(t, 200, "create dunning policy")

	customerID := flowStr(call("POST", "/v1/customers", flowReq{body: map[string]any{
		"external_id": "activity-lines-real", "display_name": "Activity lines real", "email": "billing@activity-lines.test",
	}}).want(t, 201, "create customer").body, "id")
	invoiceID := flowStr(call("POST", "/v1/invoices", flowReq{body: map[string]any{
		"customer_id": customerID,
		"line_items":  []map[string]any{{"description": "Activity lines real", "quantity": 1, "unit_amount_cents": 4900}},
	}}).want(t, 201, "create invoice").body, "id")
	call("POST", "/v1/invoices/"+invoiceID+"/finalize", flowReq{}).want(t, 200, "finalize")
	call("POST", "/v1/invoices/"+invoiceID+"/send", flowReq{body: map[string]any{"email": "billing@activity-lines.test"}}).want(t, 200, "email invoice")
	call("POST", "/v1/invoices/"+invoiceID+"/record-payment", flowReq{body: map[string]any{"note": "Wire 1042"}}).want(t, 200, "record payment")

	invoiceRows := flowList(call("GET", "/v1/invoices/"+invoiceID+"/payment-timeline", flowReq{}).want(t, 200, "invoice timeline").body, "events")
	runs := flowList(call("GET", "/v1/dunning/runs?invoice_id="+invoiceID, flowReq{}).want(t, 200, "list runs").body, "data")
	if len(runs) != 1 {
		t.Fatalf("invoice has %d payment-recovery runs, want 1", len(runs))
	}
	detail := call("GET", "/v1/dunning/runs/"+flowStr(runs[0], "id"), flowReq{}).want(t, 200, "get run").body
	if d, _ := detail["degraded"].([]any); len(d) != 0 {
		t.Errorf("Dunning page degraded lanes %v", d)
	}
	dunningRows := flowList(detail, "events")
	emailRows := flowList(call("GET", "/v1/customers/"+customerID+"/sent-emails", flowReq{}).want(t, 200, "sent emails").body, "data")

	// The Dunning page reads as the standard words it, in the order it
	// happened. Nothing is folded there: the run's end shows.
	dunningLines := linesOf(t, dunningRows)
	if got, want := summaries(normalizeLines(dunningLines)), []string{
		"Payment recovery started | Automatic",
		"Payment recovery ended | Invoice paid · Automatic",
	}; !reflect.DeepEqual(got, want) {
		t.Errorf("Dunning page lines\n got: %q\nwant: %q", got, want)
	}
	if len(dunningLines) == 2 {
		if p := dunningLines[0]["problem"]; p != "No payment method on file. Reminders ask the customer to pay the invoice." {
			t.Errorf("payment recovery started: problem %q", p)
		}
		if dunningLines[0]["tone"] != "warning" || dunningLines[1]["tone"] != "success" {
			t.Errorf("tones: started %v, ended %v; want warning, success", dunningLines[0]["tone"], dunningLines[1]["tone"])
		}
	}

	// The Sent emails card names the invoice, linked, and who it went to.
	emailLines := linesOf(t, emailRows)
	// No Stripe is connected, so finalizing emailed the customer the invoice
	// itself (ADR-060 amendment 2026-10-30); the second is the operator's.
	if got, want := summaries(normalizeLines(emailLines)), []string{
		"Invoice email queued | <number> · To billing@activity-lines.test",
		"Invoice email queued | <number> · To billing@activity-lines.test",
	}; !reflect.DeepEqual(got, want) {
		t.Errorf("Sent emails lines\n got: %q\nwant: %q", got, want)
	}
	for _, l := range emailLines {
		if link := firstLink(l); link == nil || link["kind"] != "invoice" || link["id"] != invoiceID {
			t.Errorf("Sent emails line %v: link %v, want the invoice %s", l["title"], link, invoiceID)
		}
	}
	// One event, one wording: a row on the Dunning page or the Sent emails
	// card says what the invoice's Activity card says for the same event.
	onInvoice := map[string]map[string]any{}
	for _, e := range invoiceRows {
		if l, ok := e["line"].(map[string]any); ok {
			onInvoice[fmt.Sprint(e["id"])] = l
		}
	}
	compared := 0
	for _, e := range dunningRows {
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
	for _, e := range emailRows {
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
	// The setup-link email is sent only when its page can add the card.
	t.Setenv("PAYMENT_UPDATE_RETURN_URL", "https://billing.skipped-email.test/payment-method-added")
	srv := NewServer(db, clock.Real(), nil)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	tenantID := testutil.CreateTestTenant(t, db, "Skipped Email Co")
	key := createAPIKeyWithMode(t, db, tenantID, false)
	// The setup-link email goes out only with Stripe connected (ADR-060
	// amendment 2026-10-30); without it the customer gets the invoice.
	if _, err := tenantstripe.NewStore(db).Upsert(callerCtx(tenantID, false), tenantstripe.UpsertInput{
		TenantID: tenantID, Livemode: false, SecretKey: "sk_test_skipped_email", PublishableKey: "pk_test_skipped_email",
	}); err != nil {
		t.Fatalf("connect stripe: %v", err)
	}
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

	// Delivery resumes: the dispatcher runs as cmd/hardcount runs it, behind
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
	sent := flowList(call("GET", "/v1/customers/"+customerID+"/sent-emails", flowReq{}).want(t, 200, "sent emails").body, "data")
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
