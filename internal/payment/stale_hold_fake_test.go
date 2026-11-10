package payment

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// fakeStripe is the Stripe side of the stale-hold tests (#976, ADR-127),
// ported from PR #1291's interrupted-charge fake. It models what the design
// relies on, because a test means nothing against a fake that does not behave
// like Stripe:
//
//   - a charge is two calls (two_step_fake_test.go): a create, which moves no
//     money, and a confirm on the id Hardcount saved;
//   - an idempotency key (with the card appended, as LiveStripeClient sends
//     it) returns its saved answer for the same parameters, and answers 409
//     "same key, different parameters" for other parameters;
//   - an answer is saved once the request starts executing, a 500 included;
//     a 429 is not saved (Stripe refuses it before executing);
//   - prune forgets every key, as Stripe may once a key is 24 hours old;
//   - a request can outlive the caller's context and still do its work;
//   - a read by id can fail (key rejected, Stripe not connected, a 500), can
//     hang as a network partition does until the caller's context ends, and
//     sees every PaymentIntent at once;
//   - a PaymentIntent can be refunded in full.
//
// Every scenario counts the PaymentIntents here, not only the invoice's state.
type fakeStripe struct {
	mu   sync.Mutex
	seq  int
	pis  []*fakePI
	keys map[string]*fakeKey
	// next decides how the next creates go, in order; when empty, a create
	// succeeds (unconfirmed).
	next []fakeMode
	// customers overrides how reads of a Stripe customer's PaymentIntents
	// answer: "auth" (the key is rejected), "fail" (a 500), "unreachable" (no
	// answer: a network error), "panic" (the check panics, standing in for a
	// nil dereference on one row).
	customers map[string]string
	// panicOn makes a read panic, standing in for a nil dereference on one
	// row: GetPaymentIntent for a PaymentIntent id here.
	panicOn       map[string]bool
	notConfigured bool
	// hang, when set, makes every read wait as a network partition does:
	// until the caller's context ends, or hang passes, then fail.
	hang    time.Duration
	creates int
	reads   int
	cancels []string
	// beforeRead, when set, runs at the start of each read (outside the lock).
	beforeRead func()
	// afterRead, when set, runs once a read has answered and before it is
	// returned (outside the lock): something that happens at Stripe, or a
	// webhook that lands, between the sweep's read and its write.
	afterRead func()
	// beforeCreate, when set, runs at the start of each create (outside the
	// lock): a charger stalling just before Stripe receives its request.
	beforeCreate func()
	// How the next confirms go, in order (empty: the confirm succeeds), how
	// many were sent, and a hook run at the start of each confirm (outside
	// the lock).
	confirmNext   []fakeMode
	confirms      int
	beforeConfirm func()
}

type fakeModeKind int

// The create's modes (queue) and the confirm's (queueConfirm); see each call
// for what a mode means there.
const (
	fakeCreate           fakeModeKind = iota // do the call and answer
	fakeDecline                              // confirm: decline; answer 402
	fakeSave500                              // create: save a 500 under the key, create nothing; confirm: charge, answer 500
	fakeRateLimit                            // create: 429, nothing saved, nothing created
	fakeOutliveCtx                           // wait for the caller's context to end, then do the call
	fakeHang                                 // create: no answer until the caller's context ends; nothing created
	fakeCrashAfterCreate                     // do the call, then the process dies
	fakeCrashBeforeSend                      // the process dies before the request leaves
)

type fakeMode struct {
	kind   fakeModeKind
	status string // the created PaymentIntent's status; default succeeded
}

// fakeCrash is the panic that stands for the process dying.
type fakeCrash struct{}

type fakePI struct {
	id, status, customer, card string
	key, purpose, invoiceID    string
	anchor                     string
	declineCode, failureMsg    string
	amount                     int64
	created                    int64
	refunded                   bool
}

type fakeKey struct {
	sig string
	pi  *fakePI
	err error
}

func newFakeStripe() *fakeStripe {
	return &fakeStripe{keys: map[string]*fakeKey{}, customers: map[string]string{}, panicOn: map[string]bool{}}
}

func (f *fakeStripe) queue(modes ...fakeMode) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.next = append(f.next, modes...)
}

func paramsSig(p PaymentIntentParams) string {
	var md []string
	for k, v := range p.Metadata {
		md = append(md, k+"="+v)
	}
	sort.Strings(md)
	return fmt.Sprintf("%d|%s|%s|%s|%s|%s", p.AmountCents, strings.ToLower(p.Currency), p.CustomerID, p.PaymentMethodID, p.Description, strings.Join(md, ","))
}

// stripeKey is the key Stripe sees: LiveStripeClient appends the card.
func stripeKey(p PaymentIntentParams) string { return p.IdempotencyKey + "_" + p.PaymentMethodID }

func (f *fakeStripe) newPI(p PaymentIntentParams, status string) *fakePI {
	f.seq++
	pi := &fakePI{
		id: fmt.Sprintf("pi_fake_%d", f.seq), status: status, customer: p.CustomerID, card: p.PaymentMethodID,
		key: p.IdempotencyKey, purpose: p.Metadata["hardcount_purpose"],
		invoiceID: p.Metadata["hardcount_invoice_id"], anchor: p.Metadata["hardcount_anchor_at"],
		amount: p.AmountCents, created: time.Now().Unix() + int64(f.seq), // strictly increasing: oldest first is well defined
	}
	if status == "requires_payment_method" {
		pi.declineCode, pi.failureMsg = "generic_decline", "Your card was declined."
	}
	return pi
}

func declineError(pi *fakePI) error {
	return &PaymentError{Message: pi.failureMsg, DeclineCode: pi.declineCode, PaymentIntentID: pi.id}
}

// prune forgets every idempotency key, as Stripe may once a key is 24 hours
// old ("We generate a new request if a key is reused after the original is
// pruned").
func (f *fakeStripe) prune() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.keys = map[string]*fakeKey{}
}

// addPI puts a PaymentIntent at Stripe directly: another attempt's, or a
// customer's on the hosted page.
func (f *fakeStripe) addPI(customer, invoiceID, purpose, status string, amount int64) *fakePI {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seq++
	pi := &fakePI{id: fmt.Sprintf("pi_fake_%d", f.seq), status: status, customer: customer,
		purpose: purpose, invoiceID: invoiceID, amount: amount, created: time.Now().Unix() + int64(f.seq)}
	f.pis = append(f.pis, pi)
	return pi
}

// refund refunds a PaymentIntent's charge in full.
func (f *fakeStripe) refund(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, pi := range f.pis {
		if pi.id == id {
			pi.refunded = true
		}
	}
}

// setStatus moves a PaymentIntent on at Stripe (a processing payment
// succeeding days later), with no webhook.
func (f *fakeStripe) setStatus(id, status string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, pi := range f.pis {
		if pi.id == id {
			pi.status = status
		}
	}
}

func (f *fakeStripe) countPIs() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.pis)
}

func (f *fakeStripe) readCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reads
}

func (f *fakeStripe) pi(id string) fakePI {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, pi := range f.pis {
		if pi.id == id {
			return *pi
		}
	}
	return fakePI{}
}

// piFor is the newest PaymentIntent for invoiceID.
func (f *fakeStripe) piFor(invoiceID string) fakePI {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := len(f.pis) - 1; i >= 0; i-- {
		if f.pis[i].invoiceID == invoiceID {
			return *f.pis[i]
		}
	}
	return fakePI{}
}

func (f *fakeStripe) onlyPI() fakePI {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.pis) != 1 {
		return fakePI{}
	}
	return *f.pis[0]
}

// panicOnRead makes the read of id panic (see panicOn).
func (f *fakeStripe) panicOnRead(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.panicOn[id] = true
}

func (f *fakeStripe) setCustomer(stripeCustomerID, mode string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.customers[stripeCustomerID] = mode
}

func (f *fakeStripe) result(pi *fakePI) PaymentIntentResult {
	res := PaymentIntentResult{
		ID: pi.id, Status: pi.status, CreatedAt: pi.created, Purpose: pi.purpose,
		AnchorAt: anchorFromPIMetadata(map[string]string{"hardcount_anchor_at": pi.anchor}),
	}
	setFakeListFields(&res, pi)
	if pi.status == "succeeded" {
		res.AmountReceivedCents = pi.amount
	}
	if pi.declineCode != "" {
		res.DeclineCode, res.PaymentMethodID = pi.declineCode, pi.card
	}
	return res
}

func (f *fakeStripe) CancelPaymentIntent(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, pi := range f.pis {
		if pi.id == id {
			if pi.status == "succeeded" || pi.status == "canceled" {
				return errors.New("stripe cancel: this PaymentIntent's status is " + pi.status)
			}
			pi.status = "canceled"
			f.cancels = append(f.cancels, id)
			return nil
		}
	}
	return errors.New("stripe cancel: No such payment_intent: " + id)
}

// GetPaymentIntent answers as LiveStripeClient does: the PaymentIntent, or an
// error typed by classifyReadError.
func (f *fakeStripe) GetPaymentIntent(ctx context.Context, id string) (PaymentIntentResult, error) {
	f.mu.Lock()
	hook, hang := f.beforeRead, f.hang
	f.mu.Unlock()
	if hook != nil {
		hook()
	}
	if hang > 0 {
		select {
		case <-ctx.Done():
		case <-time.After(hang):
		}
		f.mu.Lock()
		f.reads++
		f.mu.Unlock()
		// What LiveStripeClient returns when its request is cut off: no
		// answer from Stripe.
		return PaymentIntentResult{}, fakeUnreachableErr(`Get "https://api.stripe.com/v1/payment_intents/` + id + `": net/http: request canceled`)
	}
	res, err := f.readLocked(id)
	f.mu.Lock()
	after := f.afterRead
	f.mu.Unlock()
	if after != nil && err == nil {
		after()
	}
	return res, err
}

func (f *fakeStripe) readLocked(id string) (PaymentIntentResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads++
	if f.panicOn[id] {
		panic("reconcile: nil dereference on " + id)
	}
	if f.notConfigured {
		return PaymentIntentResult{}, ErrStripeNotConfigured
	}
	for _, pi := range f.pis {
		if pi.id != id {
			continue
		}
		switch f.customers[pi.customer] {
		case "auth":
			return PaymentIntentResult{}, fakeAuthFailedErr()
		case "fail":
			return PaymentIntentResult{}, &PaymentError{Message: "An unknown error occurred", Unknown: true}
		case "unreachable":
			return PaymentIntentResult{}, fakeUnreachableErr(`Get "https://api.stripe.com/v1/payment_intents/` + id + `": dial tcp: i/o timeout`)
		case "panic":
			panic("stale charge check: nil dereference on " + pi.customer)
		}
		return f.result(pi), nil
	}
	// LiveStripeClient's read of a PaymentIntent this account does not have.
	return PaymentIntentResult{}, fmt.Errorf("%w: %w", ErrStripePaymentIntentMissing, &PaymentError{Message: "No such payment_intent: '" + id + "'"})
}
