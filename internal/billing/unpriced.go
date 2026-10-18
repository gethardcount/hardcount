package billing

import (
	"context"

	"github.com/shopspring/decimal"

	mw "github.com/sagarsuperuser/velox/internal/api/middleware"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
)

// Unpriced usage (#981, ADR-096 amendment): usage that matches no pricing
// rule on a meter with no default binding has no price, so an invoice is
// built and sent without it. That is the peer norm (Lago bills it at a $0
// default, Metronome and Orb leave it off). It is never silent: each place
// that builds an invoice without it logs a WARN and adds the quantity to
// velox_usage_unpriced_quantity_total{meter_id}. The invoice builders are
// the period close and the cancel final invoice (buildLineItems,
// buildCancelLineItems) and the spend-threshold invoice (fireThreshold).

// unpricedUsage is the quantity per meter id that one invoice decision left
// unpriced. A builder fills it; the caller records it once the decision is
// final, so a close that rolls back and is retried does not count twice.
type unpricedUsage map[string]decimal.Decimal

func (u unpricedUsage) add(meterID string, qty decimal.Decimal) {
	u[meterID] = u[meterID].Add(qty)
}

// record adds each meter's quantity to the counter, labeled with the mode
// (live or test) the invoice was built in. The counter is an operator
// signal (float, reset on restart), never a billing record. A test-clock
// subscription is a simulation and is not counted: one clock advance can
// close many periods, and the billing metrics leave simulations out too.
// The WARN still logs it.
func (u unpricedUsage) record(ctx context.Context, sub domain.Subscription) {
	if sub.TestClockID != "" {
		return
	}
	for meterID, qty := range u {
		mw.RecordUsageUnpriced(meterID, postgres.Livemode(ctx), qty.InexactFloat64())
	}
}

// leftOutBucket is one meter's bucket that rateMeterWindow priced at
// nothing: unpriced, or with BelowZero set, below zero after corrections
// (net_negative.go). Mode is the bucket's aggregation mode: the threshold
// fire needs it to decide whether the bucket is its to report or the period
// close's (see evaluateThresholds).
type leftOutBucket struct {
	MeterID   string
	MeterKey  string
	Mode      domain.AggregationMode
	Quantity  decimal.Decimal
	BelowZero bool
}
