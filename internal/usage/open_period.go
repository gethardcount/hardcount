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
//   - after_close: its billing period was already invoiced, whatever plan
//     the subscription is on now. Only a one-off invoice can bill it.
//   - no_price: a plan of the subscription prices its meter, but no price of
//     that plan takes the event's labels (dimension_match).
//   - not_on_plan: a subscription held its time, but its plan does not price
//     the meter.
//   - outside_subscription: no active or trialing subscription billed its
//     time: the customer has none, or the time is before one started, in a
//     free trial (also on the trial-end day, before the trial end), after one
//     ends or after its scheduled cancel (a trial with a cancel at period
//     end ends at its trial end), or the meter's time belonged to another
//     subscription that has since ended.
//
// Inside a current period an event is placed by the windows the close bills
// the meter over (billing.PeriodPlan.UsageWindows), not by the period: a
// window starts no earlier than the trial end or another subscription's
// coverage end of the meter, and each instant of a meter is in one window,
// billed by one plan. Before the current period, an event from where the
// close began to bill the subscription (the start of its first period, at
// midnight of its first day, or its trial end) is after_close.
//
// An event at or after the current period's end is not checked against
// prices or plans (review round 3 on #1390). Its own period's close bills it
// at that period's prices, and ingest does not know that period while the
// current one's close has not run: the close can be behind by more than one
// period (a failing run, a stopped scheduler, a test clock catching up), and
// a price edit or a plan change can fall between. Such an event answers no
// billing_status, unless a trial or a scheduled cancel makes it
// outside_subscription. If its plan prices the meter but no price takes it,
// the close logs its unpriced-usage WARN.
//
// The rule is one function, classify, shared by ingest and the held-usage
// release. Ingest reads the open periods, with each plan's prices at its pin,
// BEFORE it writes the event, and that order is what keeps after_close true.
// Every writer of a period start (the cycle close, BillDueNow, a
// cross-interval swap, a test-clock advance) and the threshold fire's
// watermark invoice commit in the transaction that froze that window, after
// reading its usage. So when the read shows the window frozen, it was frozen
// before this event existed, and nothing counted it.
//
// The read is not a lock. A subscription change that commits between the
// read and the write is not seen. A close, a cancel or an item removal can
// only leave the event unflagged. A plan swap, an item add or a new
// subscription can make a flag wrong: the stretch it opens can bill the
// event. The LiteLLM door reads once per /spend request, so for it the gap
// is the whole request.
//
// no_price holds for the same reason the close's price does: a period's
// price set is fixed at its pin, and a price written later is in force from
// its write (ADR-130 §4), after the pin. That needs the pin to have passed
// when the prices are read. A pin still ahead (a trial end, for an event in
// the future skew) can still get a price saved now, so no flag is drawn from
// its prices (PriceSet.Ahead). The
// one exception left is a price write whose transaction spans the pin and
// commits after this read: then one no_price or not_on_plan flag can be
// wrong. Closing it would take a lock on the plan on every ingest; it is
// stated instead (ADR-120 amendment).
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
// Higher wins when subscriptions disagree: a subscription that bills the row,
// or still may, wins, then one that prices its meter, then one that held its
// time.
type Class int

const (
	// Outside: no active or trialing subscription billed the row's time.
	// Before a subscription started, in a trial, after its end, or in
	// another subscription's time for the meter.
	Outside Class = iota
	// NotOnPlan: an open period of a subscription holds the row's time, but
	// no plan it was on then prices the meter.
	NotOnPlan
	// AfterClose: a subscription already invoiced the row's time (a period
	// since its billing started, before the current one), and no future
	// invoice counts it. Whatever plan it is on now: a price added now would
	// not bill the row.
	AfterClose
	// NoPrice: the time falls in an open period whose plan prices the meter
	// at its pin, but no price of the plan takes the row's labels.
	NoPrice
	// Unsettled: a threshold invoice that is not paid billed the row's time.
	// If that invoice is paid or written off, no other invoice counts the
	// row. If it is voided before the period closes, the period's invoice
	// counts it.
	Unsettled
	// Later: the row's time is at or after the current period's end, and
	// the subscription goes on. A period not invoiced yet holds it, but its
	// plans and prices are not known yet, so it is not checked.
	Later
	// InOpen: an open period's invoice counts it.
	InOpen
)

// BillingStatus is the billing_status a written row of class c answers
// with: "" when an invoice counts the row or still can (InOpen, Unsettled),
// and when it was not checked (Later).
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

// PriceSet is a plan's prices at one pin as ingest matches them.
type PriceSet struct {
	// Ahead: the pin had not passed when the prices were read, so a price
	// saved later can still be in force at it. No flag is drawn from such a
	// set.
	Ahead bool
	// ByMeter holds, by meter id, the dimension_match of each price of the
	// meter. A meter missing from it is not priced by the plan.
	ByMeter map[string][]map[string]string
}

// prices reports whether the set prices meterID.
func (ps PriceSet) prices(meterID string) bool {
	_, ok := ps.ByMeter[meterID]
	return ok
}

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
// NoPrice when it prices the meter but none takes them, else NotOnPlan. A
// set whose pin is ahead is InOpen: a price saved now can still take the
// row.
func (ps PriceSet) classOf(meterID string, labels map[string]any) Class {
	if ps.Ahead {
		return InOpen
	}
	matches, ok := ps.ByMeter[meterID]
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

// Stretch is one stretch [From, To) of a current period during which an
// item of the subscription was on PlanID (the close's plan stretches).
type Stretch struct {
	PlanID   string
	From, To time.Time
}

// Window is one window [From, To) of a current period over which the close
// bills a meter's usage by PlanID's prices (billing.PeriodPlan.UsageWindows).
type Window struct {
	PlanID   string
	From, To time.Time
}

func (w Window) holds(at time.Time) bool  { return !at.Before(w.From) && at.Before(w.To) }
func (s Stretch) holds(at time.Time) bool { return !at.Before(s.From) && at.Before(s.To) }

// OpenPeriod is what classify needs from one active or trialing
// subscription. Build it with OpenPeriodOf.
type OpenPeriod struct {
	// Start and End are the current billing period [Start, End).
	Start, End time.Time
	// Floor is where the close began to bill the subscription's usage: the
	// trial end when it had a trial (trial usage is free), else the start of
	// its first period, at midnight of its first day (ADR-012), not the
	// instant it started. Nil when unknown: nothing before Start is then
	// after_close.
	Floor *time.Time
	// TrialEnd is the subscription's trial end, nil without a trial. Usage
	// before it is free, also in the first paid period, which starts at
	// midnight of the trial-end day, and in the grace of a period the trial
	// outlasts.
	TrialEnd *time.Time
	// Ends is where the subscription's scheduled cancel takes effect
	// (Subscription.CancelEffectiveAt): cancel_at, the period end for a
	// cancel at period end, or the trial end for a trialing subscription
	// with one, which is canceled free there (ADR-069). No invoice bills its
	// usage from it on. Nil when no cancel is scheduled.
	Ends *time.Time
	// Plans holds, by plan id, each plan's prices at its pin of the period.
	Plans map[string]PriceSet
	// Stretches are the period's plan stretches, the ones the close bills
	// by: a removed item and a plan swapped away from keep theirs.
	Stretches []Stretch
	// Windows holds, by meter id, the windows over which the close bills
	// the meter's usage. Each instant of a meter is in at most one: an item
	// added on the period's first day reaches back over one that left the
	// meter earlier that day, and the earlier one keeps that time. A window
	// starts no earlier than the trial end or another subscription's
	// coverage end of the meter.
	Windows map[string][]Window
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

// OpenPeriodOf builds sub's OpenPeriod. stretches and windows are its
// current period's plan stretches and usage windows (the close's). atPin
// holds, by plan id, the prices of each plan of the period at its pin of the
// period (the period start, or where the subscription first joined the plan
// in it). A plan missing from it prices nothing. billedFrom is where the
// close began to bill sub (billing.PeriodPlan.BilledFrom), nil while it
// trials. ok is false when sub is not active or trialing or has no current
// period.
func OpenPeriodOf(sub domain.Subscription, stretches []Stretch, windows map[string][]Window, atPin map[string]PriceSet, billedFrom *time.Time, wm ThresholdWatermark) (OpenPeriod, bool) {
	if sub.Status != domain.SubscriptionActive && sub.Status != domain.SubscriptionTrialing {
		return OpenPeriod{}, false
	}
	if sub.CurrentBillingPeriodStart == nil || sub.CurrentBillingPeriodEnd == nil {
		return OpenPeriod{}, false
	}
	// Derived here from the schedule fields, as the store's scan derives
	// it, so the rule does not depend on how sub was read.
	sub.DeriveCancelEffectiveAt()
	p := OpenPeriod{
		Start:              *sub.CurrentBillingPeriodStart,
		End:                *sub.CurrentBillingPeriodEnd,
		TrialEnd:           sub.TrialEndAt,
		Ends:               sub.CancelEffectiveAt,
		Plans:              atPin,
		Stretches:          stretches,
		Windows:            windows,
		ThresholdWatermark: wm,
	}
	p.Floor = billedFrom
	if p.TrialEnd != nil && (p.Floor == nil || p.TrialEnd.After(*p.Floor)) {
		p.Floor = p.TrialEnd
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
//   - before the trial end: Outside. Trial usage is free.
//   - at or after Ends (a scheduled cancel): Outside. The final invoice
//     bills usage only to the cancel.
//   - at >= End (the grace before End's invoice, or a close running late):
//     Later, not checked. Never AfterClose: a later close bills it.
//   - Start <= at < End: inPeriod. When InOpen and the meter is additive:
//     before PaidThrough it is AfterClose, and before BilledThrough (a
//     threshold invoice not paid yet) it is Unsettled. The close clamps
//     sum and count windows to the watermark from the period start, so
//     this holds before the subscription's start instant too.
//   - at < Start, at or past the floor: AfterClose. That period was
//     invoiced; what the current plans price does not change that.
//   - else Outside.
//
// A price set whose pin is ahead answers InOpen (classOf). The highest class
// over all periods wins, so another subscription whose open period bills
// the row keeps it out of AfterClose.
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
		switch {
		case p.TrialEnd != nil && at.Before(*p.TrialEnd):
		case p.Ends != nil && !at.Before(*p.Ends):
		case !at.Before(p.End):
			c = Later
		case !at.Before(p.Start):
			c = p.inPeriod(at, meterID, labels)
			if c == InOpen && p.BilledThrough != nil && at.Before(*p.BilledThrough) {
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
		case p.Floor != nil && !at.Before(*p.Floor):
			c = AfterClose
		}
		best = max(best, c)
	}
	return best, nil
}

// inPeriod places a row at or after the trial end, Start <= at < End, as
// the close bills the period:
//   - a stretch holding at whose plan's pin is ahead: InOpen.
//   - in a window of the meter: by that window's plan, InOpen if a price
//     takes the labels, else NoPrice. One window holds an instant.
//   - else Outside when a stretch holding at prices the meter: the window
//     starts later, at another subscription's coverage end, so that time
//     is not this subscription's. Else NotOnPlan.
func (p OpenPeriod) inPeriod(at time.Time, meterID string, labels map[string]any) Class {
	held := false
	for _, st := range p.Stretches {
		if !st.holds(at) {
			continue
		}
		ps := p.Plans[st.PlanID]
		if ps.Ahead {
			return InOpen
		}
		held = held || ps.prices(meterID)
	}
	for _, w := range p.Windows[meterID] {
		if w.holds(at) {
			return p.Plans[w.PlanID].classOf(meterID, labels)
		}
	}
	if held {
		return Outside
	}
	return NotOnPlan
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
