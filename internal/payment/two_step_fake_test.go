package payment

import (
	"context"
	"fmt"
)

// The fake's charge: create a PaymentIntent unconfirmed, then confirm it. It keeps the rules the
// one-call fake keeps (a key returns its saved answer for the same
// parameters, a 500 is saved under the key, a request can outlive its
// caller), and adds the ones Stripe applies to a confirm, checked against
// Stripe test mode on 2026-11-09:
//
//   - a PaymentIntent that already succeeded or is processing is not charged
//     again; LiveStripeClient turns Stripe's payment_intent_unexpected_state
//     into that PaymentIntent's real state, so the fake answers with it;
//   - a decline leaves the PaymentIntent in requires_payment_method.

func (f *fakeStripe) queueConfirm(modes ...fakeMode) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.confirmNext = append(f.confirmNext, modes...)
}

// fakeConfirmNeverArrived: the confirm failed before Stripe received it (the
// caller's deadline had passed, a connection refused): an unclear answer,
// and the PaymentIntent stays requires_confirmation.
const fakeConfirmNeverArrived fakeModeKind = 100

func (f *fakeStripe) CreateUnconfirmedPaymentIntent(ctx context.Context, p PaymentIntentParams) (PaymentIntentResult, error) {
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
	switch mode.kind {
	case fakeHang:
		// No answer until the caller gives up; nothing created (yet).
		f.mu.Unlock()
		<-ctx.Done()
		return PaymentIntentResult{}, &PaymentError{Message: ctx.Err().Error(), Unknown: true}
	case fakeRateLimit:
		f.mu.Unlock()
		return PaymentIntentResult{}, fmt.Errorf("%w: stripe rate limit: Request rate limit exceeded", ErrPaymentTransient)
	case fakeCrashBeforeSend:
		f.mu.Unlock()
		panic(fakeCrash{})
	case fakeOutliveCtx:
		// Stripe creates it after the caller has gone: unconfirmed, so it
		// can take no money.
		f.mu.Unlock()
		<-ctx.Done()
		f.mu.Lock()
		pi := f.newPI(p, "requires_confirmation")
		f.pis = append(f.pis, pi)
		f.keys[stripeKey(p)] = &fakeKey{sig: paramsSig(p), pi: pi}
		f.mu.Unlock()
		return PaymentIntentResult{}, &PaymentError{Message: ctx.Err().Error(), Unknown: true}
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
	if mode.kind == fakeSave500 {
		err := &PaymentError{Message: "An unknown error occurred", Unknown: true}
		f.keys[key] = &fakeKey{sig: sig, err: err}
		f.mu.Unlock()
		return PaymentIntentResult{}, err
	}
	pi := f.newPI(p, "requires_confirmation")
	f.pis = append(f.pis, pi)
	f.keys[key] = &fakeKey{sig: sig, pi: pi}
	f.mu.Unlock()
	if mode.kind == fakeCrashAfterCreate {
		panic(fakeCrash{})
	}
	return PaymentIntentResult{ID: pi.id, Status: pi.status}, nil
}

// ConfirmPaymentIntent modes: fakeCreate succeeds (or ends in mode.status),
// fakeDecline declines, fakeSave500 charges and answers 500 (the worst
// unclear answer: money moved, the reply says nothing), fakeOutliveCtx does
// the confirm after the caller's context has ended, fakeCrashAfterCreate does
// it and then the process dies, fakeCrashBeforeSend dies before sending.
// mode.status is where a confirm that went through leaves the PaymentIntent
// (default succeeded; requires_payment_method is a decline).
func (f *fakeStripe) ConfirmPaymentIntent(ctx context.Context, id string, p PaymentIntentParams) (PaymentIntentResult, error) {
	f.mu.Lock()
	hook := f.beforeConfirm
	f.mu.Unlock()
	if hook != nil {
		hook()
	}
	f.mu.Lock()
	f.confirms++
	mode := fakeMode{kind: fakeCreate}
	if len(f.confirmNext) > 0 {
		mode, f.confirmNext = f.confirmNext[0], f.confirmNext[1:]
	}
	var pi *fakePI
	for _, x := range f.pis {
		if x.id == id {
			pi = x
		}
	}
	if pi == nil {
		f.mu.Unlock()
		return PaymentIntentResult{}, &PaymentError{Message: "No such payment_intent: '" + id + "'", PaymentIntentID: id}
	}
	if pi.status == "succeeded" || pi.status == "processing" {
		res := f.result(pi)
		f.mu.Unlock()
		return res, nil
	}
	if pi.status == "canceled" {
		// LiveStripeClient: payment_intent_unexpected_state on a canceled
		// PaymentIntent is an unclear answer naming it.
		f.mu.Unlock()
		return PaymentIntentResult{}, &PaymentError{Message: "You cannot confirm this PaymentIntent because it has a status of canceled.", PaymentIntentID: id, Unknown: true}
	}
	pi.card = p.PaymentMethodID
	switch mode.kind {
	case fakeCrashBeforeSend:
		f.mu.Unlock()
		panic(fakeCrash{})
	case fakeDecline:
		pi.status = "requires_payment_method"
		pi.declineCode, pi.failureMsg = "generic_decline", "Your card was declined."
		f.mu.Unlock()
		return PaymentIntentResult{}, declineError(pi)
	case fakeSave500:
		pi.status = "succeeded"
		f.mu.Unlock()
		return PaymentIntentResult{}, &PaymentError{Message: "An unknown error occurred", Unknown: true, PaymentIntentID: id}
	case fakeOutliveCtx:
		f.mu.Unlock()
		<-ctx.Done()
		f.mu.Lock()
		f.endConfirm(pi, mode.status)
		f.mu.Unlock()
		return PaymentIntentResult{}, &PaymentError{Message: ctx.Err().Error(), Unknown: true, PaymentIntentID: id}
	case fakeCrashAfterCreate:
		f.endConfirm(pi, mode.status)
		f.mu.Unlock()
		panic(fakeCrash{})
	case fakeConfirmNeverArrived:
		f.mu.Unlock()
		return PaymentIntentResult{}, &PaymentError{Message: "context deadline exceeded", Unknown: true, PaymentIntentID: id}
	}
	f.endConfirm(pi, mode.status)
	res := f.result(pi)
	f.mu.Unlock()
	return res, nil
}

// endConfirm leaves pi where a confirm that went through leaves it: status
// (default succeeded); requires_payment_method is a decline.
func (f *fakeStripe) endConfirm(pi *fakePI, status string) {
	if status == "" {
		status = "succeeded"
	}
	pi.status = status
	pi.declineCode, pi.failureMsg = "", ""
	if status == "requires_payment_method" {
		pi.declineCode, pi.failureMsg = "generic_decline", "Your card was declined."
	}
}
