package api

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/email"
	"github.com/gethardcount/hardcount/internal/invoice"
	"github.com/gethardcount/hardcount/internal/platform/clock"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/platform/walk"
	"github.com/gethardcount/hardcount/internal/tenant"
	"github.com/gethardcount/hardcount/internal/tenantstripe"
	"github.com/gethardcount/hardcount/internal/testutil"
)

// A top-up through the real router and database: an invoice created with one
// commit line, which stays a draft until it is paid.

type topupEnv struct {
	db       *postgres.DB
	ts       *httptest.Server
	tenantID string
	key      string
}

func newTopupEnv(t *testing.T, name string) topupEnv {
	t.Helper()
	db := testutil.SetupTestDB(t)
	ts := httptest.NewServer(NewServer(db, clock.Real(), nil))
	t.Cleanup(ts.Close)
	tenantID := testutil.CreateTestTenant(t, db, name)
	return topupEnv{db: db, ts: ts, tenantID: tenantID, key: createAPIKeyWithMode(t, db, tenantID, false)}
}

func (e topupEnv) count(t *testing.T, q string, args ...any) int {
	t.Helper()
	tx, err := e.db.BeginTx(t.Context(), postgres.TxBypass, "")
	if err != nil {
		t.Fatal(err)
	}
	defer postgres.Rollback(tx)
	var n int
	if err := tx.QueryRowContext(t.Context(), q, args...).Scan(&n); err != nil {
		t.Fatalf("count %q: %v", q, err)
	}
	return n
}

func (e topupEnv) call(t *testing.T, method, path string, body any) flowResp {
	t.Helper()
	return flowDo(t, e.ts, method, path, flowReq{bearer: e.key, body: body})
}

// create makes a customer and a top-up for it, and returns both ids.
func (e topupEnv) create(t *testing.T) (customerID, invoiceID string) {
	t.Helper()
	customerID = flowStr(e.call(t, "POST", "/v1/customers", map[string]any{
		"external_id": "topup", "display_name": "Topup", "email": "topup@topup.test",
	}).want(t, 201, "create customer").body, "id")
	inv := e.call(t, "POST", "/v1/invoices", map[string]any{
		"customer_id": customerID,
		"line_items":  []map[string]any{{"description": "Prepaid credit", "quantity": 1, "unit_amount_cents": 9000, "commit_granted_cents": 10000}},
	}).want(t, 201, "create top-up").body
	if flowStr(inv, "billing_reason") != "topup" || flowStr(inv, "invoice_number") != "" {
		t.Fatalf("created billing_reason %q number %q, want an unnumbered topup", flowStr(inv, "billing_reason"), flowStr(inv, "invoice_number"))
	}
	return customerID, flowStr(inv, "id")
}

// A top-up is sold whole: its hosted page may already be open, so no line
// can be added to it, a charge or a second commit line alike.
func TestAddLineItem_TopupIsFrozen(t *testing.T) {
	e := newTopupEnv(t, "Topup Frozen Co")
	_, id := e.create(t)
	for name, line := range map[string]map[string]any{
		"a charge":      {"description": "Setup", "quantity": 1, "unit_amount_cents": 1000},
		"a commit line": {"description": "More credit", "quantity": 1, "unit_amount_cents": 1000, "commit_granted_cents": 1000},
	} {
		r := e.call(t, "POST", "/v1/invoices/"+id+"/line-items", line)
		if r.status != 409 || flowStr(r.body, "error", "code") != "invoice_not_manual" ||
			!strings.Contains(flowStr(r.body, "error", "message"), "A top-up is created whole") {
			t.Errorf("add %s: %d %s, want 409 invoice_not_manual saying a top-up is created whole", name, r.status, r.raw)
		}
	}
}

// An unpaid top-up owes nothing, so it stays out of every figure of money
// owed, even after a declined charge.
func TestTopup_UnpaidIsOutOfHomeOpenListOutstandingAndDunning(t *testing.T) {
	e := newTopupEnv(t, "Topup Unpaid Co")
	customerID, id := e.create(t)
	// A declined saved-card charge leaves the draft payment_status failed;
	// Stripe is not wired here, so the row is stamped as that charge leaves it.
	ctx := postgres.WithLivemode(context.Background(), false)
	tx, err := e.db.BeginTx(ctx, postgres.TxBypass, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE invoices SET payment_status = 'failed' WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	ov := e.call(t, "GET", "/v1/analytics/overview?period=30d", nil).want(t, 200, "overview").body
	if got := flowInt(ov, "open_invoices"); got != 0 {
		t.Errorf("Home open_invoices = %d, want 0", got)
	}
	if owed := flowList(ov, "outstanding_by_currency"); len(owed) != 0 {
		t.Errorf("Home outstanding_by_currency = %v, want none", owed)
	}
	if open := flowList(e.call(t, "GET", "/v1/invoices?status=finalized", nil).want(t, 200, "open list").body, "data"); len(open) != 0 {
		t.Errorf("open list has %d invoices, want 0", len(open))
	}
	cust := e.call(t, "GET", "/v1/customers/"+customerID+"/overview", nil).want(t, 200, "customer overview").body
	if n, owed := flowInt(cust, "outstanding_balance", "unpaid_count"), flowInt(cust, "outstanding_balance", "total_cents"); n != 0 || owed != 0 {
		t.Errorf("Outstanding: %d unpaid, %d cents, want none", n, owed)
	}
	store := invoice.NewPostgresStore(e.db)
	failed, err := store.ListFailedWithoutDunningRun(ctx, time.Now().Add(time.Hour), 100)
	if err != nil {
		t.Fatal(err)
	}
	queued, err := store.ListAutoChargePending(ctx, walk.Key{}, 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, inv := range append(failed, queued...) {
		if inv.ID == id {
			t.Errorf("the top-up is listed for dunning or automatic collection")
		}
	}
}

// Finalize refuses a top-up, which its payment issues, and changes nothing.
// Without the refusal in Service.Finalize the draft takes the engine's
// finalize (a top-up is not an operator's draft), and the
// invoices_invoice_number_check constraint refuses a finalized row with no
// number: a 500, not this 409 and its sentence. Without that constraint too,
// the top-up is finalized unnumbered and sends invoice.finalized, which the
// checks after the answer catch.
func TestTopup_FinalizeIsRefused(t *testing.T) {
	e := newTopupEnv(t, "Topup Finalize Co")
	_, id := e.create(t)
	r := e.call(t, "POST", "/v1/invoices/"+id+"/finalize", nil)
	const sentence = "A top-up is issued when it is paid: collect it, send it, or record a payment."
	if r.status != 409 || flowStr(r.body, "error", "code") != "invalid_state" || flowStr(r.body, "error", "message") != sentence {
		t.Fatalf("finalize a top-up: %d %s, want 409 invalid_state %q", r.status, r.raw, sentence)
	}
	inv := flowObj(e.call(t, "GET", "/v1/invoices/"+id, nil).want(t, 200, "get the top-up").body, "invoice")
	if flowStr(inv, "status") != "draft" || flowStr(inv, "invoice_number") != "" {
		t.Errorf("after the refusal: %s %q, want an unnumbered draft", flowStr(inv, "status"), flowStr(inv, "invoice_number"))
	}
	if n := e.count(t, `SELECT count(*) FROM customer_credit_ledger WHERE source_invoice_id = $1 AND grant_kind = 'commit'`, id); n != 0 {
		t.Errorf("commit grants = %d, want 0", n)
	}
	if n := e.count(t, `SELECT count(*) FROM webhook_outbox WHERE event_type = $1 AND payload->'invoice'->>'id' = $2`, domain.EventInvoiceFinalized, id); n != 0 {
		t.Errorf("invoice.finalized events = %d, want 0", n)
	}
}

// The hosted page is where a customer buys the credit: an unpaid top-up's
// page answers with the credit and its validity, and no number or due date,
// and its Checkout is admitted. Once withdrawn it answers as any voided
// invoice. It fails if the hosted view refuses a draft again
// (hostedinvoice.Handler.resolveInvoice), or if the Checkout claim admits
// only status = 'finalized' again (payment.CheckoutSessionStore.ClaimOpenAudited):
// either answers something other than 200 below.
func TestTopup_HostedPageAndCheckout(t *testing.T) {
	// The hosted page takes payment only with these addresses set.
	t.Setenv("HOSTED_INVOICE_BASE_URL", "https://billing.topup.test")
	t.Setenv("CUSTOMER_PORTAL_URL", "https://app.topup.test")
	e := newTopupEnv(t, "Topup Hosted Co")
	if _, err := tenantstripe.NewStore(e.db).Upsert(callerCtx(e.tenantID, false), tenantstripe.UpsertInput{
		TenantID: e.tenantID, Livemode: false, SecretKey: "sk_test_topup_hosted", PublishableKey: "pk_test_topup_hosted",
	}); err != nil {
		t.Fatalf("connect stripe: %v", err)
	}
	stripe := installStripeStub(t)
	stripe.extra = func(call string, form url.Values) (int, string, bool) {
		switch {
		case call == "POST /v1/customers":
			return 200, `{"id":"cus_topup_hosted","object":"customer"}`, true
		case call == "POST /v1/checkout/sessions" && form.Get("mode") == "payment":
			return 200, fmt.Sprintf(`{"id":"cs_topup","object":"checkout.session","mode":"payment","status":"open","url":"https://checkout.stripe.test/c/cs_topup","expires_at":%d}`,
				time.Now().Add(time.Hour).Unix()), true
		case call == "POST /v1/checkout/sessions/cs_topup/expire":
			return 200, `{"id":"cs_topup","object":"checkout.session","status":"expired"}`, true
		}
		return 0, "", false
	}
	days := 365
	if _, err := tenant.NewSettingsStore(e.db).Update(callerCtx(e.tenantID, false), e.tenantID, func(ts *domain.TenantSettings) error {
		ts.TopupCreditValidDays = &days
		return nil
	}); err != nil {
		t.Fatalf("set the credit validity: %v", err)
	}
	_, id := e.create(t)
	token := flowStr(flowObj(e.call(t, "GET", "/v1/invoices/"+id, nil).want(t, 200, "get the top-up").body, "invoice"), "public_token")
	page := func() map[string]any {
		return flowDo(t, e.ts, "GET", "/v1/public/invoices/"+token, flowReq{}).want(t, 200, "hosted page").body
	}
	checkout := func() flowResp {
		return flowDo(t, e.ts, "POST", "/v1/public/invoices/"+token+"/checkout", flowReq{})
	}

	view := page()
	inv := flowObj(view, "invoice")
	if _, due := inv["due_at"]; due || flowStr(inv, "invoice_number") != "" {
		t.Errorf("unpaid top-up page shows number %q, due_at present %t: want neither", flowStr(inv, "invoice_number"), due)
	}
	if got := flowInt(view, "topup", "credit_cents"); got != 10000 {
		t.Errorf("credit = %d, want 10000", got)
	}
	if got := flowStr(view, "topup", "validity"); got != "It is valid for 365 days after payment." {
		t.Errorf("validity %q, want the days the grant is valid for", got)
	}
	checkout().want(t, 200, "start the top-up's Checkout")
	if n := e.count(t, `SELECT count(*) FROM checkout_sessions WHERE invoice_id = $1 AND status = 'open'`, id); n != 1 {
		t.Errorf("open Checkout claims = %d, want 1", n)
	}

	e.call(t, "POST", "/v1/invoices/"+id+"/void", nil).want(t, 200, "withdraw the top-up")
	voided := page()
	if flowStr(voided, "invoice", "status") != "voided" || voided["pay_enabled"] != false {
		t.Errorf("withdrawn top-up page: status %q pay_enabled %v, want voided and not payable", flowStr(voided, "invoice", "status"), voided["pay_enabled"])
	}
	if r := checkout(); r.status != 409 || flowStr(r.body, "error", "code") != "not_payable" {
		t.Errorf("Checkout on a withdrawn top-up: %d %s, want 409 not_payable", r.status, r.raw)
	}
}

// An unpaid top-up's history shows before its payment issues it: its card
// was declined and its link sent, on the invoice's Activity and on the
// customer's Sent emails. It fails if the timeline answers every draft with
// nothing again (invoice.Handler.paymentTimeline), or if the email lists find
// rows by invoice number alone or leave out topup_offer
// (email.OutboxStore.ListByInvoice and ListByCustomer): a top-up has no
// number until it is paid.
func TestTopup_HistoryBeforePayment(t *testing.T) {
	e := newTopupEnv(t, "Topup History Co")
	customerID, id := e.create(t)
	ctx := callerCtx(e.tenantID, false)
	if err := invoice.NewPostgresStore(e.db).RecordChargeAttempt(ctx, e.tenantID, domain.InvoiceChargeAttempt{
		InvoiceID: id, Trigger: domain.ChargeTriggerChargeNow, Outcome: domain.ChargeAttemptFailed,
		DeclineCode: "insufficient_funds", AmountCents: 9000, OccurredAt: time.Now(),
	}); err != nil {
		t.Fatalf("record the declined charge: %v", err)
	}
	if err := email.NewOutboxSender(email.NewOutboxStore(e.db)).SendTopupOffer(ctx, e.tenantID, id, "topup@topup.test", nil,
		"Topup", 9000, domain.TopupCredit{CreditCents: 10000}, "USD", "hc_pinv_topup_history"); err != nil {
		t.Fatalf("queue the top-up's link: %v", err)
	}

	titles := map[string]bool{}
	for _, ev := range flowList(e.call(t, "GET", "/v1/invoices/"+id+"/payment-timeline", nil).want(t, 200, "timeline").body, "events") {
		titles[flowStr(ev, "line", "title")] = true
	}
	for _, want := range []string{"Top-up created", "Payment failed", "Top-up link queued"} {
		if !titles[want] {
			t.Errorf("the top-up's timeline has no %q row; it has %v", want, titles)
		}
	}

	sent := flowList(e.call(t, "GET", "/v1/customers/"+customerID+"/sent-emails", nil).want(t, 200, "sent emails").body, "data")
	if len(sent) != 1 || flowStr(sent[0], "email_type") != "topup_offer" || flowStr(sent[0], "line", "title") != "Top-up link queued" {
		t.Errorf("Sent emails = %v, want the top-up's link", sent)
	}
}

// A top-up has no document until it is paid: both PDF routes refuse an
// unpaid one and a withdrawn one with one sentence. It fails if either route
// renders it again (invoice.DocumentRefusal is checked in both
// downloadPDF handlers): the answer is then a 200 PDF.
func TestTopup_NoDocumentUntilPaid(t *testing.T) {
	e := newTopupEnv(t, "Topup Document Co")
	_, id := e.create(t)
	token := flowStr(flowObj(e.call(t, "GET", "/v1/invoices/"+id, nil).want(t, 200, "get the top-up").body, "invoice"), "public_token")
	const sentence = "A top-up has no document until it is paid."
	check := func(state string) {
		t.Helper()
		for route, r := range map[string]flowResp{
			"operator": e.call(t, "GET", "/v1/invoices/"+id+"/pdf", nil),
			"hosted":   flowDo(t, e.ts, "GET", "/v1/public/invoices/"+token+"/pdf", flowReq{}),
		} {
			if r.status != 409 || r.errCode() != "invalid_state" || flowStr(r.body, "error", "message") != sentence {
				t.Errorf("%s PDF of a %s top-up: %d %.200s, want 409 invalid_state %q", route, state, r.status, r.raw, sentence)
			}
		}
	}
	check("unpaid")
	e.call(t, "POST", "/v1/invoices/"+id+"/void", nil).want(t, 200, "withdraw the top-up")
	check("withdrawn")
}

// A paid top-up's unused credit is refunded through the real router, as the
// guide shows: the router wires the unbilled usage relief counts as used, and
// a relief refuses to run without it. The top-up was paid by bank transfer,
// so the refund is paid back outside Stripe, and there is no failed card
// refund to mark as returned another way.
func TestTopup_RefundUnusedCredit(t *testing.T) {
	e := newTopupEnv(t, "Topup Refund Co")
	_, id := e.create(t)
	e.call(t, "POST", "/v1/invoices/"+id+"/record-payment", map[string]any{"note": "Wire 1042"}).want(t, 200, "record the payment")

	cn := e.call(t, "POST", "/v1/credit-notes", map[string]any{
		"invoice_id": id, "commit_relief": map[string]any{"retire_all": true},
	}).want(t, 201, "refund the unused credit").body
	if flowStr(cn, "status") != "issued" || flowInt(cn, "total_cents") != 9000 || flowInt(cn, "commit_retired_cents") != 10000 ||
		flowInt(cn, "refund_amount_cents") != 0 || flowInt(cn, "out_of_band_amount_cents") != 9000 ||
		flowStr(cn, "reason") != "Unused prepaid credit refunded" {
		t.Errorf("relief %v, want issued, 9000 out of band for 10000 of credit, reason Unused prepaid credit refunded", cn)
	}

	r := e.call(t, "POST", "/v1/credit-notes/"+flowStr(cn, "id")+"/mark-returned-another-way", nil)
	if r.status != 409 || r.errCode() != "invalid_state" {
		t.Errorf("returned another way with no failed refund: %d %.200s, want 409 invalid_state", r.status, r.raw)
	}
}

// The usage a customer has used and no invoice has billed yet is credit
// already used, and relief takes only what is left: the router's adapter reads
// it from the billing engine. 700 calls at 1c on the open period leave
// 93.00 of the 100.00 bought unused, refunded at 0.90 each.
func TestTopup_RefundCountsUnbilledUsage(t *testing.T) {
	r := newLagRig(t, "in_arrears", false)
	at := lagP0.Add(10 * 24 * time.Hour)
	r.ingest(lagP0.Add(24*time.Hour), 700)
	inv := r.must(http.MethodPost, "/v1/invoices", at, map[string]any{
		"customer_id": r.custID,
		"line_items":  []map[string]any{{"description": "Prepaid credit", "quantity": 1, "unit_amount_cents": 9000, "commit_granted_cents": 10000}},
	}, http.StatusCreated)
	id := inv["id"].(string)
	r.must(http.MethodPost, "/v1/invoices/"+id+"/record-payment", at, map[string]any{"note": "Wire"}, http.StatusOK)

	cn := r.must(http.MethodPost, "/v1/credit-notes", at, map[string]any{
		"invoice_id": id, "commit_relief": map[string]any{"retire_all": true},
	}, http.StatusCreated)
	if got := int64(cn["commit_retired_cents"].(float64)); got != 9300 {
		t.Errorf("credit taken back %d, want 9300: the 700 of usage not invoiced yet is credit already used", got)
	}
	if got := int64(cn["total_cents"].(float64)); got != 8370 {
		t.Errorf("refund %d, want 8370 (9300 of credit at 0.90)", got)
	}
}
