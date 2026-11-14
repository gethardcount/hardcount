package billing

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/errs"
	"github.com/gethardcount/hardcount/internal/payment"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
)

// transientCharger refuses with payment.ErrPaymentTransient while transient is
// set, releasing the hold as the real charger does when it sent nothing;
// otherwise it records the charge and its success.
type transientCharger struct {
	recordingCharger
	transient bool
}

func (c *transientCharger) ChargeInvoice(ctx context.Context, tenantID string, inv domain.Invoice, hold domain.ChargeHold, cus, pm string) (domain.Invoice, error) {
	if c.transient {
		_ = c.store.ReleaseChargeHold(ctx, tenantID, inv.ID, hold)
		return domain.Invoice{}, payment.ErrPaymentTransient
	}
	return c.recordingCharger.ChargeInvoice(ctx, tenantID, inv, hold, cus, pm)
}

// leftHarness is one queued invoice of 1000, on a clock's queue too, with 400
// of credits to apply and a charger that can be made transient.
func leftHarness(t *testing.T) (*mockInvoices, *fakeCreditApplier, *transientCharger, *Engine) {
	t.Helper()
	inv := &mockInvoices{invoices: []domain.Invoice{pendingInvoice()}, clockQueue: map[string]bool{"inv_1": true}}
	applier := &fakeCreditApplier{inv: inv, applyCents: 400}
	charger := &transientCharger{recordingCharger: recordingCharger{store: inv}}
	pms := &fakePaymentSetups{ready: true, stripeCustomerID: "cus_stripe_1"}
	engine := wireBaseTax(NewEngine(&mockSubs{cycleUpdated: make(map[string]bool)}, &mockUsage{}, &mockPricing{}, inv, applier, &mockSettings{}, pms, charger, billingTestClock()))
	return inv, applier, charger, engine
}

// Work a collection leaves for its next run is domain.ErrLeftForNextRun. A
// clock's job returns it, so the advance fails, and the retry charges the
// invoice once, after its credits. The real-time sweep logs it and goes on;
// the invoice stays queued for the next tick.
func TestCollect_WorkLeftForTheNextRun(t *testing.T) {
	dbBlip := errors.New("db blip")
	for _, c := range []struct {
		name      string
		cause     error
		leave     func(*fakeCreditApplier, *transientCharger)
		fix       func(*fakeCreditApplier, *transientCharger)
		wantCents int64
	}{
		{"credits that could not be applied", dbBlip,
			func(a *fakeCreditApplier, _ *transientCharger) { a.err = dbBlip },
			func(a *fakeCreditApplier, _ *transientCharger) { a.err = nil },
			600},
		// The fake applies its 400 on every call, so this case has none.
		{"a transient refusal", payment.ErrPaymentTransient,
			func(a *fakeCreditApplier, ch *transientCharger) { a.applyCents = 0; ch.transient = true },
			func(_ *fakeCreditApplier, ch *transientCharger) { ch.transient = false },
			1000},
	} {
		t.Run(c.name+", on a clock", func(t *testing.T) {
			inv, applier, charger, engine := leftHarness(t)
			ctx := postgres.WithLivemode(context.Background(), false)
			c.leave(applier, charger)

			charged, errs := engine.RetryPendingChargesForClock(ctx, "t1", "tc_1", 50)
			if charged != 0 || len(errs) != 1 {
				t.Fatalf("charged %d, errors %v; want 0 and the one left for the next run", charged, errs)
			}
			if !errors.Is(errs[0], domain.ErrLeftForNextRun) || !errors.Is(errs[0], c.cause) {
				t.Errorf("error %v: want ErrLeftForNextRun, wrapping %v", errs[0], c.cause)
			}
			if len(charger.got) != 0 || !inv.invoices[0].AutoChargePending {
				t.Fatalf("charges %d, queued %v: want none, and the invoice still queued", len(charger.got), inv.invoices[0].AutoChargePending)
			}

			c.fix(applier, charger) // the advance's retry
			charged, errs = engine.RetryPendingChargesForClock(ctx, "t1", "tc_1", 50)
			if charged != 1 || len(errs) != 0 {
				t.Fatalf("retry: charged %d, errors %v; want 1 and none", charged, errs)
			}
			if len(charger.got) != 1 || charger.got[0].AmountDueCents != c.wantCents {
				t.Errorf("charges %+v: want one, of %d", charger.got, c.wantCents)
			}
		})

		t.Run(c.name+", in real time", func(t *testing.T) {
			inv, applier, charger, engine := leftHarness(t)
			c.leave(applier, charger)

			charged, errs := engine.RetryPendingCharges(postgres.WithLivemode(context.Background(), true), 50, nil)
			if charged != 0 || len(errs) != 0 {
				t.Fatalf("charged %d, errors %v; want 0 and none: the sweep logs it", charged, errs)
			}
			if !inv.invoices[0].AutoChargePending {
				t.Error("the invoice is no longer queued; the next tick must find it")
			}
		})
	}
}

// answerCharger answers every charge with err, and does what the real charger
// does on that path: releases its hold or keeps it, and stamps the invoice
// (unknown at the confirm) or finds it settled meanwhile.
type answerCharger struct {
	store    *mockInvoices
	err      error
	keepHold bool
	stamp    domain.InvoicePaymentStatus
	paid     bool
	readErr  error // fails the read of the hold after the charge
}

func (c *answerCharger) ChargeInvoice(ctx context.Context, tenantID string, inv domain.Invoice, hold domain.ChargeHold, _, _ string) (domain.Invoice, error) {
	if c.stamp != "" {
		c.store.stampPaymentStatus(inv.ID, c.stamp)
	}
	if c.paid {
		for i := range c.store.invoices {
			if c.store.invoices[i].ID == inv.ID {
				c.store.invoices[i].Status, c.store.invoices[i].PaymentStatus = domain.InvoicePaid, domain.PaymentSucceeded
			}
		}
	}
	if !c.keepHold {
		_ = c.store.ReleaseChargeHold(ctx, tenantID, inv.ID, hold)
	}
	c.store.holdReadErr = c.readErr
	return domain.Invoice{}, c.err
}

// What a test clock's failure reason shows for a charge that was not made
// (errs.SummarizeForOperator, as the advance writes it): what happened, and
// what to do, read from the invoice as stored after the charge. The real-time
// run returns none of them.
func TestCollect_WhatTheOperatorReads(t *testing.T) {
	keyRefused := &payment.StripeKeyError{Reason: payment.StripeKeyRejected, Message: "Invalid API Key provided"}
	for _, c := range []struct {
		name    string
		credits error
		charger *answerCharger
		want    string // "" = no error: nothing is left
	}{
		{"credits that could not be applied", errors.New("db blip"), nil,
			"Invoice HC-1 was not charged: its credits could not be applied first. Nothing was charged. A later run charges it (on a test clock, Retry advance)."},
		{"a transient answer, hold released", nil, &answerCharger{err: payment.ErrPaymentTransient},
			"Invoice HC-1 was not charged: Stripe could not take the charge just then. Nothing was charged. A later run charges it (on a test clock, Retry advance)."},
		{"a refused key before the confirm", nil, &answerCharger{err: keyRefused},
			"Invoice HC-1 was not charged: Stripe refused this account's API key, or none is connected. Nothing was charged. Fix the key under Settings → Payments; a later run then charges it (on a test clock, Retry advance)."},
		{"a transient answer that kept the hold (an unclear create)", nil, &answerCharger{err: payment.ErrPaymentTransient, keepHold: true},
			"Invoice HC-1: an earlier charge attempt is not resolved yet. Hardcount reads its result from Stripe on a later billing run, and a run after that charges what is left (on a test clock, Retry advance)."},
		{"a key refused at the confirm (the invoice unknown, the hold kept)", nil,
			&answerCharger{err: &payment.StripeKeyError{Reason: payment.StripeKeyRejected, AtConfirm: true}, keepHold: true, stamp: domain.PaymentUnknown},
			"Invoice HC-1: Stripe refused this account's API key while charging it, so the result of the charge is not known yet. Fix the key under Settings → Payments; Hardcount then reads the result from Stripe on a later billing run, and a run after that charges what is left (on a test clock, Retry advance)."},
		{"a transient answer, the invoice settled meanwhile", nil, &answerCharger{err: payment.ErrPaymentTransient, paid: true}, ""},
		{"a transient answer, the invoice unreadable afterwards", nil, &answerCharger{err: payment.ErrPaymentTransient, readErr: errors.New("conn reset")},
			"Invoice HC-1 was not charged: Stripe could not take the charge just then, and the invoice could not be read afterwards. A later run looks at it again (on a test clock, Retry advance)."},
	} {
		setup := func(t *testing.T) (*Engine, *mockInvoices) {
			inv, applier, _, engine := leftHarness(t)
			applier.applyCents = 0
			applier.err = c.credits
			if c.charger != nil {
				ch := *c.charger
				ch.store = inv
				engine.charger = &ch
			}
			return engine, inv
		}
		t.Run(c.name+", on a clock", func(t *testing.T) {
			engine, _ := setup(t)
			_, runErrs := engine.RetryPendingChargesForClock(postgres.WithLivemode(context.Background(), false), "t1", "tc_1", 50)
			if c.want == "" {
				if len(runErrs) != 0 {
					t.Fatalf("errors = %v, want none: nothing is left to charge", runErrs)
				}
				return
			}
			if len(runErrs) != 1 || !errors.Is(runErrs[0], domain.ErrLeftForNextRun) {
				t.Fatalf("errors = %v, want one ErrLeftForNextRun", runErrs)
			}
			got := errs.SummarizeForOperator(runErrs, "")
			if got != c.want {
				t.Errorf("the operator reads:\n  %q\nwant\n  %q", got, c.want)
			}
		})
		t.Run(c.name+", in real time", func(t *testing.T) {
			engine, _ := setup(t)
			if _, runErrs := engine.RetryPendingCharges(postgres.WithLivemode(context.Background(), true), 50, nil); len(runErrs) != 0 {
				t.Fatalf("errors = %v, want none: the run logs it", runErrs)
			}
		})
	}
}

// A charge whose transient answer kept the hold: the next run's claim is
// refused, and that run says so too, charging nothing.
func TestCollect_KeptHoldIsReportedOnTheNextRunToo(t *testing.T) {
	inv, applier, _, engine := leftHarness(t)
	applier.applyCents = 0
	ch := &answerCharger{store: inv, err: payment.ErrPaymentTransient, keepHold: true}
	engine.charger = ch
	ctx := postgres.WithLivemode(context.Background(), false)

	if _, runErrs := engine.RetryPendingChargesForClock(ctx, "t1", "tc_1", 50); len(runErrs) != 1 {
		t.Fatalf("first run: errors = %v, want the unresolved attempt", runErrs)
	}
	charged, runErrs := engine.RetryPendingChargesForClock(ctx, "t1", "tc_1", 50)
	if charged != 0 || len(runErrs) != 1 || !errors.Is(runErrs[0], domain.ErrLeftForNextRun) {
		t.Errorf("next run: charged %d, errors %v; want 0 and the unresolved attempt", charged, runErrs)
	}
}

// claimHook runs before on the store, then claims as the store does, refusing
// an invoice whose collection is paused as the real claim does.
type claimHook struct {
	*mockInvoices
	before func(*mockInvoices)
}

func (c *claimHook) ClaimAutoCharge(ctx context.Context, tenantID, id string) (domain.ChargeHold, error) {
	if c.before != nil {
		c.before(c.mockInvoices)
	}
	if c.paused[id] {
		return domain.ChargeHold{}, nil
	}
	return c.mockInvoices.ClaimAutoCharge(ctx, tenantID, id)
}

// A run whose claim of a listed invoice is refused says what is left, from the
// invoice as stored: an earlier attempt that holds it or whose result is not
// known is an error on a clock, so the advance does not end ready with the
// invoice uncharged; an invoice settled meanwhile, or whose collection is
// paused, leaves nothing to do. The real-time run returns none of them.
func TestCollect_RefusedClaimSaysWhatIsLeft(t *testing.T) {
	const unresolved = "Invoice HC-1: an earlier charge attempt is not resolved yet. Hardcount reads its result from Stripe on a later billing run, and a run after that charges what is left (on a test clock, Retry advance)."
	for _, c := range []struct {
		name   string
		before func(*mockInvoices)
		want   string // "" = no error
	}{
		{"held by an earlier attempt", func(m *mockInvoices) {
			if m.claimed == nil {
				m.claimed = map[string]domain.ChargeHold{}
			}
			m.claimed["inv_1"] = domain.ChargeHold{Until: time.Now().Add(domain.ChargeHoldTTL)}
		}, unresolved},
		{"its charge outcome unknown", func(m *mockInvoices) { m.stampPaymentStatus("inv_1", domain.PaymentUnknown) }, unresolved},
		{"paid meanwhile", func(m *mockInvoices) {
			m.invoices[0].Status, m.invoices[0].PaymentStatus = domain.InvoicePaid, domain.PaymentSucceeded
		}, ""},
		{"collection paused", func(m *mockInvoices) { m.paused = map[string]bool{"inv_1": true} }, ""},
	} {
		setup := func() (*recordingCharger, *Engine) {
			inv := &mockInvoices{invoices: []domain.Invoice{pendingInvoice()}, clockQueue: map[string]bool{"inv_1": true}}
			charger := &recordingCharger{store: inv}
			pms := &fakePaymentSetups{ready: true, stripeCustomerID: "cus_stripe_1"}
			engine := wireBaseTax(NewEngine(&mockSubs{cycleUpdated: make(map[string]bool)}, &mockUsage{}, &mockPricing{}, &claimHook{mockInvoices: inv, before: c.before}, nil, &mockSettings{}, pms, charger, billingTestClock()))
			return charger, engine
		}
		t.Run(c.name+", on a clock", func(t *testing.T) {
			charger, engine := setup()
			_, runErrs := engine.RetryPendingChargesForClock(postgres.WithLivemode(context.Background(), false), "t1", "tc_1", 50)
			if len(charger.got) != 0 {
				t.Fatalf("charged %d times, want none", len(charger.got))
			}
			if c.want == "" {
				if len(runErrs) != 0 {
					t.Fatalf("errors = %v, want none: nothing is left to charge", runErrs)
				}
				return
			}
			if len(runErrs) != 1 || !errors.Is(runErrs[0], domain.ErrLeftForNextRun) {
				t.Fatalf("errors = %v, want one ErrLeftForNextRun", runErrs)
			}
			if got := errs.SummarizeForOperator(runErrs, ""); got != c.want {
				t.Errorf("the operator reads:\n  %q\nwant\n  %q", got, c.want)
			}
		})
		t.Run(c.name+", in real time", func(t *testing.T) {
			_, engine := setup()
			if _, runErrs := engine.RetryPendingCharges(postgres.WithLivemode(context.Background(), true), 50, nil); len(runErrs) != 0 {
				t.Fatalf("errors = %v, want none: the run logs it", runErrs)
			}
		})
	}
}
