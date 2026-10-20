package billing_test

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/payment"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/tenantstripe"
)

// A charge Stripe refuses for the key (G2 design, approved before go-live),
// through the auto-charge sweep, on the real invoice store with the real
// charger and the real Stripe client. Only Stripe's HTTP replies are canned.
// The real trigger: the tenant rolls their Stripe key, or swaps in a
// restricted key without PaymentIntent write access, and every due charge
// meets a 401 or 403 until someone updates Settings → Payments.

const keyRefused401 = `{"error":{"message":"Invalid API Key provided: sk_test_****abcd","type":"invalid_request_error"}}`

// stripeReply answers every Stripe request with the current reply, counting
// requests and the most that were in flight at once.
type stripeReply struct {
	mu       sync.Mutex
	status   int
	body     string
	delay    time.Duration
	calls    int
	inFlight int
	maxIn    int
}

func (s *stripeReply) set(status int, body string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.status, s.body = status, body
}

func (s *stripeReply) RoundTrip(req *http.Request) (*http.Response, error) {
	s.mu.Lock()
	s.calls++
	s.inFlight++
	if s.inFlight > s.maxIn {
		s.maxIn = s.inFlight
	}
	status, body, delay := s.status, s.body, s.delay
	s.mu.Unlock()
	time.Sleep(delay)
	s.mu.Lock()
	s.inFlight--
	s.mu.Unlock()
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    req,
	}, nil
}

func (s *stripeReply) counts() (calls, maxIn int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls, s.maxIn
}

type oneKey struct{}

func (oneKey) GetPlaintext(context.Context, string, bool) (tenantstripe.PlaintextSecrets, error) {
	return tenantstripe.PlaintextSecrets{SecretKey: "sk_test_rolled_abcd"}, nil
}

// liveCharger is router.go's charger over the real Stripe client, sending
// through stub, with dunning started on starter.
func liveCharger(h collectHarness, stub *stripeReply, starter payment.DunningStarter) *payment.Stripe {
	client := payment.NewLiveStripeClient(payment.NewStripeClientsWithHTTPClient(oneKey{}, &http.Client{Transport: stub}))
	return payment.NewStripe(client, h.invoices, payment.NewPostgresWebhookStore(h.db), nil, starter)
}

// countingDunning counts StartDunning calls.
type countingDunning struct {
	mu sync.Mutex
	n  int
}

func (c *countingDunning) StartDunning(_ context.Context, _, invoiceID, _ string, _ time.Time, _ domain.DunningStartCause) (domain.InvoiceDunningRun, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.n++
	return domain.InvoiceDunningRun{ID: "drun_x", InvoiceID: invoiceID}, nil
}

func (c *countingDunning) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n
}

func (h collectHarness) chargeAttemptRows(t *testing.T, invoiceID string) int {
	t.Helper()
	tx, err := h.db.BeginTx(h.ctx, postgres.TxBypass, "")
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer postgres.Rollback(tx)
	var n int
	if err := tx.QueryRowContext(h.ctx, `SELECT count(*) FROM invoice_charge_attempts WHERE invoice_id = $1`, invoiceID).Scan(&n); err != nil {
		t.Fatalf("count charge attempts: %v", err)
	}
	return n
}

// assertUntouched checks the invoice is as the refused charge found it: still
// queued for the sweep, pending, no error, same seq, no attempt row, no hold.
func (h collectHarness) assertUntouched(t *testing.T, before domain.Invoice) {
	t.Helper()
	got, err := h.invoices.Get(h.ctx, h.tenantID, before.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Status != domain.InvoiceFinalized || got.PaymentStatus != domain.PaymentPending || got.LastPaymentError != "" {
		t.Errorf("invoice %s/%s error %q, want finalized/pending with no error: the key was refused, not the card",
			got.Status, got.PaymentStatus, got.LastPaymentError)
	}
	if !got.AutoChargePending {
		t.Error("auto_charge_pending cleared: the invoice was handed to dunning for a refused key")
	}
	if got.ChargeAttemptSeq != before.ChargeAttemptSeq {
		t.Errorf("charge_attempt_seq %d, want %d", got.ChargeAttemptSeq, before.ChargeAttemptSeq)
	}
	if n := h.chargeAttemptRows(t, before.ID); n != 0 {
		t.Errorf("charge attempt rows = %d, want 0", n)
	}
	if at := h.hold(t, before.ID); at != nil {
		t.Errorf("charge hold still set (%v): Collect would be blocked after the key is fixed", *at)
	}
}

// The sweep meets a refused key: the invoice stays queued, untouched, and no
// dunning starts. Once the key works, the next sweep charges it: recovery
// needs nothing from a person beyond fixing the key.
//
// Mutation seam: drop the 401/403 branch in CreatePaymentIntent → the first
// sweep marks the invoice failed and clears auto_charge_pending, and the
// second never charges it.
func TestAutoChargeSweep_RefusedKey_StaysQueuedAndRecovers(t *testing.T) {
	h := newCollectHarness(t)
	inv := h.seed(t, "INV-KEY-401")
	stub := &stripeReply{status: http.StatusUnauthorized, body: keyRefused401}
	dun := &countingDunning{}
	e := h.newEng(testPaymentSetupsReady{}, liveCharger(h, stub, dun), nil)

	for tick := 0; tick < 2; tick++ {
		e.RetryPendingCharges(h.ctx, 50, nil)
	}
	if calls, _ := stub.counts(); calls != 2 {
		t.Errorf("Stripe received %d creates over 2 ticks, want 2 (one per due invoice per tick)", calls)
	}
	h.assertUntouched(t, inv)
	if n := dun.count(); n != 0 {
		t.Errorf("dunning started %d times for a refused key", n)
	}

	// The operator pastes the new key; Stripe now charges the card.
	stub.set(http.StatusOK, `{"id":"pi_after_key_fix","object":"payment_intent","status":"succeeded","amount":5000,"currency":"usd"}`)
	e.RetryPendingCharges(h.ctx, 50, nil)
	got, err := h.invoices.Get(h.ctx, h.tenantID, inv.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Status != domain.InvoicePaid || got.StripePaymentIntentID != "pi_after_key_fix" {
		t.Fatalf("invoice %s (PI %q) after the key was fixed, want paid by pi_after_key_fix", got.Status, got.StripePaymentIntentID)
	}
}

// Collision: the auto-charge sweep and a dunning retry reach the same invoice
// while the key is refused. They share the hold, so the two never send at
// once; each refused charge writes nothing and releases the hold.
func TestAutoChargeSweepAndDunningRetry_RefusedKey_OneAtATimeNoWrites(t *testing.T) {
	h := newCollectHarness(t)
	inv := h.seed(t, "INV-KEY-RACE")
	stub := &stripeReply{status: http.StatusUnauthorized, body: keyRefused401, delay: 50 * time.Millisecond}
	dun := &countingDunning{}
	charger := liveCharger(h, stub, dun)
	e := h.newEng(testPaymentSetupsReady{}, charger, nil)

	var (
		wg     sync.WaitGroup
		mu     sync.Mutex
		claims int
	)
	retry := func() {
		// What the production retry adapter does: claim, then charge.
		hold, err := h.invoices.ClaimChargeForDunningRetry(h.ctx, h.tenantID, inv.ID)
		if err != nil || !hold.Held() {
			return
		}
		mu.Lock()
		claims++
		mu.Unlock()
		fresh, err := h.invoices.Get(h.ctx, h.tenantID, inv.ID)
		if err != nil {
			t.Errorf("get: %v", err)
			return
		}
		if _, err := charger.ChargeInvoiceForDunningRetry(h.ctx, h.tenantID, fresh, hold, "cus_stripe", "pm_card"); err == nil {
			t.Error("a dunning retry succeeded against a refused key")
		}
	}
	for i := 0; i < 3; i++ {
		wg.Add(2)
		go func() { defer wg.Done(); e.RetryPendingCharges(h.ctx, 50, nil) }()
		go func() { defer wg.Done(); retry() }()
	}
	wg.Wait()

	calls, maxIn := stub.counts()
	if maxIn > 1 {
		t.Errorf("%d charges for the invoice were at Stripe at once, want at most 1", maxIn)
	}
	if calls == 0 {
		t.Fatal("no charge reached Stripe; the race did not run")
	}
	mu.Lock()
	retryClaims := claims
	mu.Unlock()
	if retryClaims > calls {
		t.Errorf("%d dunning claims but %d Stripe calls: a claim sent nothing", retryClaims, calls)
	}
	h.assertUntouched(t, inv)
	if n := dun.count(); n != 0 {
		t.Errorf("dunning started %d times for a refused key", n)
	}
}
