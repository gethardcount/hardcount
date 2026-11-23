package billing

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/errs"
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

// leftHarness is one queued invoice of 1000, on a clock's queue too, with 400
// of credits to apply and a charger that can be made transient. The store
// answers that the invoice is claimable, as the real one does for a queued,
// pending invoice with no hold, no hosted page and no pause.
func leftHarness(t *testing.T) (*mockInvoices, *fakeCreditApplier, *transientCharger, *Engine) {
	t.Helper()
	inv := &mockInvoices{invoices: []domain.Invoice{pendingInvoice()}, clockQueue: map[string]bool{"inv_1": true},
		claimState: map[string]domain.ChargeClaimState{"inv_1": domain.ChargeClaimable}}
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

// The sentences the operator reads, one per answer.
const (
	readsHeld       = "Invoice HC-1: an earlier charge attempt has not finished, so it was not charged again: it cannot be charged twice. Hardcount checks that attempt on a billing run, within the hour. Retry advance once the attempt is done; in real time a later run charges what is left."
	readsHeldKey    = "Invoice HC-1: Stripe refused this account's API key while charging it, so the result of that charge is not known, and the invoice is not charged again until it is: it cannot be charged twice. Fix the key under Settings → Payments; Hardcount then reads the result on a billing run, within the hour. Retry advance once it has; in real time a later run charges what is left."
	readsKey        = "Invoice HC-1 was not charged: Stripe refused this account's API key, or none is connected. Nothing was charged. Fix the key under Settings → Payments, then Retry advance; in real time a later run charges it."
	readsNotMadeWhy = "Invoice HC-1 was not charged: %s. Nothing was charged. Retry advance; in real time a later run charges it."
	readsReadFailed = "and what is left of it could not be read: conn reset"
)

// What a test clock's failure reason shows for a charge that was not made
// (errs.SummarizeForOperator, as the advance writes it), for each answer the
// store gives after the charge. Nothing to charge, a pause and the hosted page
// leave this job nothing to do. The real-time run returns none of them; a
// failed read is a failed read in both.
func TestCollect_WhatTheOperatorReads(t *testing.T) {
	keyRefused := &payment.StripeKeyError{Reason: payment.StripeKeyRejected, Message: "Invalid API Key provided"}
	transient := func(state domain.ChargeClaimState) *answerCharger {
		return &answerCharger{err: payment.ErrPaymentTransient, state: state}
	}
	for _, c := range []struct {
		name    string
		credits error
		charger *answerCharger
		want    string // "" = no error; readsReadFailed = a failed read, compared by suffix
	}{
		{"credits that could not be applied", errors.New("db blip"), nil, fmt.Sprintf(readsNotMadeWhy, "its credits could not be applied first")},
		{"a transient answer, hold released", nil, transient(domain.ChargeClaimable), fmt.Sprintf(readsNotMadeWhy, "Stripe could not take the charge just then")},
		{"a refused key before the confirm", nil, &answerCharger{err: keyRefused, state: domain.ChargeClaimable}, readsKey},
		{"a transient answer that kept the hold (an unclear create)", nil,
			&answerCharger{err: payment.ErrPaymentTransient, keepHold: true, state: domain.ChargeHeldByAttempt}, readsHeld},
		{"a key refused at the confirm (the invoice unknown, the hold kept)", nil,
			&answerCharger{err: &payment.StripeKeyError{Reason: payment.StripeKeyRejected, AtConfirm: true}, keepHold: true, state: domain.ChargeHeldByAttempt}, readsHeldKey},
		{"a transient answer, the invoice settled meanwhile", nil, transient(domain.ChargeNothingToCharge), ""},
		{"a transient answer, collection paused meanwhile", nil, transient(domain.ChargeCollectionPaused), ""},
		{"a transient answer, the customer on the hosted page", nil, transient(domain.ChargePayingOnHostedPage), ""},
		{"a transient answer, the read failing", nil, &answerCharger{err: payment.ErrPaymentTransient, readErr: errors.New("conn reset")}, readsReadFailed},
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
		t.Run(c.name+", on a clock", func(t *testing.T) {
			_, runErrs := setup(t).RetryPendingChargesForClock(postgres.WithLivemode(context.Background(), false), "t1", "tc_1", 50)
			switch c.want {
			case "":
				if len(runErrs) != 0 {
					t.Fatalf("errors = %v, want none: nothing is this job's to do", runErrs)
				}
			case readsReadFailed:
				if len(runErrs) != 1 || errors.Is(runErrs[0], domain.ErrLeftForNextRun) || !strings.HasSuffix(runErrs[0].Error(), readsReadFailed) {
					t.Fatalf("errors = %v, want one failed read, not ErrLeftForNextRun", runErrs)
				}
			default:
				if len(runErrs) != 1 || !errors.Is(runErrs[0], domain.ErrLeftForNextRun) {
					t.Fatalf("errors = %v, want one ErrLeftForNextRun", runErrs)
				}
				if got := errs.SummarizeForOperator(runErrs, ""); got != c.want {
					t.Errorf("the operator reads:\n  %q\nwant\n  %q", got, c.want)
				}
			}
		})
		t.Run(c.name+", in real time", func(t *testing.T) {
			engine := setup(t)
			logs := warnLogs(t)
			_, runErrs := engine.RetryPendingCharges(postgres.WithLivemode(context.Background(), true), 50, nil)
			if want := map[bool]int{true: 1, false: 0}[c.want == readsReadFailed]; len(runErrs) != want {
				t.Fatalf("errors = %v, want %d: the run logs what is left", runErrs, want)
			}
			// A charge that was tried and not made is a Warn, as #1751 has it;
			// nothing left to charge is not.
			if warned := strings.Contains(logs.String(), "auto-charge: left for the next run"); warned != (c.want != "" && c.want != readsReadFailed) {
				t.Errorf("warned = %v; logs:\n%s", warned, logs.String())
			}
		})
	}
}

// A charge whose transient answer kept the hold: the next run's claim is
// refused, and on a clock that run says so too, charging nothing.
func TestCollect_KeptHoldIsReportedOnTheNextRunToo(t *testing.T) {
	inv, applier, _, engine := leftHarness(t)
	applier.applyCents = 0
	engine.charger = &answerCharger{store: inv, err: payment.ErrPaymentTransient, keepHold: true, state: domain.ChargeHeldByAttempt}
	ctx := postgres.WithLivemode(context.Background(), false)

	if _, runErrs := engine.RetryPendingChargesForClock(ctx, "t1", "tc_1", 50); len(runErrs) != 1 {
		t.Fatalf("first run: errors = %v, want the unfinished attempt", runErrs)
	}
	charged, runErrs := engine.RetryPendingChargesForClock(ctx, "t1", "tc_1", 50)
	if charged != 0 || len(runErrs) != 1 || !refusedClaim(runErrs[0]) || errs.SummarizeForOperator(runErrs, "") != readsHeld {
		t.Errorf("next run: charged %d, errors %v; want 0 and the refused claim, read as the unfinished attempt", charged, runErrs)
	}
}

// refusingClaims refuses every claim, as the store does for each of its
// refusal reasons; the store's answer says which.
type refusingClaims struct{ *mockInvoices }

func (refusingClaims) ClaimAutoCharge(context.Context, string, string) (domain.ChargeHold, error) {
	return domain.ChargeHold{}, nil
}

// A run whose claim of a listed invoice is refused does what the store's
// answer says, the same as at the other exits that send no charge: an
// earlier attempt that has not finished, or a claim that would be taken now
// (the other attempt let go in between), is an error on a clock, so the
// advance does not end ready with the invoice uncharged; nothing to charge, a
// pause, or the customer on the hosted page leaves this job nothing to do.
// The real-time run returns none of them and stays as quiet as before (a
// refused claim is two collectors meeting); a failed read is a failed read
// on a clock.
func TestCollect_RefusedClaimSaysWhatIsLeft(t *testing.T) {
	for _, c := range []struct {
		name    string
		state   domain.ChargeClaimState
		readErr error
		want    string // "" = no error; readsReadFailed = a failed read
	}{
		{"an earlier attempt has not finished", domain.ChargeHeldByAttempt, nil, readsHeld},
		{"claimable after all", domain.ChargeClaimable, nil, fmt.Sprintf(readsNotMadeWhy, "another charge of it was running just then, and has since stopped")},
		{"nothing to charge", domain.ChargeNothingToCharge, nil, ""},
		{"collection paused", domain.ChargeCollectionPaused, nil, ""},
		{"the customer on the hosted page", domain.ChargePayingOnHostedPage, nil, ""},
		{"the read failing", 0, errors.New("conn reset"), readsReadFailed},
	} {
		setup := func() (*recordingCharger, *Engine) {
			inv := &mockInvoices{invoices: []domain.Invoice{pendingInvoice()}, clockQueue: map[string]bool{"inv_1": true},
				claimState: map[string]domain.ChargeClaimState{"inv_1": c.state}, claimStateErr: c.readErr}
			charger := &recordingCharger{store: inv}
			pms := &fakePaymentSetups{ready: true, stripeCustomerID: "cus_stripe_1"}
			engine := wireBaseTax(NewEngine(&mockSubs{cycleUpdated: make(map[string]bool)}, &mockUsage{}, &mockPricing{}, refusingClaims{inv}, nil, &mockSettings{}, pms, charger, billingTestClock()))
			return charger, engine
		}
		t.Run(c.name+", on a clock", func(t *testing.T) {
			charger, engine := setup()
			_, runErrs := engine.RetryPendingChargesForClock(postgres.WithLivemode(context.Background(), false), "t1", "tc_1", 50)
			if len(charger.got) != 0 {
				t.Fatalf("charged %d times, want none", len(charger.got))
			}
			switch c.want {
			case "":
				if len(runErrs) != 0 {
					t.Fatalf("errors = %v, want none: nothing is this job's to do", runErrs)
				}
			case readsReadFailed:
				if len(runErrs) != 1 || errors.Is(runErrs[0], domain.ErrLeftForNextRun) || !strings.HasSuffix(runErrs[0].Error(), readsReadFailed) {
					t.Fatalf("errors = %v, want one failed read, not ErrLeftForNextRun", runErrs)
				}
			default:
				if len(runErrs) != 1 || !errors.Is(runErrs[0], domain.ErrLeftForNextRun) {
					t.Fatalf("errors = %v, want one ErrLeftForNextRun", runErrs)
				}
				if got := errs.SummarizeForOperator(runErrs, ""); got != c.want {
					t.Errorf("the operator reads:\n  %q\nwant\n  %q", got, c.want)
				}
			}
		})
		t.Run(c.name+", in real time", func(t *testing.T) {
			_, engine := setup()
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
