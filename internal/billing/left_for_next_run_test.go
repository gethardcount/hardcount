package billing

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/payment"
	"github.com/gethardcount/hardcount/internal/platform/clock"
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

// leftHarness is one queued invoice of 1000, with 400 of credits to apply and a charger that can be made transient. The store
// answers that the invoice is claimable, as the real one does for a queued,
// pending invoice with no hold, no hosted page and no pause.
func leftHarness(t *testing.T) (*mockInvoices, *fakeCreditApplier, *transientCharger, *Engine) {
	t.Helper()
	inv := &mockInvoices{invoices: []domain.Invoice{pendingInvoice()},
		claimState: map[string]domain.ChargeClaimState{"inv_1": domain.ChargeClaimable}}
	applier := &fakeCreditApplier{inv: inv, applyCents: 400}
	charger := &transientCharger{recordingCharger: recordingCharger{store: inv}}
	pms := &fakePaymentSetups{ready: true, stripeCustomerID: "cus_stripe_1"}
	engine := wireBaseTax(NewEngine(&mockSubs{cycleUpdated: make(map[string]bool)}, &mockUsage{}, &mockPricing{}, inv, applier, &mockSettings{}, pms, charger, billingTestClock()))
	return inv, applier, charger, engine
}

// Work a collection leaves for its next run is domain.ErrLeftForNextRun. The
// sweep logs it and goes on; the invoice stays queued for the next tick.
func TestCollect_WorkLeftForTheNextRun(t *testing.T) {
	for _, c := range []struct {
		name  string
		leave func(*fakeCreditApplier, *transientCharger)
	}{
		{"credits that could not be applied",
			func(a *fakeCreditApplier, _ *transientCharger) { a.err = errors.New("db blip") }},
		{"a transient refusal",
			func(a *fakeCreditApplier, ch *transientCharger) { a.applyCents = 0; ch.transient = true }},
	} {
		t.Run(c.name, func(t *testing.T) {
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

// answerCharger answers every charge with err, releasing its hold or keeping
// it as the real charger does on that path. After the charge, the store
// answers state for the invoice, or fails the read with readErr.
type answerCharger struct {
	store    *mockInvoices
	err      error
	keepHold bool
	state    domain.ChargeClaimState
	readErr  error
}

func (c *answerCharger) ChargeInvoice(ctx context.Context, tenantID string, inv domain.Invoice, hold domain.ChargeHold, _, _ string) (domain.Invoice, error) {
	if !c.keepHold {
		_ = c.store.ReleaseChargeHold(ctx, tenantID, inv.ID, hold)
	}
	c.store.mu.Lock()
	c.store.claimState[inv.ID], c.store.claimStateErr = c.state, c.readErr
	c.store.mu.Unlock()
	return domain.Invoice{}, c.err
}

// warnLogs collects what the default logger writes at Warn and above, for the
// rest of the test.
func warnLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &logs
}

// What the run does for a charge that was not made, for each answer the store
// gives after the charge: it returns none of them, and logs a Warn for each
// that left a charge to make (nothing to charge, a pause and the hosted page
// leave it nothing to do); a failed read is an error.
func TestCollect_WhatTheOperatorReads(t *testing.T) {
	keyRefused := &payment.StripeKeyError{Reason: payment.StripeKeyRejected, Message: "Invalid API Key provided"}
	transient := func(state domain.ChargeClaimState) *answerCharger {
		return &answerCharger{err: payment.ErrPaymentTransient, state: state}
	}
	const (
		nothing    = iota // nothing left for this run: no error, no Warn
		left              // a charge left for the next run: no error, a Warn
		readFailed        // what is left could not be read: an error
	)
	for _, c := range []struct {
		name    string
		credits error
		charger *answerCharger
		want    int
	}{
		{"credits that could not be applied", errors.New("db blip"), nil, left},
		{"a transient answer, hold released", nil, transient(domain.ChargeClaimable), left},
		{"a refused key before the confirm", nil, &answerCharger{err: keyRefused, state: domain.ChargeClaimable}, left},
		{"a transient answer that kept the hold (an unclear create)", nil,
			&answerCharger{err: payment.ErrPaymentTransient, keepHold: true, state: domain.ChargeHeldByAttempt}, left},
		{"a key refused at the confirm (the invoice unknown, the hold kept)", nil,
			&answerCharger{err: &payment.StripeKeyError{Reason: payment.StripeKeyRejected, AtConfirm: true}, keepHold: true, state: domain.ChargeHeldByAttempt}, left},
		{"a transient answer, the invoice settled meanwhile", nil, transient(domain.ChargeNothingToCharge), nothing},
		{"a transient answer, collection paused meanwhile", nil, transient(domain.ChargeCollectionPaused), nothing},
		{"a transient answer, the customer on the hosted page", nil, transient(domain.ChargePayingOnHostedPage), nothing},
		{"a transient answer, the read failing", nil, &answerCharger{err: payment.ErrPaymentTransient, readErr: errors.New("conn reset")}, readFailed},
	} {
		setup := func(t *testing.T) *Engine {
			inv, applier, _, engine := leftHarness(t)
			applier.applyCents = 0
			applier.err = c.credits
			if c.charger != nil {
				ch := *c.charger
				ch.store = inv
				engine.charger = &ch
			}
			return engine
		}
		t.Run(c.name, func(t *testing.T) {
			engine := setup(t)
			logs := warnLogs(t)
			_, runErrs := engine.RetryPendingCharges(postgres.WithLivemode(context.Background(), true), 50, nil)
			if want := map[bool]int{true: 1, false: 0}[c.want == readFailed]; len(runErrs) != want {
				t.Fatalf("errors = %v, want %d: the run logs what is left", runErrs, want)
			}
			// A charge that was tried and not made is a Warn, as #1751 has it;
			// nothing left to charge is not.
			if warned := strings.Contains(logs.String(), "auto-charge: left for the next run"); warned != (c.want == left) {
				t.Errorf("warned = %v; logs:\n%s", warned, logs.String())
			}
		})
	}
}

// refusingClaims refuses every claim, as the store does for each of its
// refusal reasons; the store's answer says which.
type refusingClaims struct{ *mockInvoices }

func (refusingClaims) ClaimAutoCharge(context.Context, string, string) (domain.ChargeHold, error) {
	return domain.ChargeHold{}, nil
}

// A run whose claim of a listed invoice is refused returns no error and logs
// nothing at Warn or above, whatever the store answers about what is left, a
// failed read included: a refused claim is two collectors meeting.
func TestCollect_RefusedClaimSaysWhatIsLeft(t *testing.T) {
	for _, c := range []struct {
		name    string
		state   domain.ChargeClaimState
		readErr error
	}{
		{"an earlier attempt has not finished", domain.ChargeHeldByAttempt, nil},
		{"claimable after all", domain.ChargeClaimable, nil},
		{"nothing to charge", domain.ChargeNothingToCharge, nil},
		{"collection paused", domain.ChargeCollectionPaused, nil},
		{"the customer on the hosted page", domain.ChargePayingOnHostedPage, nil},
		{"the read failing", 0, errors.New("conn reset")},
	} {
		t.Run(c.name, func(t *testing.T) {
			inv := &mockInvoices{invoices: []domain.Invoice{pendingInvoice()},
				claimState: map[string]domain.ChargeClaimState{"inv_1": c.state}, claimStateErr: c.readErr}
			charger := &recordingCharger{store: inv}
			pms := &fakePaymentSetups{ready: true, stripeCustomerID: "cus_stripe_1"}
			engine := wireBaseTax(NewEngine(&mockSubs{cycleUpdated: make(map[string]bool)}, &mockUsage{}, &mockPricing{}, refusingClaims{inv}, nil, &mockSettings{}, pms, charger, billingTestClock()))
			logs := warnLogs(t)
			ctx := postgres.WithLivemode(context.Background(), true)
			if _, runErrs := engine.RetryPendingCharges(ctx, 50, nil); len(runErrs) != 0 {
				t.Fatalf("errors = %v, want none: a refused claim is quiet in real time", runErrs)
			}
			engine.CollectInvoice(ctx, "t1", "inv_1", nil) // the finalize-time collect, the race's other side
			if logs.Len() != 0 {
				t.Errorf("a refused claim logged at Warn or above in real time:\n%s", logs.String())
			}
		})
	}
}

// An invoice collected right after it is made (FinalizeOnCreateInvoice) is
// collected at the instant bound on its context, not at its clock's frozen
// time, and its credits are applied then. On a clock its callers bind a
// contracted instant earlier than the advance's target: the clock's trial job
// a trial's end, for its first invoice, and dunning's cancel in a catch-up
// the escalation, for its final invoice. (A trial that ends inside a period
// is collected at the period's close:
// TestBillOnePeriod_ATrialEndingInThePeriodIsCollectedAtTheClose.)
func TestFinalizeOnCreateInvoice_CollectsAtTheCallersInstant(t *testing.T) {
	frozen := time.Date(2026, 5, 20, 0, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		name string
		at   time.Time
	}{
		{"an invoice collected under a context bound to a trial's end", time.Date(2026, 5, 3, 0, 0, 0, 0, time.UTC)},
		{"an invoice collected under a context bound to an escalation", time.Date(2026, 5, 10, 6, 0, 0, 0, time.UTC)},
	} {
		t.Run(c.name, func(t *testing.T) {
			inv, applier, _, engine := leftHarness(t)
			inv.invoices[0].IsSimulated = true
			engine.SetCustomerReader(&stubCustomerReader{customers: map[string]domain.Customer{
				"cus_1": {ID: "cus_1", TenantID: "t1", TestClockID: "tc_1"},
			}})
			engine.SetTestClockReader(&stubClockReader{clk: domain.TestClock{ID: "tc_1", FrozenTime: frozen}})
			ctx := clock.WithSim(postgres.WithLivemode(context.Background(), false), clock.Sim{At: c.at, TestClockID: "tc_1"})

			engine.FinalizeOnCreateInvoice(ctx, domain.Subscription{ID: "sub_1", TenantID: "t1", TestClockID: "tc_1"}, inv.invoices[0])
			if len(applier.at) != 1 || !applier.at[0].Equal(c.at) {
				t.Errorf("credits applied at %v; want once, at %v (not the clock's frozen %v)", applier.at, c.at, frozen)
			}
		})
	}
}
