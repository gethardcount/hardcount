package api

import (
	"context"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/platform/clock"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/tenantstripe"
	"github.com/gethardcount/hardcount/internal/testutil"
)

// noStripeRig is one tenant on the real router and database, with a dunning
// policy of two retries (day 3, then day 6) that writes the invoice off, a
// test clock, and a server that can send email (SMTP set, no dispatcher: each
// email stays queued in the outbox, where the test reads it).
type noStripeRig struct {
	t        *testing.T
	db       *postgres.DB
	srv      *Server
	ts       *httptest.Server
	tenantID string
	key      string
	clockID  string
	frozen   time.Time
}

func newNoStripeRig(t *testing.T, name string) *noStripeRig {
	t.Helper()
	db := testutil.SetupTestDB(t)
	t.Setenv("SMTP_HOST", "smtp.no-stripe.invalid")
	t.Setenv("PAYMENT_UPDATE_URL", "https://billing.no-stripe.test/update-payment")
	srv := NewServer(db, clock.Real(), nil)
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	r := &noStripeRig{t: t, db: db, srv: srv, ts: ts, frozen: time.Date(2027, 6, 1, 9, 0, 0, 0, time.UTC)}
	r.tenantID = testutil.CreateTestTenant(t, db, name)
	r.key = createAPIKeyWithMode(t, db, r.tenantID, false)
	r.call("POST", "/v1/dunning/policies", flowReq{body: map[string]any{
		"name": "No Stripe policy", "enabled": true, "retry_days": []int{3, 3},
		"final_subscription_action": "none", "final_invoice_action": "mark_uncollectible",
	}}).want(t, 201, "create dunning policy")
	r.clockID = flowStr(r.call("POST", "/v1/test-clocks", flowReq{body: map[string]any{
		"name": name, "frozen_time": r.frozen.Format(time.RFC3339),
	}}).want(t, 201, "create test clock").body, "id")
	return r
}

func (r *noStripeRig) call(method, path string, req flowReq) flowResp {
	r.t.Helper()
	req.bearer = r.key
	return flowDo(r.t, r.ts, method, path, req)
}

// advanceTo moves the clock to the given number of hours after its start;
// the advance runs the auto-charge sweep and the due dunning retries.
func (r *noStripeRig) advanceTo(hours int) {
	r.t.Helper()
	r.call("POST", "/v1/test-clocks/"+r.clockID+"/advance", flowReq{body: map[string]any{
		"frozen_time": r.frozen.Add(time.Duration(hours) * time.Hour).Format(time.RFC3339),
	}}).want(r.t, 200, fmt.Sprintf("advance clock to hour %d", hours))
}

// finalizedInvoice is a $49 one-off invoice, finalized, for a new customer
// with an email address and no card, on the rig's clock.
func (r *noStripeRig) finalizedInvoice(name string) string {
	r.t.Helper()
	customerID := flowStr(r.call("POST", "/v1/customers", flowReq{body: map[string]any{
		"external_id": "no-stripe-" + name, "display_name": "No Stripe " + name, "email": name + "@no-stripe.test",
		"test_clock_id": r.clockID,
	}}).want(r.t, 201, name+": create customer").body, "id")
	id := flowStr(r.call("POST", "/v1/invoices", flowReq{body: map[string]any{
		"customer_id": customerID,
		"line_items":  []map[string]any{{"description": "Seats " + name, "quantity": 1, "unit_amount_cents": 4900}},
	}}).want(r.t, 201, name+": create invoice").body, "id")
	r.call("POST", "/v1/invoices/"+id+"/finalize", flowReq{}).want(r.t, 200, name+": finalize")
	return id
}

// queuedEmail is one customer email in the outbox for an invoice.
type queuedEmail struct{ kind, ask string }

// emailsOf lists the customer emails queued for the invoice, oldest first.
func (r *noStripeRig) emailsOf(invoiceID string) []queuedEmail {
	r.t.Helper()
	tx, err := r.db.BeginTx(r.t.Context(), postgres.TxBypass, "")
	if err != nil {
		r.t.Fatalf("begin tx: %v", err)
	}
	defer postgres.Rollback(tx)
	rows, err := tx.QueryContext(r.t.Context(), `SELECT email_type, COALESCE(payload->>'decline_ask', '')
		FROM email_outbox
		WHERE tenant_id = $1 AND payload->>'invoice_number' = (SELECT invoice_number FROM invoices WHERE id = $2)
		ORDER BY created_at, id`, r.tenantID, invoiceID)
	if err != nil {
		r.t.Fatalf("read email outbox: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var out []queuedEmail
	for rows.Next() {
		var q queuedEmail
		if err := rows.Scan(&q.kind, &q.ask); err != nil {
			r.t.Fatalf("scan email outbox: %v", err)
		}
		out = append(out, q)
	}
	return out
}

// markers reads the invoice's two send-once markers.
func (r *noStripeRig) markers(invoiceID string) (invoiceEmailed, setupLinkSent bool) {
	r.t.Helper()
	return flowCount(r.t, r.db, `SELECT count(*) FROM invoices WHERE id = $1 AND invoice_emailed_at IS NOT NULL`, invoiceID) == 1,
		flowCount(r.t, r.db, `SELECT count(*) FROM invoices WHERE id = $1 AND no_pm_notified_at IS NOT NULL`, invoiceID) == 1
}

func (r *noStripeRig) runs(invoiceID string) []map[string]any {
	r.t.Helper()
	return flowList(r.call("GET", "/v1/dunning/runs?invoice_id="+invoiceID, flowReq{}).want(r.t, 200, "list dunning runs").body, "data")
}

func (r *noStripeRig) connectStripe() {
	r.t.Helper()
	if _, err := tenantstripe.NewStore(r.db).Upsert(context.Background(), tenantstripe.UpsertInput{
		TenantID: r.tenantID, Livemode: false, SecretKey: "sk_test_no_stripe_later", PublishableKey: "pk_test_no_stripe_later",
	}); err != nil {
		r.t.Fatalf("connect stripe: %v", err)
	}
}

func countKind(emails []queuedEmail, kind string) int {
	n := 0
	for _, e := range emails {
		if e.kind == kind {
			n++
		}
	}
	return n
}

func sameEmails(got []queuedEmail, want ...queuedEmail) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// TestE2E_NoStripe_EmailsTheInvoiceAndRemindsToPay walks ADR-060's
// 2026-10-30 amendment over the real router and database. With no Stripe
// connected for the invoice's mode, an owed invoice with no card is
// collected like a send-invoice tenant's:
//
//   - finalize emails the customer the invoice, once (invoice_emailed_at),
//     never a card-setup link, and starts the no-payment dunning run;
//   - the next visits of the auto-charge sweep send nothing more;
//   - the invoice's notice says Stripe isn't connected and offers no
//     Resend setup link;
//   - the retry is a reminder that asks the customer to pay the invoice, with
//     no card ask and no charge attempted;
//   - the last retry ends the run in the policy's final action (written off).
//
// Mutations: drop the collector's !connected guard (a setup link is queued);
// drop the invoice_emailed_at check (a second invoice email on the next
// visit); drop the retrier's !connected return (the reminder asks for a
// card); drop retryAsk's no-Stripe case (decline_ask add_card).
func TestE2E_NoStripe_EmailsTheInvoiceAndRemindsToPay(t *testing.T) {
	r := newNoStripeRig(t, "No Stripe Co")
	inv := r.finalizedInvoice("a")

	invoiceEmail := queuedEmail{kind: "invoice"}
	if got := r.emailsOf(inv); !sameEmails(got, invoiceEmail) {
		t.Fatalf("emails after finalize: %+v, want only the invoice email", got)
	}
	if emailed, linked := r.markers(inv); !emailed || linked {
		t.Fatalf("invoice_emailed_at set %v, no_pm_notified_at set %v; want the invoice email stamped and no setup link", emailed, linked)
	}
	runs := r.runs(inv)
	if len(runs) != 1 || flowStr(runs[0], "reason") != "no_payment_method" || flowStr(runs[0], "state") != "active" {
		t.Fatalf("dunning runs after finalize: %v, want one active no_payment_method run", runs)
	}

	// The operator's notice: no card can be added online, the invoice is
	// emailed instead, and there is no setup link to resend.
	att := flowObj(flowObj(r.call("GET", "/v1/invoices/"+inv, flowReq{}).want(t, 200, "get invoice").body, "invoice"), "attention")
	if msg := flowStr(att, "message"); flowStr(att, "code") != "payment.no_payment_method" ||
		!strings.Contains(msg, "Stripe isn't connected in this mode") || !strings.Contains(msg, "The invoice is emailed to the customer once.") {
		t.Errorf("attention %v, want the no-Stripe no-payment-method notice", att)
	}
	for _, a := range flowList(att, "actions") {
		if flowStr(a, "code") == "send_reminder" {
			t.Errorf("attention offers %v; there is no setup link to resend without Stripe", a)
		}
	}

	// The sweep visits the queued invoice again on every advance: nothing more
	// is sent before the first retry.
	r.advanceTo(24)
	r.advanceTo(48)
	if n := countKind(r.emailsOf(inv), "invoice"); n != 1 {
		t.Errorf("invoice emails after two more sweeps: %d, want still 1 (sent once)", n)
	}

	// Day 3: the first retry charges nothing and reminds the customer to pay.
	// Each fact is checked on its own, so one broken guard reads as one error.
	r.advanceTo(3*24 + 1)
	var reminders []queuedEmail
	for _, e := range r.emailsOf(inv) {
		if e.kind == "dunning_warning" {
			reminders = append(reminders, e)
		}
	}
	if len(reminders) != 1 || reminders[0].ask != "pay_invoice" {
		t.Errorf("reminders after the first retry: %+v, want one asking to pay the invoice (decline_ask pay_invoice)", reminders)
	}
	if n := flowCount(t, r.db, `SELECT count(*) FROM invoice_charge_attempts WHERE invoice_id = $1`, inv); n != 0 {
		t.Errorf("charge attempts on the invoice: %d, want 0 (no Stripe, no charge)", n)
	}
	if runs = r.runs(inv); len(runs) != 1 || flowInt(runs[0], "attempt_count") != 1 || flowStr(runs[0], "state") != "active" {
		t.Fatalf("run after the first retry: %v, want active with 1 attempt", runs)
	}

	// Day 6: the last retry ends the run in the policy's final action.
	r.advanceTo(6*24 + 1)
	if runs = r.runs(inv); len(runs) != 1 || flowStr(runs[0], "state") != "escalated" {
		t.Fatalf("run after the last retry: %v, want escalated", runs)
	}
	if status := flowStr(flowObj(r.call("GET", "/v1/invoices/"+inv, flowReq{}).want(t, 200, "get invoice").body, "invoice"), "status"); status != "uncollectible" {
		t.Errorf("invoice status after the final action: %q, want uncollectible (the policy writes it off)", status)
	}
	got := r.emailsOf(inv)
	for _, e := range got {
		if e.kind == "payment_setup_request" || e.ask == "add_card" {
			t.Errorf("email %+v asks for a card; without Stripe nothing may", e)
		}
	}
	if countKind(got, "dunning_escalation") != 1 || countKind(got, "dunning_warning") != 1 {
		t.Errorf("emails at the end: %+v, want one reminder and the final notice", got)
	}
	if n := flowCount(t, r.db, `SELECT count(*) FROM invoice_charge_attempts WHERE invoice_id = $1`, inv); n != 0 {
		t.Errorf("charge attempts on the invoice: %d, want 0", n)
	}
}

// TestE2E_NoStripe_ConnectLaterSendsTheSetupLinkOnce: the invoice was emailed
// while Stripe was not connected. Once Stripe is connected, the next visit
// sends the card-setup link, once, and the run carries on as for any card-less
// invoice: its next reminder asks for a card. The invoice email is not sent
// again.
func TestE2E_NoStripe_ConnectLaterSendsTheSetupLinkOnce(t *testing.T) {
	r := newNoStripeRig(t, "Connect Later Co")
	stripe := installStripeStub(t)
	inv := r.finalizedInvoice("b")
	invoiceEmail := queuedEmail{kind: "invoice"}
	if got := r.emailsOf(inv); !sameEmails(got, invoiceEmail) {
		t.Fatalf("emails after finalize: %+v, want only the invoice email", got)
	}

	r.connectStripe()
	r.advanceTo(24)
	r.advanceTo(48)
	setupLink := queuedEmail{kind: "payment_setup_request"}
	if got := r.emailsOf(inv); !sameEmails(got, invoiceEmail, setupLink) {
		t.Fatalf("emails after connecting Stripe and two sweeps: %+v, want the invoice email then one setup link", got)
	}
	if emailed, linked := r.markers(inv); !emailed || !linked {
		t.Fatalf("invoice_emailed_at set %v, no_pm_notified_at set %v; want both", emailed, linked)
	}

	// The same run continues: with Stripe connected and still no card, the
	// first retry is today's no-card reminder.
	r.advanceTo(3*24 + 1)
	runs := r.runs(inv)
	if len(runs) != 1 || flowInt(runs[0], "attempt_count") != 1 {
		t.Fatalf("runs after the first retry: %v, want the one run with 1 attempt", runs)
	}
	if got := r.emailsOf(inv); !sameEmails(got, invoiceEmail, setupLink, queuedEmail{kind: "dunning_warning", ask: "add_card"}) {
		t.Fatalf("emails after the first retry: %+v, want the no-card reminder last", got)
	}
	if u := stripe.unexpected(); len(u) != 0 {
		t.Errorf("unexpected Stripe calls: %v (no card, so nothing reaches Stripe)", u)
	}
}

// TestE2E_StripeReadError_SendsAndStartsNothing: a failed read of the Stripe
// connection (its stored key cannot be decrypted) is not "not connected". The
// collector counts an error, sends nothing (neither the invoice email nor a
// setup link), starts no dunning, and leaves the invoice queued for the next
// visit. Mutation: treat the read error as not connected (the invoice email
// is queued and the run starts).
func TestE2E_StripeReadError_SendsAndStartsNothing(t *testing.T) {
	r := newNoStripeRig(t, "Stripe Read Error Co")
	r.connectStripe()
	tx, err := r.db.BeginTx(t.Context(), postgres.TxBypass, "")
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	if _, err := tx.ExecContext(t.Context(), `UPDATE stripe_provider_credentials SET secret_key_encrypted = 'enc:not-base64!' WHERE tenant_id = $1`, r.tenantID); err != nil {
		postgres.Rollback(tx)
		t.Fatalf("break the stored key: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	inv := r.finalizedInvoice("c")
	if got := r.emailsOf(inv); len(got) != 0 {
		t.Fatalf("emails after finalize: %+v, want none", got)
	}
	if runs := r.runs(inv); len(runs) != 0 {
		t.Fatalf("dunning runs: %v, want none", runs)
	}

	// The sweep's own visit reports it as an error.
	_, errs := r.srv.BillingEngine.RetryPendingChargesForClock(postgres.WithLivemode(context.Background(), false), r.tenantID, r.clockID, 100)
	if len(errs) != 1 || !strings.Contains(errs[0].Error(), "read the Stripe connection") {
		t.Fatalf("sweep errors %v, want one Stripe-connection read error", errs)
	}
	if got := r.emailsOf(inv); len(got) != 0 {
		t.Errorf("emails after the sweep: %+v, want none", got)
	}
	if runs := r.runs(inv); len(runs) != 0 {
		t.Errorf("dunning runs after the sweep: %v, want none", runs)
	}
	if emailed, linked := r.markers(inv); emailed || linked {
		t.Errorf("markers set (invoice emailed %v, setup link %v), want neither", emailed, linked)
	}
	if n := flowCount(t, r.db, `SELECT count(*) FROM invoices WHERE id = $1 AND auto_charge_pending AND auto_charge_claimed_until IS NULL`, inv); n != 1 {
		t.Errorf("invoice not left queued with its hold released")
	}
}
