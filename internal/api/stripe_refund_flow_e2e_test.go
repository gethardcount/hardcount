package api

import (
	"context"
	"fmt"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/platform/clock"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/tenantstripe"
	"github.com/gethardcount/hardcount/internal/testutil"
)

// Refunds made in Stripe outside Hardcount (ADR-134), over the real router
// and database. Every refund event is a signed delivery to the tenant's
// Stripe webhook endpoint, checked with the endpoint's secret; Stripe itself
// is the stub (installStripeStub), which also counts the calls Hardcount
// makes there: a refund recorded from Stripe must make no refund call, and
// its tax reversal must make one.

const refundFlowSecret = "whsec_refund_flow"

// refundFlow is one tenant with Stripe connected in test mode, its webhook
// endpoint, and one customer.
type refundFlow struct {
	t          *testing.T
	db         *postgres.DB
	ts         *httptest.Server
	stub       *stripeStub
	tenantID   string
	key        string
	endpoint   string
	customerID string

	mu sync.Mutex
	// stripeRefunds are the refunds the stub's GET /v1/refunds/{id} returns.
	stripeRefunds map[string]string
	// madeRefunds counts the refunds Hardcount asked the stub to make.
	madeRefunds int
	// duringRefund runs inside the stub's POST /v1/refunds, while
	// Hardcount's refund call is "at Stripe", with the refund's id and form.
	duringRefund func(refundID string, form url.Values)
}

func newRefundFlow(t *testing.T, db *postgres.DB, ts *httptest.Server, name string) *refundFlow {
	t.Helper()
	f := &refundFlow{t: t, db: db, ts: ts, stripeRefunds: map[string]string{}}
	f.tenantID = testutil.CreateTestTenant(t, db, name)
	f.key = createAPIKeyWithMode(t, db, f.tenantID, false)
	if _, err := tenantstripe.NewStore(db).Upsert(context.Background(), tenantstripe.UpsertInput{
		TenantID: f.tenantID, Livemode: false, SecretKey: "sk_test_refund_flow", PublishableKey: "pk_test_refund_flow",
	}); err != nil {
		t.Fatalf("connect stripe: %v", err)
	}
	creds := f.call("PATCH", "/v1/settings/stripe/test/webhook", map[string]any{"webhook_secret": refundFlowSecret}).want(t, 200, "set webhook secret").body
	f.endpoint = "/v1/webhooks/stripe/" + flowStr(creds, "id")
	f.customerID = flowStr(f.call("POST", "/v1/customers", map[string]any{
		"external_id": "refund-flow-" + f.tenantID, "display_name": "Refund Flow Customer",
	}).want(t, 201, "create customer").body, "id")
	return f
}

// newRefundServer starts the server and the Stripe stub, which answers the
// refund and tax-reversal calls through the flows registered on it.
func newRefundServer(t *testing.T) (*postgres.DB, *httptest.Server, *stripeStub, func(*refundFlow)) {
	t.Helper()
	db := testutil.SetupTestDB(t)
	ts := httptest.NewServer(NewServer(db, clock.Real(), nil))
	t.Cleanup(ts.Close)
	stub := installStripeStub(t)
	var flows []*refundFlow
	var mu sync.Mutex
	stub.extra = func(call string, form url.Values) (int, string, bool) {
		mu.Lock()
		fs := append([]*refundFlow(nil), flows...)
		mu.Unlock()
		for _, f := range fs {
			if status, body, ok := f.answer(call, form); ok {
				return status, body, true
			}
		}
		return 0, "", false
	}
	register := func(f *refundFlow) {
		mu.Lock()
		defer mu.Unlock()
		f.stub = stub
		flows = append(flows, f)
	}
	return db, ts, stub, register
}

// answer is the stub's Stripe for refunds and tax reversals. The flows of
// one test share the stub, so the first flow answers.
func (f *refundFlow) answer(call string, form url.Values) (int, string, bool) {
	switch {
	case call == "POST /v1/refunds":
		f.mu.Lock()
		f.madeRefunds++
		id := fmt.Sprintf("re_hc_%s_%d", f.tenantID[len(f.tenantID)-6:], f.madeRefunds)
		during := f.duringRefund
		f.mu.Unlock()
		if during != nil {
			during(id, form)
		}
		amount, _ := strconv.ParseInt(form.Get("amount"), 10, 64)
		return 200, refundJSON(id, form.Get("payment_intent"), amount, "succeeded",
			map[string]string{"hardcount_cn_id": form.Get("metadata[hardcount_cn_id]")}), true
	case strings.HasPrefix(call, "GET /v1/refunds/"):
		f.mu.Lock()
		body, ok := f.stripeRefunds[strings.TrimPrefix(call, "GET /v1/refunds/")]
		f.mu.Unlock()
		if !ok {
			return 404, `{"error":{"type":"invalid_request_error","code":"resource_missing","message":"No such refund"}}`, true
		}
		return 200, body, true
	case call == "POST /v1/tax/transactions/create_reversal":
		return 200, fmt.Sprintf(`{"id":"tax_rev_%s","object":"tax.transaction","type":"reversal"}`, form.Get("reference")), true
	}
	return 0, "", false
}

func refundJSON(id, pi string, amount int64, status string, md map[string]string) string {
	var mds []string
	for k, v := range md {
		if v != "" {
			mds = append(mds, fmt.Sprintf("%q:%q", k, v))
		}
	}
	return fmt.Sprintf(`{"id":%q,"object":"refund","amount":%d,"currency":"usd","status":%q,"payment_intent":%q,"charge":"ch_%s","created":%d,"metadata":{%s}}`,
		id, amount, status, pi, pi, time.Now().Unix(), strings.Join(mds, ","))
}

func (f *refundFlow) call(method, path string, body any) flowResp {
	f.t.Helper()
	return flowDo(f.t, f.ts, method, path, flowReq{bearer: f.key, body: body})
}

// deliver signs a Stripe event with the endpoint's secret and posts it to
// the tenant's webhook endpoint, returning the status.
func (f *refundFlow) deliver(event map[string]any) flowResp {
	f.t.Helper()
	payload, sig := signedStripeEvent(f.t, refundFlowSecret, event)
	return flowDo(f.t, f.ts, "POST", f.endpoint, flowReq{header: map[string]string{"Stripe-Signature": sig}, body: payload})
}

// paidInvoice creates and finalizes a one-line invoice of cents, adds tax
// to it when tax > 0, and pays it by card through payment_intent.succeeded.
//
// The tax is written to the row directly, as finalize under Stripe Tax
// would leave it (the tax, the committed transaction and the provider):
// asking the stub for a real tax calculation tests Stripe Tax, not refunds.
func (f *refundFlow) paidInvoice(suffix string, cents, tax int64) (invID, pi string) {
	f.t.Helper()
	invID = f.finalizedInvoice(suffix, cents, nil)
	total := cents
	if tax > 0 {
		total += tax
		f.exec(`UPDATE invoices SET tax_amount_cents = $2, total_amount_cents = $3, amount_due_cents = $3,
			tax_transaction_id = $4, tax_provider = 'stripe_tax' WHERE id = $1`, invID, tax, total, "tax_txn_"+suffix)
	}
	pi = "pi_" + suffix
	f.pay(invID, pi, total).want(f.t, 200, "payment_intent.succeeded "+pi)
	inv := f.invoice(invID)
	if flowStr(inv, "status") != "paid" || flowInt(inv, "amount_paid_cents") != total {
		f.t.Fatalf("invoice %s after its payment: status %s, paid %d, want paid %d", invID, flowStr(inv, "status"), flowInt(inv, "amount_paid_cents"), total)
	}
	return invID, pi
}

func (f *refundFlow) finalizedInvoice(suffix string, cents int64, line map[string]any) string {
	f.t.Helper()
	if line == nil {
		line = map[string]any{"description": "Service " + suffix, "quantity": 1, "unit_amount_cents": cents}
	}
	invID := flowStr(f.call("POST", "/v1/invoices", map[string]any{
		"customer_id": f.customerID, "line_items": []map[string]any{line},
	}).want(f.t, 201, "create invoice").body, "id")
	f.call("POST", "/v1/invoices/"+invID+"/finalize", nil).want(f.t, 200, "finalize")
	return invID
}

func (f *refundFlow) pay(invID, pi string, amount int64) flowResp {
	f.t.Helper()
	return f.deliver(map[string]any{
		"id": "evt_paid_" + pi, "object": "event", "type": "payment_intent.succeeded",
		"created": time.Now().Unix(), "livemode": false,
		"data": map[string]any{"object": map[string]any{
			"id": pi, "object": "payment_intent", "status": "succeeded", "amount": amount, "currency": "usd",
			"metadata": map[string]any{"hardcount_tenant_id": f.tenantID, "hardcount_invoice_id": invID, "hardcount_customer_id": f.customerID},
		}},
	})
}

// useStripeTax switches the tenant to Stripe Tax, so a credit note's tax
// reversal goes to the stub. Called after the test's invoices are finalized.
func (f *refundFlow) useStripeTax() {
	f.t.Helper()
	f.call("PUT", "/v1/settings", map[string]any{"tax_provider": "stripe_tax"}).want(f.t, 200, "use Stripe Tax")
}

// refundEvent is a refund webhook: data.object is a Stripe Refund, whose
// metadata is its own (empty for a refund made in the Stripe dashboard).
type refundSpec struct {
	id, pi, status, reason string
	amount                 int64
	created                time.Time
	md                     map[string]any
}

func refundEvent(eventID, eventType string, r refundSpec, at time.Time) map[string]any {
	md := r.md
	if md == nil {
		md = map[string]any{}
	}
	obj := map[string]any{
		"id": r.id, "object": "refund", "amount": r.amount, "currency": "usd", "status": r.status,
		"charge": "ch_" + r.pi, "payment_intent": r.pi, "created": r.created.Unix(), "metadata": md,
		"balance_transaction": "txn_" + r.id,
	}
	if r.reason != "" {
		obj["reason"] = r.reason
	} else {
		obj["reason"] = nil
	}
	if r.status == "failed" {
		obj["failure_reason"] = "expired_or_canceled_card"
	}
	return map[string]any{
		"id": eventID, "object": "event", "type": eventType, "created": at.Unix(), "livemode": false,
		"data": map[string]any{"object": obj},
	}
}

func (f *refundFlow) invoice(invID string) map[string]any {
	f.t.Helper()
	return flowObj(f.call("GET", "/v1/invoices/"+invID, nil).want(f.t, 200, "get invoice").body, "invoice")
}

func (f *refundFlow) notes(invID string) []map[string]any {
	f.t.Helper()
	return flowList(f.call("GET", "/v1/credit-notes?invoice_id="+invID, nil).want(f.t, 200, "list credit notes").body, "data")
}

func (f *refundFlow) refundRows(invID string) []map[string]any {
	f.t.Helper()
	return flowList(f.call("GET", "/v1/invoices/"+invID+"/stripe-refunds", nil).want(f.t, 200, "list stripe refunds").body, "data")
}

func (f *refundFlow) events(refundID string) int {
	f.t.Helper()
	return flowCount(f.t, f.db, `SELECT count(*) FROM webhook_outbox WHERE event_type = 'payment.refunded_in_stripe'
		AND payload->'refund'->>'stripe_refund_id' = $1`, refundID)
}

func (f *refundFlow) refundCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.madeRefunds
}

func (f *refundFlow) exec(query string, args ...any) {
	f.t.Helper()
	tx, err := f.db.BeginTx(f.t.Context(), postgres.TxBypass, "")
	if err != nil {
		f.t.Fatalf("begin tx: %v", err)
	}
	defer postgres.Rollback(tx)
	if _, err := tx.ExecContext(f.t.Context(), query, args...); err != nil {
		f.t.Fatalf("exec (%s): %v", oneLineSQL(query), err)
	}
	if err := tx.Commit(); err != nil {
		f.t.Fatalf("commit: %v", err)
	}
}

func oneLineSQL(q string) string { return strings.Join(strings.Fields(q), " ") }

func (f *refundFlow) timeline(invID string) []map[string]any {
	f.t.Helper()
	tl := f.call("GET", "/v1/invoices/"+invID+"/payment-timeline", nil).want(f.t, 200, "payment timeline")
	if deg, _ := tl.body["degraded"].([]any); len(deg) != 0 {
		f.t.Errorf("timeline degraded lanes: %v", deg)
	}
	return flowList(tl.body, "events")
}

// TestE2E_StripeRefund_RecordedAsCreditNote: a full refund of a paid invoice
// made in the Stripe dashboard becomes one issued credit note whose card
// refund is that refund. Hardcount calls nothing at Stripe for the refund,
// reverses the tax once, queues payment.refunded_in_stripe once, writes the
// audit rows, and leaves the invoice paid with no dunning and no credit
// ledger entry. A Hardcount refund after it is refused up front.
func TestE2E_StripeRefund_RecordedAsCreditNote(t *testing.T) {
	db, ts, _, register := newRefundServer(t)
	f := newRefundFlow(t, db, ts, "Refund recorded Co")
	register(f)
	invID, pi := f.paidInvoice("rf_full", 10000, 1000)
	f.useStripeTax()
	before := f.invoice(invID)

	created := time.Now().Add(-time.Minute).Truncate(time.Second).UTC()
	res := f.deliver(refundEvent("evt_rf_full", "refund.created", refundSpec{
		id: "re_full", pi: pi, status: "succeeded", reason: "requested_by_customer", amount: 11000, created: created,
	}, created)).want(t, 200, "refund.created")
	if flowStr(res.body, "status") != "processed" {
		t.Fatalf("refund.created answered %s, want processed (not skipped for missing metadata)", res.raw)
	}

	notes := f.notes(invID)
	if len(notes) != 1 {
		t.Fatalf("credit notes: %d, want 1", len(notes))
	}
	cn := notes[0]
	cnID := flowStr(cn, "id")
	for field, want := range map[string]any{
		"status": "issued", "stripe_refund_id": "re_full", "refund_status": "succeeded",
		"reason": "Refunded in Stripe: requested by customer",
	} {
		if got := flowStr(cn, field); got != want {
			t.Errorf("credit note %s = %q, want %q", field, got, want)
		}
	}
	if cn["recorded_from_stripe"] != true {
		t.Errorf("recorded_from_stripe = %v, want true", cn["recorded_from_stripe"])
	}
	for field, want := range map[string]int64{
		"total_cents": 11000, "refund_amount_cents": 11000, "credit_amount_cents": 0,
		"out_of_band_amount_cents": 0, "tax_amount_cents": 1000, "subtotal_cents": 10000,
	} {
		if got := flowInt(cn, field); got != want {
			t.Errorf("credit note %s = %d, want %d", field, got, want)
		}
	}

	rows := f.refundRows(invID)
	if len(rows) != 1 || flowStr(rows[0], "outcome") != "credit_note" || flowStr(rows[0], "credit_note_id") != cnID ||
		flowStr(rows[0], "status") != "succeeded" || flowInt(rows[0], "amount_cents") != 11000 || flowStr(rows[0], "currency") != "USD" {
		t.Errorf("refund rows = %v, want one credit_note row naming %s", rows, cnID)
	}
	if n := f.events("re_full"); n != 1 {
		t.Errorf("payment.refunded_in_stripe events: %d, want 1", n)
	}
	if n := flowCount(t, db, `SELECT count(*) FROM webhook_outbox WHERE event_type = 'payment.refunded_in_stripe'
		AND payload->'invoice'->>'id' = $1 AND payload->'refund'->>'outcome' = 'credit_note'
		AND payload->'refund'->>'credit_note_id' = $2`, invID, cnID); n != 1 {
		t.Errorf("event payload with the invoice and the recorded refund: %d, want 1", n)
	}

	// No refund call at Stripe; one tax reversal, for this note's gross.
	if n := f.refundCalls(); n != 0 {
		t.Errorf("refund calls at Stripe: %d, want 0: the refund is already made", n)
	}
	if n := f.stub.count("POST /v1/tax/transactions/create_reversal"); n != 1 {
		t.Fatalf("tax reversals: %d, want 1", n)
	}
	form := f.stub.form("POST /v1/tax/transactions/create_reversal")
	if form.Get("original_transaction") != "tax_txn_rf_full" || form.Get("reference") != cnID || form.Get("flat_amount") != "-11000" {
		t.Errorf("tax reversal form = %v, want original tax_txn_rf_full, reference %s, flat_amount -11000", form, cnID)
	}
	if got := flowStr(f.call("GET", "/v1/credit-notes/"+cnID, nil).want(t, 200, "get credit note").body, "tax_transaction_id"); got != "tax_rev_"+cnID {
		t.Errorf("credit note tax_transaction_id = %q, want the reversal", got)
	}

	// The invoice is untouched: paid, nothing due, no notice; no dunning, no
	// credit ledger entry.
	after := f.invoice(invID)
	for _, k := range []string{"status", "payment_status", "amount_paid_cents", "amount_due_cents", "total_amount_cents", "paid_at"} {
		if fmt.Sprint(after[k]) != fmt.Sprint(before[k]) {
			t.Errorf("invoice %s moved from %v to %v", k, before[k], after[k])
		}
	}
	if after["attention"] != nil {
		t.Errorf("attention = %v, want none: the books are right", after["attention"])
	}
	if n := flowCount(t, db, `SELECT count(*) FROM invoice_dunning_runs WHERE invoice_id = $1`, invID); n != 0 {
		t.Errorf("dunning runs: %d, want 0", n)
	}
	if n := flowCount(t, db, `SELECT count(*) FROM customer_credit_ledger WHERE customer_id = $1`, f.customerID); n != 0 {
		t.Errorf("credit ledger entries: %d, want 0", n)
	}

	// Audit: the note's create and issued rows say where it came from, and
	// the invoice carries the refund.
	if n := flowCount(t, db, `SELECT count(*) FROM audit_log WHERE resource_id = $1 AND action IN ('create', 'credit_note.issued')
		AND metadata->>'origin' = 'stripe_refund' AND metadata->>'stripe_refund_id' = 're_full'`, cnID); n != 2 {
		t.Errorf("credit note audit rows from the refund: %d, want 2", n)
	}
	if n := flowCount(t, db, `SELECT count(*) FROM audit_log WHERE resource_type = 'invoice' AND resource_id = $1
		AND metadata->>'action' = 'refunded_in_stripe' AND metadata->>'outcome' = 'credit_note'`, invID); n != 1 {
		t.Errorf("invoice audit rows for the refund: %d, want 1", n)
	}

	// The timeline tells it once, as the note's row.
	var titles []string
	for _, e := range f.timeline(invID) {
		switch flowStr(e, "source") {
		case "credit_note":
			titles = append(titles, flowStr(e, "description"))
		case "stripe_refund":
			t.Errorf("timeline has a stripe_refund row %v for a refund its credit note records", e)
		}
	}
	if len(titles) != 1 || titles[0] != "Refunded in Stripe" {
		t.Errorf("timeline credit-note rows = %v, want one \"Refunded in Stripe\"", titles)
	}

	// The refund cap counts the recorded note: a Hardcount refund is refused
	// before Stripe is asked.
	if r := f.call("POST", "/v1/invoices/"+invID+"/refund", map[string]any{"reason": "requested_by_customer"}); r.status/100 == 2 {
		t.Errorf("Hardcount refund after the recorded one answered %d: %s", r.status, r.raw)
	}
	if n := f.refundCalls(); n != 0 {
		t.Errorf("refund calls at Stripe after the refused refund: %d, want 0", n)
	}
}

// TestE2E_StripeRefund_RedeliveryAndConcurrentDeliveries: the same event
// twice and a later refund.updated for the same refund, all at once from
// three goroutines (two replicas and Stripe's redelivery), record one row,
// one credit note and one event. Then the crash point after the commit and
// before the dedup row: the event's dedup row is gone, the event is
// delivered again, and nothing changes.
func TestE2E_StripeRefund_RedeliveryAndConcurrentDeliveries(t *testing.T) {
	db, ts, _, register := newRefundServer(t)
	f := newRefundFlow(t, db, ts, "Refund collision Co")
	register(f)
	invID, pi := f.paidInvoice("rf_race", 5000, 0)

	created := time.Now().Add(-time.Minute).Truncate(time.Second).UTC()
	spec := refundSpec{id: "re_race", pi: pi, status: "pending", amount: 5000, created: created}
	createdEv := refundEvent("evt_rf_race_created", "refund.created", spec, created)
	spec.status = "succeeded"
	updatedEv := refundEvent("evt_rf_race_updated", "refund.updated", spec, created.Add(time.Second))

	start := make(chan struct{})
	var wg sync.WaitGroup
	statuses := make([]int, 3)
	for i, ev := range []map[string]any{createdEv, createdEv, updatedEv} {
		payload, sig := signedStripeEvent(t, refundFlowSecret, ev)
		wg.Add(1)
		go func(i int, payload []byte, sig string) {
			defer wg.Done()
			<-start
			statuses[i] = flowDo(t, ts, "POST", f.endpoint, flowReq{header: map[string]string{"Stripe-Signature": sig}, body: payload}).status
		}(i, payload, sig)
	}
	close(start)
	wg.Wait()
	for i, s := range statuses {
		if s != 200 {
			t.Errorf("delivery %d answered %d, want 200: concurrent events for one refund serialize on the invoice", i, s)
		}
	}
	notes := f.notes(invID)
	rows := f.refundRows(invID)
	if len(notes) != 1 || len(rows) != 1 || f.events("re_race") != 1 {
		t.Fatalf("after concurrent deliveries: %d notes, %d rows, %d events; want 1 each", len(notes), len(rows), f.events("re_race"))
	}
	if got := flowStr(notes[0], "refund_status"); got != "succeeded" {
		t.Errorf("note refund_status = %q, want succeeded (the newer event)", got)
	}
	if got := flowStr(rows[0], "status"); got != "succeeded" {
		t.Errorf("row status = %q, want succeeded (the newer event)", got)
	}

	// Crash after the commit, before the dedup row: Stripe delivers the event
	// again and nothing changes.
	f.exec(`DELETE FROM stripe_webhook_events WHERE stripe_event_id = 'evt_rf_race_created'`)
	f.deliver(createdEv).want(t, 200, "redelivery after a lost dedup row")
	if len(f.notes(invID)) != 1 || len(f.refundRows(invID)) != 1 || f.events("re_race") != 1 {
		t.Errorf("the redelivery changed something: %d notes, %d rows, %d events", len(f.notes(invID)), len(f.refundRows(invID)), f.events("re_race"))
	}
	if got := flowStr(f.notes(invID)[0], "refund_status"); got != "succeeded" {
		t.Errorf("an older event regressed the note to %q", got)
	}
	if n := flowCount(t, db, `SELECT count(*) FROM stripe_webhook_events WHERE stripe_event_id = 'evt_rf_race_created'`); n != 1 {
		t.Errorf("dedup rows for the redelivered event: %d, want 1", n)
	}
	if n := f.refundCalls(); n != 0 {
		t.Errorf("refund calls at Stripe: %d, want 0", n)
	}
}

// TestE2E_StripeRefund_HardcountOwnRefundBeforeItsNoteIsSaved: Hardcount's
// own refund webhook arrives while Hardcount's refund call is still at
// Stripe, before the note's refund id is saved (the concurrent-resolver
// fake: the stub delivers it from inside the refund call). Its
// hardcount_cn_id metadata marks it Hardcount's, so it is never recorded as
// a refund made in Stripe: the delivery asks for redelivery, and the
// redelivery after the id is saved lands on the note. One note, one refund.
func TestE2E_StripeRefund_HardcountOwnRefundBeforeItsNoteIsSaved(t *testing.T) {
	db, ts, _, register := newRefundServer(t)
	f := newRefundFlow(t, db, ts, "Refund own Co")
	register(f)
	invID, pi := f.paidInvoice("rf_own", 5000, 0)

	var inner flowResp
	var innerEvent map[string]any
	f.duringRefund = func(refundID string, form url.Values) {
		innerEvent = refundEvent("evt_rf_own", "refund.created", refundSpec{
			id: refundID, pi: pi, status: "succeeded", amount: 2000, created: time.Now(),
			md: map[string]any{"hardcount_cn_id": form.Get("metadata[hardcount_cn_id]")},
		}, time.Now())
		inner = f.deliver(innerEvent)
	}
	f.call("POST", "/v1/invoices/"+invID+"/refund", map[string]any{"amount_cents": 2000, "reason": "requested_by_customer"}).want(t, 200, "Hardcount refund")
	f.duringRefund = nil

	if inner.status != 500 {
		t.Errorf("Hardcount's own refund webhook before its note's id was saved answered %d, want 500 (redeliver)", inner.status)
	}
	if rows := f.refundRows(invID); len(rows) != 0 {
		t.Errorf("refund rows: %v, want none: a refund with hardcount_cn_id is never a refund made in Stripe", rows)
	}
	notes := f.notes(invID)
	if len(notes) != 1 || notes[0]["recorded_from_stripe"] != false {
		t.Fatalf("credit notes: %v, want the one Hardcount note", notes)
	}
	refundID := flowStr(notes[0], "stripe_refund_id")
	if refundID == "" || flowInt(notes[0], "refund_amount_cents") != 2000 {
		t.Fatalf("the Hardcount note holds refund %q of %d, want the stub's refund of 2000", refundID, flowInt(notes[0], "refund_amount_cents"))
	}

	f.deliver(innerEvent).want(t, 200, "redelivery once the note holds the id")
	if rows := f.refundRows(invID); len(rows) != 0 {
		t.Errorf("refund rows after the redelivery: %v, want none", rows)
	}
	if notes := f.notes(invID); len(notes) != 1 || flowStr(notes[0], "refund_status") != "succeeded" {
		t.Errorf("credit notes after the redelivery: %v, want the one note, succeeded", notes)
	}
	if n := f.refundCalls(); n != 1 {
		t.Errorf("refund calls at Stripe: %d, want 1", n)
	}
	if n := f.events(refundID); n != 0 {
		t.Errorf("payment.refunded_in_stripe events for Hardcount's own refund: %d, want 0", n)
	}
}

// TestE2E_StripeRefund_PartialRefunds: two partial refunds of one charge are
// two notes, each checked against the caps as they then stand. The one that
// exhausts the invoice takes the tax residual, so the notes' tax adds up to
// the invoice's. A third refund beyond the payment is flagged, not recorded.
func TestE2E_StripeRefund_PartialRefunds(t *testing.T) {
	db, ts, _, register := newRefundServer(t)
	f := newRefundFlow(t, db, ts, "Refund partial Co")
	register(f)
	invID, pi := f.paidInvoice("rf_part", 10000, 1000)
	f.useStripeTax()
	created := time.Now().Add(-time.Minute).Truncate(time.Second).UTC()

	f.deliver(refundEvent("evt_rf_part_a", "refund.created", refundSpec{id: "re_part_a", pi: pi, status: "succeeded", amount: 3000, created: created}, created)).want(t, 200, "first partial")
	f.deliver(refundEvent("evt_rf_part_b", "refund.created", refundSpec{id: "re_part_b", pi: pi, status: "succeeded", amount: 8000, created: created.Add(time.Minute)}, created.Add(time.Minute))).want(t, 200, "second partial")

	taxByRefund := map[string]int64{}
	var total int64
	for _, cn := range f.notes(invID) {
		taxByRefund[flowStr(cn, "stripe_refund_id")] = flowInt(cn, "tax_amount_cents")
		total += flowInt(cn, "total_cents")
	}
	// 1000 × 3000 / 11000 = 272.7, truncated; the exhausting note takes the rest.
	if len(taxByRefund) != 2 || taxByRefund["re_part_a"] != 272 || taxByRefund["re_part_b"] != 728 || total != 11000 {
		t.Errorf("notes' tax by refund = %v, total %d; want re_part_a 272, re_part_b 728, total 11000", taxByRefund, total)
	}
	if n := f.stub.count("POST /v1/tax/transactions/create_reversal"); n != 2 {
		t.Errorf("tax reversals: %d, want 2", n)
	}

	f.deliver(refundEvent("evt_rf_part_c", "refund.created", refundSpec{id: "re_part_c", pi: pi, status: "succeeded", amount: 100, created: created.Add(2 * time.Minute)}, created.Add(2*time.Minute))).want(t, 200, "refund beyond the payment")
	if n := len(f.notes(invID)); n != 2 {
		t.Errorf("credit notes after a refund beyond the payment: %d, want 2", n)
	}
	var flagged map[string]any
	for _, r := range f.refundRows(invID) {
		if flowStr(r, "stripe_refund_id") == "re_part_c" {
			flagged = r
		}
	}
	if flowStr(flagged, "outcome") != "needs_review" || flowStr(flagged, "review_reason") != "exceeds_card_refundable" {
		t.Errorf("refund beyond the payment = %v, want needs_review exceeds_card_refundable", flagged)
	}
	if att := flowObj(f.invoice(invID), "attention"); flowStr(att, "reason") != "refunded_in_stripe" || flowStr(att, "severity") != "warning" {
		t.Errorf("attention = %v, want warning refunded_in_stripe", att)
	}
	if n := f.refundCalls(); n != 0 {
		t.Errorf("refund calls at Stripe: %d, want 0", n)
	}
}

// TestE2E_StripeRefund_OverCreditedIsFlaggedAndDismissed: after a $50 note
// to the credit balance, a full $100 card refund in Stripe passes the card
// cap but not the creditable cap. It is flagged, never a partial note; the
// operator dismisses it with a note, which clears the notice.
func TestE2E_StripeRefund_OverCreditedIsFlaggedAndDismissed(t *testing.T) {
	db, ts, _, register := newRefundServer(t)
	f := newRefundFlow(t, db, ts, "Refund over-credit Co")
	register(f)
	invID, pi := f.paidInvoice("rf_over", 10000, 0)
	f.call("POST", "/v1/credit-notes", map[string]any{
		"invoice_id": invID, "reason": "goodwill", "auto_issue": true, "credit_amount_cents": 5000,
		"lines": []map[string]any{{"description": "Goodwill", "quantity": 1, "unit_amount_cents": 5000}},
	}).want(t, 201, "credit-balance note")

	created := time.Now().Add(-time.Minute).Truncate(time.Second).UTC()
	f.deliver(refundEvent("evt_rf_over", "refund.created", refundSpec{id: "re_over", pi: pi, status: "succeeded", amount: 10000, created: created}, created)).want(t, 200, "refund.created")
	if n := len(f.notes(invID)); n != 1 {
		t.Errorf("credit notes: %d, want 1 (the credit-balance note only)", n)
	}
	rows := f.refundRows(invID)
	if len(rows) != 1 || flowStr(rows[0], "outcome") != "needs_review" || flowStr(rows[0], "review_reason") != "exceeds_creditable" {
		t.Fatalf("refund rows = %v, want needs_review exceeds_creditable", rows)
	}
	att := flowObj(f.invoice(invID), "attention")
	if flowStr(att, "reason") != "refunded_in_stripe" || flowStr(att, "code") != "payment.refunded_in_stripe.exceeds_creditable" ||
		!strings.Contains(flowStr(att, "message"), "100.00") {
		t.Errorf("attention = %v, want the refunded_in_stripe notice naming the amount", att)
	}
	if n := f.events("re_over"); n != 1 {
		t.Errorf("payment.refunded_in_stripe events: %d, want 1 (sent when flagged)", n)
	}

	d := f.call("POST", "/v1/invoices/"+invID+"/stripe-refunds/re_over/dismiss", map[string]any{"note": "took the goodwill credit back by hand"}).want(t, 200, "dismiss")
	if flowStr(d.body, "outcome") != "dismissed" || flowStr(d.body, "dismissed_note") != "took the goodwill credit back by hand" || flowStr(d.body, "dismissed_at") == "" {
		t.Errorf("dismissed refund = %s", d.raw)
	}
	if att := f.invoice(invID)["attention"]; att != nil {
		t.Errorf("attention after the dismissal = %v, want none", att)
	}
	if r := f.call("POST", "/v1/invoices/"+invID+"/stripe-refunds/re_over/dismiss", map[string]any{"note": "again"}); r.status != 409 {
		t.Errorf("a second dismissal answered %d, want 409", r.status)
	}
	if r := f.call("POST", "/v1/invoices/"+invID+"/stripe-refunds/re_none/dismiss", map[string]any{"note": "x"}); r.status != 404 {
		t.Errorf("dismissing an unknown refund answered %d, want 404", r.status)
	}
	if n := flowCount(t, db, `SELECT count(*) FROM audit_log WHERE resource_id = $1 AND metadata->>'action' = 'stripe_refund_dismissed'`, invID); n != 1 {
		t.Errorf("dismissal audit rows: %d, want 1", n)
	}
	var rowsSeen []string
	for _, e := range f.timeline(invID) {
		if flowStr(e, "source") == "stripe_refund" {
			rowsSeen = append(rowsSeen, flowStr(e, "description")+" / "+flowStr(e, "status"))
		}
	}
	if len(rowsSeen) != 1 || rowsSeen[0] != "Refunded in Stripe / neutral" {
		t.Errorf("timeline refund rows = %v, want one neutral \"Refunded in Stripe\"", rowsSeen)
	}
	if n := f.refundCalls(); n != 0 {
		t.Errorf("refund calls at Stripe: %d, want 0", n)
	}
}

// TestE2E_StripeRefund_OtherPayment: a refund of the second charge on a paid
// invoice (the duplicate-charge anomaly's PaymentIntent) is recorded as a
// refund of another payment. No credit note: that money never paid the
// invoice, which stays as it was.
func TestE2E_StripeRefund_OtherPayment(t *testing.T) {
	db, ts, _, register := newRefundServer(t)
	f := newRefundFlow(t, db, ts, "Refund other payment Co")
	register(f)
	invID, _ := f.paidInvoice("rf_other", 5000, 0)
	f.pay(invID, "pi_rf_other_dup", 5000).want(t, 200, "duplicate charge")
	before := f.invoice(invID)

	created := time.Now().Add(-time.Minute).Truncate(time.Second).UTC()
	f.deliver(refundEvent("evt_rf_other", "refund.created", refundSpec{id: "re_other", pi: "pi_rf_other_dup", status: "succeeded", amount: 5000, created: created}, created)).want(t, 200, "refund of the duplicate")
	rows := f.refundRows(invID)
	if len(rows) != 1 || flowStr(rows[0], "outcome") != "other_payment" || flowStr(rows[0], "review_reason") != "" {
		t.Fatalf("refund rows = %v, want one other_payment", rows)
	}
	if n := len(f.notes(invID)); n != 0 {
		t.Errorf("credit notes: %d, want 0", n)
	}
	after := f.invoice(invID)
	for _, k := range []string{"status", "payment_status", "amount_paid_cents", "stripe_payment_intent_id"} {
		if fmt.Sprint(after[k]) != fmt.Sprint(before[k]) {
			t.Errorf("invoice %s moved from %v to %v", k, before[k], after[k])
		}
	}
	found := false
	for _, e := range f.timeline(invID) {
		if flowStr(e, "source") == "stripe_refund" && flowStr(e, "description") == "Extra payment refunded in Stripe" {
			found = true
		}
	}
	if !found {
		t.Error("timeline has no \"Extra payment refunded in Stripe\" row")
	}
}

// TestE2E_StripeRefund_Ordering: refund.failed before refund.created records
// a refund that moved no money and no note, and the older created event
// changes nothing. A pending refund then failed: the note exists, its refund
// status goes to failed and it shows in the refunds needing attention; the
// note is never undone.
func TestE2E_StripeRefund_Ordering(t *testing.T) {
	db, ts, _, register := newRefundServer(t)
	f := newRefundFlow(t, db, ts, "Refund ordering Co")
	register(f)
	invX, piX := f.paidInvoice("rf_ord_x", 5000, 0)
	invY, piY := f.paidInvoice("rf_ord_y", 5000, 0)
	created := time.Now().Add(-time.Hour).Truncate(time.Second).UTC()

	failed := refundSpec{id: "re_ord_x", pi: piX, status: "failed", amount: 5000, created: created}
	f.deliver(refundEvent("evt_ord_x_failed", "refund.failed", failed, created.Add(time.Minute))).want(t, 200, "refund.failed first")
	failed.status = "pending"
	f.deliver(refundEvent("evt_ord_x_created", "refund.created", failed, created)).want(t, 200, "older refund.created")
	rows := f.refundRows(invX)
	if len(rows) != 1 || flowStr(rows[0], "outcome") != "not_refunded" || flowStr(rows[0], "status") != "failed" {
		t.Errorf("refund rows = %v, want one not_refunded, failed", rows)
	}
	if n := len(f.notes(invX)); n != 0 {
		t.Errorf("credit notes: %d, want 0", n)
	}

	pending := refundSpec{id: "re_ord_y", pi: piY, status: "pending", amount: 5000, created: created}
	f.deliver(refundEvent("evt_ord_y_created", "refund.created", pending, created)).want(t, 200, "pending refund")
	notes := f.notes(invY)
	if len(notes) != 1 || flowStr(notes[0], "refund_status") != "pending" {
		t.Fatalf("credit notes = %v, want one with a pending refund", notes)
	}
	cnID := flowStr(notes[0], "id")
	pending.status = "failed"
	f.deliver(refundEvent("evt_ord_y_failed", "refund.failed", pending, created.Add(time.Hour))).want(t, 200, "refund.failed later")
	notes = f.notes(invY)
	if len(notes) != 1 || flowStr(notes[0], "refund_status") != "failed" || flowStr(notes[0], "status") != "issued" {
		t.Errorf("credit notes = %v, want the issued note with a failed refund", notes)
	}
	attention := flowList(f.call("GET", "/v1/credit-notes?refund_status=needs_attention", nil).want(t, 200, "refunds needing attention").body, "data")
	listed := false
	for _, cn := range attention {
		listed = listed || flowStr(cn, "id") == cnID
	}
	if !listed {
		t.Errorf("the note whose refund failed is not in the refunds needing attention")
	}
	if rows := f.refundRows(invY); len(rows) != 1 || flowStr(rows[0], "status") != "failed" || flowStr(rows[0], "outcome") != "credit_note" {
		t.Errorf("refund rows = %v, want the credit_note row, status failed", rows)
	}
	if n := f.events("re_ord_y"); n != 1 {
		t.Errorf("events for the refund that later failed: %d, want 1 (a later change sends nothing)", n)
	}
}

// TestE2E_StripeRefund_InvoiceNotPaidYet: a young refund of a payment on an
// invoice Hardcount has not marked paid asks Stripe to deliver it again; an
// old one is flagged. A young refund of a payment no invoice holds asks for
// redelivery too; an old one is acknowledged and recorded nowhere.
func TestE2E_StripeRefund_InvoiceNotPaidYet(t *testing.T) {
	db, ts, _, register := newRefundServer(t)
	f := newRefundFlow(t, db, ts, "Refund not paid Co")
	register(f)
	invID := f.finalizedInvoice("rf_unpaid", 5000, nil)
	const pi = "pi_rf_unpaid"
	f.deliver(map[string]any{
		"id": "evt_failed_" + pi, "object": "event", "type": "payment_intent.payment_failed",
		"created": time.Now().Unix(), "livemode": false,
		"data": map[string]any{"object": map[string]any{
			"id": pi, "object": "payment_intent", "status": "requires_payment_method", "amount": 5000, "currency": "usd",
			"last_payment_error": map[string]any{"message": "Your card was declined.", "type": "card_error", "code": "card_declined", "decline_code": "generic_decline"},
			"metadata":           map[string]any{"hardcount_tenant_id": f.tenantID, "hardcount_invoice_id": invID, "hardcount_customer_id": f.customerID},
		}},
	}).want(t, 200, "payment_intent.payment_failed")
	if got := flowStr(f.invoice(invID), "stripe_payment_intent_id"); got != pi {
		t.Fatalf("invoice holds payment %q, want %s", got, pi)
	}

	now := time.Now().Truncate(time.Second).UTC()
	young := refundEvent("evt_rf_unpaid_young", "refund.created", refundSpec{id: "re_unpaid_young", pi: pi, status: "succeeded", amount: 5000, created: now}, now)
	f.deliver(young).want(t, 500, "young refund on an unpaid invoice")
	if n := flowCount(t, db, `SELECT count(*) FROM stripe_webhook_events WHERE stripe_event_id = 'evt_rf_unpaid_young'`); n != 0 {
		t.Errorf("dedup rows for an event Stripe must redeliver: %d, want 0", n)
	}
	old := now.Add(-2 * time.Hour)
	f.deliver(refundEvent("evt_rf_unpaid_old", "refund.created", refundSpec{id: "re_unpaid_old", pi: pi, status: "succeeded", amount: 5000, created: old}, old)).want(t, 200, "old refund on an unpaid invoice")
	rows := f.refundRows(invID)
	if len(rows) != 1 || flowStr(rows[0], "stripe_refund_id") != "re_unpaid_old" || flowStr(rows[0], "outcome") != "needs_review" ||
		flowStr(rows[0], "review_reason") != "invoice_not_paid" {
		t.Errorf("refund rows = %v, want one needs_review invoice_not_paid", rows)
	}
	if n := len(f.notes(invID)); n != 0 {
		t.Errorf("credit notes: %d, want 0", n)
	}

	f.deliver(refundEvent("evt_rf_foreign_young", "refund.created", refundSpec{id: "re_foreign_young", pi: "pi_not_hardcount", status: "succeeded", amount: 700, created: now}, now)).want(t, 500, "young refund on a payment no invoice holds")
	f.deliver(refundEvent("evt_rf_foreign_old", "refund.created", refundSpec{id: "re_foreign_old", pi: "pi_not_hardcount", status: "succeeded", amount: 700, created: old}, old)).want(t, 200, "old refund on a payment no invoice holds")
	if n := flowCount(t, db, `SELECT count(*) FROM invoice_stripe_refunds WHERE stripe_refund_id LIKE 're_foreign_%'`); n != 0 {
		t.Errorf("rows for refunds of a payment no invoice holds: %d, want 0", n)
	}
}

// TestE2E_StripeRefund_SeenLateThenLinked: a refund Hardcount first sees
// more than a day after it was made (only its refund.updated arrived) is
// flagged: the operator may already have recorded it. The operator then
// records it with a credit note that names it: no Stripe refund call, the
// row marked recorded, the notice gone. Linking it twice, linking a refund
// Hardcount made, or one of another payment is refused.
func TestE2E_StripeRefund_SeenLateThenLinked(t *testing.T) {
	db, ts, _, register := newRefundServer(t)
	f := newRefundFlow(t, db, ts, "Refund late Co")
	register(f)
	invID, pi := f.paidInvoice("rf_late", 5000, 0)
	made := time.Now().Add(-72 * time.Hour).Truncate(time.Second).UTC()
	f.deliver(refundEvent("evt_rf_late_updated", "refund.updated", refundSpec{id: "re_late", pi: pi, status: "succeeded", amount: 4000, created: made}, made.Add(48*time.Hour))).want(t, 200, "late refund.updated")
	rows := f.refundRows(invID)
	if len(rows) != 1 || flowStr(rows[0], "outcome") != "needs_review" || flowStr(rows[0], "review_reason") != "seen_late" {
		t.Fatalf("refund rows = %v, want needs_review seen_late", rows)
	}
	if n := len(f.notes(invID)); n != 0 {
		t.Errorf("credit notes: %d, want 0", n)
	}
	if att := flowObj(f.invoice(invID), "attention"); flowStr(att, "reason") != "refunded_in_stripe" {
		t.Errorf("attention = %v, want refunded_in_stripe", att)
	}

	f.mu.Lock()
	f.stripeRefunds["re_late"] = refundJSON("re_late", pi, 4000, "succeeded", nil)
	f.stripeRefunds["re_hc_made"] = refundJSON("re_hc_made", pi, 1000, "succeeded", map[string]string{"hardcount_cn_id": "hc_cn_elsewhere"})
	f.stripeRefunds["re_elsewhere"] = refundJSON("re_elsewhere", "pi_another", 1000, "succeeded", nil)
	f.mu.Unlock()
	link := func(refundID string, amount int64) flowResp {
		return f.call("POST", "/v1/credit-notes", map[string]any{
			"invoice_id": invID, "reason": "Refund made in Stripe", "auto_issue": true,
			"refund_amount_cents": amount, "stripe_refund_id": refundID,
			"lines": []map[string]any{{"description": "Refund made in Stripe", "quantity": 1, "unit_amount_cents": amount}},
		})
	}
	if r := f.call("POST", "/v1/credit-notes", map[string]any{
		"invoice_id": invID, "reason": "Refund made in Stripe", "refund_amount_cents": 4000, "stripe_refund_id": "re_late",
		"lines": []map[string]any{{"description": "Refund made in Stripe", "quantity": 1, "unit_amount_cents": 4000}},
	}); r.status != 422 {
		t.Errorf("linking a refund to a draft answered %d (%s), want 422: the note must be issued at once", r.status, r.raw)
	}
	cn := link("re_late", 4000).want(t, 201, "link the late refund").body
	if flowStr(cn, "status") != "issued" || flowStr(cn, "stripe_refund_id") != "re_late" || flowStr(cn, "refund_status") != "succeeded" ||
		cn["recorded_from_stripe"] != true || flowInt(cn, "refund_amount_cents") != 4000 {
		t.Errorf("linked note = %v, want issued, re_late, succeeded, recorded_from_stripe", cn)
	}
	if n := f.refundCalls(); n != 0 {
		t.Errorf("refund calls at Stripe: %d, want 0: the linked refund is already made", n)
	}
	rows = f.refundRows(invID)
	if len(rows) != 1 || flowStr(rows[0], "outcome") != "credit_note" || flowStr(rows[0], "credit_note_id") != flowStr(cn, "id") ||
		flowStr(rows[0], "review_reason") != "seen_late" {
		t.Errorf("refund rows = %v, want the row recorded by %s", rows, flowStr(cn, "id"))
	}
	if att := f.invoice(invID)["attention"]; att != nil {
		t.Errorf("attention after the link = %v, want none", att)
	}

	if r := link("re_late", 4000); r.status != 409 {
		t.Errorf("linking the same refund again answered %d (%s), want 409", r.status, r.raw)
	}
	if r := link("re_hc_made", 1000); r.status != 422 {
		t.Errorf("linking a refund Hardcount made answered %d (%s), want 422", r.status, r.raw)
	}
	if r := link("re_elsewhere", 1000); r.status != 422 {
		t.Errorf("linking a refund of another payment answered %d (%s), want 422", r.status, r.raw)
	}
	if r := link("re_late", 3000); r.status != 422 {
		t.Errorf("linking with another amount answered %d (%s), want 422", r.status, r.raw)
	}
	if n := len(f.notes(invID)); n != 1 {
		t.Errorf("credit notes: %d, want 1", n)
	}
}

// TestE2E_StripeRefund_PrepaidCommitInvoice: a refund of an invoice that
// bought prepaid credit is flagged: an ordinary note is refused there, and
// the credit stays spendable until commit relief retires it.
func TestE2E_StripeRefund_PrepaidCommitInvoice(t *testing.T) {
	db, ts, _, register := newRefundServer(t)
	f := newRefundFlow(t, db, ts, "Refund commit Co")
	register(f)
	invID := f.finalizedInvoice("rf_commit", 0, map[string]any{
		"description": "Prepaid credit", "quantity": 1, "unit_amount_cents": 5000, "commit_granted_cents": 5000,
	})
	const pi = "pi_rf_commit"
	f.pay(invID, pi, 5000).want(t, 200, "pay the commit invoice")
	if got := flowStr(f.invoice(invID), "status"); got != "paid" {
		t.Fatalf("commit invoice status %q, want paid", got)
	}
	created := time.Now().Add(-time.Minute).Truncate(time.Second).UTC()
	f.deliver(refundEvent("evt_rf_commit", "refund.created", refundSpec{id: "re_commit", pi: pi, status: "succeeded", amount: 5000, created: created}, created)).want(t, 200, "refund of the commit invoice")
	rows := f.refundRows(invID)
	if len(rows) != 1 || flowStr(rows[0], "outcome") != "needs_review" || flowStr(rows[0], "review_reason") != "prepaid_commit" {
		t.Errorf("refund rows = %v, want needs_review prepaid_commit", rows)
	}
	if n := len(f.notes(invID)); n != 0 {
		t.Errorf("credit notes: %d, want 0", n)
	}
	if att := flowObj(f.invoice(invID), "attention"); flowStr(att, "reason") != "refunded_in_stripe" || !strings.Contains(flowStr(att, "message"), "commit relief") {
		t.Errorf("attention = %v, want refunded_in_stripe naming commit relief", att)
	}
}

// TestE2E_StripeRefund_TenantAndModeIsolation: another tenant's endpoint
// receiving the refund event cannot see the invoice, so records nothing; and
// the rows are fenced by the database itself, by tenant and by mode, for a
// query with no WHERE clause.
func TestE2E_StripeRefund_TenantAndModeIsolation(t *testing.T) {
	db, ts, _, register := newRefundServer(t)
	a := newRefundFlow(t, db, ts, "Refund isolation A")
	register(a)
	b := newRefundFlow(t, db, ts, "Refund isolation B")
	register(b)
	invID, pi := a.paidInvoice("rf_iso", 5000, 0)
	old := time.Now().Add(-2 * time.Hour).Truncate(time.Second).UTC()
	ev := refundEvent("evt_rf_iso", "refund.created", refundSpec{id: "re_iso", pi: pi, status: "succeeded", amount: 5000, created: old}, old)

	b.deliver(ev).want(t, 200, "the event at the other tenant's endpoint")
	if n := flowCount(t, db, `SELECT count(*) FROM invoice_stripe_refunds WHERE stripe_refund_id = 're_iso'`); n != 0 {
		t.Fatalf("rows after the other tenant's delivery: %d, want 0", n)
	}
	a.deliver(ev).want(t, 200, "the event at the tenant's own endpoint")
	if rows := a.refundRows(invID); len(rows) != 1 {
		t.Fatalf("refund rows: %v, want 1", rows)
	}

	countAs := func(tenantID string, live bool) int {
		ctx := postgres.WithLivemode(t.Context(), live)
		tx, err := db.BeginTx(ctx, postgres.TxTenant, tenantID)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer postgres.Rollback(tx)
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM invoice_stripe_refunds`).Scan(&n); err != nil {
			t.Fatalf("count: %v", err)
		}
		return n
	}
	if n := countAs(a.tenantID, false); n != 1 {
		t.Errorf("tenant A test mode sees %d rows, want 1", n)
	}
	if n := countAs(a.tenantID, true); n != 0 {
		t.Errorf("tenant A live mode sees %d rows, want 0", n)
	}
	if n := countAs(b.tenantID, false); n != 0 {
		t.Errorf("tenant B sees %d rows, want 0", n)
	}
}
