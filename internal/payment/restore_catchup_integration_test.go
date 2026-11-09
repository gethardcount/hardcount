package payment_test

import (
	"context"
	"database/sql"
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
	"github.com/gethardcount/hardcount/internal/creditnote"
	"github.com/gethardcount/hardcount/internal/customer"
	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/email"
	"github.com/gethardcount/hardcount/internal/errs"
	"github.com/gethardcount/hardcount/internal/invoice"
	"github.com/gethardcount/hardcount/internal/payment"
	"github.com/gethardcount/hardcount/internal/paymentmethods"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/tenantstripe"
	"github.com/gethardcount/hardcount/internal/testutil"
	"github.com/gethardcount/hardcount/internal/webhook"
)

// The restore catch-up (ADR-135) on a real database: Stripe's events since
// the backup are applied once through HandleWebhook, a payment whose invoice
// the restore lost is listed instead of dropped, and what HandleWebhook
// cannot apply, or would apply wrongly, is listed or held and does not stall
// the run. Stripe is a fake HTTP API behind the real stripe-go client, so
// the catch-up's own requests and decoding are real. The saved-card path
// runs through the real payment-methods service and customer store.

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
// webhook handlers make: List Events and List Checkout sessions (newest
// first, paged), Expire and Retrieve a session, Retrieve a PaymentIntent and
// a PaymentMethod, and Update a customer. It records every request in order.
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
	case req.Method == http.MethodGet && strings.HasPrefix(path, "/v1/payment_methods/"):
		id := strings.TrimPrefix(path, "/v1/payment_methods/")
		return reply(req, http.StatusOK, fmt.Sprintf(`{"id":%q,"object":"payment_method","type":"card","card":{"brand":"visa","last4":"4242","exp_month":12,"exp_year":2030,"fingerprint":"fp_%s"}}`, id, id)), nil
	case req.Method == http.MethodPost && strings.HasPrefix(path, "/v1/customers/"):
		return reply(req, http.StatusOK, fmt.Sprintf(`{"id":%q,"object":"customer"}`, strings.TrimPrefix(path, "/v1/customers/"))), nil
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

// page returns one page of items, each with its id, as Stripe pages a list:
// the items after starting_after, at most limit of them (10 when unset).
func page(q url.Values, ids, items []string) (data []string, more bool) {
	from := 0
	if after := q.Get("starting_after"); after != "" {
		for i, id := range ids {
			if id == after {
				from = i + 1
				break
			}
		}
	}
	items = items[from:]
	limit, _ := strconv.Atoi(q.Get("limit")) // absent reads as 0: Stripe's default of 10
	if limit == 0 {
		limit = 10
	}
	if len(items) > limit {
		return items[:limit], true
	}
	return items, false
}

func (f *catchUpStripe) listSessions(key string, q url.Values) string {
	gte, _ := strconv.ParseInt(q.Get("created[gte]"), 10, 64) // absent reads as 0: every session
	var ids, match []string
	for _, s := range f.sessions[key] {
		if (q.Get("status") == "" || s.status == q.Get("status")) && s.created >= gte {
			ids = append(ids, s.id)
			match = append(match, sessionJSON(s))
		}
	}
	data, more := page(q, ids, match)
	return fmt.Sprintf(`{"object":"list","url":"/v1/checkout/sessions","has_more":%t,"data":[%s]}`, more, strings.Join(data, ","))
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
	var ids, match []string
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
			ids = append(ids, head.ID)
			match = append(match, ev)
		}
	}
	data, more := page(q, ids, match)
	return fmt.Sprintf(`{"object":"list","url":"/v1/events","has_more":%t,"data":[%s]}`, more, strings.Join(data, ","))
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

// catchUpCards answers FetchCardDetails as the live client does when the
// Stripe customer holds a saved card: the card, with its PaymentMethod id.
type catchUpCards struct{}

func (catchUpCards) FetchCardDetails(_ context.Context, stripeCustomerID string) (payment.CardDetails, error) {
	return payment.CardDetails{PaymentMethodID: "pm_of_" + stripeCustomerID, Brand: "visa", Last4: "4242", ExpMonth: 12, ExpYear: 2030}, nil
}

func (catchUpCards) FetchCardForPaymentIntent(context.Context, string) (payment.CardDetails, error) {
	return payment.CardDetails{Brand: "visa", Last4: "4242"}, nil
}

// catchUpCustomerResolver maps a Stripe customer to the Hardcount customer
// that holds it, as router.go's stripeCustomerResolverAdapter does.
type catchUpCustomerResolver struct{ customers *customer.PostgresStore }

func (r catchUpCustomerResolver) CustomerIDByStripeID(ctx context.Context, tenantID, stripeCustomerID string) (string, error) {
	c, err := r.customers.GetByStripeCustomerID(ctx, tenantID, stripeCustomerID)
	if err != nil {
		return "", err
	}
	return c.ID, nil
}

// catchUpPaymentSetups writes the customer's Stripe customer id, the one
// write router.go's compositePaymentSetupStore makes, and reads the
// customer's default card from the payment-methods service.
type catchUpPaymentSetups struct {
	customers *customer.PostgresStore
	pms       *paymentmethods.Service
}

func (p catchUpPaymentSetups) GetPaymentSetup(ctx context.Context, tenantID, customerID string) (domain.CustomerPaymentSetup, error) {
	cust, err := p.customers.Get(ctx, tenantID, customerID)
	if err != nil {
		return domain.CustomerPaymentSetup{}, err
	}
	ps := domain.CustomerPaymentSetup{CustomerID: customerID, TenantID: tenantID, StripeCustomerID: cust.StripeCustomerID}
	pms, err := p.pms.List(ctx, tenantID, customerID)
	if err != nil {
		return domain.CustomerPaymentSetup{}, err
	}
	for _, pm := range pms {
		if pm.IsDefault {
			ps.SetupStatus, ps.StripePaymentMethodID = domain.PaymentSetupReady, pm.StripePaymentMethodID
		}
	}
	return ps, nil
}

func (p catchUpPaymentSetups) UpsertPaymentSetup(ctx context.Context, tenantID string, ps domain.CustomerPaymentSetup) (domain.CustomerPaymentSetup, error) {
	return p.UpsertPaymentSetupAudited(ctx, tenantID, ps, nil)
}

func (p catchUpPaymentSetups) UpsertPaymentSetupAudited(ctx context.Context, tenantID string, ps domain.CustomerPaymentSetup, emit func(tx *sql.Tx) error) (domain.CustomerPaymentSetup, error) {
	if ps.StripeCustomerID != "" && ps.CustomerID != "" {
		if err := p.customers.SetStripeCustomerIDAudited(ctx, tenantID, ps.CustomerID, ps.StripeCustomerID, emit); err != nil {
			return domain.CustomerPaymentSetup{}, err
		}
	}
	return p.GetPaymentSetup(ctx, tenantID, ps.CustomerID)
}

type catchUpRig struct {
	db        *postgres.DB
	stripe    *catchUpStripe
	creds     accountCreds
	clients   *payment.StripeClients
	invoices  *invoice.PostgresStore
	customers *customer.PostgresStore
	refunds   *refundBook
	notes     *creditnote.PostgresStore
	adapter   *payment.Stripe
	catchUp   *payment.CatchUp
	since     time.Time
}

// newCatchUpRig wires the adapter as router.go does for the parts the events
// reach, with the real stores, outboxes and payment-methods service.
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
	notes := creditnote.NewPostgresStore(db)
	pmStore := paymentmethods.NewPostgresStore(db)
	pmStore.SetOutboxEnqueuer(outbox)
	pms := paymentmethods.NewService(pmStore, paymentmethods.NewStripeAdapter(clients, customers), customers)

	s := payment.NewStripe(client, invStore, payment.NewPostgresWebhookStore(db), catchUpPaymentSetups{customers: customers, pms: pms})
	s.SetCardFetcher(catchUpCards{})
	s.SetAnomalyRecorder(invStore)
	s.SetEmailReceipt(sender, settleRaceEmails{store: customers})
	s.SetRefundStatusUpdater(refunds)
	s.SetExternalRefundRecorder(refunds)
	s.SetPaymentMethodAttacher(pms)
	s.SetCustomerResolver(catchUpCustomerResolver{customers: customers})

	return &catchUpRig{db: db, stripe: api, creds: creds, clients: clients, invoices: invStore, customers: customers,
		refunds: refunds, notes: notes, adapter: s, catchUp: payment.NewCatchUp(s, clients, creds, customers, notes),
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
	return refundWith(id, piID, amount, created, "succeeded", `{}`)
}

func refundWith(id, piID string, amount, created int64, status, metadata string) string {
	return fmt.Sprintf(`{"id":%q,"object":"refund","amount":%d,"currency":"usd","charge":"ch_%s","payment_intent":%q,"status":%q,"reason":null,"created":%d,"metadata":%s}`,
		id, amount, piID, piID, status, created, metadata)
}

// addPayment puts a succeeded PaymentIntent at the fake and its event.
func (r *catchUpRig) addPayment(key, evtID, piID, tenantID, invoiceID, customerID string, created int64) {
	pi := paymentIntentJSON(piID, tenantID, invoiceID, customerID, created)
	r.stripe.pis[piID] = pi
	r.stripe.events[key] = append(r.stripe.events[key], stripeEventJSON(evtID, "payment_intent.succeeded", created, strings.HasPrefix(key, "sk_live"), pi))
}

// addEvent puts an event at the fake, after the ones already there.
func (r *catchUpRig) addEvent(key, evtID, eventType string, created int64, object string) {
	r.stripe.events[key] = append(r.stripe.events[key], stripeEventJSON(evtID, eventType, created, strings.HasPrefix(key, "sk_live"), object))
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

// exec runs a live-mode write across tenants.
func (r *catchUpRig) exec(t *testing.T, q string, args ...any) {
	t.Helper()
	tx, err := r.db.BeginTx(context.Background(), postgres.TxBypass, "")
	if err != nil {
		t.Fatal(err)
	}
	defer postgres.Rollback(tx)
	if _, err := tx.ExecContext(context.Background(), `SELECT set_config('app.livemode', 'on', true)`); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(context.Background(), q, args...); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

// recorded reports whether the event has its dedup row.
func (r *catchUpRig) recorded(t *testing.T, tenantID, eventID string) bool {
	t.Helper()
	return r.count(t, `SELECT count(*) FROM stripe_webhook_events WHERE tenant_id = $1 AND stripe_event_id = $2`, tenantID, eventID) == 1
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

func (r *catchUpRig) invoiceStatus(t *testing.T, tenantID, id string) domain.Invoice {
	t.Helper()
	got, err := r.invoices.Get(modeCtx(tenantID, true), tenantID, id)
	if err != nil {
		t.Fatal(err)
	}
	return got
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
	if got := r.invoiceStatus(t, tenantID, inv.ID); got.Status != domain.InvoicePaid || got.StripePaymentIntentID != "pi_once" {
		t.Fatalf("invoice %s with %q, want paid with pi_once", got.Status, got.StripePaymentIntentID)
	}
	if got := r.invoiceStatus(t, tenantID, old.ID); got.Status != domain.InvoiceFinalized {
		t.Fatalf("the event the backup held was applied again: invoice %s", got.Status)
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

// Events are applied oldest first. Stripe's event ids carry no order, so in
// the same second a payment goes before its refund, whatever their ids. A
// refund applied before its payment is recorded would find no invoice for it
// and be listed as failed. Here the refund's id sorts before the payment's.
func TestCatchUp_AppliesInCreatedThenTypeOrder(t *testing.T) {
	r := newCatchUpRig(t)
	tenantID := testutil.CreateTestTenant(t, r.db, "catchup-order")
	r.connect(tenantID, true, "sk_live_order")
	inv := r.seedInvoice(t, tenantID, true, "order", domain.InvoiceFinalized)
	created := time.Now().Add(-30 * time.Hour).Unix()

	// Stripe lists newest first, and the order of a same-second tie is not
	// defined: here a later refund comes first, then a refund in the same
	// second as the payment, whose id sorts first, then the payment.
	r.addEvent("sk_live_order", "evt_0z", "refund.created", created+120, refundJSON("re_later", "pi_order", 500, created+120))
	r.addEvent("sk_live_order", "evt_1a", "refund.created", created, refundJSON("re_same", "pi_order", 1000, created))
	r.addPayment("sk_live_order", "evt_1b", "pi_order", tenantID, inv.ID, inv.CustomerID, created)

	acct := onlyAccount(t, r.run(t))
	if acct.Err != nil || acct.Applied != 3 || len(acct.Unapplied) != 0 {
		t.Fatalf("report %+v, want all three events applied", acct)
	}
	if strings.Join(r.refunds.recorded, ",") != "re_same,re_later" {
		t.Fatalf("refunds recorded %v, want re_same then re_later on the paid invoice", r.refunds.recorded)
	}
}

// More events and open payment pages than one page of Stripe's list: each
// event is applied exactly once and each page is closed, with two events of
// the same second on either side of the page boundary.
func TestCatchUp_ReadsEveryPage(t *testing.T) {
	r := newCatchUpRig(t)
	tenantID := testutil.CreateTestTenant(t, r.db, "catchup-pages")
	r.connect(tenantID, true, "sk_live_pages")
	first := r.seedInvoice(t, tenantID, true, "page-a", domain.InvoiceFinalized)
	second := r.seedInvoice(t, tenantID, true, "page-b", domain.InvoiceFinalized)
	now := time.Now().Add(-20 * time.Hour).Unix()

	// Newest first, as Stripe lists them: 150 events, the 100th and 101st
	// two payments of the same second.
	const total = 150
	for i := range total {
		created := now - int64(i)
		switch i {
		case 99:
			r.addPayment("sk_live_pages", "evt_page_pay_a", "pi_page_a", tenantID, first.ID, first.CustomerID, now-99)
		case 100:
			r.addPayment("sk_live_pages", "evt_page_pay_b", "pi_page_b", tenantID, second.ID, second.CustomerID, now-99)
		default:
			r.addEvent("sk_live_pages", fmt.Sprintf("evt_page_%03d", i), "setup_intent.setup_failed", created,
				fmt.Sprintf(`{"id":"seti_page_%03d","object":"setup_intent","customer":"cus_x","payment_method":null,"status":"requires_payment_method","created":%d,"metadata":{}}`, i, created))
		}
	}
	var sessions []*catchUpSession
	for i := range 130 {
		sessions = append(sessions, &catchUpSession{id: fmt.Sprintf("cs_page_%03d", i), status: "open", tenant: tenantID,
			purpose: payment.PurposeHostedInvoicePay, created: now - int64(i)})
	}
	r.stripe.sessions["sk_live_pages"] = sessions

	acct := onlyAccount(t, r.run(t))
	if acct.Err != nil || acct.Events != total || acct.Applied != total || acct.AlreadyRecorded != 0 || len(acct.Unapplied) != 0 {
		t.Fatalf("report %+v, want %d events listed and applied once each", acct, total)
	}
	if n := r.count(t, `SELECT count(DISTINCT stripe_event_id) FROM stripe_webhook_events WHERE tenant_id = $1`, tenantID); n != total {
		t.Fatalf("%d events recorded, want %d", n, total)
	}
	for _, inv := range []domain.Invoice{first, second} {
		if got := r.invoiceStatus(t, tenantID, inv.ID); got.Status != domain.InvoicePaid {
			t.Fatalf("invoice %s is %s: the payment at the page boundary was not applied", inv.ID, got.Status)
		}
	}
	if acct.SessionsExpired != len(sessions) {
		t.Fatalf("%d payment pages closed, want all %d", acct.SessionsExpired, len(sessions))
	}
	for _, s := range sessions {
		if s.status != "expired" {
			t.Fatalf("session %s is %s, want expired", s.id, s.status)
		}
	}

	again := onlyAccount(t, r.run(t))
	if again.Err != nil || again.Events != total || again.Applied != 0 || again.AlreadyRecorded != total {
		t.Fatalf("second run %+v, want every event already recorded", again)
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
	if got := r.invoiceStatus(t, tenantID, draft.ID); got.Status != domain.InvoiceDraft {
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
	if acct.Err != nil || len(acct.Unapplied) != 0 {
		t.Fatalf("the refund stalled the catch-up: %+v", acct)
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

// A customer created after the backup is lost with it. Its hosted payment's
// Checkout session and its saved card cannot be recorded: the card's row
// needs the customer's. They are held with the customer's listed payment,
// and a card event of a lost customer with no listed payment is listed by
// itself. The run finishes, and each is recorded once acknowledged.
func TestCatchUp_CardEventsOfALostCustomerAreHeld(t *testing.T) {
	r := newCatchUpRig(t)
	tenantID := testutil.CreateTestTenant(t, r.db, "catchup-lost-customer")
	r.connect(tenantID, true, "sk_live_lostcust")
	created := time.Now().Add(-30 * time.Hour).Unix()
	// The hosted payment and its session complete in the same second.
	r.addEvent("sk_live_lostcust", "evt_0_session", "checkout.session.completed", created,
		fmt.Sprintf(`{"id":"cs_lost","object":"checkout.session","mode":"payment","status":"complete","payment_status":"paid","customer":"cus_lost","payment_intent":"pi_lost","amount_total":5000,"currency":"usd","created":%d,"metadata":{"hardcount_tenant_id":%q,"hardcount_invoice_id":"hc_inv_lost","hardcount_customer_id":"hc_cus_lost","hardcount_purpose":"hosted_invoice_pay"}}`, created, tenantID))
	r.addPayment("sk_live_lostcust", "evt_1_payment", "pi_lost", tenantID, "hc_inv_lost", "hc_cus_lost", created)
	r.addEvent("sk_live_lostcust", "evt_2_card", "setup_intent.succeeded", created+30,
		fmt.Sprintf(`{"id":"seti_lost","object":"setup_intent","customer":"cus_lost","payment_method":"pm_lost","status":"succeeded","usage":"off_session","created":%d,"metadata":{"hardcount_customer_id":"hc_cus_lost"}}`, created+30))
	// Another lost customer saved a card, and paid nothing.
	r.addEvent("sk_live_lostcust", "evt_3_card_only", "setup_intent.succeeded", created+60,
		fmt.Sprintf(`{"id":"seti_lost_only","object":"setup_intent","customer":"cus_lost_only","payment_method":"pm_lost_only","status":"succeeded","usage":"off_session","created":%d,"metadata":{"hardcount_customer_id":"hc_cus_lost_only"}}`, created+60))

	acct := onlyAccount(t, r.run(t))
	if acct.Err != nil {
		t.Fatalf("a card event of a lost customer stopped the catch-up: %v", acct.Err)
	}
	if len(acct.Lost) != 1 || acct.Lost[0].PaymentIntentID != "pi_lost" || acct.Lost[0].Held != 2 {
		t.Fatalf("lost %+v, want pi_lost with its session and card held", acct.Lost)
	}
	if len(acct.Unapplied) != 1 || acct.Unapplied[0].EventID != "evt_3_card_only" || acct.Unapplied[0].CustomerID != "hc_cus_lost_only" ||
		acct.Unapplied[0].Err != nil || acct.Unapplied[0].Acknowledged {
		t.Fatalf("unapplied %+v, want the card of hc_cus_lost_only listed, not acknowledged", acct.Unapplied)
	}
	if n := r.count(t, `SELECT count(*) FROM stripe_webhook_events WHERE tenant_id = $1`, tenantID); n != 0 {
		t.Fatalf("%d events recorded before the acknowledgement, want 0", n)
	}

	acked := onlyAccount(t, r.run(t, "pi_lost", "evt_3_card_only"))
	if acked.Err != nil || !acked.Lost[0].Acknowledged || len(acked.Unapplied) != 1 || !acked.Unapplied[0].Acknowledged {
		t.Fatalf("acknowledged run %+v", acked)
	}
	for _, id := range []string{"evt_0_session", "evt_1_payment", "evt_2_card", "evt_3_card_only"} {
		if !r.recorded(t, tenantID, id) {
			t.Errorf("%s was not recorded after the acknowledgement", id)
		}
	}
	if n := r.count(t, `SELECT count(*) FROM payment_methods WHERE tenant_id = $1`, tenantID); n != 0 {
		t.Fatalf("%d payment methods saved, want none: their customers are lost", n)
	}
}

// A SetupIntent with no hardcount_customer_id whose Stripe customer no
// Hardcount customer holds is the tenant's own: skipped, as the live handler
// would after Stripe stopped redelivering. One whose Stripe customer a
// Hardcount customer holds saves the card on it.
func TestCatchUp_SetupIntentHardcountDidNotCreateIsSkipped(t *testing.T) {
	r := newCatchUpRig(t)
	tenantID := testutil.CreateTestTenant(t, r.db, "catchup-foreign-seti")
	r.connect(tenantID, true, "sk_live_seti")
	ctx := modeCtx(tenantID, true)
	cust, err := r.customers.Create(ctx, tenantID, domain.Customer{ExternalID: "cus_known_ext", DisplayName: "known", Email: "billing@known.test"})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.customers.SetStripeCustomerID(ctx, tenantID, cust.ID, "cus_known"); err != nil {
		t.Fatal(err)
	}
	created := time.Now().Add(-10 * time.Hour).Unix()
	r.addEvent("sk_live_seti", "evt_theirs", "setup_intent.succeeded", created,
		fmt.Sprintf(`{"id":"seti_theirs","object":"setup_intent","customer":"cus_tenants_own","payment_method":"pm_theirs","status":"succeeded","created":%d,"metadata":{}}`, created))
	r.addEvent("sk_live_seti", "evt_ours", "setup_intent.succeeded", created+1,
		fmt.Sprintf(`{"id":"seti_ours","object":"setup_intent","customer":"cus_known","payment_method":"pm_ours","status":"succeeded","created":%d,"metadata":{}}`, created+1))

	acct := onlyAccount(t, r.run(t))
	if acct.Err != nil || acct.Skipped != 1 || acct.Applied != 1 || len(acct.Unapplied) != 0 {
		t.Fatalf("report %+v, want the tenant's own SetupIntent skipped and Hardcount's applied", acct)
	}
	if r.recorded(t, tenantID, "evt_theirs") {
		t.Fatal("the skipped SetupIntent was recorded")
	}
	if n := r.count(t, `SELECT count(*) FROM payment_methods WHERE tenant_id = $1 AND customer_id = $2 AND stripe_payment_method_id = 'pm_ours'`, tenantID, cust.ID); n != 1 {
		t.Fatalf("%d payment methods pm_ours on the known customer, want 1", n)
	}
}

// On a Stripe account two tenants share, a refund of the other tenant's
// Hardcount payment is that tenant's: acknowledged, not asked for again.
func TestCatchUp_RefundOfAnotherTenantsPaymentIsAcknowledged(t *testing.T) {
	r := newCatchUpRig(t)
	tenantID := testutil.CreateTestTenant(t, r.db, "catchup-shared-refund")
	r.connect(tenantID, true, "sk_live_sharedref")
	created := time.Now().Add(-10 * time.Hour).Unix()
	r.stripe.pis["pi_other"] = paymentIntentJSON("pi_other", "hc_ten_other", "hc_inv_other", "hc_cus_other", created)
	r.addEvent("sk_live_sharedref", "evt_other_refund", "refund.created", created, refundJSON("re_other", "pi_other", 1000, created))

	acct := onlyAccount(t, r.run(t))
	if acct.Err != nil || acct.Applied != 1 || len(acct.Unapplied) != 0 {
		t.Fatalf("report %+v, want the other tenant's refund acknowledged", acct)
	}
	if !r.recorded(t, tenantID, "evt_other_refund") || len(r.refunds.recorded) != 0 {
		t.Fatalf("recorded %v, refunds %v: want the event acknowledged and no refund recorded", r.recorded(t, tenantID, "evt_other_refund"), r.refunds.recorded)
	}
}

// An event whose handler fails on every try is listed, and the events after
// it are still applied. Acknowledged, it is tried once more and, failing
// again, recorded without being applied.
func TestCatchUp_FailingEventIsListedAndTheRunGoesOn(t *testing.T) {
	r := newCatchUpRig(t)
	tenantID := testutil.CreateTestTenant(t, r.db, "catchup-failing")
	r.connect(tenantID, true, "sk_live_failing")
	paid := r.seedInvoice(t, tenantID, true, "failing-paid", domain.InvoiceFinalized)
	later := r.seedInvoice(t, tenantID, true, "failing-later", domain.InvoiceFinalized)
	created := time.Now().Add(-30 * time.Hour).Unix()
	// Newest first: a later payment, a refund with a status Hardcount does
	// not know (its handler fails every time), the paid invoice's payment.
	r.addPayment("sk_live_failing", "evt_later", "pi_later", tenantID, later.ID, later.CustomerID, created+600)
	r.addEvent("sk_live_failing", "evt_bad_refund", "refund.updated", created+60, refundWith("re_bad", "pi_paid", 1000, created+60, "not_a_status", `{}`))
	r.addPayment("sk_live_failing", "evt_paid", "pi_paid", tenantID, paid.ID, paid.CustomerID, created)

	acct := onlyAccount(t, r.run(t))
	if acct.Err != nil || acct.Applied != 2 {
		t.Fatalf("report %+v, want the two payments applied", acct)
	}
	if len(acct.Unapplied) != 1 || acct.Unapplied[0].EventID != "evt_bad_refund" || acct.Unapplied[0].Err == nil || acct.Unapplied[0].Acknowledged {
		t.Fatalf("unapplied %+v, want evt_bad_refund listed with its error", acct.Unapplied)
	}
	if got := r.invoiceStatus(t, tenantID, later.ID); got.Status != domain.InvoicePaid {
		t.Fatalf("the payment after the failing event is %s, want paid", got.Status)
	}
	if r.recorded(t, tenantID, "evt_bad_refund") {
		t.Fatal("the failing event was recorded before it was acknowledged")
	}

	acked := onlyAccount(t, r.run(t, "evt_bad_refund"))
	if acked.Err != nil || len(acked.Unapplied) != 1 || !acked.Unapplied[0].Acknowledged {
		t.Fatalf("acknowledged run %+v, want evt_bad_refund failed again and acknowledged", acked)
	}
	if !r.recorded(t, tenantID, "evt_bad_refund") {
		t.Fatal("the acknowledged event was not recorded")
	}
	if again := onlyAccount(t, r.run(t)); again.Err != nil || len(again.Unapplied) != 0 || again.AlreadyRecorded != 3 {
		t.Fatalf("later run %+v, want every event already recorded", again)
	}
}

// Two refunds of payments the restored database holds are listed instead of
// applied, each until acknowledged:
//   - a refund Hardcount made whose credit note the restore lost: applied,
//     it would be acknowledged with no record, and the invoice would show
//     the money as refundable again;
//   - a refund made in Stripe on an invoice reported to Stripe Tax, which
//     the restored database does not hold: the old server may have reversed
//     its tax already, and applying it would reverse the tax again.
//
// A refund Hardcount made whose credit note the database holds is applied.
func TestCatchUp_ListsRefundsItWouldApplyWrongly(t *testing.T) {
	r := newCatchUpRig(t)
	tenantID := testutil.CreateTestTenant(t, r.db, "catchup-refunds")
	r.connect(tenantID, true, "sk_live_refunds")
	plain := r.seedInvoice(t, tenantID, true, "refunds-plain", domain.InvoiceFinalized)
	taxed := r.seedInvoice(t, tenantID, true, "refunds-taxed", domain.InvoiceFinalized)
	r.exec(t, `UPDATE invoices SET tax_transaction_id = 'tax_txn_1' WHERE id = $1`, taxed.ID)
	r.exec(t, `INSERT INTO credit_notes (tenant_id, invoice_id, customer_id, credit_note_number, status, reason, stripe_refund_id, refund_status, issued_at, livemode)
		VALUES ($1, $2, $3, 'CN-KEPT', 'issued', 'refund', 're_kept', 'succeeded', now(), true)`, tenantID, plain.ID, plain.CustomerID)
	created := time.Now().Add(-30 * time.Hour).Unix()
	r.addEvent("sk_live_refunds", "evt_tax_refund", "refund.created", created+300, refundJSON("re_tax", "pi_taxed", 1500, created+300))
	r.addEvent("sk_live_refunds", "evt_kept_refund", "refund.updated", created+200, refundWith("re_kept", "pi_plain", 1000, created+200, "succeeded", `{"hardcount_cn_id":"hc_cn_kept"}`))
	r.addEvent("sk_live_refunds", "evt_lost_refund_2", "refund.updated", created+101, refundWith("re_lostcn", "pi_plain", 2000, created+100, "succeeded", `{"hardcount_cn_id":"hc_cn_gone"}`))
	r.addEvent("sk_live_refunds", "evt_lost_refund_1", "refund.created", created+100, refundWith("re_lostcn", "pi_plain", 2000, created+100, "pending", `{"hardcount_cn_id":"hc_cn_gone"}`))
	r.addPayment("sk_live_refunds", "evt_taxed_paid", "pi_taxed", tenantID, taxed.ID, taxed.CustomerID, created+1)
	r.addPayment("sk_live_refunds", "evt_plain_paid", "pi_plain", tenantID, plain.ID, plain.CustomerID, created)

	acct := onlyAccount(t, r.run(t))
	if acct.Err != nil || acct.Applied != 3 || acct.Held != 3 || len(acct.Unapplied) != 0 {
		t.Fatalf("report %+v, want the payments and re_kept applied, and three refund events held", acct)
	}
	if len(acct.Refunds) != 2 {
		t.Fatalf("refunds %+v, want re_lostcn and re_tax listed", acct.Refunds)
	}
	lost, tax := acct.Refunds[0], acct.Refunds[1]
	if lost.RefundID != "re_lostcn" || lost.Reason != payment.RefundCreditNoteLost || lost.CreditNoteID != "hc_cn_gone" ||
		lost.InvoiceID != plain.ID || lost.PaymentIntentID != "pi_plain" || lost.AmountCents != 2000 || lost.Currency != "USD" ||
		lost.Status != "succeeded" || lost.Events != 2 || lost.Acknowledged {
		t.Errorf("listed refund %+v, want re_lostcn of hc_cn_gone on %s, 2000 USD, succeeded, 2 events", lost, plain.ID)
	}
	if tax.RefundID != "re_tax" || tax.Reason != payment.RefundStripeTax || tax.InvoiceID != taxed.ID || tax.Events != 1 || tax.Acknowledged {
		t.Errorf("listed refund %+v, want re_tax on the Stripe Tax invoice %s", tax, taxed.ID)
	}
	if len(r.refunds.recorded) != 0 {
		t.Fatalf("refunds recorded %v, want none: re_tax is listed", r.refunds.recorded)
	}
	for _, id := range []string{"evt_tax_refund", "evt_lost_refund_1", "evt_lost_refund_2"} {
		if r.recorded(t, tenantID, id) {
			t.Fatalf("%s was recorded before its refund was acknowledged", id)
		}
	}
	if !r.recorded(t, tenantID, "evt_kept_refund") {
		t.Fatal("the refund whose credit note the database holds was not applied")
	}

	acked := onlyAccount(t, r.run(t, "re_lostcn", "re_tax"))
	if acked.Err != nil || len(acked.Refunds) != 2 || !acked.Refunds[0].Acknowledged || !acked.Refunds[1].Acknowledged {
		t.Fatalf("acknowledged run %+v", acked)
	}
	for _, id := range []string{"evt_tax_refund", "evt_lost_refund_1", "evt_lost_refund_2"} {
		if !r.recorded(t, tenantID, id) {
			t.Errorf("%s was not recorded after the acknowledgement", id)
		}
	}
	if len(r.refunds.recorded) != 0 {
		t.Fatalf("refunds recorded %v after the acknowledgement, want none", r.refunds.recorded)
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
	cu := payment.NewCatchUp(r.adapter, clients, r.creds, r.customers, r.notes)
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
	cu = payment.NewCatchUp(r.adapter, payment.NewStripeClientsWithHTTPClient(override, &http.Client{Transport: r.stripe}), r.creds, r.customers, r.notes)
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

// A key of the other mode lists only the other mode's events. The catch-up
// reports the key for the account instead of skipping every event and
// finishing with nothing applied.
func TestCatchUp_KeyOfTheOtherModeIsRefused(t *testing.T) {
	r := newCatchUpRig(t)
	tenantID := testutil.CreateTestTenant(t, r.db, "catchup-mode")
	r.connect(tenantID, true, "sk_live_but_test_events")
	created := time.Now().Add(-20 * time.Hour).Unix()
	pi := paymentIntentJSON("pi_testmode", tenantID, "hc_inv_testmode", "hc_cus_testmode", created)
	r.stripe.events["sk_live_but_test_events"] = []string{stripeEventJSON("evt_testmode", "payment_intent.succeeded", created, false, pi)}

	acct := onlyAccount(t, r.run(t))
	if acct.KeyRefused == "" || acct.Skipped != 0 || acct.Applied != 0 {
		t.Fatalf("report %+v, want the key reported as of the other mode", acct)
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
	r.addEvent("sk_live_shared", "evt_own_charge", "payment_intent.succeeded", created,
		`{"id":"pi_own","object":"payment_intent","amount":100,"currency":"usd","status":"succeeded","metadata":{}}`)

	acct := onlyAccount(t, r.run(t))
	if acct.Err != nil || acct.Skipped != 2 || acct.Applied != 0 || len(acct.Lost) != 0 {
		t.Fatalf("report %+v, want both events skipped", acct)
	}
}
