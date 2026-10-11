package billing_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/pricing"
	"github.com/sagarsuperuser/velox/internal/subscription"
)

// P21 (ADR-066 amendment 2026-10-11): a reset=false threshold fire bills
// [start, T) and the closer bills the rest; a graduated ladder must CONTINUE
// across them, not restart at tier 0. Ladder: 1,000 units at 10¢, then 2¢.
// 700 units before the fire (fire = $70), 600 after: the closer owes
// 300×10¢ + 300×2¢ = $36, not 600×10¢ = $60.
//
// The graduated version is published under the meter's rule key BEFORE the
// period opened while the meter's binding still pins the flat v1 — so both
// invoices resolve to v2 and the seed must be looked up by the RESOLVED id;
// a key on the binding finds nothing and restarts the tiers.
type p21Fire struct {
	f         *thresholdFixture
	ctx       context.Context
	v1, v2    domain.RatingRuleVersion
	fireID    string
	fireTotal int64
	fireUpper time.Time
}

func p21FireAt700(t *testing.T, name string, beforeFire func(p *p21Fire)) *p21Fire {
	t.Helper()
	f := newThresholdFixture(t, name)
	ctx := postgres.WithLivemode(context.Background(), false)
	p := &p21Fire{f: f, ctx: ctx}

	p.v1 = f.seededRule(t, ctx)
	backdateRuleVersion(t, f, ctx, p.v1.ID, f.cycleStart.Add(-3*time.Hour))
	v2, err := f.pricingSvc.CreateRatingRule(ctx, f.tenantID, pricing.CreateRatingRuleInput{
		RuleKey: "calls_flat", Name: "Calls Graduated", Mode: domain.PricingGraduated, Currency: "USD",
		GraduatedTiers: []domain.RatingTier{
			{UpTo: 1000, UnitAmountCents: decimal.NewFromInt(10)},
			{UpTo: 0, UnitAmountCents: decimal.NewFromInt(2)},
		},
	})
	if err != nil {
		t.Fatalf("publish graduated v2: %v", err)
	}
	backdateRuleVersion(t, f, ctx, v2.ID, f.cycleStart.Add(-2*time.Hour))
	p.v2 = v2
	if beforeFire != nil {
		beforeFire(p)
	}

	noReset := false
	if _, err := f.subSvc.SetBillingThresholds(ctx, f.tenantID, f.subID, subscription.BillingThresholdsInput{
		AmountGTE: 5000, ResetBillingCycle: &noReset,
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
	`, postgres.NewID("vlx_ue"), p.f.tenantID, p.f.customerID, p.f.meterID, qty, p.fireUpper.Add(time.Second).Truncate(time.Microsecond)); err != nil {
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

// A pricing rule whose version resolves to the same version as the meter's
// default binding makes the fire's line ambiguous (lines carry no
// pricing-rule id): the close refuses loudly instead of guessing whose tiers
// to continue. Pinned to v1 (the binding itself) and to v2 (a different pin
// that resolves to the same graduated version) — the guard must compare
// RESOLVED versions.
func TestThresholdCycle_AmbiguousSeedFailsLoud_RealStore(t *testing.T) {
	for _, pin := range []string{"v1", "v2"} {
		t.Run("rule pinned to "+pin, func(t *testing.T) {
			p := p21FireAt700(t, "P21 Ambiguous Seed "+pin, func(p *p21Fire) {
				id := p.v1.ID
				if pin == "v2" {
					id = p.v2.ID
				}
				if _, err := p.f.pricingSvc.UpsertMeterPricingRule(p.ctx, p.f.tenantID, pricing.UpsertMeterPricingRuleInput{
					MeterID: p.f.meterID, RatingRuleVersionID: id,
					DimensionMatch: map[string]any{"model": "gpt"}, AggregationMode: domain.AggSum,
				}); err != nil {
					t.Fatalf("pricing rule: %v", err)
				}
			})
			newEnd := p.fireUpper.Add(2 * time.Second).Truncate(time.Microsecond)
			p.seedAfterFire(t, 600, &newEnd)
			if wait := time.Until(newEnd.Add(500 * time.Millisecond)); wait > 0 {
				time.Sleep(wait)
			}
			generated, failures := p.f.engine.RunCycleForTenant(p.ctx, p.f.tenantID, 50)
			if generated != 0 || len(failures) == 0 || !strings.Contains(failures[0].Error(), "resolve to rating rule") {
				t.Fatalf("cycle: generated=%d failures=%v, want the ambiguous-seed error and no invoice", generated, failures)
			}
		})
	}
}

// The multi-dim close path (a meter with pricing rules) continues the
// unclaimed bucket's ladder too: a pricing rule on a DIFFERENT graduated rule
// key keeps the buckets unambiguous, so the default bucket (no dimensions)
// continues from the fire's 700 units.
func TestThresholdCycle_MultiDimBucketContinuesAcrossFire_RealStore(t *testing.T) {
	p := p21FireAt700(t, "P21 Multi-dim", func(p *p21Fire) {
		other, err := p.f.pricingSvc.CreateRatingRule(p.ctx, p.f.tenantID, pricing.CreateRatingRuleInput{
			RuleKey: "gpt_calls", Name: "GPT Calls", Mode: domain.PricingGraduated, Currency: "USD",
			GraduatedTiers: []domain.RatingTier{{UpTo: 100, UnitAmountCents: decimal.NewFromInt(50)}, {UpTo: 0, UnitAmountCents: decimal.NewFromInt(5)}},
		})
		if err != nil {
			t.Fatalf("other rule: %v", err)
		}
		backdateRuleVersion(t, p.f, p.ctx, other.ID, p.f.cycleStart.Add(-2*time.Hour))
		if _, err := p.f.pricingSvc.UpsertMeterPricingRule(p.ctx, p.f.tenantID, pricing.UpsertMeterPricingRuleInput{
			MeterID: p.f.meterID, RatingRuleVersionID: other.ID,
			DimensionMatch: map[string]any{"model": "gpt"}, AggregationMode: domain.AggSum,
		}); err != nil {
			t.Fatalf("pricing rule: %v", err)
		}
	})
	if p.fireTotal != 7000 {
		t.Fatalf("fire usage = %d, want 7000 (the unclaimed bucket on v2)", p.fireTotal)
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
			t.Errorf("multi-dim close usage = %d, want 3600 (continuing the ladder); 6000 is the restarted ladder", got)
		}
	}
}
