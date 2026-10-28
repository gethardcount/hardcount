package billing_test

import (
	"context"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/pricing"
	"github.com/gethardcount/hardcount/internal/subscription"
)

// Price edits against real Postgres (ADR-070, ADR-130 §5-§6). The decisions
// under test: (a) an override is keyed by its price and follows it across
// content edits; (b) the version (and override) in force at the period
// start prices the whole period: the cycle close, the threshold invoice and
// the preview agree across an edit saved mid-period. The fixture's plan is
// made before its subscription, so its price is in force from the beginning
// of time; the subscription makes the plan used, so an edit in the test
// appends a version from now on.

// createBackdatedOverride sets a negotiated per_unit price for the fixture
// customer on a price, effective BEFORE the period opened (an override
// created mid-period prices from the next period by design — these tests
// model a deal already in place).
func createBackdatedOverride(t *testing.T, f *thresholdFixture, ctx context.Context, priceID string, cents int64) domain.CustomerPriceOverride {
	t.Helper()
	o, err := f.pricingSvc.CreateOverride(ctx, f.tenantID, pricing.CreateOverrideInput{
		CustomerID: f.customerID,
		PriceID:    priceID,
		PriceShape: domain.PriceShape{Mode: domain.PricingPerUnit, UnitAmountCents: decimal.NewFromInt(cents)},
		Reason:     "negotiated",
	})
	if err != nil {
		t.Fatalf("create override: %v", err)
	}
	tx, err := f.db.BeginTx(ctx, postgres.TxBypass, "")
	if err != nil {
		t.Fatalf("begin bypass: %v", err)
	}
	defer postgres.Rollback(tx)
	if _, err := tx.Exec(`UPDATE customer_price_overrides SET created_at = $1 WHERE id = $2`,
		f.cycleStart.Add(-time.Hour), o.ID); err != nil {
		t.Fatalf("backdate override: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit backdate: %v", err)
	}
	return o
}

// closeCycle rewinds the sub's period end to the past and runs the
// cycle scan (the standard close-drive pattern in this suite).
func closeCycle(t *testing.T, f *thresholdFixture, ctx context.Context) {
	t.Helper()
	pastEnd := time.Now().UTC().Add(-1 * time.Hour).Truncate(time.Microsecond)
	tx, err := f.db.BeginTx(ctx, postgres.TxTenant, f.tenantID)
	if err != nil {
		t.Fatalf("begin rewind tx: %v", err)
	}
	defer postgres.Rollback(tx)
	if _, err := tx.ExecContext(ctx, `
		UPDATE subscriptions SET current_billing_period_end = $1, next_billing_at = $1 WHERE id = $2
	`, pastEnd, f.subID); err != nil {
		t.Fatalf("rewind period: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit rewind: %v", err)
	}
	generated, failures := f.engine.RunCycleForTenant(ctx, f.tenantID, 50)
	if len(failures) > 0 {
		t.Fatalf("cycle failures: %v", failures)
	}
	if generated != 1 {
		t.Fatalf("cycle generated = %d, want 1", generated)
	}
}

// editFixturePrice saves a new per_unit rate for the fixture's price now,
// mid-period on a used plan: it applies from the next period.
func editFixturePrice(t *testing.T, f *thresholdFixture, ctx context.Context, cents int64) {
	t.Helper()
	pw, err := f.pricingSvc.EditPrice(ctx, f.tenantID, f.planID, f.priceID, domain.PriceShape{Mode: domain.PricingPerUnit, UnitAmountCents: decimal.NewFromInt(cents)})
	if err != nil {
		t.Fatalf("edit price: %v", err)
	}
	if pw.Effective != pricing.EffectiveNextPeriod {
		t.Fatalf("edit effective %q, want next_period", pw.Effective)
	}
}

func usageLineTotal(t *testing.T, f *thresholdFixture, ctx context.Context, invoiceID string) (int64, []domain.InvoiceLineItem) {
	t.Helper()
	lines, err := f.invStore.ListLineItems(ctx, f.tenantID, invoiceID)
	if err != nil {
		t.Fatalf("list line items: %v", err)
	}
	var total int64
	var usageLines []domain.InvoiceLineItem
	for _, l := range lines {
		if l.LineType == domain.LineTypeUsage {
			total += l.AmountCents
			usageLines = append(usageLines, l)
			// Every usage line must carry its meter's unit (migration
			// 0173): it's what lets the invoice surfaces render token
			// rates per-1M. Asserted here — in the helper the cycle-close,
			// threshold and preview tests funnel through — so dropping the
			// stamp at THOSE construction sites fails real-Postgres tests.
			// NOT covered: the two buildCancelLineItems sites (engine.go
			// cancel path). They stamp correctly today, but no test routes
			// a cancel invoice through this assertion (2026-08-09 audit).
			if l.MeterUnit == "" {
				t.Fatalf("usage line %q has no MeterUnit — the 0173 stamp was dropped at its construction site", l.Description)
			}
		}
	}
	return total, usageLines
}

// TestPriceChange_OverrideFollowsAnEdit is the audit-High detach regression:
// a negotiated override must keep applying after its price is edited. Before
// ADR-070 the close resolved the latest version and looked the override up
// by that version's id, so a publish made the override unfindable and the
// customer silently reverted to list price for the whole period. An
// override now keys on the price id, which an edit keeps.
//
// Mutation-verify: look the override up by anything that changes on an edit
// (rate.go planPrices.Price) — this test fails.
func TestPriceChange_OverrideFollowsAnEdit(t *testing.T) {
	f := newThresholdFixture(t, "P4 Detach")
	ctx := postgres.WithLivemode(context.Background(), false)

	createBackdatedOverride(t, f, ctx, f.priceID, 5) // negotiated 5c/call (list 1c)
	editFixturePrice(t, f, ctx, 2)                   // mid-period edit to 2c/call

	f.ingestUsage(t, ctx, 100, 10) // 1000 calls
	closeCycle(t, f, ctx)

	invoices := f.listInvoices(t, ctx)
	if len(invoices) != 1 {
		t.Fatalf("invoices: got %d, want 1", len(invoices))
	}
	total, usageLines := usageLineTotal(t, f, ctx, invoices[0].ID)
	// 1000 calls × 5c override = 5000c. List would be 1000c; the edit 2000c
	// — either means the override detached or the edit repriced the period.
	if total != 5000 {
		t.Errorf("usage total: got %d cents, want 5000 (the override follows the edit; 1000=list, 2000=the edited list)", total)
	}
	if len(usageLines) == 1 && usageLines[0].PriceID != f.priceID {
		t.Errorf("line price: got %s, want %s", usageLines[0].PriceID, f.priceID)
	}
	// ADR-054 re-examination — the override trap: the stamped nominal rate is
	// the OVERRIDE-applied rate (5c/call), NOT the list price of the price id
	// (1c). The override keeps the price id, so a derive-on-read from the
	// price would misdisplay list price on this negotiated line — which is
	// exactly why the rate is stamped at build from the resolved
	// (override-applied) price. Mutation-verify: stamp the list rate instead
	// and this fails with got 1.
	if len(usageLines) == 1 {
		nom := usageLines[0].NominalUnitAmountDecimal
		if nom == nil {
			t.Errorf("nominal rate: got nil, want 5 (a per_unit line must stamp its configured rate)")
		} else if !nom.Equal(decimal.NewFromInt(5)) {
			t.Errorf("nominal rate: got %s, want 5 (the negotiated override, not the 1c list)", nom.String())
		}
	}
}

// TestPriceChange_OverrideFollowsAnEdit_ClaimedRead: the same on a meter read
// per price (AggregateClaims), which a meter with more than one price is. A
// second price for model=x is in place before the period opened.
func TestPriceChange_OverrideFollowsAnEdit_ClaimedRead(t *testing.T) {
	f := newThresholdFixture(t, "P4 Detach Claimed")
	ctx := postgres.WithLivemode(context.Background(), false)

	other := addPrice(t, f.db, ctx, f.tenantID, f.planID, priceOn{meterID: f.meterID, match: map[string]string{"model": "x"}, shape: perUnitShape("9")})
	backdatePriceVersions(t, f.db, ctx, other.ID, f.cycleStart.Add(-2*time.Hour))
	createBackdatedOverride(t, f, ctx, f.priceID, 5)
	editFixturePrice(t, f, ctx, 2)

	f.ingestUsage(t, ctx, 100, 10)
	closeCycle(t, f, ctx)

	invoices := f.listInvoices(t, ctx)
	if len(invoices) != 1 {
		t.Fatalf("invoices: got %d, want 1", len(invoices))
	}
	total, _ := usageLineTotal(t, f, ctx, invoices[0].ID)
	if total != 5000 {
		t.Errorf("usage total (claimed read): got %d cents, want 5000 (the override follows the edit)", total)
	}
}

// TestPriceChange_FireThenEditThenClose: one period, one price. The
// threshold fire bills the pre-fire window at the version in force at the
// period start; an edit lands mid-period; the cycle close bills the
// residual at the SAME version, never the edit (before ADR-070 the close
// resolved the latest version while the fire priced the pinned one: two
// prices in one period).
//
// Mutation-verify: resolve the close's prices at now instead of the period
// start — the close prices at the edit and this test fails.
func TestPriceChange_FireThenEditThenClose(t *testing.T) {
	f := newThresholdFixture(t, "P4 FireEditClose")
	ctx := postgres.WithLivemode(context.Background(), false)

	// Threshold: fire once the running subtotal crosses $5, keep the
	// cycle (reset=false) so the close bills the residual window of the
	// SAME period.
	if _, err := f.subSvc.SetBillingThresholds(ctx, f.tenantID, f.subID, subscription.BillingThresholdsInput{
		AmountGTE: 500,
	}); err != nil {
		t.Fatalf("set thresholds: %v", err)
	}

	f.ingestUsage(t, ctx, 100, 10) // 1000 calls × 1c = 1000c ≥ 500c
	fired, errs := f.engine.ScanThresholds(ctx, 50)
	if len(errs) > 0 {
		t.Fatalf("scan errors: %v", errs)
	}
	if fired != 1 {
		t.Fatalf("threshold fired = %d, want 1", fired)
	}

	// Mid-period edit to 5c/call.
	editFixturePrice(t, f, ctx, 5)

	// Residual usage AFTER the fire watermark (the fire invoice's line
	// BillingPeriodEnd), inside a period end just past it — the close's
	// watermark protocol bills exactly [fireUpper, periodEnd).
	fireInvoices := f.listInvoices(t, ctx)
	if len(fireInvoices) != 1 {
		t.Fatalf("fire invoices: got %d, want 1", len(fireInvoices))
	}
	fireLines, err := f.invStore.ListLineItems(ctx, f.tenantID, fireInvoices[0].ID)
	if err != nil || len(fireLines) == 0 || fireLines[0].BillingPeriodEnd == nil {
		t.Fatalf("read fire watermark: err=%v lines=%d", err, len(fireLines))
	}
	fireUpper := fireLines[0].BillingPeriodEnd.UTC()
	residualAt := fireUpper.Add(time.Second).Truncate(time.Microsecond)
	newEnd := fireUpper.Add(2 * time.Second).Truncate(time.Microsecond)

	tx, err := f.db.BeginTx(ctx, postgres.TxBypass, "")
	if err != nil {
		t.Fatalf("begin bypass: %v", err)
	}
	// The 0021 trigger overwrites livemode from the session GUC — a
	// bypass tx must SET LOCAL or the row lands in live mode, invisible
	// to the test-mode aggregation.
	if _, err := tx.Exec(`SET LOCAL app.livemode = 'off'`); err != nil {
		t.Fatalf("set local livemode: %v", err)
	}
	if _, err := tx.Exec(`
		INSERT INTO usage_events (id, tenant_id, customer_id, meter_id, quantity, timestamp, livemode)
		VALUES ($1, $2, $3, $4, 500, $5, false)
	`, postgres.NewID("hc_ue"), f.tenantID, f.customerID, f.meterID, residualAt); err != nil {
		t.Fatalf("seed residual usage: %v", err)
	}
	if _, err := tx.Exec(`
		UPDATE subscriptions SET current_billing_period_end = $1, next_billing_at = $1 WHERE id = $2
	`, newEnd, f.subID); err != nil {
		t.Fatalf("shrink period to just past the fire: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit residual seed: %v", err)
	}

	// The shrunken period end sits ~2s past the fire watermark — wait
	// for the boundary to elapse so the close scan sees the sub as due.
	if wait := time.Until(newEnd.Add(500 * time.Millisecond)); wait > 0 {
		time.Sleep(wait)
	}
	generated, failures := f.engine.RunCycleForTenant(ctx, f.tenantID, 50)
	if len(failures) > 0 {
		t.Fatalf("cycle failures: %v", failures)
	}
	if generated != 1 {
		t.Fatalf("cycle generated = %d, want 1", generated)
	}

	invoices := f.listInvoices(t, ctx)
	if len(invoices) != 2 {
		t.Fatalf("invoices: got %d, want 2 (fire + close)", len(invoices))
	}
	var totals []int64
	for _, inv := range invoices {
		tot, _ := usageLineTotal(t, f, ctx, inv.ID)
		totals = append(totals, tot)
	}
	// Fire billed 1000 calls at 1c (1000c); close bills the residual 500
	// calls at the SAME 1c (500c) — 2500c would mean the edit leaked in.
	seen := map[int64]bool{totals[0]: true, totals[1]: true}
	if !seen[1000] || !seen[500] {
		t.Errorf("fire+close usage totals: got %v, want {1000, 500} — the whole period prices at the version in force at its start", totals)
	}
}

// TestPriceChange_PreviewMatchesCloseAcrossAnEdit CI-locks that the preview
// prices a period the way its close will after a mid-period edit: both read
// the version and override in force at the period start, through one rater.
func TestPriceChange_PreviewMatchesCloseAcrossAnEdit(t *testing.T) {
	f := newThresholdFixture(t, "P4 PreviewParity")
	ctx := postgres.WithLivemode(context.Background(), false)

	createBackdatedOverride(t, f, ctx, f.priceID, 3)
	editFixturePrice(t, f, ctx, 7)

	f.ingestUsage(t, ctx, 100, 10) // 1000 calls

	preview := f.createPreview(t, ctx, f.customerID, f.subID)
	if len(preview.Totals) != 1 {
		t.Fatalf("preview totals: got %d entries (%v), want 1 — overridden lines must carry the plan's currency", len(preview.Totals), preview.Totals)
	}
	previewTotal := preview.Totals[0].AmountCents

	closeCycle(t, f, ctx)
	invoices := f.listInvoices(t, ctx)
	if len(invoices) != 1 {
		t.Fatalf("invoices: got %d, want 1", len(invoices))
	}
	closeTotal, _ := usageLineTotal(t, f, ctx, invoices[0].ID)

	if previewTotal != closeTotal {
		t.Errorf("preview total %d != close usage total %d (must agree across an edit)", previewTotal, closeTotal)
	}
	if closeTotal != 3000 {
		t.Errorf("close total: got %d, want 3000 (1000 calls × 3c override)", closeTotal)
	}
}

// TestThresholdFire_ZeroBaseAllOverride_ResolvesCurrency: the fixture sub
// has NO base fee, and its only usage price is overridden. A line takes the
// plan's currency: an override carries none (ADR-130 §11), so a fire on an
// all-override, zero-base subscription still resolves its invoice currency.
// (An earlier override shape fabricated a rule with an empty currency and the
// fire hard-failed "no invoice currency resolved".)
func TestThresholdFire_ZeroBaseAllOverride_ResolvesCurrency(t *testing.T) {
	f := newThresholdFixture(t, "P4 ZeroBaseOverrideFire")
	ctx := postgres.WithLivemode(context.Background(), false)

	createBackdatedOverride(t, f, ctx, f.priceID, 5)

	if _, err := f.subSvc.SetBillingThresholds(ctx, f.tenantID, f.subID, subscription.BillingThresholdsInput{
		AmountGTE: 500,
	}); err != nil {
		t.Fatalf("set thresholds: %v", err)
	}

	f.ingestUsage(t, ctx, 100, 10) // 1000 calls × 5c override = 5000c ≥ 500c
	fired, errs := f.engine.ScanThresholds(ctx, 50)
	if len(errs) > 0 {
		t.Fatalf("scan errors: %v (pre-fix: 'no invoice currency resolved')", errs)
	}
	if fired != 1 {
		t.Fatalf("threshold fired = %d, want 1", fired)
	}

	invoices := f.listInvoices(t, ctx)
	if len(invoices) != 1 {
		t.Fatalf("invoices: got %d, want 1", len(invoices))
	}
	if invoices[0].Currency != "USD" {
		t.Errorf("fire invoice currency: got %q, want USD (the plan's)", invoices[0].Currency)
	}
	total, _ := usageLineTotal(t, f, ctx, invoices[0].ID)
	if total != 5000 {
		t.Errorf("fire usage total: got %d, want 5000 (override price)", total)
	}
}
