package api

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/sagarsuperuser/velox/internal/auth"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/invoice"
	"github.com/sagarsuperuser/velox/internal/payment"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/tenantstripe"
)

// A charge Stripe refuses for the key, not the card, is "not attempted"
// (G2 design, approved before go-live). The real trigger: the tenant rolls
// their Stripe key (the old one now answers 401) or swaps in a restricted key
// without PaymentIntent write access (403). Before, both read as a definite
// failure: the invoice marked failed, a dunning attempt used, Stripe's
// sentence in the customer's email, and on the last attempt the final action.
//
// Real Postgres and production wiring: the real Stripe client (only its HTTP
// replies are canned), the real charger, the real tenantstripe service for
// the key and its Settings mark, the real dunning service and retry adapter,
// and the real Collect handler.

const (
	keyStripe401 = `{"error":{"message":"Invalid API Key provided: sk_test_****abcd","type":"invalid_request_error"}}`
	keyStripe403 = `{"error":{"message":"The provided key 'rk_test_****abcd' does not have the required permissions for this endpoint on account 'acct_x'. Having the 'rak_payment_intent_write' permission would allow this request to continue.","type":"invalid_request_error"}}`
)

// keyStripeStub answers every Stripe request with one canned reply.
type keyStripeStub struct {
	mu     sync.Mutex
	status int
	body   string
	calls  int
}

func (s *keyStripeStub) RoundTrip(req *http.Request) (*http.Response, error) {
	s.mu.Lock()
	s.calls++
	status, body := s.status, s.body
	s.mu.Unlock()
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    req,
	}, nil
}

func (s *keyStripeStub) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

// lazyDunningStarter starts dunning on the rig's dunning service, which is
// built after the charger it retries through.
type lazyDunningStarter struct{ d *dunningRetryRig }

func (l lazyDunningStarter) StartDunning(ctx context.Context, tenantID, invoiceID, customerID string, simFailedAt time.Time, cause domain.DunningStartCause) (domain.InvoiceDunningRun, error) {
	return l.d.svc.StartDunning(ctx, tenantID, invoiceID, customerID, simFailedAt, cause)
}

type keyRefusedRig struct {
	*dunningRetryRig
	stub   *keyStripeStub
	creds  *tenantstripe.Store
	live   *payment.Stripe
	setups payment.PaymentSetupStore
}

// newKeyRefusedRig wires the charger as router.go does, over the real Stripe
// client. connected stores a test-mode key for the tenant; without it the
// tenant has no key for the mode.
func newKeyRefusedRig(t *testing.T, name string, connected bool, status int, body string) *keyRefusedRig {
	t.Helper()
	d := newDunningRetryRig(t, name)
	r := &keyRefusedRig{dunningRetryRig: d, stub: &keyStripeStub{status: status, body: body}, creds: tenantstripe.NewStore(d.db)}
	if connected {
		if _, err := r.creds.Upsert(d.ctx, tenantstripe.UpsertInput{
			TenantID: d.tenantID, Livemode: false, SecretKey: "sk_test_rolled_abcd", PublishableKey: "pk_test_abcd",
		}); err != nil {
			t.Fatalf("store stripe key: %v", err)
		}
	}
	keys := tenantstripe.NewService(r.creds, nil)
	client := payment.NewLiveStripeClient(payment.NewStripeClientsWithHTTPClient(keys, &http.Client{Transport: r.stub}))
	r.setups = d.retrier.paymentSetups
	r.live = payment.NewStripe(client, d.invoices, payment.NewPostgresWebhookStore(d.db), r.setups, lazyDunningStarter{d})
	r.live.SetKeyHealth(keys)
	d.useCharger(r.live)
	return r
}

func (r *keyRefusedRig) count(q string, args ...any) int {
	r.t.Helper()
	tx, err := r.db.BeginTx(r.ctx, postgres.TxBypass, "")
	if err != nil {
		r.t.Fatalf("begin: %v", err)
	}
	defer postgres.Rollback(tx)
	var n int
	if err := tx.QueryRowContext(r.ctx, q, args...).Scan(&n); err != nil {
		r.t.Fatalf("%s: %v", q, err)
	}
	return n
}

// assertNothingWritten checks the invoice is as the refused charge found it,
// with no attempt row, no hold, no dunning run and no email.
func (r *keyRefusedRig) assertNothingWritten(before domain.Invoice) {
	r.t.Helper()
	got := r.invoice(before.ID)
	if got.Status != before.Status || got.PaymentStatus != before.PaymentStatus || got.LastPaymentError != before.LastPaymentError {
		r.t.Errorf("invoice %s/%s error %q, want %s/%s error %q: the key was refused, not the card",
			got.Status, got.PaymentStatus, got.LastPaymentError, before.Status, before.PaymentStatus, before.LastPaymentError)
	}
	if got.ChargeAttemptSeq != before.ChargeAttemptSeq {
		r.t.Errorf("charge_attempt_seq %d, want %d", got.ChargeAttemptSeq, before.ChargeAttemptSeq)
	}
	if n := r.count(`SELECT count(*) FROM invoice_charge_attempts WHERE invoice_id = $1`, before.ID); n != 0 {
		r.t.Errorf("charge attempt rows = %d, want 0: nothing was attempted", n)
	}
	if at := r.holdAt(before.ID); at != nil {
		r.t.Errorf("charge hold still set (%v): Collect would be blocked after the key is fixed", *at)
	}
	if n := r.count(`SELECT count(*) FROM invoice_dunning_runs WHERE invoice_id = $1`, before.ID); n != 0 {
		r.t.Errorf("dunning runs = %d, want 0", n)
	}
	if n := r.count(`SELECT count(*) FROM email_outbox WHERE tenant_id = $1`, r.tenantID); n != 0 {
		r.t.Errorf("emails queued = %d, want 0: no customer email for a refused key", n)
	}
}

// settingsMark is the test-mode key row's last_verified_error and its kind.
func (r *keyRefusedRig) settingsMark() (found bool, msg, kind string) {
	r.t.Helper()
	rows, err := r.creds.ListByTenant(r.ctx, r.tenantID)
	if err != nil {
		r.t.Fatalf("list stripe keys: %v", err)
	}
	for _, c := range rows {
		if !c.Livemode {
			return true, c.LastVerifiedError, c.LastVerifiedErrorKind
		}
	}
	return false, "", ""
}

func keyUnusableTotal(t *testing.T, reason string) float64 {
	t.Helper()
	mfs, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatalf("gather metrics: %v", err)
	}
	for _, mf := range mfs {
		if mf.GetName() != "velox_stripe_key_unusable_total" {
			continue
		}
		for _, m := range mf.GetMetric() {
			for _, l := range m.GetLabel() {
				if l.GetName() == "reason" && l.GetValue() == reason {
					return m.GetCounter().GetValue()
				}
			}
		}
	}
	return 0
}

// A charge (Collect, the finalize-time collect and the sweep all charge
// through ChargeInvoice) meets a refused key. Nothing is written; the hold
// is released; Settings shows the refused key; the metric counts it.
//
// Mutation seams: drop the 401/403 branch in CreatePaymentIntent → the
// invoice is failed, has an attempt row and a dunning run. Drop the
// releaseUnsentHold call → the second claim fails.
func TestChargeInvoice_RefusedKey_RealPostgres(t *testing.T) {
	t.Run("401 with a key connected", func(t *testing.T) {
		r := newKeyRefusedRig(t, "Key Refused Charge", true, http.StatusUnauthorized, keyStripe401)
		custID := r.customerWithCard("keycharge", "pm_keycharge")
		inv := r.finalizedInvoice(custID, "INV-KEY-CHARGE")
		before := keyUnusableTotal(t, "rejected")
		hold, err := r.invoices.ClaimChargeForManualCollect(r.ctx, r.tenantID, inv.ID)
		if err != nil || !hold.Held() {
			t.Fatalf("claim: held=%v err=%v", hold.Held(), err)
		}

		if _, err := r.live.ChargeInvoice(r.ctx, r.tenantID, inv, hold, "cus_stripe_keycharge", "pm_keycharge"); err == nil {
			t.Fatal("charge succeeded against a refused key")
		}
		if n := r.stub.count(); n != 1 {
			t.Fatalf("Stripe received %d creates, want 1", n)
		}
		r.assertNothingWritten(inv)
		if again, err := r.invoices.ClaimChargeForManualCollect(r.ctx, r.tenantID, inv.ID); err != nil || !again.Held() {
			t.Errorf("second claim: held=%v err=%v; the refused charge must release its hold", again.Held(), err)
		}
		if found, msg, kind := r.settingsMark(); !found || kind != "rejected" || !strings.Contains(msg, "Invalid API Key") {
			t.Errorf("Settings mark found=%v kind %q msg %q, want rejected with Stripe's sentence", found, kind, msg)
		}
		if got := keyUnusableTotal(t, "rejected") - before; got != 1 {
			t.Errorf("velox_stripe_key_unusable_total{reason=rejected} rose by %v, want 1", got)
		}
	})

	t.Run("no key for the mode", func(t *testing.T) {
		r := newKeyRefusedRig(t, "Key Missing Charge", false, http.StatusOK, `{}`)
		custID := r.customerWithCard("keymissing", "pm_keymissing")
		inv := r.finalizedInvoice(custID, "INV-KEY-MISSING")
		before := keyUnusableTotal(t, "not_configured")
		hold, err := r.invoices.ClaimChargeForManualCollect(r.ctx, r.tenantID, inv.ID)
		if err != nil || !hold.Held() {
			t.Fatalf("claim: held=%v err=%v", hold.Held(), err)
		}

		if _, err := r.live.ChargeInvoice(r.ctx, r.tenantID, inv, hold, "cus_stripe_keymissing", "pm_keymissing"); err == nil {
			t.Fatal("charge succeeded with no key")
		}
		if n := r.stub.count(); n != 0 {
			t.Fatalf("Stripe received %d requests with no key", n)
		}
		r.assertNothingWritten(inv)
		if found, _, _ := r.settingsMark(); found {
			t.Error("a key row appeared for a tenant that never connected one")
		}
		if got := keyUnusableTotal(t, "not_configured") - before; got != 1 {
			t.Errorf("velox_stripe_key_unusable_total{reason=not_configured} rose by %v, want 1", got)
		}
	})
}

// A dunning retry meets a restricted key (403): the attempt is not counted,
// no retry event or warning email, and on the last attempt the final action
// does not run, because no final charge was made.
//
// Mutation seam: make StripeKeyError wrap ErrStripeNotConfigured (a
// *PaymentError) → the retry is counted and the final action runs.
func TestDunningRetry_RefusedKey_NotCounted(t *testing.T) {
	t.Run("an ordinary attempt", func(t *testing.T) {
		r := newKeyRefusedRig(t, "Key Refused Retry", true, http.StatusForbidden, keyStripe403)
		custID := r.customerWithCard("keyretry", "pm_keyretry")
		inv := r.dunnedInvoice(custID, "INV-KEY-RETRY")
		before := r.dueRun(inv.ID, custID, 1)

		r.tick()
		if n := r.stub.count(); n != 1 {
			t.Fatalf("Stripe received %d creates, want 1", n)
		}
		r.assertNotCounted(before)
		if at := r.holdAt(inv.ID); at != nil {
			t.Errorf("charge hold still set (%v) after a refused key", *at)
		}
		if got := r.invoice(inv.ID); got.LastPaymentError != inv.LastPaymentError || got.ChargeAttemptSeq != inv.ChargeAttemptSeq {
			t.Errorf("invoice error %q seq %d, want unchanged %q seq %d", got.LastPaymentError, got.ChargeAttemptSeq, inv.LastPaymentError, inv.ChargeAttemptSeq)
		}
	})

	t.Run("the last attempt", func(t *testing.T) {
		r := newKeyRefusedRig(t, "Key Refused Final", true, http.StatusForbidden, keyStripe403)
		custID := r.customerWithCard("keyfinal", "pm_keyfinal")
		inv := r.dunnedInvoice(custID, "INV-KEY-FINAL")
		before := r.dueRun(inv.ID, custID, r.policy.MaxRetryAttempts-1)

		r.tick()
		if got := r.invoice(inv.ID); got.Status != domain.InvoiceFinalized {
			t.Errorf("invoice %s, want finalized: the final action ran after a refused key", got.Status)
		}
		if evs := r.events(before.ID, domain.DunningEventEscalated); len(evs) != 0 {
			t.Errorf("escalated events %+v, want none", evs)
		}
		r.assertNotCounted(before)
	})
}

// Collect payment meets a refused key: 502 stripe_key_unusable with the
// sentence that says where to fix it, never Stripe's text, and the invoice
// unchanged.
func TestCollectPayment_RefusedKey_SaysWhatToFix(t *testing.T) {
	r := newKeyRefusedRig(t, "Key Refused Collect", true, http.StatusUnauthorized, keyStripe401)
	custID := r.customerWithCard("keycollect", "pm_keycollect")
	inv := r.finalizedInvoice(custID, "INV-KEY-COLLECT")
	h := invoice.NewHandler(r.invSvc, nil, nil, invoice.HandlerDeps{Charger: r.live, PaymentSetups: r.setups})

	req := httptest.NewRequest(http.MethodPost, "/"+inv.ID+"/collect", nil)
	req = req.WithContext(auth.WithTenantID(r.ctx, r.tenantID))
	rec := httptest.NewRecorder()
	h.Routes().ServeHTTP(rec, req)

	body := rec.Body.String()
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status %d, want 502; body %s", rec.Code, body)
	}
	if !strings.Contains(body, `"code":"stripe_key_unusable"`) {
		t.Errorf("body %s, want code stripe_key_unusable", body)
	}
	if !strings.Contains(body, "Settings → Payments") && !strings.Contains(body, `Settings → Payments`) {
		t.Errorf("body %s, want the sentence naming Settings → Payments", body)
	}
	if strings.Contains(body, "Invalid API Key") {
		t.Errorf("body %s carries Stripe's own text", body)
	}
	r.assertNothingWritten(inv)
}
