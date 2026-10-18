package billing

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"time"

	"github.com/shopspring/decimal"

	"github.com/sagarsuperuser/velox/internal/audit"
	"github.com/sagarsuperuser/velox/internal/domain"
)

// Usage below zero (#1193). Ingest accepts negative quantities as usage
// corrections, so a usage bucket can total below zero over a billing window
// when the corrections are larger than the usage in that window. A negative
// quantity has no price (ComputeAmountCents refuses it). Before #1193 the
// period close and the cancel's final invoice failed on a pricing-rule
// bucket below zero, so the whole invoice, base fee included, was not made
// and the close was retried. A single-rule meter below zero was billed 0
// with no word, because the usage store dropped meter totals at or below
// zero; it now returns them so the rule below can apply.
//
// The rule now, the same in every path that prices usage (period close,
// cancel final invoice, spend-threshold invoice, preview, live balance,
// customer usage view): the bucket counts as 0 for that window and the rest
// of the invoice is built. A block-mode usage cap totals what the invoice
// bills, so it counts the bucket as 0 too (capUsageTotals).
//
// It is never silent. The preview and the usage view show a warning. Each
// invoice decision that leaves such a bucket out logs a WARN and writes one
// subscription.net_negative_usage audit row on its own transaction, so a
// rolled-back or retried close writes none or one. The row shows on the
// subscription's activity timeline with each meter, its amount below zero
// and the window it was priced over. A correction larger than the window's
// usage usually means an earlier invoice billed the usage it corrects, so
// that invoice may need a credit note.
//
// Granularity: one usage bucket (a meter, or one pricing rule of a meter)
// over one billing window, the unit each path prices. A meter whose buckets
// or windows have different signs bills its positive ones in full. The
// window is not always the billing period: after a reset_billing_cycle=false
// threshold invoice the close and the cancel price usage from that invoice's
// end, and a meter added or removed mid-period is priced over its own
// segment. The row names each meter's window.

// auditActionNetNegativeUsage is the audit action of that row.
const auditActionNetNegativeUsage = "subscription.net_negative_usage"

// netNegativeUsage is the usage below zero, per meter id, that one invoice
// decision counted as 0. A builder fills it; the caller writes it on the
// decision's transaction.
type netNegativeUsage map[string]netNegativeMeter

type netNegativeMeter struct {
	key      string
	qty      decimal.Decimal // below zero
	from, to time.Time       // the window it was priced over
}

// add records one bucket below zero, priced over [from, to). A meter with
// buckets below zero in more than one window gets their sum, and the window
// that spans them.
func (n netNegativeUsage) add(meterID, meterKey string, qty decimal.Decimal, from, to time.Time) {
	m, seen := n[meterID]
	m.key = meterKey
	m.qty = m.qty.Add(qty)
	if !seen || from.Before(m.from) {
		m.from = from
	}
	if !seen || to.After(m.to) {
		m.to = to
	}
	n[meterID] = m
}

// billableUsageTotal is the usage cap's total: the sum of the meters'
// quantities, with a meter below zero counted as 0, as it bills. That is what
// the cap saw when the store dropped such totals; counting one at its value
// would lower the total and scale the other meters above the cap. The
// quantities come from capUsageTotals.
func billableUsageTotal(totals map[string]decimal.Decimal) decimal.Decimal {
	sum := decimal.Zero
	for _, q := range totals {
		if q.IsPositive() {
			sum = sum.Add(q)
		}
	}
	return sum
}

// capUsageTotals returns totals, the meter totals over [from, to), as a
// block usage cap counts them: each meter at what the invoice bills for it
// before the cap. A meter priced by pricing rules bills its rule buckets,
// not its total, so its entry is the sum of its buckets above zero. Its
// total nets a bucket below zero in, while the invoice bills that bucket 0:
// buckets of +150 and -50 have a total of +100, and a cap of 80 taken from
// that total billed 120. Every other meter keeps its total.
func (e *Engine) capUsageTotals(ctx context.Context, sub domain.Subscription, meterAggs map[string]string, totals map[string]decimal.Decimal, from, to time.Time) (map[string]decimal.Decimal, error) {
	out := make(map[string]decimal.Decimal, len(totals))
	for id, q := range totals {
		out[id] = q
	}
	for meterID, agg := range meterAggs {
		rules, err := e.pricing.ListMeterPricingRulesByMeter(ctx, sub.TenantID, meterID)
		if err != nil {
			return nil, fmt.Errorf("list pricing rules for meter %s: %w", meterID, err)
		}
		if len(rules) == 0 {
			continue
		}
		aggs, err := e.usage.AggregateByPricingRules(ctx, sub.TenantID, sub.CustomerID, meterID, mapMeterAggregation(agg), from, to)
		if err != nil {
			return nil, fmt.Errorf("aggregate by pricing rules for meter %s: %w", meterID, err)
		}
		billed := decimal.Zero
		for _, a := range aggs {
			if a.Quantity.IsPositive() {
				billed = billed.Add(a.Quantity)
			}
		}
		if billed.IsZero() {
			delete(out, meterID)
		} else {
			out[meterID] = billed
		}
	}
	return out, nil
}

// noteNetNegativeTx writes the decision's subscription.net_negative_usage row
// on tx. reason is the invoice kind the decision builds, [periodStart,
// periodEnd) its billing period, and inv the invoice it made (zero when it
// made none). Each meter's from and to are the window its usage below zero
// was priced over, which can start after periodStart (see the header). A
// no-op when nothing was below zero or no audit logger is wired. An error
// fails the decision, like the finalize row (ADR-090).
func (e *Engine) noteNetNegativeTx(ctx context.Context, tx *sql.Tx, sub domain.Subscription, n netNegativeUsage, reason domain.InvoiceBillingReason, periodStart, periodEnd time.Time, inv domain.Invoice) error {
	if len(n) == 0 || e.auditLogger == nil {
		return nil
	}
	ids := make([]string, 0, len(n))
	for id := range n {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool {
		if n[ids[i]].key != n[ids[j]].key {
			return n[ids[i]].key < n[ids[j]].key
		}
		return ids[i] < ids[j]
	})
	meters := make([]map[string]any, 0, len(ids))
	for _, id := range ids {
		meters = append(meters, map[string]any{
			"meter_id":  id,
			"meter_key": n[id].key,
			"quantity":  n[id].qty.String(),
			"from":      n[id].from.UTC().Format(time.RFC3339Nano),
			"to":        n[id].to.UTC().Format(time.RFC3339Nano),
		})
	}
	meta := map[string]any{
		"meters":         meters,
		"billing_reason": string(reason),
		"period_start":   periodStart.UTC().Format(time.RFC3339),
		"period_end":     periodEnd.UTC().Format(time.RFC3339),
	}
	if inv.ID != "" {
		meta["invoice_id"] = inv.ID
		meta["invoice_number"] = inv.InvoiceNumber
	}
	return e.auditLogger.LogInTx(ctx, tx, audit.Entry{
		Action:        auditActionNetNegativeUsage,
		ResourceType:  "subscription",
		ResourceID:    sub.ID,
		ResourceLabel: sub.Code,
		Metadata:      meta,
	})
}
