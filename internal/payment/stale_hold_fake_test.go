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
//   - an idempotency key (with the card appended, as LiveStripeClient sends
//     it) returns its saved answer for the same parameters, and answers 409
//     "same key, different parameters" for other parameters;
//   - an answer is saved once the request starts executing, a 500 included;
//     a 429 is not saved (Stripe refuses it before executing);
//   - prune forgets every key, as Stripe may once a key is 24 hours old;
//   - a request can outlive the caller's context and still create its
//     PaymentIntent;
//   - List is consistent (every PaymentIntent created is listed at once), is
//     per customer, can fail part-way, can refuse for good (customer
//     missing, key rejected, Stripe not connected), and can hang as a
//     network partition does until the caller's context ends;
//   - a PaymentIntent can be refunded in full.
//
// Every scenario counts the PaymentIntents here, not only the invoice's state.
type fakeStripe struct {
	mu   sync.Mutex
	seq  int
	pis  []*fakePI
	keys map[string]*fakeKey
	// next decides how the next creates go, in order; when empty, a create
	// succeeds.
	next []fakeMode
	// customers overrides a Stripe customer's list: "missing" (not in this
	// account), "auth" (the key is rejected), "fail" (a page fails part-way
	// with a 500), "unreachable" (no answer: a network error), "panic" (the
	// check panics, standing in for a nil dereference on one row).
	customers     map[string]string
	notConfigured bool
	// hang, when set, makes every list wait as a network partition does:
	// until the caller's context ends, or hang passes, then fail.
	hang     time.Duration
	creates  int
	lists    int
	searches int
	cancels  []string
	// beforeList, when set, runs at the start of each list (outside the lock).
	beforeList func()
	// beforeCreate, when set, runs at the start of each create (outside the
	// lock): a charger stalling just before Stripe receives its request.
	beforeCreate func()
}

type fakeModeKind int

const (
	fakeCreate           fakeModeKind = iota // create the PaymentIntent and answer
	fakeDecline                              // create it declined; answer 402
	fakeSave500                              // save a 500 under the key; create nothing
	fakeRateLimit                            // 429: nothing saved, nothing created
	fakeOutliveCtx                           // wait for the caller's context to end, then create it
	fakeCrashAfterCreate                     // create it, then the process dies
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
	return &fakeStripe{keys: map[string]*fakeKey{}, customers: map[string]string{}}
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
		key: p.IdempotencyKey, purpose: p.Metadata["velox_purpose"],
		invoiceID: p.Metadata["velox_invoice_id"], anchor: p.Metadata["velox_anchor_at"],
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

func (f *fakeStripe) CreatePaymentIntent(ctx context.Context, p PaymentIntentParams) (PaymentIntentResult, error) {
	f.mu.Lock()
	hook := f.beforeCreate
	f.mu.Unlock()
	if hook != nil {
		hook()
	}
	f.mu.Lock()
	f.creates++
	if f.notConfigured {
		f.mu.Unlock()
		return PaymentIntentResult{}, ErrStripeNotConfigured
	}
	mode := fakeMode{kind: fakeCreate}
	if len(f.next) > 0 {
		mode, f.next = f.next[0], f.next[1:]
	}
	if mode.status == "" {
		mode.status = "succeeded"
		if mode.kind == fakeDecline {
			mode.status = "requires_payment_method"
		}
	}
	switch mode.kind {
	case fakeCrashBeforeSend:
		f.mu.Unlock()
		panic(fakeCrash{})
	case fakeRateLimit:
		f.mu.Unlock()
		return PaymentIntentResult{}, fmt.Errorf("%w: stripe rate limit: Request rate limit exceeded", ErrPaymentTransient)
	}

	key, sig := stripeKey(p), paramsSig(p)
	if k, ok := f.keys[key]; ok {
		defer f.mu.Unlock()
		switch {
		case k.sig != sig:
			return PaymentIntentResult{}, &PaymentError{Message: "Keys for idempotent requests can only be used with the same parameters they were first used with.", Unknown: true}
		case k.err != nil:
			return PaymentIntentResult{}, k.err
		default:
			return PaymentIntentResult{ID: k.pi.id, Status: k.pi.status}, nil
		}
	}

	switch mode.kind {
	case fakeSave500:
		err := &PaymentError{Message: "An unknown error occurred", Unknown: true}
		f.keys[key] = &fakeKey{sig: sig, err: err}
		f.mu.Unlock()
		return PaymentIntentResult{}, err
	case fakeOutliveCtx:
		f.mu.Unlock()
		<-ctx.Done()
		f.mu.Lock()
		pi := f.newPI(p, mode.status)
		f.pis = append(f.pis, pi)
		f.keys[key] = &fakeKey{sig: sig, pi: pi}
		f.mu.Unlock()
		return PaymentIntentResult{}, fmt.Errorf("Post \"https://api.stripe.com/v1/payment_intents\": %w", ctx.Err())
	}

	pi := f.newPI(p, mode.status)
	f.pis = append(f.pis, pi)
	k := &fakeKey{sig: sig, pi: pi}
	if mode.status == "requires_payment_method" {
		k.err = declineError(pi)
	}
	f.keys[key] = k
	f.mu.Unlock()
	if mode.kind == fakeCrashAfterCreate {
		panic(fakeCrash{})
	}
	if k.err != nil {
		return PaymentIntentResult{}, k.err
	}
	return PaymentIntentResult{ID: pi.id, Status: pi.status}, nil
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

func (f *fakeStripe) searchCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.searches
}

func (f *fakeStripe) countPIs() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.pis)
}

func (f *fakeStripe) listCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lists
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

func (f *fakeStripe) onlyPI() fakePI {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.pis) != 1 {
		return fakePI{}
	}
	return *f.pis[0]
}

func (f *fakeStripe) setCustomer(stripeCustomerID, mode string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.customers[stripeCustomerID] = mode
}

func (f *fakeStripe) result(pi *fakePI) PaymentIntentResult {
	res := PaymentIntentResult{
		ID: pi.id, Status: pi.status, CreatedAt: pi.created, Purpose: pi.purpose,
		AnchorAt: anchorFromPIMetadata(map[string]string{"velox_anchor_at": pi.anchor}),
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

// ListPaymentIntentsForCustomer answers as LiveStripeClient does: every
// PaymentIntent of the customer created at or after createdGTE, or an error
// for the whole read.
func (f *fakeStripe) ListPaymentIntentsForCustomer(ctx context.Context, stripeCustomerID string, createdGTE time.Time) ([]PaymentIntentResult, error) {
	f.mu.Lock()
	hook, hang := f.beforeList, f.hang
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
		f.lists++
		f.mu.Unlock()
		// What LiveStripeClient returns when its request is cut off: no
		// answer from Stripe.
		return nil, fakeUnreachableErr(`Get "https://api.stripe.com/v1/payment_intents": net/http: request canceled`)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lists++
	if f.notConfigured {
		return nil, ErrStripeNotConfigured
	}
	switch f.customers[stripeCustomerID] {
	case "missing":
		return nil, fakeCustomerMissingErr(stripeCustomerID)
	case "auth":
		return nil, fakeAuthFailedErr()
	case "fail":
		// The first page came back and a later one failed: LiveStripeClient
		// returns the error alone, never the partial list.
		return nil, &PaymentError{Message: "An unknown error occurred", Unknown: true}
	case "unreachable":
		// Stripe never answered (a network error).
		return nil, fakeUnreachableErr(`Get "https://api.stripe.com/v1/payment_intents": dial tcp: i/o timeout`)
	case "panic":
		// Not Stripe: a check that panics on one unusual row.
		panic("stale charge check: nil dereference on " + stripeCustomerID)
	}
	var out []PaymentIntentResult
	for _, pi := range f.pis {
		if pi.customer != stripeCustomerID || pi.created < createdGTE.Unix() {
			continue
		}
		out = append(out, f.result(pi))
	}
	// Newest first, as Stripe lists.
	sort.SliceStable(out, func(i, j int) bool { return out[i].CreatedAt > out[j].CreatedAt })
	return out, nil
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

func (f *fakeStripe) GetPaymentIntent(_ context.Context, id string) (PaymentIntentResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, pi := range f.pis {
		if pi.id == id {
			return f.result(pi), nil
		}
	}
	return PaymentIntentResult{}, &PaymentError{Message: "No such payment_intent: '" + id + "'"}
}

func (f *fakeStripe) SearchPaymentIntentsByInvoiceID(_ context.Context, invoiceID string) ([]PaymentIntentResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.searches++
	var out []PaymentIntentResult
	for _, pi := range f.pis {
		if pi.invoiceID == invoiceID {
			out = append(out, f.result(pi))
		}
	}
	return out, nil
}
