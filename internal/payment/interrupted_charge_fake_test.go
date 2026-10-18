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

// fakeStripe is the Stripe side of the interrupted-charge tests (ADR-127). It
// models what the design relies on, because a mutation check means nothing
// against a fake that does not behave like Stripe:
//
//   - an idempotency key returns its saved answer for the same parameters,
//     answers 409 "same key, different parameters" for other parameters, and
//     409 "key in use" while its first request is still running;
//   - an answer is saved once the request starts executing, a 500 included;
//     a 429 is not saved (Stripe refuses it before executing);
//   - prune forgets every key, as Stripe may once a key is 24 hours old;
//   - a request can outlive the caller's context and still create its
//     PaymentIntent;
//   - List is consistent (every PaymentIntent created is listed at once), it
//     needs the customer to exist in this account, and it can fail part-way.
//
// It answers with what LiveStripeClient returns for each Stripe reply (the
// mapping is pinned against stripe-go's decoder in stripe_client_list_test.go
// and stripe_409_test.go).
type fakeStripe struct {
	mu   sync.Mutex
	seq  int
	pis  []*fakePI
	keys map[string]*fakeKey
	// next decides how the next creates go, in order; when empty, a create
	// succeeds.
	next []fakeMode
	// customers overrides a Stripe customer: "deleted", or "missing" (never
	// existed in this account, or another account's).
	customers     map[string]string
	listPageFails bool // a list fails on its second page
	notConfigured bool
	creates       int
	lists         int
	cancels       []string
	// beforeList, when set, runs at the start of each list (outside the lock).
	beforeList func()
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
	fakeKeyInUse                             // the first request is still running at Stripe: 409; completeInFlight creates it
)

type fakeMode struct {
	kind   fakeModeKind
	status string // the created PaymentIntent's status; default succeeded
}

// fakeCrash is the panic that stands for the process dying.
type fakeCrash struct{}

type fakePI struct {
	id, status, customer, card string
	attemptKey, purpose        string
	invoiceID, anchor          string
	declineCode, failureMsg    string
	amount                     int64
	created                    int64
}

type fakeKey struct {
	sig     string
	pi      *fakePI
	err     error
	running bool
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

func (f *fakeStripe) newPI(p PaymentIntentParams, status string) *fakePI {
	f.seq++
	pi := &fakePI{
		id: fmt.Sprintf("pi_fake_%d", f.seq), status: status, customer: p.CustomerID, card: p.PaymentMethodID,
		attemptKey: p.Metadata["velox_attempt_key"], purpose: p.Metadata["velox_purpose"],
		invoiceID: p.Metadata["velox_invoice_id"], anchor: p.Metadata["velox_anchor_at"],
		amount: p.AmountCents, created: time.Now().Unix(),
	}
	if status == "requires_payment_method" {
		pi.declineCode, pi.failureMsg = "generic_decline", "Your card was declined."
	}
	return pi
}

func (f *fakeStripe) result(pi *fakePI) PaymentIntentResult {
	res := PaymentIntentResult{
		ID: pi.id, Status: pi.status, CreatedAt: pi.created, Purpose: pi.purpose,
		AttemptKey: pi.attemptKey, AnchorAt: anchorFromPIMetadata(map[string]string{"velox_anchor_at": pi.anchor}),
	}
	if pi.status == "succeeded" {
		res.AmountReceivedCents = pi.amount
	}
	if pi.declineCode != "" {
		res.DeclineCode, res.PaymentMethodID, res.FailureMessage = pi.declineCode, pi.card, pi.failureMsg
	}
	return res
}

func declineError(pi *fakePI) error {
	return &PaymentError{Message: pi.failureMsg, DeclineCode: pi.declineCode, PaymentIntentID: pi.id}
}

func (f *fakeStripe) CreatePaymentIntent(ctx context.Context, p PaymentIntentParams) (PaymentIntentResult, error) {
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

	sig := paramsSig(p)
	if k, ok := f.keys[p.IdempotencyKey]; ok {
		defer f.mu.Unlock()
		switch {
		case k.running:
			return PaymentIntentResult{}, &PaymentError{Message: "The idempotency key provided is currently being used in another request.", Unknown: true}
		case k.sig != sig:
			return PaymentIntentResult{}, &PaymentError{Message: "Keys for idempotent requests can only be used with the same parameters they were first used with.", Unknown: true, IdempotencyConflict: true}
		case k.err != nil:
			return PaymentIntentResult{}, k.err
		default:
			return PaymentIntentResult{ID: k.pi.id, Status: k.pi.status}, nil
		}
	}

	switch mode.kind {
	case fakeSave500:
		err := &PaymentError{Message: "An unknown error occurred", Unknown: true}
		f.keys[p.IdempotencyKey] = &fakeKey{sig: sig, err: err}
		f.mu.Unlock()
		return PaymentIntentResult{}, err
	case fakeKeyInUse:
		pi := f.newPI(p, mode.status)
		f.keys[p.IdempotencyKey] = &fakeKey{sig: sig, pi: pi, running: true}
		f.mu.Unlock()
		return PaymentIntentResult{}, &PaymentError{Message: "The idempotency key provided is currently being used in another request.", Unknown: true}
	case fakeOutliveCtx:
		f.mu.Unlock()
		<-ctx.Done()
		f.mu.Lock()
		pi := f.newPI(p, mode.status)
		f.pis = append(f.pis, pi)
		f.keys[p.IdempotencyKey] = &fakeKey{sig: sig, pi: pi}
		f.mu.Unlock()
		return PaymentIntentResult{}, fmt.Errorf("Post \"https://api.stripe.com/v1/payment_intents\": %w", ctx.Err())
	}

	pi := f.newPI(p, mode.status)
	f.pis = append(f.pis, pi)
	k := &fakeKey{sig: sig, pi: pi}
	if mode.status == "requires_payment_method" {
		k.err = declineError(pi)
	}
	f.keys[p.IdempotencyKey] = k
	f.mu.Unlock()
	if mode.kind == fakeCrashAfterCreate {
		panic(fakeCrash{})
	}
	if k.err != nil {
		return PaymentIntentResult{}, k.err
	}
	return PaymentIntentResult{ID: pi.id, Status: pi.status}, nil
}

// completeInFlight finishes every request still running at Stripe: its
// PaymentIntent now exists.
func (f *fakeStripe) completeInFlight() {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, k := range f.keys {
		if k.running {
			k.running = false
			f.pis = append(f.pis, k.pi)
		}
	}
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
func (f *fakeStripe) addPI(customer, invoiceID, attemptKey, status string, amount int64) *fakePI {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seq++
	pi := &fakePI{id: fmt.Sprintf("pi_fake_%d", f.seq), status: status, customer: customer,
		attemptKey: attemptKey, invoiceID: invoiceID, amount: amount, created: time.Now().Unix()}
	f.pis = append(f.pis, pi)
	return pi
}

func (f *fakeStripe) countPIs() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.pis)
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

func (f *fakeStripe) ListPaymentIntentsForCustomer(_ context.Context, stripeCustomerID string, createdGTE time.Time) ([]PaymentIntentResult, error) {
	f.mu.Lock()
	hook := f.beforeList
	f.mu.Unlock()
	if hook != nil {
		hook()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lists++
	if f.notConfigured {
		return nil, ErrStripeNotConfigured
	}
	switch f.customers[stripeCustomerID] {
	case "deleted":
		return nil, &PaymentError{Message: "customer " + stripeCustomerID + " is deleted"}
	case "missing":
		return nil, &PaymentError{Message: "No such customer: '" + stripeCustomerID + "'"}
	}
	if f.listPageFails {
		// The first page came back and a later one failed. LiveStripeClient
		// then returns the error alone (stripe_client_list_test.go pins it).
		return nil, &PaymentError{Message: "An unknown error occurred", Unknown: true}
	}
	var out []PaymentIntentResult
	for _, pi := range f.pis {
		if pi.customer != stripeCustomerID || pi.created < createdGTE.Unix() {
			continue
		}
		out = append(out, f.result(pi))
	}
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
	var out []PaymentIntentResult
	for _, pi := range f.pis {
		if pi.invoiceID == invoiceID {
			out = append(out, f.result(pi))
		}
	}
	return out, nil
}
