package billing

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/gethardcount/hardcount/internal/domain"
)

// Review finding (PR #1376, round 2). A closer read a meter twice: once for
// the aggregation its usage is totalled under (planMeterAggregations), and
// once for the aggregation its bucket is routed and laddered by (the rater).
// Since a meter's aggregation can change until its first event (ADR-130 §2),
// a PATCH could land between the two reads, and the close totalled the usage
// under one aggregation while routing it as the other. A close now reads
// each meter once.

// patchedBetweenReads is the pricing store with a concurrent resolver: the
// first read of meterID returns it as sum, and every later read returns it
// as max, as if an aggregation PATCH committed right after the first read.
type patchedBetweenReads struct {
	*mockPricing
	meterID string
	mu      sync.Mutex
	reads   int
}

func (p *patchedBetweenReads) GetMeter(ctx context.Context, tenantID, id string) (domain.Meter, error) {
	m, err := p.mockPricing.GetMeter(ctx, tenantID, id)
	if err != nil || id != p.meterID {
		return m, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.reads++
	m.Aggregation = "sum"
	if p.reads > 1 {
		m.Aggregation = "max"
	}
	return m, nil
}

// aggReads records, for every whole-invoice usage read, where its window
// starts and the aggregation it reads meterID under.
type aggReads struct {
	*mockUsage
	meterID string
	mu      sync.Mutex
	reads   []aggRead
}

type aggRead struct {
	from time.Time
	agg  string
}

func (r *aggReads) AggregateForBillingPeriodByAgg(ctx context.Context, tenantID, customerID string, meters map[string]string, from, to time.Time) (map[string]decimal.Decimal, error) {
	r.mu.Lock()
	r.reads = append(r.reads, aggRead{from: from, agg: meters[r.meterID]})
	r.mu.Unlock()
	return r.mockUsage.AggregateForBillingPeriodByAgg(ctx, tenantID, customerID, meters, from, to)
}

// After a threshold invoice that billed only mtr_api, mtr_storage (no
// pricing rules) is read over the whole period only if the close treats it
// as max (deferred), and over the clamped window if it treats it as sum. A
// read of the whole period under sum is the close mixing the two.
//
// Mutation-verify: route the closers' rater through e.prices() again (a
// second meter read) → the whole period is read under sum, and this fails.
func TestCloser_ReadsEachMeterOnce(t *testing.T) {
	billedThrough := time.Date(2026, 3, 10, 0, 0, 0, 0, time.UTC)
	apiOnly := thresholdWatermark{billedThrough: &billedThrough, lines: []domain.InvoiceLineItem{
		usageLine("mtr_api", "prc_api", 700, 7000),
	}}
	for name, run := range map[string]func(e *Engine, sub domain.Subscription, plans map[string]domain.Plan) error{
		"cancel": func(e *Engine, sub domain.Subscription, plans map[string]domain.Plan) error {
			canceledAt := time.Date(2026, 3, 20, 0, 0, 0, 0, time.UTC)
			_, _, err := e.buildCancelLineItems(context.Background(), sub, apiOnly, plans, "USD", *sub.CurrentBillingPeriodStart, canceledAt, time.UTC, unpricedUsage{}, netNegativeUsage{}, cancelUsageSuffix)
			return err
		},
		"cycle close": func(e *Engine, sub domain.Subscription, plans map[string]domain.Plan) error {
			ps, pe := *sub.CurrentBillingPeriodStart, *sub.CurrentBillingPeriodEnd
			_, _, _, err := e.buildLineItems(context.Background(), sub, pe, ps, pe, plans, "USD", time.UTC, unpricedUsage{}, netNegativeUsage{})
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			engine, subs, usage, pricing, invoices := setupEngine()
			invoices.invoices = append(invoices.invoices, domain.Invoice{
				ID: "inv_thr", TenantID: "t1", CustomerID: "cus_1", SubscriptionID: "sub_1",
				BillingReason: domain.BillingReasonThreshold, Status: domain.InvoiceFinalized,
				BillingPeriodStart: time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC), BillingPeriodEnd: billedThrough,
			})
			for _, li := range apiOnly.lines {
				li.InvoiceID = "inv_thr"
				invoices.lineItems = append(invoices.lineItems, li)
			}
			engine.pricing = &patchedBetweenReads{mockPricing: pricing, meterID: "mtr_storage"}
			reads := &aggReads{mockUsage: usage, meterID: "mtr_storage"}
			engine.usage = reads
			sub := subs.subs["sub_1"]
			if err := run(engine, sub, clonePlans(pricing.plans)); err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			ps := *sub.CurrentBillingPeriodStart
			for _, r := range reads.reads {
				if r.from.Equal(ps) && !nonAdditive(r.agg) {
					t.Fatalf("reads %+v: the whole period was read under %q, a window only a deferred max or last bucket is read over; the close totalled under one aggregation and routed under another", reads.reads, r.agg)
				}
			}
		})
	}
}
