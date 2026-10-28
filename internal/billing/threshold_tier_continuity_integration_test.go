package billing_test

import (
	"context"
	"github.com/sagarsuperuser/velox/internal/usage"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/subscription"
)

// P21 (ADR-066 amendment 2026-10-11): a reset=false threshold fire bills
// [start, T) and the closer bills the rest; a graduated ladder must CONTINUE
// across them, not restart at tier 0. Ladder: 1,000 units at 10¢, then 2¢.
// 700 units before the fire (fire = $70), 600 after: the closer owes
// 300×10¢ + 300×2¢ = $36, not 600×10¢ = $60. Nothing turns the ladder off:
// the block usage cap that did (#973: under a 1,000-unit cap the close
// billed $46.15, so fire + close was $116.15 for F(1300) = $106) is cut.
//
// The fixture's price is edited to the graduated ladder in a version in
// force from before the period opened, so both invoices price at it; the
// ladder's seed is looked up by the price's id (ADR-130 §9), which the edit
// keeps.
type p21Fire struct {
	f         *thresholdFixture
	ctx       context.Context
	fireID    string
	fireTotal int64
	fireUpper time.Time
}

func p21FireAt700(t *testing.T, name string, beforeFire func(p *p21Fire)) *p21Fire {
	t.Helper()
	f := newThresholdFixture(t, name)
	ctx := postgres.WithLivemode(context.Background(), false)
	p := &p21Fire{f: f, ctx: ctx}

	editPrice(t, f.db, ctx, f.tenantID, f.planID, f.priceID, domain.PriceShape{Mode: domain.PricingGraduated, Tiers: []domain.RatingTier{
		{UpTo: new(int64(1000)), UnitAmountCents: decimal.NewFromInt(10)},
		{UpTo: nil, UnitAmountCents: decimal.NewFromInt(2)},
	}})
	backdateLatestVersion(t, f.db, ctx, f.priceID, f.cycleStart.Add(-2*time.Hour))
	if beforeFire != nil {
		beforeFire(p)
	}

	if _, err := f.subSvc.SetBillingThresholds(ctx, f.tenantID, f.subID, subscription.BillingThresholdsInput{
		AmountGTE: 5000,
	}); err != nil {
		t.Fatalf("set thresholds: %v", err)
	}
	f.ingestUsage(t, ctx, 7, 100) // 700 units → $70 ≥ $50
	fired, errs := f.engine.ScanThresholds(ctx, 50)
	if len(errs) > 0 || fired != 1 {
		t.Fatalf("threshold scan: fired=%d errs=%v, want 1 fire", fired, errs)
	}
	invs := f.listInvoices(t, ctx)
	if len(invs) != 1 {
		t.Fatalf("fire invoices: got %d, want 1", len(invs))
	}
	p.fireID = invs[0].ID
	var lines []domain.InvoiceLineItem
	p.fireTotal, lines = usageLineTotal(t, f, ctx, p.fireID)
	if len(lines) == 0 || lines[0].BillingPeriodEnd == nil {
		t.Fatalf("fire has no usage line")
	}
	p.fireUpper = lines[0].BillingPeriodEnd.UTC()
	return p
}

// seedAfterFire inserts usage after the fire watermark; shrinkTo, when set,
// also ends the period just past it so the cycle close is due.
func (p *p21Fire) seedAfterFire(t *testing.T, qty int64, shrinkTo *time.Time) {
	t.Helper()
	tx, err := p.f.db.BeginTx(p.ctx, postgres.TxBypass, "")
	if err != nil {
		t.Fatalf("begin bypass: %v", err)
	}
	defer postgres.Rollback(tx)
	if _, err := tx.Exec(`SET LOCAL app.livemode = 'off'`); err != nil {
		t.Fatalf("set local livemode: %v", err)
	}
	if _, err := tx.Exec(`
		INSERT INTO usage_events (id, tenant_id, customer_id, meter_id, quantity, timestamp, livemode)
		VALUES ($1, $2, $3, $4, $5, $6, false)
	`, postgres.NewID("hc_ue"), p.f.tenantID, p.f.customerID, p.f.meterID, qty, p.fireUpper.Add(time.Second).Truncate(time.Microsecond)); err != nil {
		t.Fatalf("seed residual usage: %v", err)
	}
	if shrinkTo != nil {
		if _, err := tx.Exec(`UPDATE subscriptions SET current_billing_period_end = $1, next_billing_at = $1 WHERE id = $2`, *shrinkTo, p.f.subID); err != nil {
			t.Fatalf("shrink period: %v", err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit residual seed: %v", err)
	}
}

func TestThresholdCycle_GraduatedTiersContinueAcrossFire_RealStore(t *testing.T) {
	p := p21FireAt700(t, "P21 Tier Continuity", nil)
	if p.fireTotal != 7000 {
		t.Fatalf("fire usage = %d, want 7000", p.fireTotal)
	}
	newEnd := p.fireUpper.Add(2 * time.Second).Truncate(time.Microsecond)
	p.seedAfterFire(t, 600, &newEnd)
	if wait := time.Until(newEnd.Add(500 * time.Millisecond)); wait > 0 {
		time.Sleep(wait)
	}
	generated, failures := p.f.engine.RunCycleForTenant(p.ctx, p.f.tenantID, 50)
	if len(failures) > 0 || generated != 1 {
		t.Fatalf("cycle: generated=%d failures=%v, want 1", generated, failures)
	}
	var closeTotal int64 = -1
	for _, inv := range p.f.listInvoices(t, p.ctx) {
		if inv.ID != p.fireID {
			closeTotal, _ = usageLineTotal(t, p.f, p.ctx, inv.ID)
		}
	}
	if closeTotal != 3600 {
		t.Errorf("close usage = %d, want 3600 (300×10¢ + 300×2¢, continuing the ladder); 6000 is the restarted ladder", closeTotal)
	}
	if p.fireTotal+closeTotal != 10600 {
		t.Errorf("fire + close = %d, want 10600 = F(1300)", p.fireTotal+closeTotal)
	}
}

// The immediate-cancel closer continues the same ladder.
func TestImmediateCancel_GraduatedTiersContinueAcrossFire_RealStore(t *testing.T) {
	p := p21FireAt700(t, "P21 Cancel Continuity", nil)
	p.seedAfterFire(t, 600, nil)
	time.Sleep(1500 * time.Millisecond) // the residual event must be before the cancel instant
	canceledAt := time.Now().UTC().Truncate(time.Microsecond)
	tx, err := p.f.db.BeginTx(p.ctx, postgres.TxTenant, p.f.tenantID)
	if err != nil {
		t.Fatalf("begin cancel: %v", err)
	}
	if _, err := tx.ExecContext(p.ctx, `UPDATE subscriptions SET status = 'canceled', canceled_at = $1 WHERE id = $2`, canceledAt, p.f.subID); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit cancel: %v", err)
	}
	sub, err := p.f.subStore.Get(p.ctx, p.f.tenantID, p.f.subID)
	if err != nil {
		t.Fatalf("get sub: %v", err)
	}
	finalInv, err := p.f.engine.BillFinalOnImmediateCancel(p.ctx, sub)
	if err != nil || finalInv.ID == "" {
		t.Fatalf("bill final on cancel: inv=%q err=%v", finalInv.ID, err)
	}
	if got, _ := usageLineTotal(t, p.f, p.ctx, finalInv.ID); got != 3600 {
		t.Errorf("cancel-final usage = %d, want 3600 (continuing the ladder); 6000 is the restarted ladder", got)
	}
}

// A meter with several prices is read per price (AggregateClaims); each
// price's ladder continues there. The ladder is keyed by price id (ADR-130
// §9): the fire bills two lines on the meter, the catch-all's 700 units and
// a graduated model=gpt price's 50, and the close seeds each price from its
// own line. Keyed by meter, the close could not tell the two lines apart.
func TestThresholdCycle_MultiPriceBucketContinuesAcrossFire_RealStore(t *testing.T) {
	p := p21FireAt700(t, "P21 Multi-price", func(p *p21Fire) {
		other := addPrice(t, p.f.db, p.ctx, p.f.tenantID, p.f.planID, priceOn{meterID: p.f.meterID, match: map[string]string{"model": "gpt"},
			shape: domain.PriceShape{Mode: domain.PricingGraduated, Tiers: []domain.RatingTier{{UpTo: new(int64(100)), UnitAmountCents: decimal.NewFromInt(50)}, {UpTo: nil, UnitAmountCents: decimal.NewFromInt(5)}}}})
		backdatePriceVersions(t, p.f.db, p.ctx, other.ID, p.f.cycleStart.Add(-2*time.Hour))
		// 50 gpt units: 50 × 50 = 2500, inside the first tier.
		ts := p.f.cycleStart.Add(30 * time.Second)
		if _, err := p.f.usageSvc.Ingest(p.ctx, p.f.tenantID, usage.IngestInput{
			CustomerID: p.f.customerID, MeterID: p.f.meterID, Quantity: decimal.NewFromInt(50),
			Timestamp: &ts, Dimensions: map[string]any{"model": "gpt"},
		}); err != nil {
			t.Fatalf("ingest gpt usage: %v", err)
		}
	})
	if p.fireTotal != 9500 {
		t.Fatalf("fire usage = %d, want 9500 (7000 for the catch-all bucket, 2500 for gpt)", p.fireTotal)
	}
	newEnd := p.fireUpper.Add(2 * time.Second).Truncate(time.Microsecond)
	p.seedAfterFire(t, 600, &newEnd)
	if wait := time.Until(newEnd.Add(500 * time.Millisecond)); wait > 0 {
		time.Sleep(wait)
	}
	generated, failures := p.f.engine.RunCycleForTenant(p.ctx, p.f.tenantID, 50)
	if len(failures) > 0 || generated != 1 {
		t.Fatalf("cycle: generated=%d failures=%v, want 1", generated, failures)
	}
	for _, inv := range p.f.listInvoices(t, p.ctx) {
		if inv.ID == p.fireID {
			continue
		}
		if got, _ := usageLineTotal(t, p.f, p.ctx, inv.ID); got != 3600 {
			t.Errorf("multi-price close usage = %d, want 3600 (the catch-all continuing its ladder, gpt adding nothing); 6000 is the restarted ladder", got)
		}
	}
}
