package usage

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/sagarsuperuser/velox/internal/domain"
)

// Every written usage row that no invoice will bill is answered with a
// billing_status (ADR-120 amendment 2026-10-22; ADR-130, PR-6, which builds
// D07). Velox stores such an event and answers 2xx. The caller and the
// operator are told: the event answer carries the status, the batch answer
// carries it per event and counts each, the LiteLLM answer and the held-usage
// release count them, and velox_usage_unbilled_total{reason, origin} counts
// every written row by status. The statuses:
//
//   - after_close: its billing period was already invoiced.
//   - no_price: a plan of the subscription prices its meter, but no price of
//     that plan takes the event's labels (dimension_match).
//   - not_on_plan: a subscription held its time, but its plan does not price
//     the meter.
//   - outside_subscription: no active or trialing subscription held its
//     time: the customer has none, or the time is before one started, in a
//     free trial, or after one ends.
//
// The rule is one function, classify, shared by ingest and the held-usage
// release. Ingest reads the open periods, with each plan's prices at its pin,
// BEFORE it writes the event, and that order is what keeps after_close true.
// Every writer of a period start (the cycle close, BillDueNow, a
// cross-interval swap, a test-clock advance) and the threshold fire's
// watermark invoice commit in the transaction that froze that window, after
// reading its usage. So when the read shows the window frozen, it was frozen
// before this event existed, and nothing counted it. A close that commits
// between the read and the write is not seen: that event goes unflagged,
// never wrongly flagged.
//
// no_price holds for the same reason the close's price does: a period's
// price set is fixed at its pin, and a price written later is in force from
// its write (ADR-130 §4), after the pin. The one exception is a price write
// whose transaction spans the pin and commits after this read: then one
// no_price or not_on_plan flag can be wrong. Closing it would take a lock on
// the plan on every ingest; it is stated instead (ADR-120 amendment).
//
// not_on_plan and outside_subscription describe the subscriptions when the
// event arrived. A subscription's first period starts at the start of its
// first day, and an item added on a period's first day bills from the period
// start (ADR-012). So a subscription created, or an item added on that first
// day, later on the event's day can still bill an event flagged earlier that
// day.
//
// The meter's aggregation is read the other way round: on the write's own
// transaction, after its insert (classifyWritten). The insert's foreign key
// holds the meter row FOR KEY SHARE until commit, and an aggregation PATCH
// locks the meter FOR UPDATE before it reads whether the meter has events
// (ADR-130 §2). So the aggregation read there is the one the event is
// counted under: a PATCH that committed first is seen, and a later one
// waits, then is refused. Read before the write, a PATCH committing in
// between could make the flag wrong, not only missed: before a paid
// threshold invoice's end, the close bills a max event but not a sum one.
//
// A threshold invoice that is not paid can still be voided, and then the
// period's close bills usage from the period start again (#243). A write-off
// does not reopen the window (ADR-066 amendment 2026-10-23), but a
// written-off invoice can still be voided. So only a paid threshold
// invoice's watermark flags: a paid invoice can be neither voided nor
// written off. An event before the watermark of a threshold invoice that is
// open or written off is Unsettled and not flagged.

// The billing_status values (UsageEvent.BillingStatus).
const (
	BillingStatusAfterClose          = "after_close"
	BillingStatusNoPrice             = "no_price"
	BillingStatusNotOnPlan           = "not_on_plan"
	BillingStatusOutsideSubscription = "outside_subscription"
)

// Class is where one usage row falls against a customer's open periods.
// Higher wins when subscriptions disagree: a subscription that bills the row
// wins, then one that prices its meter, then one that held its time.
type Class int

const (
	// Outside: no active or trialing subscription held the row's time.
	// Before a subscription started, in a trial, or after a final period.
	Outside Class = iota
	// NotOnPlan: a subscription held the row's time (an open period, or an
	// invoiced one since its billing started), but no plan it was on then
	// prices the meter. For an invoiced period, its current plans.
	NotOnPlan
	// AfterClose: a subscription whose current plan prices the meter already
	// invoiced the row's time, and no future invoice counts it.
	AfterClose
	// NoPrice: the time falls in an open period whose plan prices the meter
	// at its pin, but no price of the plan takes the row's labels.
	NoPrice
	// Unsettled: a threshold invoice that is not paid billed the row's time.
	// If that invoice is paid or written off, no other invoice counts the
	// row. If it is voided before the period closes, the period's invoice
	// counts it.
	Unsettled
	// InOpen: an open period's invoice counts it.
	InOpen
)

// BillingStatus is the billing_status a written row of class c answers
// with: "" when an invoice counts the row or still can (InOpen, Unsettled).
func (c Class) BillingStatus() string {
	switch c {
	case Outside:
		return BillingStatusOutsideSubscription
	case NotOnPlan:
		return BillingStatusNotOnPlan
	case AfterClose:
		return BillingStatusAfterClose
	case NoPrice:
		return BillingStatusNoPrice
	default:
		return ""
	}
}

// PriceSet is a plan's prices at one pin as ingest matches them: by meter
// id, the dimension_match of each price of the meter. A meter missing from
// it is not priced by the plan.
type PriceSet map[string][]map[string]string

// takes reports whether a price whose dimension_match is match takes an
// event with labels: every key of match is a label whose value is the same
// string. It is the close's `properties @> dimension_match` (usage
// AggregateClaims) for string match values, which are the only kind
// (ADR-130 §1): a number or boolean label never equals a string, and labels
// the match does not name do not matter.
func takes(match map[string]string, labels map[string]any) bool {
	for k, want := range match {
		got, ok := labels[k].(string)
		if !ok || got != want {
			return false
		}
	}
	return true
}

// classOf is InOpen when the set has a price of meterID that takes labels,
// NoPrice when it prices the meter but none takes them, else NotOnPlan.
func (ps PriceSet) classOf(meterID string, labels map[string]any) Class {
	matches, ok := ps[meterID]
	if !ok {
		return NotOnPlan
	}
	for _, m := range matches {
		if takes(m, labels) {
			return InOpen
		}
	}
	return NoPrice
}

// PlanSpan is one stretch [From, To) of a current period during which an
// item of the subscription was on PlanID, and the plan's prices at its pin
// of the period: the stretch's usage is billed by those prices.
type PlanSpan struct {
	PlanID   string
	From, To time.Time
	Prices   PriceSet
}

// Stretch is one stretch [From, To) of a current period during which an
// item of the subscription was on PlanID (the close's plan stretches).
type Stretch struct {
	PlanID   string
	From, To time.Time
}

// OpenPeriod is what classify needs from one active or trialing
// subscription. Build it with OpenPeriodOf.
type OpenPeriod struct {
	// Start and End are the current billing period [Start, End).
	Start, End time.Time
	// Floor is where the subscription's billing of usage began: the trial
	// end when it had a trial (trial usage is free), else its start. Nil
	// means unknown, and nothing before Start is flagged.
	Floor *time.Time
	// Spans are the period's plan stretches with their prices, the ones the
	// close bills by: a removed item and a plan swapped away from keep
	// theirs. Two spans can overlap (an item added on the period's first day
	// bills from the period start).
	Spans []PlanSpan
	// Meters are the meters the current items' plans price at each plan's
	// pin of the period: Start, or the first instant in the period the
	// subscription had an item on the plan (ADR-130 §4 step 3). A row before
	// Start is AfterClose only for these.
	Meters map[string]bool
	// Next are the prices from End on: of each plan after any change pending
	// at or before End, at End, the next period's pin. Nil when the
	// subscription ends at End (cancel at period end, or cancel_at at or
	// before End).
	Next []PriceSet
	// BilledThrough and PaidThrough are the subscription's threshold
	// watermarks (ThresholdWatermark).
	ThresholdWatermark
}

// ThresholdWatermark is how far the threshold fires of a subscription's
// current period billed its sum and count buckets. Read
// from the period's threshold invoices for every subscription, whether or
// not it still has thresholds: the close keys the watermark on the
// invoices, not on the setting.
type ThresholdWatermark struct {
	// BilledThrough is the close's watermark: the latest end of a threshold
	// invoice that is not voided (a written-off one counts). The close bills
	// sum and count buckets from it. Nil when none. It falls back only on a
	// void, which is final.
	BilledThrough *time.Time
	// PaidThrough is the latest end of a paid threshold invoice, at or
	// before BilledThrough. A paid invoice can be neither voided nor written
	// off, so the close never bills those buckets before it. Nil when none.
	PaidThrough *time.Time
}

// MeterBuckets is what classify needs from the meter itself.
type MeterBuckets struct {
	// Additive: the meter's aggregation is sum or count. Only then does
	// the threshold watermark leave a pre-watermark event unbilled: a max
	// or last bucket the fire deferred is billed over the whole period at
	// close (ADR-066 §4). One aggregation per meter (ADR-130 §2) makes this
	// a property of the meter, the same for every price of it.
	Additive bool
}

// OpenPeriodOf builds sub's OpenPeriod. stretches are its current period's
// plan stretches (the close's). atPin holds, by plan id, the prices of each
// plan of the period at its pin of the period (the period start, or where
// the subscription first joined the plan in it); atEnd, the prices of each
// plan of its items or of their pending changes at the period end. A plan
// missing from them prices nothing. ok is false when sub is not active or
// trialing or has no current period.
func OpenPeriodOf(sub domain.Subscription, stretches []Stretch, atPin, atEnd map[string]PriceSet, wm ThresholdWatermark) (OpenPeriod, bool) {
	if sub.Status != domain.SubscriptionActive && sub.Status != domain.SubscriptionTrialing {
		return OpenPeriod{}, false
	}
	if sub.CurrentBillingPeriodStart == nil || sub.CurrentBillingPeriodEnd == nil {
		return OpenPeriod{}, false
	}
	p := OpenPeriod{
		Start:              *sub.CurrentBillingPeriodStart,
		End:                *sub.CurrentBillingPeriodEnd,
		Meters:             map[string]bool{},
		ThresholdWatermark: wm,
	}
	p.Floor = sub.BillingStartedAt()
	for _, st := range stretches {
		p.Spans = append(p.Spans, PlanSpan{PlanID: st.PlanID, From: st.From, To: st.To, Prices: atPin[st.PlanID]})
	}
	continues := !sub.CancelAtPeriodEnd && (sub.CancelAt == nil || sub.CancelAt.After(p.End))
	if continues {
		p.Next = []PriceSet{}
	}
	for _, it := range sub.Items {
		for id := range atPin[it.PlanID] {
			p.Meters[id] = true
		}
		if !continues {
			continue
		}
		next := it.PlanID
		if it.PendingPlanID != "" && it.PendingPlanEffectiveAt != nil && !it.PendingPlanEffectiveAt.After(p.End) {
			next = it.PendingPlanID
		}
		if ps, ok := atEnd[next]; ok {
			p.Next = append(p.Next, ps)
		}
	}
	return p, true
}

// BucketsOf builds a meter's MeterBuckets. An empty aggregation reads as
// sum, as billing reads it.
func BucketsOf(meter domain.Meter) MeterBuckets {
	return MeterBuckets{Additive: meter.Aggregation == "" || domain.AdditiveAggregation(meter.Aggregation)}
}

// classify places one row of meter meterID at time at, with labels, against
// periods. The meter is read only when the answer depends on it, through
// meter.
//
// For each period:
//   - at >= End (the grace before End's invoice, or a close running late):
//     the next period counts it if the subscription continues and a plan
//     from End on has a price that takes it: InOpen. NoPrice when such a
//     plan prices the meter but no price takes it, else NotOnPlan. Never
//     AfterClose: the next close bills it.
//   - Start <= at < End: by the spans holding at. InOpen if a span's plan
//     has a price of the meter that takes the labels, NoPrice if a span's
//     plan prices the meter but none takes them, else NotOnPlan. When InOpen,
//     the meter is additive and at is past the floor: before PaidThrough it
//     is AfterClose, and before BilledThrough (a threshold invoice not paid
//     yet) it is Unsettled.
//   - at < Start, at or past the floor: AfterClose when a current plan prices
//     the meter, else NotOnPlan.
//   - else Outside.
//
// The highest class over all periods wins, so another subscription whose
// open period bills the row keeps it out of AfterClose.
func classify(at time.Time, meterID string, labels map[string]any, periods []OpenPeriod, meter func() (MeterBuckets, error)) (Class, error) {
	best := Outside
	var mb *MeterBuckets
	buckets := func() (MeterBuckets, error) {
		if mb == nil {
			b, err := meter()
			if err != nil {
				return MeterBuckets{}, err
			}
			mb = &b
		}
		return *mb, nil
	}
	for _, p := range periods {
		c := Outside
		pastFloor := p.Floor != nil && !at.Before(*p.Floor)
		switch {
		case !at.Before(p.End):
			if p.Next != nil {
				c = NotOnPlan
				for _, ps := range p.Next {
					c = max(c, ps.classOf(meterID, labels))
				}
			}
		case !at.Before(p.Start):
			c = NotOnPlan
			for _, sp := range p.Spans {
				if !at.Before(sp.From) && at.Before(sp.To) {
					c = max(c, sp.Prices.classOf(meterID, labels))
				}
			}
			if c == InOpen && p.BilledThrough != nil && at.Before(*p.BilledThrough) && pastFloor {
				b, err := buckets()
				if err != nil {
					return Outside, err
				}
				if b.Additive {
					c = Unsettled
					if p.PaidThrough != nil && at.Before(*p.PaidThrough) {
						c = AfterClose
					}
				}
			}
		case pastFloor:
			c = NotOnPlan
			if p.Meters[meterID] {
				c = AfterClose
			}
		}
		best = max(best, c)
	}
	return best, nil
}

// OpenPeriodReader reads what ingest and the held-usage release classify
// against. Implemented in internal/api from the subscription store, the
// pricing store and the invoice store (the threshold watermarks).
type OpenPeriodReader interface {
	// OpenPeriods returns the OpenPeriod of every active and trialing
	// subscription of the customer, with each plan's prices at its pins.
	OpenPeriods(ctx context.Context, tenantID, customerID string) ([]OpenPeriod, error)
	// MeterBucketsTx returns the meter's MeterBuckets, read on tx (which
	// carries the tenant): the transaction that wrote the meter's rows,
	// after its inserts, so the read sees the aggregation they are counted
	// under and never waits for a second connection.
	MeterBucketsTx(ctx context.Context, tx *sql.Tx, meterID string) (MeterBuckets, error)
}

// periodCache holds one request's open-period reads, so a batch reads each
// customer's periods, and the prices they name, once. Every read still
// happens before the writes it classifies. The meters are not kept here: each write reads them on its
// own transaction (classifyWritten). Not for concurrent use.
type periodCache struct {
	periods map[string][]OpenPeriod
}

type periodCacheKey struct{}

// WithPeriodCache returns ctx carrying a cache for the open-period reads of
// the ingest calls made with it. A door that ingests many rows one call at a
// time (the LiteLLM callback) uses it for its request.
func WithPeriodCache(ctx context.Context) context.Context {
	return context.WithValue(ctx, periodCacheKey{}, &periodCache{periods: map[string][]OpenPeriod{}})
}

func periodCacheFrom(ctx context.Context) *periodCache {
	if c, ok := ctx.Value(periodCacheKey{}).(*periodCache); ok {
		return c
	}
	return &periodCache{periods: map[string][]OpenPeriod{}}
}

// openPeriodsFor reads the customer's open periods before the write. With
// no reader wired it reads nothing, and every row is Outside, answered with
// no billing_status (markWritten). A failed read
// is returned wrapped in errIngestUnavailable: the doors answer 503 and
// nothing is written, the same contract as a failed customer or meter
// lookup.
func (s *Service) openPeriodsFor(ctx context.Context, tenantID, customerID string, cache *periodCache) ([]OpenPeriod, error) {
	if s.periods == nil {
		return nil, nil
	}
	if periods, ok := cache.periods[customerID]; ok {
		return periods, nil
	}
	periods, err := s.periods.OpenPeriods(ctx, tenantID, customerID)
	if err != nil {
		return nil, fmt.Errorf("%w: open periods: %w", errIngestUnavailable, err)
	}
	cache.periods[customerID] = periods
	return periods, nil
}

// classifyWritten classifies one row the write just inserted, against
// periods read before the write. The meter is read only when the class
// depends on it, on tx, after the insert (see the top of this file); meters
// keeps what this transaction already read. A failed read is returned
// wrapped in errIngestUnavailable and rolls the write back: 503, nothing
// written.
func (s *Service) classifyWritten(ctx context.Context, tx *sql.Tx, ev domain.UsageEvent, periods []OpenPeriod, meters map[string]MeterBuckets) (Class, error) {
	return classify(ev.Timestamp, ev.MeterID, ev.Dimensions, periods, func() (MeterBuckets, error) {
		if b, ok := meters[ev.MeterID]; ok {
			return b, nil
		}
		b, err := s.periods.MeterBucketsTx(ctx, tx, ev.MeterID)
		if err != nil {
			return MeterBuckets{}, fmt.Errorf("%w: meter buckets: %w", errIngestUnavailable, err)
		}
		meters[ev.MeterID] = b
		return b, nil
	})
}

var unbilledEvents *prometheus.CounterVec

func init() {
	c := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "velox_usage_unbilled_total",
		Help: "Usage events written that no invoice will bill, by billing_status (reason: after_close, no_price, not_on_plan, outside_subscription). Counted once per written row; a replay is not counted.",
	}, []string{"reason", "origin"})
	if err := prometheus.DefaultRegisterer.Register(c); err != nil {
		if are, ok := err.(prometheus.AlreadyRegisteredError); ok {
			unbilledEvents = are.ExistingCollector.(*prometheus.CounterVec)
		} else {
			panic(err)
		}
	} else {
		unbilledEvents = c
	}
}

// UnbilledCounts counts written rows by billing_status: the batch and
// LiteLLM answers carry them.
type UnbilledCounts struct {
	AfterClose          int `json:"after_close"`
	NoPrice             int `json:"no_price"`
	NotOnPlan           int `json:"not_on_plan"`
	OutsideSubscription int `json:"outside_subscription"`
}

// Add counts one row answered with status ("" counts nothing).
func (u *UnbilledCounts) Add(status string) {
	switch status {
	case BillingStatusAfterClose:
		u.AfterClose++
	case BillingStatusNoPrice:
		u.NoPrice++
	case BillingStatusNotOnPlan:
		u.NotOnPlan++
	case BillingStatusOutsideSubscription:
		u.OutsideSubscription++
	}
}

// Plus is u and o counted together.
func (u UnbilledCounts) Plus(o UnbilledCounts) UnbilledCounts {
	return UnbilledCounts{
		AfterClose:          u.AfterClose + o.AfterClose,
		NoPrice:             u.NoPrice + o.NoPrice,
		NotOnPlan:           u.NotOnPlan + o.NotOnPlan,
		OutsideSubscription: u.OutsideSubscription + o.OutsideSubscription,
	}
}

// statusOf is the billing_status of a row the write classified as class.
// With no open-period reader wired nothing was read, so nothing is claimed.
func (s *Service) statusOf(class Class) string {
	if s.periods == nil {
		return ""
	}
	return class.BillingStatus()
}

// recordUnbilled counts one written row that no invoice will bill, and logs
// an after_close row (the other statuses are a setup to fix, not a late
// event, and could be every event of a customer: the answer and the counter
// say so). Called only after the write committed, for rows the write
// inserted.
func recordUnbilled(ctx context.Context, ev domain.UsageEvent, status string) {
	if status == "" {
		return
	}
	unbilledEvents.WithLabelValues(status, string(ev.Origin)).Inc()
	if status != BillingStatusAfterClose {
		return
	}
	slog.WarnContext(ctx, "usage event arrived after its period was invoiced — stored, billed on no invoice",
		"customer_id", ev.CustomerID,
		"meter_id", ev.MeterID,
		"timestamp", ev.Timestamp,
		"origin", string(ev.Origin),
	)
}
