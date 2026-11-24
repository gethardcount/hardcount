package api

import (
	"context"
	"fmt"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/domain"
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
	for _, figure := range []string{"outstanding_ar", "open_invoices", "failed_payments"} {
		if got := flowInt(ov, figure); got != 0 {
			t.Errorf("Home %s = %d, want 0", figure, got)
		}
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
