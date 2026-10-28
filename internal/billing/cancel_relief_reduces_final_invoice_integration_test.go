package billing_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/gethardcount/hardcount/internal/billing"
	"github.com/gethardcount/hardcount/internal/credit"
	"github.com/gethardcount/hardcount/internal/creditnote"
	"github.com/gethardcount/hardcount/internal/customer"
	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/invoice"
	"github.com/gethardcount/hardcount/internal/platform/clock"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/pricing"
	"github.com/gethardcount/hardcount/internal/subscription"
	"github.com/gethardcount/hardcount/internal/subscription/subscriptiontest"
	"github.com/gethardcount/hardcount/internal/tax"
	"github.com/gethardcount/hardcount/internal/tenant"
	"github.com/gethardcount/hardcount/internal/testutil"
	"github.com/gethardcount/hardcount/internal/usage"
)

// Issue #1272, on real Postgres. A mid-period cancel of an in-advance
// subscription that also bills usage owes the customer two things at once:
// a final invoice for the usage so far, and relief for the unused part of the
// prepaid base fee. The relief must reduce the final invoice (ADR-088,
// ADR-118 amendment of 2026-10-19), and the outcome must not depend on
// whether a card is on file.
//
// Fixture: a monthly plan, 4900c base billed in advance, usage at 1c a unit.
// Period [2026-06-01, 2026-07-01), 30 days. The day-1 invoice is paid. The
// cancel lands on 2026-06-16: 15 of 30 days unused, so the relief is a credit
// note of 4900 × 15/30 = 2450c on the paid day-1 invoice, credited to the
// balance. Both cancel paths run: the immediate Cancel and the scheduled
// cancel's FireDueScheduledCancel, wired as internal/api/router.go wires them.
//
// Before the fix the final invoice was collected before the relief was
// issued: with a card, the card was charged the whole final invoice and the
// 2450c sat on the balance of a customer with no subscription; with no card,
// the no-card email went out for the whole amount.
//
// Mutation seam: in subscription.Service.finishReliefThenCollect, call
// FinalizeOnCreateInvoice before FinishRelief → every case below fails.

const (
	reliefBaseCents = 4900
	reliefCents     = 2450 // 4900 × 15/30
)

var (
	reliefPeriodStart = time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	reliefPeriodEnd   = time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	reliefCancelAt    = time.Date(2026, 6, 16, 0, 0, 0, 0, time.UTC)
)

// invoiceCharger records every charge, by invoice, with the amount asked.
type invoiceCharger struct {
	mu  sync.Mutex
	got map[string][]int64
}

func (c *invoiceCharger) ChargeInvoice(_ context.Context, _ string, inv domain.Invoice, _ domain.ChargeHold, _, _ string) (domain.Invoice, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.got == nil {
		c.got = map[string][]int64{}
	}
	c.got[inv.ID] = append(c.got[inv.ID], inv.AmountDueCents)
	return inv, nil
}

func (c *invoiceCharger) charges(invoiceID string) []int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]int64(nil), c.got[invoiceID]...)
}

type reliefCancelRig struct {
	ctx        context.Context
	db         *postgres.DB
	tenantID   string
	customerID string
	subID      string
	day1ID     string
	invStore   *invoice.PostgresStore
	credit     *credit.Service
	subSvc     *subscription.Service
	charger    *invoiceCharger
	noPM       *testNoPMNotifier
	// wantCredited is the balance credit Cancel reports: a paid day-1
	// invoice's share. An unpaid share's note reduces its own invoice, so it
	// reports none.
	wantCredited int64
}

// reliefRigOpts shapes one rig.
type reliefRigOpts struct {
	card       bool  // a card on file
	usageUnits int64 // usage on 2026-06-03, at 1c a unit
	// day1Credit, when set, is granted before the day-1 invoice, which then
	// stays unpaid with that much applied. It needs card=false. When zero the
	// day-1 invoice is paid.
	day1Credit int64
}

// newReliefCancelRig seeds one tenant: the plan, an active subscription in
// its 2026-06 period with its day-1 invoice, and the usage on 2026-06-03.
func newReliefCancelRig(t *testing.T, db *postgres.DB, name string, o reliefRigOpts) *reliefCancelRig {
	t.Helper()
	if o.day1Credit > 0 && o.card {
		t.Fatal("rig: a day-1 credit leaves the day-1 invoice unpaid, which needs card=false")
	}
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, name)

	pricingStore := pricing.NewPostgresStore(db)
	pricingSvc := pricing.NewService(pricingStore)
	subStore := subscription.NewPostgresStore(db)
	usageStore := usage.NewPostgresStore(db)
	invStore := invoice.NewPostgresStore(db)
	settings := tenant.NewSettingsStore(db)
	creditSvc := credit.NewService(credit.NewPostgresStore(db))
	cnSvc := creditnote.NewService(creditnote.NewPostgresStore(db), invStore, nil, &cnGrantAdapter{creditSvc})
	cnSvc.SetNumberGenerator(settings)

	cust, err := customer.NewPostgresStore(db).Create(ctx, tenantID, domain.Customer{
		ExternalID: "cus_relief", DisplayName: "Relief", Email: "relief@example.test",
	})
	if err != nil {
		t.Fatalf("create customer: %v", err)
	}
	meter, err := pricingSvc.CreateMeter(ctx, tenantID, pricing.CreateMeterInput{
		EventName: "calls", Name: "Calls", Unit: "calls", Aggregation: "sum",
	})
	if err != nil {
		t.Fatalf("create meter: %v", err)
	}
	plan, _, err := pricingSvc.CreatePlan(ctx, tenantID, pricing.CreatePlanInput{
		Code: "pro-adv-usage", Name: "Pro", Currency: "USD", BillingInterval: domain.BillingMonthly,
		BaseAmountCents: reliefBaseCents, BaseBillTiming: domain.BillInAdvance, Prices: []pricing.PriceInput{{MeterID: meter.ID, PriceShape: domain.PriceShape{Mode: domain.PricingPerUnit, UnitAmountCents: decimal.NewFromInt(1)}}},
	})
	if err != nil {
		t.Fatalf("create plan: %v", err)
	}

	var payments billing.PaymentReadiness = testPaymentSetupsNoPM{}
	if o.card {
		payments = testPaymentSetupsReady{}
	}
	charger := &invoiceCharger{}
	noPM := &testNoPMNotifier{}
	e := billing.NewEngine(
		&subStoreAdapter{subStore}, &usageStoreAdapter{usageStore},
		&pricingStoreAdapter{pricingStore}, &invoiceStoreAdapter{invStore},
		creditSvc, settings, payments, charger, clock.NewFake(reliefPeriodStart.Add(time.Hour)),
	)
	e.SetIntervalReader(subStore)
	e.SetTaxProviderResolver(tax.NewResolver(nil))
	e.SetNoPaymentMethodNotifier(noPM)
	e.SetDunningStarter(testDunningStarter{})
	e.SetDunningResolver(&testDunningResolver{})
	e.SetInvoiceVoider(invoice.NewService(invStore, clock.Real(), settings))
	e.SetCreditNoteAdjuster(cnSvc)
	e.SetCreditHeadroomReader(cnSvc)

	periodStart, periodEnd := reliefPeriodStart, reliefPeriodEnd
	sub, err := subStore.Create(ctx, tenantID, domain.Subscription{
		Code: "sub-relief", DisplayName: "Relief", CustomerID: cust.ID,
		Items:  []domain.SubscriptionItem{{PlanID: plan.ID, Quantity: 1}},
		Status: domain.SubscriptionActive, BillingTime: domain.BillingTimeCalendar,
		StartedAt:                 &periodStart,
		CurrentBillingPeriodStart: &periodStart, CurrentBillingPeriodEnd: &periodEnd,
	})
	if err != nil {
		t.Fatalf("create subscription: %v", err)
	}
	subscriptiontest.SetBillingCycle(t, ctx, db, tenantID, sub.ID, periodStart, periodEnd, periodEnd, 0)
	sub.CurrentBillingPeriodStart = &periodStart
	sub.CurrentBillingPeriodEnd = &periodEnd

	wantCredited := int64(reliefCents)
	if o.day1Credit > 0 {
		if _, err := creditSvc.Grant(ctx, tenantID, credit.GrantInput{
			Currency:   "USD",
			CustomerID: cust.ID, AmountCents: o.day1Credit, Description: "seed balance", At: periodStart,
		}); err != nil {
			t.Fatalf("grant credit: %v", err)
		}
		wantCredited = 0
	}
	day1, err := e.BillOnCreate(ctx, sub)
	if err != nil || day1.SubtotalCents != reliefBaseCents {
		t.Fatalf("day-1 invoice: %v subtotal=%dc, want %dc", err, day1.SubtotalCents, reliefBaseCents)
	}
	if o.day1Credit == 0 {
		if _, err := invStore.MarkPaid(ctx, tenantID, day1.ID, "pi_day1", periodStart); err != nil {
			t.Fatalf("pay day-1 invoice: %v", err)
		}
	}

	ts := periodStart.AddDate(0, 0, 2)
	if _, err := usage.NewService(usageStore).Ingest(ctx, tenantID, usage.IngestInput{
		CustomerID: cust.ID, MeterID: meter.ID, Quantity: decimal.NewFromInt(o.usageUnits), Timestamp: &ts,
	}); err != nil {
		t.Fatalf("ingest usage: %v", err)
	}

	subSvc := subscription.NewService(subStore, clock.NewFake(reliefCancelAt))
	subSvc.SetBiller(e)
	return &reliefCancelRig{
		ctx: ctx, db: db, tenantID: tenantID, customerID: cust.ID, subID: sub.ID, day1ID: day1.ID,
		invStore: invStore, credit: creditSvc, subSvc: subSvc, charger: charger, noPM: noPM,
		wantCredited: wantCredited,
	}
}

// cancelImmediately is the operator's or API's immediate cancel at 2026-06-16.
func (r *reliefCancelRig) cancelImmediately(t *testing.T) {
	t.Helper()
	canceled, credited, err := r.subSvc.Cancel(clock.WithEffectiveNow(r.ctx, reliefCancelAt), r.tenantID, r.subID)
	if err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if canceled.Status != domain.SubscriptionCanceled || credited != r.wantCredited {
		t.Fatalf("cancel: status=%s credited=%dc, want canceled with %dc balance credit", canceled.Status, credited, r.wantCredited)
	}
}

// fireScheduledCancel is a cancel scheduled for 2026-06-16, fired by the
// billing run an hour later.
func (r *reliefCancelRig) fireScheduledCancel(t *testing.T) {
	t.Helper()
	tx, err := r.db.BeginTx(r.ctx, postgres.TxTenant, r.tenantID)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer postgres.Rollback(tx)
	if _, err := tx.ExecContext(r.ctx, `UPDATE subscriptions SET cancel_at = $1 WHERE id = $2`, reliefCancelAt, r.subID); err != nil {
		t.Fatalf("schedule cancel: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if err := r.subSvc.FireDueScheduledCancel(clock.WithEffectiveNow(r.ctx, reliefCancelAt.Add(time.Hour)), r.tenantID, r.subID, reliefCancelAt); err != nil {
		t.Fatalf("fire scheduled cancel: %v", err)
	}
	sub, err := subscription.NewPostgresStore(r.db).Get(r.ctx, r.tenantID, r.subID)
	if err != nil || sub.Status != domain.SubscriptionCanceled {
		t.Fatalf("after the fire: %v status=%s, want canceled", err, sub.Status)
	}
}

func (r *reliefCancelRig) finalInvoice(t *testing.T) domain.Invoice {
	t.Helper()
	invs, _, err := r.invStore.List(r.ctx, invoice.ListFilter{TenantID: r.tenantID})
	if err != nil {
		t.Fatalf("list invoices: %v", err)
	}
	var found []domain.Invoice
	for _, inv := range invs {
		if inv.BillingReason == domain.BillingReasonSubscriptionCancel {
			found = append(found, inv)
		}
	}
	if len(found) != 1 {
		t.Fatalf("%d final invoices, want 1", len(found))
	}
	return found[0]
}

func (r *reliefCancelRig) invoice(t *testing.T, id string) domain.Invoice {
	t.Helper()
	inv, err := r.invStore.Get(r.ctx, r.tenantID, id)
	if err != nil {
		t.Fatalf("get invoice %s: %v", id, err)
	}
	return inv
}

func (r *reliefCancelRig) balance(t *testing.T) int64 {
	t.Helper()
	bal, err := testutil.BalanceUSD(r.credit.GetBalance(r.ctx, r.tenantID, r.customerID))
	if err != nil {
		t.Fatalf("credit balance: %v", err)
	}
	return bal.BalanceCents
}

func (r *reliefCancelRig) noPMNotified(invoiceID string) []int64 {
	var out []int64
	for _, inv := range r.noPM.got {
		if inv.ID == invoiceID {
			out = append(out, inv.AmountDueCents)
		}
	}
	return out
}

func TestCancel_ReliefReducesFinalInvoice_CardOrNoCard(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: skipped in -short mode")
	}
	db := testutil.SetupTestDB(t)

	cancels := []struct {
		name string
		run  func(*reliefCancelRig, *testing.T)
	}{
		{"immediate cancel", (*reliefCancelRig).cancelImmediately},
		{"scheduled cancel", (*reliefCancelRig).fireScheduledCancel},
	}

	for _, c := range cancels {
		// Usage 3000c is more than the 2450c relief: the relief is spent on the
		// final invoice, and 550c is left to collect.
		t.Run(c.name+"/relief covers part of the final invoice", func(t *testing.T) {
			const usageCents, wantDue = 3000, 3000 - reliefCents
			for _, card := range []bool{true, false} {
				r := newReliefCancelRig(t, db, fmt.Sprintf("1272 %s part card=%v", c.name, card), reliefRigOpts{card: card, usageUnits: usageCents})
				c.run(r, t)

				final := r.finalInvoice(t)
				if final.TotalAmountCents != usageCents {
					t.Fatalf("card=%v: final invoice total = %dc, want %dc (the usage; the in-advance base is not billed again)", card, final.TotalAmountCents, usageCents)
				}
				if final.CreditsAppliedCents != reliefCents || final.AmountDueCents != wantDue {
					t.Errorf("card=%v: final invoice credits_applied=%dc amount_due=%dc, want %dc and %dc: the relief must reduce the final invoice",
						card, final.CreditsAppliedCents, final.AmountDueCents, reliefCents, wantDue)
				}
				if bal := r.balance(t); bal != 0 {
					t.Errorf("card=%v: credit balance = %dc, want 0 (the relief was spent on the final invoice)", card, bal)
				}
				charges, notified := r.charger.charges(final.ID), r.noPMNotified(final.ID)
				if card {
					if len(charges) != 1 || charges[0] != wantDue {
						t.Errorf("card on file: final invoice charges = %v, want one charge of %dc (the rest after the relief)", charges, wantDue)
					}
				} else {
					if len(charges) != 0 {
						t.Errorf("no card: final invoice charges = %v, want none", charges)
					}
					if len(notified) != 1 || notified[0] != wantDue {
						t.Errorf("no card: no-card email for the final invoice = %v, want one, for %dc", notified, wantDue)
					}
				}
			}
		})

		// Usage 1000c is less than the 2450c relief: the final invoice is paid
		// by the relief alone, and 1450c stays on the balance.
		t.Run(c.name+"/relief covers the whole final invoice", func(t *testing.T) {
			const usageCents = 1000
			for _, card := range []bool{true, false} {
				r := newReliefCancelRig(t, db, fmt.Sprintf("1272 %s whole card=%v", c.name, card), reliefRigOpts{card: card, usageUnits: usageCents})
				c.run(r, t)

				final := r.finalInvoice(t)
				if final.Status != domain.InvoicePaid || final.CreditsAppliedCents != usageCents || final.AmountDueCents != 0 {
					t.Errorf("card=%v: final invoice status=%s credits_applied=%dc amount_due=%dc, want paid by %dc of the relief",
						card, final.Status, final.CreditsAppliedCents, final.AmountDueCents, usageCents)
				}
				if bal := r.balance(t); bal != reliefCents-usageCents {
					t.Errorf("card=%v: credit balance = %dc, want %dc (the relief left after paying the final invoice)", card, bal, reliefCents-usageCents)
				}
				if charges := r.charger.charges(final.ID); len(charges) != 0 {
					t.Errorf("card=%v: final invoice charges = %v, want none", card, charges)
				}
				if notified := r.noPMNotified(final.ID); len(notified) != 0 {
					t.Errorf("card=%v: no-card email for the final invoice = %v, want none (nothing is owed)", card, notified)
				}
			}
		})

		// The day-1 invoice is unpaid: 4000c of customer credits paid part of
		// it, leaving 900c due, and there is no card. The 2450c relief is a
		// credit note on that invoice. It clears the 900c due and takes 1550c
		// of the credits off the invoice, back to the balance (ADR-061). Those
		// 1550c must reduce the final invoice. In the old order the final
		// invoice was collected first, with an empty balance.
		t.Run(c.name+"/unpaid day-1 invoice: the credits the relief returns reduce the final invoice", func(t *testing.T) {
			const (
				usageCents      = 3000
				day1Credit      = 4000
				day1Due         = reliefBaseCents - day1Credit // 900
				creditsReturned = reliefCents - day1Due        // 1550
				wantDue         = usageCents - creditsReturned // 1450
			)
			r := newReliefCancelRig(t, db, "1272 "+c.name+" unpaid day-1", reliefRigOpts{usageUnits: usageCents, day1Credit: day1Credit})
			if d := r.invoice(t, r.day1ID); d.PaymentStatus == domain.PaymentSucceeded || d.CreditsAppliedCents != day1Credit || d.AmountDueCents != day1Due {
				t.Fatalf("fixture: day-1 invoice payment=%s credits_applied=%dc amount_due=%dc, want unpaid with %dc applied and %dc due",
					d.PaymentStatus, d.CreditsAppliedCents, d.AmountDueCents, day1Credit, day1Due)
			}
			c.run(r, t)

			if d := r.invoice(t, r.day1ID); d.AmountDueCents != 0 || d.CreditsAppliedCents != day1Credit-creditsReturned {
				t.Errorf("day-1 invoice after the relief: credits_applied=%dc amount_due=%dc, want %dc and 0",
					d.CreditsAppliedCents, d.AmountDueCents, day1Credit-creditsReturned)
			}
			final := r.finalInvoice(t)
			if final.CreditsAppliedCents != creditsReturned || final.AmountDueCents != wantDue {
				t.Errorf("final invoice credits_applied=%dc amount_due=%dc, want %dc and %dc: the credits the relief returned must reduce the final invoice",
					final.CreditsAppliedCents, final.AmountDueCents, creditsReturned, wantDue)
			}
			if bal := r.balance(t); bal != 0 {
				t.Errorf("credit balance = %dc, want 0 (the returned credits were spent on the final invoice)", bal)
			}
			if notified := r.noPMNotified(final.ID); len(notified) != 1 || notified[0] != wantDue {
				t.Errorf("no-card email for the final invoice = %v, want one, for %dc", notified, wantDue)
			}
		})
	}
}
