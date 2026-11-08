package payment_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/auth"
	"github.com/gethardcount/hardcount/internal/customer"
	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/email"
	"github.com/gethardcount/hardcount/internal/errs"
	"github.com/gethardcount/hardcount/internal/invoice"
	"github.com/gethardcount/hardcount/internal/payment"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/tenantstripe"
	"github.com/gethardcount/hardcount/internal/testutil"
	"github.com/gethardcount/hardcount/internal/webhook"
)

// The restore catch-up (ADR-135) on a real database: Stripe's events since
// the backup are applied once through HandleWebhook, a payment whose invoice
// the restore lost is listed instead of dropped, and the refunds of such a
// payment do not stall the run. Stripe is a fake HTTP API behind the real
// stripe-go client, so the catch-up's own requests and decoding are real.

const catchUpDue = int64(5000)

// catchUpSession is one Checkout session at the fake Stripe.
type catchUpSession struct {
	id, status, tenant, purpose string
	created                     int64
	// completeOnExpire: the customer pays as the catch-up expires the
	// session, so Stripe refuses the expire and the session is complete.
	completeOnExpire bool
}

// catchUpStripe answers, per secret key, the requests the catch-up and the
// webhook handlers make: List Events (newest first, paged), List, Expire and
// Retrieve Checkout sessions, and Retrieve PaymentIntent. It records every
// request in order.
type catchUpStripe struct {
	mu       sync.Mutex
	refused  map[string]bool
	events   map[string][]string
	sessions map[string][]*catchUpSession
	pis      map[string]string
	calls    []string
}

func newCatchUpStripe() *catchUpStripe {
	return &catchUpStripe{refused: map[string]bool{}, events: map[string][]string{},
		sessions: map[string][]*catchUpSession{}, pis: map[string]string{}}
}

func (f *catchUpStripe) callLog() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func reply(req *http.Request, status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": []string{"application/json"}},
		Body: io.NopCloser(strings.NewReader(body)), Request: req}
}

func (f *catchUpStripe) RoundTrip(req *http.Request) (*http.Response, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := strings.TrimPrefix(req.Header.Get("Authorization"), "Bearer ")
	path := req.URL.Path
	f.calls = append(f.calls, req.Method+" "+path)
	if f.refused[key] {
		return reply(req, http.StatusUnauthorized, `{"error":{"message":"Invalid API Key provided: sk_****","type":"invalid_request_error"}}`), nil
	}
	if req.Body != nil {
		defer func() { _ = req.Body.Close() }() // the body is fully read or not needed
	}
	q := req.URL.Query()
	switch {
	case req.Method == http.MethodGet && path == "/v1/events":
		return reply(req, http.StatusOK, f.listEvents(key, q)), nil
	case req.Method == http.MethodGet && path == "/v1/checkout/sessions":
		return reply(req, http.StatusOK, f.listSessions(key, q)), nil
	case req.Method == http.MethodPost && strings.HasSuffix(path, "/expire"):
		s := f.session(key, strings.TrimSuffix(strings.TrimPrefix(path, "/v1/checkout/sessions/"), "/expire"))
		if s == nil {
			return reply(req, http.StatusNotFound, `{"error":{"type":"invalid_request_error","code":"resource_missing","message":"No such checkout.session"}}`), nil
		}
		if s.completeOnExpire {
			s.status = "complete"
			return reply(req, http.StatusBadRequest, `{"error":{"type":"invalid_request_error","message":"This Checkout Session is not in an expirable state."}}`), nil
		}
		s.status = "expired"
		return reply(req, http.StatusOK, sessionJSON(s)), nil
	case req.Method == http.MethodGet && strings.HasPrefix(path, "/v1/checkout/sessions/"):
		s := f.session(key, strings.TrimPrefix(path, "/v1/checkout/sessions/"))
		if s == nil {
			return reply(req, http.StatusNotFound, `{"error":{"type":"invalid_request_error","code":"resource_missing","message":"No such checkout.session"}}`), nil
		}
		return reply(req, http.StatusOK, sessionJSON(s)), nil
	case req.Method == http.MethodGet && strings.HasPrefix(path, "/v1/payment_intents/"):
		pi, ok := f.pis[strings.TrimPrefix(path, "/v1/payment_intents/")]
		if !ok {
			return reply(req, http.StatusNotFound, `{"error":{"type":"invalid_request_error","code":"resource_missing","message":"No such payment_intent"}}`), nil
		}
		return reply(req, http.StatusOK, pi), nil
	}
	return reply(req, http.StatusNotFound, `{"error":{"type":"invalid_request_error","message":"unrouted in the fake"}}`), nil
}

func (f *catchUpStripe) session(key, id string) *catchUpSession {
	for _, s := range f.sessions[key] {
		if s.id == id {
			return s
		}
	}
	return nil
}

func sessionJSON(s *catchUpSession) string {
	return fmt.Sprintf(`{"id":%q,"object":"checkout.session","mode":"payment","status":%q,"created":%d,"metadata":{"hardcount_tenant_id":%q,"hardcount_purpose":%q}}`,
		s.id, s.status, s.created, s.tenant, s.purpose)
}

func (f *catchUpStripe) listSessions(key string, q url.Values) string {
	gte, _ := strconv.ParseInt(q.Get("created[gte]"), 10, 64) // absent reads as 0: every session
	var data []string
	for _, s := range f.sessions[key] {
		if (q.Get("status") == "" || s.status == q.Get("status")) && s.created >= gte {
			data = append(data, sessionJSON(s))
		}
	}
	return `{"object":"list","url":"/v1/checkout/sessions","has_more":false,"data":[` + strings.Join(data, ",") + `]}`
}

// listEvents filters by created[gte] and types[], and pages with limit and
// starting_after, in the order the test stored the events.
func (f *catchUpStripe) listEvents(key string, q url.Values) string {
	gte, _ := strconv.ParseInt(q.Get("created[gte]"), 10, 64) // absent reads as 0: every event
	types := map[string]bool{}
	for k, vs := range q {
		if strings.HasPrefix(k, "types[") {
			for _, v := range vs {
				types[v] = true
			}
		}
	}
	var match []string
	for _, ev := range f.events[key] {
		var head struct {
			ID      string `json:"id"`
			Type    string `json:"type"`
			Created int64  `json:"created"`
		}
		if err := json.Unmarshal([]byte(ev), &head); err != nil {
			panic(err)
		}
		if head.Created >= gte && (len(types) == 0 || types[head.Type]) {
			match = append(match, ev)
		}
	}
	if after := q.Get("starting_after"); after != "" {
		for i, ev := range match {
			if strings.Contains(ev, `"id":"`+after+`"`) {
				match = match[i+1:]
				break
			}
		}
	}
	limit, _ := strconv.Atoi(q.Get("limit")) // absent reads as 0: Stripe's default of 10
	if limit == 0 {
		limit = 10
	}
	more := len(match) > limit
	if more {
		match = match[:limit]
	}
	return fmt.Sprintf(`{"object":"list","url":"/v1/events","has_more":%t,"data":[%s]}`, more, strings.Join(match, ","))
}

// accountCreds is the stored keys of each account, and the account list.
type accountCreds struct {
	keys map[tenantstripe.Account]string
}

func (a accountCreds) GetPlaintext(_ context.Context, tenantID string, livemode bool) (tenantstripe.PlaintextSecrets, error) {
	k, ok := a.keys[tenantstripe.Account{TenantID: tenantID, Livemode: livemode}]
	if !ok {
		return tenantstripe.PlaintextSecrets{}, errs.ErrNotFound
	}
	return tenantstripe.PlaintextSecrets{SecretKey: k}, nil
}

func (a accountCreds) ListAccounts(context.Context) ([]tenantstripe.Account, error) {
	var out []tenantstripe.Account
	for acct := range a.keys {
		out = append(out, acct)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].TenantID < out[j].TenantID || (out[i].TenantID == out[j].TenantID && out[i].Livemode)
	})
	return out, nil
}

// refundBook records refunds made in Stripe the way creditnote's recorder
// answers the handler: by the invoice that holds the refunded payment, and
// errs.ErrNotFound when no invoice holds it.
type refundBook struct {
	invoices *invoice.PostgresStore
	mu       sync.Mutex
	recorded []string
}

func (r *refundBook) RecordStripeRefund(ctx context.Context, tenantID string, snap domain.StripeRefundSnapshot) (domain.StripeRefundRecordResult, error) {
	if _, err := r.invoices.GetByStripePaymentIntentID(ctx, tenantID, snap.StripePaymentIntentID); err != nil {
		return domain.StripeRefundRecordResult{}, err
	}
	r.mu.Lock()
	r.recorded = append(r.recorded, snap.StripeRefundID)
	r.mu.Unlock()
	return domain.StripeRefundRecordResult{Recorded: true}, nil
}

func (r *refundBook) ApplyRefundWebhook(context.Context, string, string, domain.RefundStatus, string) error {
	return errs.ErrNotFound
}

type catchUpRig struct {
	db        *postgres.DB
	stripe    *catchUpStripe
	creds     accountCreds
	invoices  *invoice.PostgresStore
	customers *customer.PostgresStore
	refunds   *refundBook
	adapter   *payment.Stripe
	catchUp   *payment.CatchUp
	since     time.Time
}

// newCatchUpRig wires the adapter as router.go does for the parts the events
// reach, with the real stores and outboxes.
func newCatchUpRig(t *testing.T) *catchUpRig {
	t.Helper()
	db := testutil.SetupTestDB(t)
	invStore := invoice.NewPostgresStore(db)
	outbox := webhook.NewOutboxStore(db)
	invStore.SetOutboxEnqueuer(outbox)
	customers := customer.NewPostgresStore(db)
	sender := email.NewOutboxSender(email.NewOutboxStore(db))

	api := newCatchUpStripe()
	creds := accountCreds{keys: map[tenantstripe.Account]string{}}
	clients := payment.NewStripeClientsWithHTTPClient(creds, &http.Client{Transport: api})
	client := payment.NewLiveStripeClient(clients)
	refunds := &refundBook{invoices: invStore}

	s := payment.NewStripe(client, invStore, payment.NewPostgresWebhookStore(db), nil)
	s.SetCardFetcher(settleRaceCard{})
	s.SetEventDispatcher(webhook.NewOutboxDispatcher(outbox))
	s.SetAnomalyRecorder(invStore)
	s.SetEmailReceipt(sender, settleRaceEmails{store: customers})
	s.SetRefundStatusUpdater(refunds)
	s.SetExternalRefundRecorder(refunds)

	return &catchUpRig{db: db, stripe: api, creds: creds, invoices: invStore, customers: customers,
		refunds: refunds, adapter: s, catchUp: payment.NewCatchUp(s, clients, creds),
		since: time.Now().UTC().Add(-96 * time.Hour)}
}

// connect stores a key for the tenant's mode.
func (r *catchUpRig) connect(tenantID string, live bool, key string) tenantstripe.Account {
	acct := tenantstripe.Account{TenantID: tenantID, Livemode: live}
	r.creds.keys[acct] = key
	return acct
}

func modeCtx(tenantID string, live bool) context.Context {
	return postgres.WithLivemode(auth.WithTenantID(context.Background(), tenantID), live)
}

// seedInvoice makes a customer and an unpaid invoice of catchUpDue with the
// given status.
func (r *catchUpRig) seedInvoice(t *testing.T, tenantID string, live bool, name string, status domain.InvoiceStatus) domain.Invoice {
	t.Helper()
	ctx := modeCtx(tenantID, live)
	cust, err := r.customers.Create(ctx, tenantID, domain.Customer{ExternalID: "cus_" + name, DisplayName: name, Email: "billing@" + name + ".test"})
	if err != nil {
		t.Fatalf("create customer: %v", err)
	}
	now := time.Now().UTC()
	inv := domain.Invoice{CustomerID: cust.ID, InvoiceNumber: "INV-" + name, Status: status,
		PaymentStatus: domain.PaymentPending, Currency: "USD", SubtotalCents: catchUpDue,
		TotalAmountCents: catchUpDue, AmountDueCents: catchUpDue,
		BillingPeriodStart: now.Add(-time.Hour), BillingPeriodEnd: now}
	if status != domain.InvoiceDraft {
		inv.IssuedAt = &now
	}
	inv, err = r.invoices.CreateWithLineItems(ctx, tenantID, inv, nil)
	if err != nil {
		t.Fatalf("create invoice: %v", err)
	}
	return inv
}

func stripeEventJSON(id, eventType string, created int64, live bool, object string) string {
	return fmt.Sprintf(`{"id":%q,"object":"event","api_version":"2025-03-31.basil","created":%d,"data":{"object":%s},"livemode":%t,"pending_webhooks":0,"request":{"id":null,"idempotency_key":null},"type":%q}`,
		id, created, object, live, eventType)
}

func paymentIntentJSON(id, tenantID, invoiceID, customerID string, created int64) string {
	return fmt.Sprintf(`{"id":%q,"object":"payment_intent","amount":%d,"amount_received":%d,"currency":"usd","status":"succeeded","created":%d,"latest_charge":"ch_%s","metadata":{"hardcount_tenant_id":%q,"hardcount_invoice_id":%q,"hardcount_customer_id":%q}}`,
		id, catchUpDue, catchUpDue, created, id, tenantID, invoiceID, customerID)
}

func refundJSON(id, piID string, amount, created int64) string {
	return fmt.Sprintf(`{"id":%q,"object":"refund","amount":%d,"currency":"usd","charge":"ch_%s","payment_intent":%q,"status":"succeeded","reason":null,"created":%d,"metadata":{}}`,
		id, amount, piID, piID, created)
}

// addPayment puts a succeeded PaymentIntent at the fake and its event.
func (r *catchUpRig) addPayment(key, evtID, piID, tenantID, invoiceID, customerID string, created int64) {
	pi := paymentIntentJSON(piID, tenantID, invoiceID, customerID, created)
	r.stripe.pis[piID] = pi
	r.stripe.events[key] = append(r.stripe.events[key], stripeEventJSON(evtID, "payment_intent.succeeded", created, strings.HasPrefix(key, "sk_live"), pi))
}

func (r *catchUpRig) count(t *testing.T, q string, args ...any) int {
	t.Helper()
	tx, err := r.db.BeginTx(context.Background(), postgres.TxBypass, "")
	if err != nil {
		t.Fatal(err)
	}
	defer postgres.Rollback(tx)
	var n int
	if err := tx.QueryRowContext(context.Background(), q, args...).Scan(&n); err != nil {
		t.Fatalf("count %q: %v", q, err)
	}
	return n
}

func (r *catchUpRig) run(t *testing.T, acks ...string) payment.CatchUpReport {
	t.Helper()
	ack := map[string]bool{}
	for _, a := range acks {
		ack[a] = true
	}
	rep, err := r.catchUp.Run(context.Background(), payment.CatchUpRun{Since: r.since, Acknowledged: ack})
	if err != nil {
		t.Fatalf("catch-up: %v", err)
	}
	return rep
}

func onlyAccount(t *testing.T, rep payment.CatchUpReport) payment.AccountCatchUp {
	t.Helper()
	if len(rep.Accounts) != 1 {
		t.Fatalf("report has %d accounts, want 1: %+v", len(rep.Accounts), rep)
	}
	return rep.Accounts[0]
}

// A payment Stripe took after the backup is recorded on its invoice once:
// one receipt, one payment.succeeded event. A second run changes nothing,
// and an event the backup already recorded is not applied again.
func TestCatchUp_AppliesEachEventOnce(t *testing.T) {
	r := newCatchUpRig(t)
	tenantID := testutil.CreateTestTenant(t, r.db, "catchup-once")
	r.connect(tenantID, true, "sk_live_once")
	inv := r.seedInvoice(t, tenantID, true, "once", domain.InvoiceFinalized)
	created := time.Now().Add(-48 * time.Hour).Unix()
	r.addPayment("sk_live_once", "evt_paid", "pi_once", tenantID, inv.ID, inv.CustomerID, created)

	// An event in the overlap that the restored database already holds.
	old := r.seedInvoice(t, tenantID, true, "old", domain.InvoiceFinalized)
	r.addPayment("sk_live_once", "evt_old", "pi_old", tenantID, old.ID, old.CustomerID, created-3600)
	if _, _, err := payment.NewPostgresWebhookStore(r.db).IngestEvent(modeCtx(tenantID, true), tenantID,
		domain.StripeWebhookEvent{StripeEventID: "evt_old", EventType: "payment_intent.succeeded", ObjectType: "payment_intent", Payload: map[string]any{}}); err != nil {
		t.Fatal(err)
	}

	acct := onlyAccount(t, r.run(t))
	if acct.Err != nil || acct.Applied != 1 || acct.AlreadyRecorded != 1 || len(acct.Lost) != 0 {
		t.Fatalf("first run: %+v, want 1 applied, 1 already recorded, nothing lost", acct)
	}
	got, err := r.invoices.Get(modeCtx(tenantID, true), tenantID, inv.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != domain.InvoicePaid || got.StripePaymentIntentID != "pi_once" {
		t.Fatalf("invoice %s with %q, want paid with pi_once", got.Status, got.StripePaymentIntentID)
	}
	gotOld, err := r.invoices.Get(modeCtx(tenantID, true), tenantID, old.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotOld.Status != domain.InvoiceFinalized {
		t.Fatalf("the event the backup held was applied again: invoice %s", gotOld.Status)
	}

	receipts := func() int {
		return r.count(t, `SELECT count(*) FROM email_outbox WHERE tenant_id = $1 AND email_type = 'payment_receipt'`, tenantID)
	}
	paidEvents := func() int {
		return r.count(t, `SELECT count(*) FROM webhook_outbox WHERE tenant_id = $1 AND event_type = 'payment.succeeded'`, tenantID)
	}
	if receipts() != 1 || paidEvents() != 1 {
		t.Fatalf("after the first run: %d receipts, %d payment.succeeded, want 1 each", receipts(), paidEvents())
	}

	again := onlyAccount(t, r.run(t))
	if again.Err != nil || again.Applied != 0 || again.AlreadyRecorded != 2 {
		t.Fatalf("second run: %+v, want 0 applied, 2 already recorded", again)
	}
	if receipts() != 1 || paidEvents() != 1 {
		t.Fatalf("after the second run: %d receipts, %d payment.succeeded, want 1 each", receipts(), paidEvents())
	}
}

// Events are applied oldest first, and events of the same second in id
// order, whatever order Stripe lists them in. A refund applied before its
// payment is recorded would find no invoice and stop the run.
func TestCatchUp_AppliesInCreatedThenIDOrder(t *testing.T) {
	r := newCatchUpRig(t)
	tenantID := testutil.CreateTestTenant(t, r.db, "catchup-order")
	r.connect(tenantID, true, "sk_live_order")
	inv := r.seedInvoice(t, tenantID, true, "order", domain.InvoiceFinalized)
	created := time.Now().Add(-30 * time.Hour).Unix()

	// Stripe lists newest first, and the order of a same-second tie is not
	// defined: here a later refund comes first, then a refund in the same
	// second as the payment, then the payment.
	r.stripe.events["sk_live_order"] = append(r.stripe.events["sk_live_order"],
		stripeEventJSON("evt_0z", "refund.created", created+120, true, refundJSON("re_later", "pi_order", 500, created+120)),
		stripeEventJSON("evt_1b", "refund.created", created, true, refundJSON("re_same", "pi_order", 1000, created)))
	r.addPayment("sk_live_order", "evt_1a", "pi_order", tenantID, inv.ID, inv.CustomerID, created)

	acct := onlyAccount(t, r.run(t))
	if acct.Err != nil || acct.Applied != 3 {
		t.Fatalf("report %+v, want all three events applied", acct)
	}
	if strings.Join(r.refunds.recorded, ",") != "re_same,re_later" {
		t.Fatalf("refunds recorded %v, want re_same then re_later on the paid invoice", r.refunds.recorded)
	}
}

// A payment whose invoice the restore lost, or left as a draft, is listed
// with what the operator needs to match it by hand, and is not recorded. It
// is recorded as handled only once acknowledged, and stays listed as
// acknowledged on a later run.
func TestCatchUp_ListsPaymentsWhoseInvoiceIsLostOrDraft(t *testing.T) {
	r := newCatchUpRig(t)
	tenantID := testutil.CreateTestTenant(t, r.db, "catchup-lost")
	r.connect(tenantID, true, "sk_live_lost")
	draft := r.seedInvoice(t, tenantID, true, "draft", domain.InvoiceDraft)
	created := time.Now().Add(-40 * time.Hour).Unix()
	r.addPayment("sk_live_lost", "evt_gone", "pi_gone", tenantID, "hc_inv_lost", "hc_cus_lost", created)
	r.addPayment("sk_live_lost", "evt_draft", "pi_draft", tenantID, draft.ID, draft.CustomerID, created+1)

	acct := onlyAccount(t, r.run(t))
	if acct.Err != nil || acct.Applied != 0 || len(acct.Lost) != 2 {
		t.Fatalf("report %+v, want 2 lost payments and nothing applied", acct)
	}
	gone, dr := acct.Lost[0], acct.Lost[1]
	if gone.PaymentIntentID != "pi_gone" || gone.InvoiceID != "hc_inv_lost" || gone.InvoiceDraft ||
		gone.CustomerID != "hc_cus_lost" || gone.AmountCents != catchUpDue || gone.Currency != "USD" || gone.Acknowledged {
		t.Errorf("lost payment %+v, want pi_gone for missing hc_inv_lost of hc_cus_lost, %d USD, not acknowledged", gone, catchUpDue)
	}
	if dr.PaymentIntentID != "pi_draft" || dr.InvoiceID != draft.ID || !dr.InvoiceDraft || dr.Acknowledged {
		t.Errorf("draft payment %+v, want pi_draft on draft %s", dr, draft.ID)
	}
	if n := r.count(t, `SELECT count(*) FROM stripe_webhook_events WHERE tenant_id = $1`, tenantID); n != 0 {
		t.Fatalf("%d events recorded, want 0: a listed payment stays unrecorded until acknowledged", n)
	}
	got, err := r.invoices.Get(modeCtx(tenantID, true), tenantID, draft.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != domain.InvoiceDraft {
		t.Fatalf("the draft is %s, want it untouched", got.Status)
	}

	acked := onlyAccount(t, r.run(t, "pi_gone", "pi_draft"))
	if acked.Err != nil || len(acked.Lost) != 2 || !acked.Lost[0].Acknowledged || !acked.Lost[1].Acknowledged {
		t.Fatalf("acknowledged run %+v, want both listed as acknowledged", acked)
	}
	if n := r.count(t, `SELECT count(*) FROM stripe_webhook_events WHERE tenant_id = $1`, tenantID); n != 2 {
		t.Fatalf("%d events recorded after the acknowledgement, want 2", n)
	}

	later := onlyAccount(t, r.run(t))
	if later.Err != nil || len(later.Lost) != 2 || !later.Lost[0].Acknowledged || !later.Lost[1].Acknowledged {
		t.Fatalf("later run %+v, want both still listed as acknowledged", later)
	}
}

// The refund of a payment whose invoice was lost is held with that payment.
// Through HandleWebhook it fails every time, because the payment names a
// Hardcount invoice that no row holds, so the catch-up would never finish.
func TestCatchUp_RefundOfListedPaymentDoesNotStall(t *testing.T) {
	r := newCatchUpRig(t)
	tenantID := testutil.CreateTestTenant(t, r.db, "catchup-refund")
	r.connect(tenantID, true, "sk_live_refund")
	created := time.Now().Add(-40 * time.Hour).Unix()
	r.addPayment("sk_live_refund", "evt_pay", "pi_refunded", tenantID, "hc_inv_gone", "hc_cus_gone", created)
	refund := stripeEventJSON("evt_refund", "refund.created", created+60, true, refundJSON("re_1", "pi_refunded", 2000, created+60))
	r.stripe.events["sk_live_refund"] = append([]string{refund}, r.stripe.events["sk_live_refund"]...)

	// The premise: the live apply path cannot finish this refund.
	premise := domain.StripeWebhookEvent{StripeEventID: "evt_refund", EventType: "refund.created", ObjectType: "refund",
		PaymentIntentID: "re_1", Payload: map[string]any{"raw": refund}, Livemode: true}
	if err := r.adapter.HandleWebhook(modeCtx(tenantID, true), tenantID, premise); err == nil {
		t.Fatal("premise: HandleWebhook recorded the refund of a payment whose invoice is lost; the catch-up's hold is not needed")
	}

	acct := onlyAccount(t, r.run(t))
	if acct.Err != nil {
		t.Fatalf("the refund stalled the catch-up: %v", acct.Err)
	}
	if len(acct.Lost) != 1 || acct.Lost[0].PaymentIntentID != "pi_refunded" || acct.Lost[0].Held != 1 {
		t.Fatalf("lost %+v, want pi_refunded with its refund held", acct.Lost)
	}
	if n := r.count(t, `SELECT count(*) FROM stripe_webhook_events WHERE tenant_id = $1`, tenantID); n != 0 {
		t.Fatalf("%d events recorded before the acknowledgement, want 0", n)
	}

	acked := onlyAccount(t, r.run(t, "pi_refunded"))
	if acked.Err != nil || len(acked.Lost) != 1 || !acked.Lost[0].Acknowledged {
		t.Fatalf("acknowledged run %+v", acked)
	}
	if n := r.count(t, `SELECT count(*) FROM stripe_webhook_events WHERE tenant_id = $1`, tenantID); n != 2 {
		t.Fatalf("%d events recorded after the acknowledgement, want the payment and its refund", n)
	}
	if len(r.refunds.recorded) != 0 {
		t.Fatalf("refunds recorded %v, want none: no invoice holds the payment", r.refunds.recorded)
	}
}

// Open hosted payment pages are closed before the events are listed, so no
// payment can arrive after the listing. A page the customer pays while it
// is being closed is complete, and its payment is in the listing. Only
// Hardcount's pages of this tenant are closed.
func TestCatchUp_ExpiresOpenHostedSessionsFirst(t *testing.T) {
	r := newCatchUpRig(t)
	tenantID := testutil.CreateTestTenant(t, r.db, "catchup-hosted")
	r.connect(tenantID, true, "sk_live_hosted")
	now := time.Now().Unix()
	open := &catchUpSession{id: "cs_open", status: "open", tenant: tenantID, purpose: payment.PurposeHostedInvoicePay, created: now - 600}
	paying := &catchUpSession{id: "cs_paying", status: "open", tenant: tenantID, purpose: payment.PurposeHostedInvoicePay, created: now - 300, completeOnExpire: true}
	other := &catchUpSession{id: "cs_other_tenant", status: "open", tenant: "hc_ten_other", purpose: payment.PurposeHostedInvoicePay, created: now - 300}
	theirs := &catchUpSession{id: "cs_not_hardcount", status: "open", created: now - 300}
	r.stripe.sessions["sk_live_hosted"] = []*catchUpSession{open, paying, other, theirs}
	r.addPayment("sk_live_hosted", "evt_paying", "pi_paying", tenantID, "hc_inv_hosted_lost", "hc_cus_hosted", now-60)

	acct := onlyAccount(t, r.run(t))
	if acct.Err != nil || acct.SessionsExpired != 1 {
		t.Fatalf("report %+v, want 1 session expired and no error", acct)
	}
	if open.status != "expired" || other.status != "open" || theirs.status != "open" {
		t.Fatalf("sessions: ours %s, another tenant's %s, not Hardcount's %s; want only ours expired", open.status, other.status, theirs.status)
	}
	if len(acct.Lost) != 1 || acct.Lost[0].PaymentIntentID != "pi_paying" {
		t.Fatalf("lost %+v, want the payment made as its page closed", acct.Lost)
	}
	lastExpire, firstEvents := -1, -1
	for i, c := range r.stripe.callLog() {
		if strings.HasSuffix(c, "/expire") {
			lastExpire = i
		}
		if c == "GET /v1/events" && firstEvents < 0 {
			firstEvents = i
		}
	}
	if lastExpire < 0 || firstEvents < lastExpire {
		t.Fatalf("calls %v: the sessions must be closed before the events are listed", r.stripe.callLog())
	}
}

// A live key Stripe refuses is reported for the account and nothing is
// applied for it; a test-mode refusal is reported the same way. A key given
// for this run only replaces the stored one, including for an account the
// restored database holds no key for.
func TestCatchUp_RefusedKeyAndRunOnlyKey(t *testing.T) {
	r := newCatchUpRig(t)
	tenantID := testutil.CreateTestTenant(t, r.db, "catchup-key")
	r.connect(tenantID, true, "sk_live_revoked")
	r.connect(tenantID, false, "sk_test_revoked")
	r.stripe.refused["sk_live_revoked"] = true
	r.stripe.refused["sk_test_revoked"] = true
	inv := r.seedInvoice(t, tenantID, true, "key", domain.InvoiceFinalized)
	created := time.Now().Add(-20 * time.Hour).Unix()
	r.addPayment("sk_live_current", "evt_key", "pi_key", tenantID, inv.ID, inv.CustomerID, created)

	rep := r.run(t)
	if len(rep.Accounts) != 2 {
		t.Fatalf("report %+v, want both modes", rep)
	}
	for _, a := range rep.Accounts {
		if a.KeyRefused == "" || a.Applied != 0 {
			t.Errorf("account %+v: want the refused key reported and nothing applied", a)
		}
	}

	// The current key, for this run only.
	override := payment.KeyOverrides(r.creds, map[tenantstripe.Account]string{{TenantID: tenantID, Livemode: true}: "sk_live_current"})
	clients := payment.NewStripeClientsWithHTTPClient(override, &http.Client{Transport: r.stripe})
	cu := payment.NewCatchUp(r.adapter, clients, r.creds)
	rep, err := cu.Run(context.Background(), payment.CatchUpRun{Since: r.since, Acknowledged: map[string]bool{}})
	if err != nil {
		t.Fatal(err)
	}
	var live payment.AccountCatchUp
	for _, a := range rep.Accounts {
		if a.Account.Livemode {
			live = a
		}
	}
	if live.KeyRefused != "" || live.Err != nil || live.Applied != 1 {
		t.Fatalf("live account with the run-only key: %+v, want the payment applied", live)
	}

	// An account the restored database has no key for: connected after the
	// backup. The run-only key and Extra cover it.
	newTenant := testutil.CreateTestTenant(t, r.db, "catchup-new-account")
	newInv := r.seedInvoice(t, newTenant, true, "newacct", domain.InvoiceFinalized)
	r.addPayment("sk_live_new_account", "evt_new", "pi_new", newTenant, newInv.ID, newInv.CustomerID, created)
	added := tenantstripe.Account{TenantID: newTenant, Livemode: true}
	override = payment.KeyOverrides(r.creds, map[tenantstripe.Account]string{added: "sk_live_new_account"})
	cu = payment.NewCatchUp(r.adapter, payment.NewStripeClientsWithHTTPClient(override, &http.Client{Transport: r.stripe}), r.creds)
	rep, err = cu.Run(context.Background(), payment.CatchUpRun{Since: r.since, Extra: []tenantstripe.Account{added}, Acknowledged: map[string]bool{}})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, a := range rep.Accounts {
		if a.Account == added {
			found = true
			if a.Err != nil || a.Applied != 1 {
				t.Fatalf("account added for the run: %+v, want its payment applied", a)
			}
		}
	}
	if !found {
		t.Fatalf("report %+v has no entry for the account added for the run", rep)
	}
}

// On a Stripe account that two tenants share, each tenant's catch-up applies
// only its own events, as the webhook endpoint does.
func TestCatchUp_SkipsAnotherTenantsEvents(t *testing.T) {
	r := newCatchUpRig(t)
	tenantID := testutil.CreateTestTenant(t, r.db, "catchup-shared")
	r.connect(tenantID, true, "sk_live_shared")
	created := time.Now().Add(-10 * time.Hour).Unix()
	r.addPayment("sk_live_shared", "evt_theirs", "pi_theirs", "hc_ten_someone_else", "hc_inv_theirs", "hc_cus_theirs", created)
	r.stripe.events["sk_live_shared"] = append(r.stripe.events["sk_live_shared"], stripeEventJSON("evt_own_charge", "payment_intent.succeeded", created,
		true, `{"id":"pi_own","object":"payment_intent","amount":100,"currency":"usd","status":"succeeded","metadata":{}}`))

	acct := onlyAccount(t, r.run(t))
	if acct.Err != nil || acct.Skipped != 2 || acct.Applied != 0 || len(acct.Lost) != 0 {
		t.Fatalf("report %+v, want both events skipped", acct)
	}
}
