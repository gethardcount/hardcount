package billing_test

import (
	"context"
	"testing"
	"time"

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
	"github.com/gethardcount/hardcount/internal/tenant"
	"github.com/gethardcount/hardcount/internal/testutil"
)

var (
	uctStart = time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	uctEnd   = time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	uctDay16 = time.Date(2026, 6, 16, 0, 0, 0, 0, time.UTC) // 15 of 30 days unused
)

// taxedRig is a tenant on manual tax at a rate with a monthly plan billed in
// advance, priced so that one item's month is subtotal 10000 plus its tax:
// at 20%, subtotal 10000, tax 2000, total 12000, from a base of 12000 with
// tax-inclusive prices and 10000 without.
type taxedRig struct {
	t         *testing.T
	ctx       context.Context
	db        *postgres.DB
	tenantID  string
	inclusive bool
	tax       int64 // one item's month's tax
	plan      domain.Plan
	invoices  *invoice.PostgresStore
	subs      *subscription.PostgresStore
	engine    *billing.Engine
	sub       domain.Subscription
}

func newTaxedRig(t *testing.T, inclusive bool) *taxedRig {
	t.Helper()
	return newTaxedRigAt(t, inclusive, 20)
}

// newTaxedRigAt is newTaxedRig at a manual rate in percent.
func newTaxedRigAt(t *testing.T, inclusive bool, rate float64) *taxedRig {
	t.Helper()
	db := testutil.SetupTestDB(t)
	r := &taxedRig{t: t, ctx: postgres.WithLivemode(context.Background(), false), db: db, inclusive: inclusive,
		tax: int64(10000 * rate / 100), invoices: invoice.NewPostgresStore(db), subs: subscription.NewPostgresStore(db)}
	settings := tenant.NewSettingsStore(db)
	r.tenantID = testutil.CreateTestTenant(t, db, "Unused Credit Tax")
	base := int64(10000)
	if inclusive {
		setInclusiveManualTax(t, r.ctx, settings, r.tenantID, rate)
		base = 10000 + r.tax
	} else {
		setManualTax(t, r.ctx, settings, r.tenantID, rate)
	}
	var err error
	r.plan, err = pricing.NewPostgresStore(db).CreatePlan(r.ctx, r.tenantID, domain.Plan{
		Code: "adv", Name: "Advance", Currency: "USD", BillingInterval: domain.BillingMonthly, Status: domain.PlanActive,
		BaseAmountCents: base, BaseBillTiming: domain.BillInAdvance,
	})
	if err != nil {
		t.Fatalf("create plan: %v", err)
	}
	creditSvc := credit.NewService(credit.NewPostgresStore(db))
	creditNoteSvc := creditnote.NewService(creditnote.NewPostgresStore(db), r.invoices, nil, &cnGrantAdapter{creditSvc})
	creditNoteSvc.SetNumberGenerator(settings)
	r.engine = newRealEngine(db, clock.NewFake(uctStart.Add(time.Hour)))
	r.engine.SetInvoiceVoider(invoice.NewService(r.invoices, clock.Real(), settings))
	r.engine.SetCreditNoteAdjuster(creditNoteSvc)
	r.engine.SetCreditHeadroomReader(creditNoteSvc)

	cust, err := customer.NewPostgresStore(db).Create(r.ctx, r.tenantID, domain.Customer{ExternalID: "cus_uct", DisplayName: "UCT"})
	if err != nil {
		t.Fatalf("create customer: %v", err)
	}
	r.sub, err = r.subs.Create(r.ctx, r.tenantID, domain.Subscription{
		Code: "sub-uct", DisplayName: "UCT", CustomerID: cust.ID,
		Items:  []domain.SubscriptionItem{{PlanID: r.plan.ID, Quantity: 1}},
		Status: domain.SubscriptionActive, BillingTime: domain.BillingTimeCalendar,
		StartedAt: &uctStart, CurrentBillingPeriodStart: &uctStart, CurrentBillingPeriodEnd: &uctEnd,
	})
	if err != nil {
		t.Fatalf("create sub: %v", err)
	}
	subscriptiontest.SetBillingCycle(t, r.ctx, db, r.tenantID, r.sub.ID, uctStart, uctEnd, uctEnd, 0)
	r.sub.CurrentBillingPeriodStart, r.sub.CurrentBillingPeriodEnd = &uctStart, &uctEnd
	return r
}

// billMonth issues the month's invoice for the subscription's items and,
// when paid is set, pays it.
func (r *taxedRig) billMonth(paid bool) domain.Invoice {
	r.t.Helper()
	inv, err := r.engine.BillOnCreate(r.ctx, r.sub)
	if err != nil {
		r.t.Fatalf("BillOnCreate: %v", err)
	}
	if inv.SubtotalCents != 10000 || inv.TaxAmountCents != r.tax || inv.TotalAmountCents != 10000+r.tax {
		r.t.Fatalf("the month's invoice = subtotal %d, tax %d, total %d; want 10000, %d, %d",
			inv.SubtotalCents, inv.TaxAmountCents, inv.TotalAmountCents, r.tax, 10000+r.tax)
	}
	if inv.TaxInclusive != r.inclusive {
		r.t.Fatalf("the month's invoice records tax_inclusive %v, want the tenant's %v", inv.TaxInclusive, r.inclusive)
	}
	if paid {
		if _, err := r.invoices.MarkPaid(r.ctx, r.tenantID, inv.ID, "pi_uct", uctStart); err != nil {
			r.t.Fatalf("mark paid: %v", err)
		}
	}
	return inv
}

// addPaidItem adds a second item at the start of the month, billed by its own
// paid invoice priced in the tenant's terms: one base line for the month,
// subtotal 10000, tax 2000, total 12000. With tax-inclusive prices the line's
// unit price and total carry the price (12000) and its amount the net.
func (r *taxedRig) addPaidItem() {
	r.t.Helper()
	// A subscription has one item per plan: the second item is on a twin plan.
	twin := r.plan
	twin.ID, twin.Code = "", "adv-twin"
	twin, err := pricing.NewPostgresStore(r.db).CreatePlan(r.ctx, r.tenantID, twin)
	if err != nil {
		r.t.Fatalf("create the twin plan: %v", err)
	}
	item, err := r.subs.AddItem(r.ctx, r.tenantID, domain.SubscriptionItem{SubscriptionID: r.sub.ID, PlanID: twin.ID, Quantity: 1})
	if err != nil {
		r.t.Fatalf("add item: %v", err)
	}
	r.sub.Items = append(r.sub.Items, item)
	inv, err := r.invoices.CreateWithLineItems(r.ctx, r.tenantID, domain.Invoice{
		CustomerID: r.sub.CustomerID, SubscriptionID: r.sub.ID, InvoiceNumber: "UCT-ADD",
		Status: domain.InvoiceFinalized, PaymentStatus: domain.PaymentPending, Currency: "USD",
		BillingReason:       domain.BillingReasonSubscriptionUpdate,
		TaxInclusive:        r.inclusive,
		SourcePlanChangedAt: &uctStart, SourceSubscriptionItemID: item.ID, SourceChangeType: domain.ItemChangeTypeAdd,
		SubtotalCents: 10000, TaxFacts: domain.TaxFacts{TaxAmountCents: 2000, TaxStatus: domain.InvoiceTaxOK},
		TotalAmountCents: 12000, AmountDueCents: 12000,
		BillingPeriodStart: uctStart, BillingPeriodEnd: uctEnd, IssuedAt: &uctStart,
	}, []domain.InvoiceLineItem{{
		LineType: domain.LineTypeBaseFee, Description: "Advance - base fee (qty 1)", Quantity: 1,
		UnitAmountCents: twin.BaseAmountCents, AmountCents: 10000, TaxAmountCents: 2000, TotalAmountCents: 12000, Currency: "USD",
		BillingPeriodStart: &uctStart, BillingPeriodEnd: &uctEnd,
	}})
	if err != nil {
		r.t.Fatalf("create the item's invoice: %v", err)
	}
	if _, err := r.invoices.MarkPaid(r.ctx, r.tenantID, inv.ID, "pi_uct_add", uctStart); err != nil {
		r.t.Fatalf("pay the item's invoice: %v", err)
	}
}

// cancel writes the cancel's relief at cancelAt and returns it.
func (r *taxedRig) cancel(cancelAt time.Time) domain.Relief {
	r.t.Helper()
	sub := r.sub
	sub.Status, sub.CanceledAt = domain.SubscriptionCanceled, &cancelAt
	tx, err := r.db.BeginTx(r.ctx, postgres.TxTenant, r.tenantID)
	if err != nil {
		r.t.Fatalf("begin: %v", err)
	}
	defer postgres.Rollback(tx)
	relief, err := r.engine.WriteCancelReliefTx(r.ctx, tx, sub)
	if err != nil {
		r.t.Fatalf("WriteCancelReliefTx: %v", err)
	}
	return relief
}

// cancelCommitted is cancel with the relief committed, so its credit notes
// can be read.
func (r *taxedRig) cancelCommitted(cancelAt time.Time) domain.Relief {
	r.t.Helper()
	sub := r.sub
	sub.Status, sub.CanceledAt = domain.SubscriptionCanceled, &cancelAt
	tx, err := r.db.BeginTx(r.ctx, postgres.TxTenant, r.tenantID)
	if err != nil {
		r.t.Fatalf("begin: %v", err)
	}
	defer postgres.Rollback(tx)
	relief, err := r.engine.WriteCancelReliefTx(r.ctx, tx, sub)
	if err != nil {
		r.t.Fatalf("WriteCancelReliefTx: %v", err)
	}
	if err := tx.Commit(); err != nil {
		r.t.Fatalf("commit: %v", err)
	}
	return relief
}

// switchTaxInclusive sets the tenant's tax_inclusive.
func (r *taxedRig) switchTaxInclusive(on bool) {
	r.t.Helper()
	if _, err := tenant.NewSettingsStore(r.db).Update(r.ctx, r.tenantID, func(ts *domain.TenantSettings) error {
		ts.TaxInclusive = on
		return nil
	}); err != nil {
		r.t.Fatalf("switch tax_inclusive: %v", err)
	}
}

// A cancel with half the month unused credits half of what the customer paid
// for it, 6000 of 12000, whether the plan's price includes the tax or not.
// With tax-inclusive prices the unused amount (12000 × 15/30 = 6000) already
// carries tax; it was taken as an amount before tax and grossed up a second
// time by the invoice's 12000/10000, crediting 7200.
func TestCancelRelief_TaxInclusive_CreditsHalfOfWhatWasPaid(t *testing.T) {
	r := newTaxedRig(t, true)
	r.billMonth(true)
	if got := r.cancel(uctDay16).CreditedCents; got != 6000 {
		t.Errorf("credited %d, want 6000 (half of the 12000 paid)", got)
	}
}

// The exclusive twin is unchanged: 10000 × 15/30 = 5000 before tax, 6000 with
// it.
func TestCancelRelief_TaxExclusive_CreditsHalfOfWhatWasPaid(t *testing.T) {
	r := newTaxedRig(t, false)
	r.billMonth(true)
	if got := r.cancel(uctDay16).CreditedCents; got != 6000 {
		t.Errorf("credited %d, want 6000 (half of the 12000 paid)", got)
	}
}

// A cancel at the start of the month, with one item on the unpaid invoice for
// it and a second added at the start on its own paid invoice: the unpaid one
// is voided and the paid one credited in full, 12000, with nothing left over.
// The unused amount with tax in it (24000) had the voided invoice's amount
// before tax (10000) taken off, so 14000 went on to be credited; the paid
// invoice had room for 12000, and 4000 was reported as uncredited.
func TestCancelRelief_TaxInclusive_VoidTakesTheVoidedInvoicesTotal(t *testing.T) {
	r := newTaxedRig(t, true)
	r.billMonth(false)
	r.addPaidItem()
	got := r.cancel(uctStart)
	if len(got.Voided) != 1 || got.CreditedCents != 12000 || got.UncreditedCents != 0 {
		t.Errorf("voided %d, credited %d, uncredited %d; want 1, 12000, 0", len(got.Voided), got.CreditedCents, got.UncreditedCents)
	}
}

// The exclusive twin is unchanged: 20000 unused before tax, 10000 of it on the
// voided invoice, the rest credited as 12000 with tax.
func TestCancelRelief_TaxExclusive_VoidTakesTheVoidedInvoicesTotal(t *testing.T) {
	r := newTaxedRig(t, false)
	r.billMonth(false)
	r.addPaidItem()
	got := r.cancel(uctStart)
	if len(got.Voided) != 1 || got.CreditedCents != 12000 || got.UncreditedCents != 0 {
		t.Errorf("voided %d, credited %d, uncredited %d; want 1, 12000, 0", len(got.Voided), got.CreditedCents, got.UncreditedCents)
	}
}

// A tenant that switches tax_inclusive between the bill and the cancel: each
// invoice is credited in the terms it was priced in, which it records.
// Billed with tax-inclusive prices (12000 with tax in it) and switched off
// before the cancel, the 6000 unused is half of what was paid. It was read
// in the new terms, as before tax, and grossed up to 7200.
func TestCancelRelief_BilledTaxInclusive_SwitchedOffBeforeTheCancel(t *testing.T) {
	r := newTaxedRig(t, true)
	r.billMonth(true)
	r.switchTaxInclusive(false)
	if got := r.cancel(uctDay16).CreditedCents; got != 6000 {
		t.Errorf("credited %d, want 6000 (half of the 12000 paid, in the invoice's terms)", got)
	}
}

// The other direction: billed before tax (10000 + 2000) and switched on
// before the cancel, the 5000 unused before tax is grossed up to 6000. Read in
// the new terms it would be taken as a price with tax in it, 5000.
func TestCancelRelief_BilledTaxExclusive_SwitchedOnBeforeTheCancel(t *testing.T) {
	r := newTaxedRig(t, false)
	r.billMonth(true)
	r.switchTaxInclusive(true)
	if got := r.cancel(uctDay16).CreditedCents; got != 6000 {
		t.Errorf("credited %d, want 6000 (half of the 12000 paid, in the invoice's terms)", got)
	}
}

// A mid-month swap credits the unused time of the plan swapped away from by
// the same rule (BillOnPlanSwapDraftsTx, given the snapshot before the swap,
// as a monthly → yearly swap gives it): half the month unused, half of the
// 12000 paid.
func TestSwapRelief_HalfTheMonthUnused_CreditsHalfOfWhatWasPaid(t *testing.T) {
	for _, inclusive := range []bool{true, false} {
		t.Run(map[bool]string{true: "tax-inclusive", false: "tax-exclusive"}[inclusive], func(t *testing.T) {
			r := newTaxedRig(t, inclusive)
			r.billMonth(true)
			tx, err := r.db.BeginTx(r.ctx, postgres.TxTenant, r.tenantID)
			if err != nil {
				t.Fatalf("begin: %v", err)
			}
			defer postgres.Rollback(tx)
			relief, err := r.engine.BillOnPlanSwapDraftsTx(r.ctx, tx, r.sub, uctDay16)
			if err != nil {
				t.Fatalf("BillOnPlanSwapDraftsTx: %v", err)
			}
			if relief.CreditedCents != 6000 {
				t.Errorf("credited %d, want 6000 (half of the 12000 paid)", relief.CreditedCents)
			}
		})
	}
}

// At 18% the month is subtotal 10000, tax 1800, total 11800. A cancel with
// half of it unused writes one credit note of 5900: subtotal 5000, tax 900,
// in either terms. With tax-inclusive prices the 5900 unused was grossed up
// to 6962 (subtotal 5900, tax 1062).
func TestCancelRelief_At18Percent_TheCreditNoteSplitsLikeTheInvoice(t *testing.T) {
	for _, inclusive := range []bool{true, false} {
		t.Run(map[bool]string{true: "tax-inclusive", false: "tax-exclusive"}[inclusive], func(t *testing.T) {
			r := newTaxedRigAt(t, inclusive, 18)
			inv := r.billMonth(true)
			if got := r.cancelCommitted(uctDay16).CreditedCents; got != 5900 {
				t.Errorf("credited %d, want 5900", got)
			}
			notes, err := creditnote.NewPostgresStore(r.db).List(r.ctx, creditnote.ListFilter{TenantID: r.tenantID, InvoiceID: inv.ID})
			if err != nil {
				t.Fatalf("list credit notes: %v", err)
			}
			if len(notes) != 1 {
				t.Fatalf("credit notes on the month's invoice = %d, want 1", len(notes))
			}
			if n := notes[0]; n.SubtotalCents != 5000 || n.TaxAmountCents != 900 || n.TotalCents != 5900 {
				t.Errorf("credit note = subtotal %d, tax %d, total %d; want 5000, 900, 5900", n.SubtotalCents, n.TaxAmountCents, n.TotalCents)
			}
		})
	}
}
