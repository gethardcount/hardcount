package billing

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/errs"
	"github.com/sagarsuperuser/velox/internal/tax"
)

var errBlip = errors.New("connection reset")

// A billing-profile read ERROR stops the tax calculation; it used to leave an
// empty profile, so tax was computed (and committed) without the customer's
// exemption, reverse charge, address and tax ID. A missing profile is "not
// set" and still computes.
func TestApplyTax_ProfileReadErrorFails(t *testing.T) {
	e := newManualEngine(1850, "VAT", map[string]domain.CustomerBillingProfile{})
	e.profiles = &taxProfiles{err: errBlip}
	lines := []domain.InvoiceLineItem{{AmountCents: 10000, Quantity: 1}}
	if _, err := e.ApplyTaxToLineItems(context.Background(), "t1", "cus_1", "USD", 10000, 0, lines); !errors.Is(err, errBlip) {
		t.Fatalf("err = %v, want the profile read error", err)
	}

	e.profiles = &taxProfiles{profiles: map[string]domain.CustomerBillingProfile{}} // cus_1 has none
	if _, err := e.ApplyTaxToLineItems(context.Background(), "t1", "cus_1", "USD", 10000, 0, []domain.InvoiceLineItem{{AmountCents: 10000, Quantity: 1}}); err != nil {
		t.Fatalf("no billing profile must still compute tax: %v", err)
	}
}

// guardCalcStore lets each step of the expiry guard fail.
type guardCalcStore struct {
	linkErr, lookupErr, recordErr error
	createdAt                     time.Time
}

func (g *guardCalcStore) Record(context.Context, string, string, tax.Request, *tax.Result) (string, error) {
	return "taxcalc_x", g.recordErr
}
func (g *guardCalcStore) LinkInvoice(context.Context, string, string, string) error { return g.linkErr }
func (g *guardCalcStore) LookupCalculationCreatedAt(context.Context, string, string, string) (time.Time, error) {
	return g.createdAt, g.lookupErr
}

// The finalize expiry guard fails closed on a read error (it returned nil, so
// a finalize went ahead on a guard that could not tell whether the calc had
// expired — the one failure nothing heals). "No row" and "no committing
// provider" still pass; a genuinely expired calc keeps its stable code.
func TestCheckTaxCalculationUsable_FailsClosedOnErrors(t *testing.T) {
	fresh := time.Now().UTC()
	newE := func(store *guardCalcStore) *Engine {
		return &Engine{settings: &taxSettings{provider: "stripe_tax"}, taxProviders: stubResolver(&stubProvider{}), taxCalcStore: store}
	}
	ctx := context.Background()

	for name, store := range map[string]*guardCalcStore{
		"link fails":   {linkErr: errBlip, createdAt: fresh},
		"lookup fails": {lookupErr: errBlip},
	} {
		if err := newE(store).CheckTaxCalculationUsable(ctx, "t1", "inv_1", "taxcalc_1"); !errors.Is(err, errBlip) {
			t.Errorf("%s: err = %v, want the read error", name, err)
		}
	}
	if err := newE(&guardCalcStore{lookupErr: errs.ErrNotFound}).CheckTaxCalculationUsable(ctx, "t1", "inv_1", "taxcalc_1"); err != nil {
		t.Errorf("no audit row: err = %v, want nil", err)
	}
	if err := newE(&guardCalcStore{createdAt: fresh}).CheckTaxCalculationUsable(ctx, "t1", "inv_1", "taxcalc_1"); err != nil {
		t.Errorf("fresh calc: err = %v, want nil", err)
	}
	err := newE(&guardCalcStore{createdAt: fresh.Add(-30 * time.Hour)}).CheckTaxCalculationUsable(ctx, "t1", "inv_1", "taxcalc_1")
	if errs.Code(err) != "tax_calculation_expired" {
		t.Errorf("expired calc: err = %v, want code tax_calculation_expired", err)
	}

	settingsErr := &Engine{settings: &failingSettings{}, taxProviders: stubResolver(&stubProvider{}), taxCalcStore: &guardCalcStore{createdAt: fresh}}
	if err := settingsErr.CheckTaxCalculationUsable(ctx, "t1", "inv_1", "taxcalc_1"); err == nil || !strings.Contains(err.Error(), "read settings") {
		t.Errorf("settings read fails: err = %v, want a settings error", err)
	}
}

// failingSettings is taxSettings whose Get fails.
type failingSettings struct{ taxSettings }

func (f *failingSettings) Get(context.Context, string) (domain.TenantSettings, error) {
	return domain.TenantSettings{}, errBlip
}

// A meter whose read fails ONCE (a blip on the aggregation lookup) fails the
// close; it retries next tick. It used to default the aggregation to "sum" and
// carry on — every later read of the meter succeeded, so a max/last meter was
// billed its TOTAL usage instead of its peak or last value, silently.
type blipOncePricing struct {
	*mockPricing
	meterID string
	blipped bool
}

func (b *blipOncePricing) GetMeter(ctx context.Context, tenantID, id string) (domain.Meter, error) {
	if id == b.meterID && !b.blipped {
		b.blipped = true
		return domain.Meter{}, errBlip
	}
	return b.mockPricing.GetMeter(ctx, tenantID, id)
}

func TestRunCycle_MeterReadBlipFailsTheClose(t *testing.T) {
	engine, _, _, pricing, invoices := setupEngine()
	engine.pricing = &blipOncePricing{mockPricing: pricing, meterID: "mtr_api"}
	count, errs := engine.RunCycle(context.Background(), 50, nil)
	if count != 0 || len(errs) == 0 {
		t.Fatalf("RunCycle = %d invoice(s), %v; want no invoice and an error", count, errs)
	}
	if len(invoices.invoices) != 0 {
		t.Fatalf("%d invoice(s) stored; a meter read blip must not bill", len(invoices.invoices))
	}
}

// blipOnSecondAggregate fails the second full-period aggregation — the cap
// path's pre-cap re-aggregation.
type blipOnSecondAggregate struct {
	*mockUsage
	calls int
}

func (b *blipOnSecondAggregate) AggregateForBillingPeriodByAgg(ctx context.Context, tenantID, customerID string, meters map[string]string, from, to time.Time) (map[string]decimal.Decimal, error) {
	b.calls++
	if b.calls == 2 {
		return nil, errBlip
	}
	return b.mockUsage.AggregateForBillingPeriodByAgg(ctx, tenantID, customerID, meters, from, to)
}

// A block-capped subscription whose cap re-aggregation fails stops the close.
// It used to leave the cap scale at 1, billing every unit above the cap.
func TestRunCycle_CapReaggregationBlipFailsTheClose(t *testing.T) {
	engine, subs, usage, _, invoices := setupEngine()
	sub := subs.subs["sub_1"]
	capUnits := int64(100)
	sub.UsageCapUnits, sub.OverageAction = &capUnits, "block"
	subs.subs["sub_1"] = sub
	engine.usage = &blipOnSecondAggregate{mockUsage: usage}

	count, errs := engine.RunCycle(context.Background(), 50, nil)
	if count != 0 || len(errs) == 0 || len(invoices.invoices) != 0 {
		t.Fatalf("RunCycle = %d invoice(s) stored=%d, errs %v; want no invoice and an error", count, len(invoices.invoices), errs)
	}
}

// A threshold fire's lines come from the preview path. A meter whose pinned
// rating rule is missing used to be skipped with a warning, so the fire
// invoice under-billed while the cycle close failed loudly on the same rule.
// Now the evaluation fails like the close does.
func TestEvaluateThresholds_MissingRuleFails(t *testing.T) {
	thresholds := &domain.BillingThresholds{AmountGTE: 100, ResetBillingCycle: false}
	engine, _, _ := setupThresholdEngine(thresholds, 1500)
	mp := engine.pricing.(*mockPricing)
	for id := range mp.rules {
		delete(mp.rules, id) // every pinned rule is now missing
	}
	sub, _ := engine.subs.Get(context.Background(), "t1", "sub_1")
	periodStart := *sub.CurrentBillingPeriodStart
	if _, err := engine.evaluateThresholds(context.Background(), sub, periodStart, periodStart.AddDate(0, 0, 5)); err == nil {
		t.Fatal("evaluateThresholds with a missing pinned rule must fail, not drop the line")
	}
}

// The tax-calculation row is required when there is a provider calculation to
// commit (the expiry guard reads it); a manual/none result has no calculation
// id and nothing reads its row, so a failed write stays a warning there.
func TestApplyTax_RecordRequiredOnlyForProviderCalculations(t *testing.T) {
	lines := func() []domain.InvoiceLineItem { return []domain.InvoiceLineItem{{AmountCents: 10000, Quantity: 1}} }
	withCalc := &Engine{
		settings:     &taxSettings{provider: "stripe_tax"},
		taxProviders: stubResolver(&stubProvider{result: &tax.Result{Provider: "stripe_tax", CalculationID: "taxcalc_1", TotalTaxCents: 0}}),
		taxCalcStore: &guardCalcStore{recordErr: errBlip},
	}
	if _, err := withCalc.ApplyTaxToLineItems(context.Background(), "t1", "cus_1", "USD", 10000, 0, lines()); !errors.Is(err, errBlip) {
		t.Fatalf("provider calculation, record fails: err = %v, want the record error", err)
	}
	manual := newManualEngine(1850, "VAT", nil)
	manual.taxCalcStore = &guardCalcStore{recordErr: errBlip}
	if _, err := manual.ApplyTaxToLineItems(context.Background(), "t1", "cus_1", "USD", 10000, 0, lines()); err != nil {
		t.Fatalf("manual tax, record fails: err = %v, want nil (audit-only)", err)
	}
}

// A provider that cannot be resolved fails the expiry guard too.
func TestCheckTaxCalculationUsable_ResolveErrorFails(t *testing.T) {
	e := &Engine{
		settings: &taxSettings{provider: "stripe_tax"},
		taxProviders: resolverFunc(func(context.Context, domain.TenantSettings) (tax.Provider, error) {
			return nil, errBlip
		}),
		taxCalcStore: &guardCalcStore{createdAt: time.Now().UTC()},
	}
	if err := e.CheckTaxCalculationUsable(context.Background(), "t1", "inv_1", "taxcalc_1"); !errors.Is(err, errBlip) {
		t.Fatalf("err = %v, want the resolve error", err)
	}
}

// Ingest accepts negative corrections, so a meter's running total can be
// negative mid-period. That must not abort the threshold evaluation (which
// would stop every threshold on the subscription from firing): it counts as 0.
func TestEvaluateThresholds_NegativeRunningUsageCountsAsZero(t *testing.T) {
	thresholds := &domain.BillingThresholds{AmountGTE: 100, ResetBillingCycle: false}
	engine, _, _ := setupThresholdEngine(thresholds, -500)
	sub, _ := engine.subs.Get(context.Background(), "t1", "sub_1")
	periodStart := *sub.CurrentBillingPeriodStart
	if _, err := engine.evaluateThresholds(context.Background(), sub, periodStart, periodStart.AddDate(0, 0, 5)); err != nil {
		t.Fatalf("a net-negative running total must not fail the evaluation: %v", err)
	}
}

// countingProvider counts Calculate calls (each one is a paid provider call).
type countingProvider struct {
	stubProvider
	calls int
}

func (c *countingProvider) Calculate(ctx context.Context, r tax.Request) (*tax.Result, error) {
	c.calls++
	return c.stubProvider.Calculate(ctx, r)
}

// Retry tax refreshes an OK draft's calculation only when the guard PROVES it
// expired. A guard that cannot read its inputs is not proof: refreshing on it
// mints a second calculation beside one that may still be committable.
func TestRetryTax_RefreshesOnlyAProvenExpiredCalculation(t *testing.T) {
	prov := &countingProvider{stubProvider: stubProvider{result: &tax.Result{Provider: "stripe_tax", CalculationID: "taxcalc_new"}}}
	e := &Engine{
		settings:     &taxSettings{provider: "stripe_tax"},
		taxProviders: stubResolver(prov),
		taxCalcStore: &guardCalcStore{lookupErr: errBlip},
		invoices: &mockInvoices{invoices: []domain.Invoice{{
			ID: "inv_1", TenantID: "t1", Status: domain.InvoiceDraft,
			TaxFacts: domain.TaxFacts{TaxStatus: domain.InvoiceTaxOK, TaxCalculationID: "taxcalc_old"},
		}}},
	}
	if _, err := e.RetryTaxForInvoice(context.Background(), "t1", "inv_1"); !errors.Is(err, errBlip) {
		t.Fatalf("err = %v, want the guard's read error", err)
	}
	if prov.calls != 0 {
		t.Fatalf("%d provider calculation(s) on an unproven expiry, want 0", prov.calls)
	}
}
