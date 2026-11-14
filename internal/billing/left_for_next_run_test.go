package billing

import (
	"context"
	"errors"
	"testing"

	"github.com/gethardcount/hardcount/internal/domain"
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
