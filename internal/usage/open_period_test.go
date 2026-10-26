package usage

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/errs"
)

var (
	opApr = time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	opMay = time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	opJun = time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
)

func opAt(t time.Time) *time.Time { return &t }

// everyEvent is a price set with one catch-all price ({}) per meter.
func everyEvent(ids ...string) PriceSet {
	ps := PriceSet{ByMeter: map[string][]map[string]string{}}
	for _, id := range ids {
		ps.ByMeter[id] = []map[string]string{{}}
	}
	return ps
}

// whole is a period [start, end) on one plan for all of it, pricing every
// event of meters now and from end on.
func whole(start, end time.Time, floor *time.Time, meters ...string) OpenPeriod {
	return onPlans(start, end, floor, map[string]PriceSet{"pln": everyEvent(meters...)}, []Stretch{{PlanID: "pln", From: start, To: end}}, everyEvent(meters...))
}

// onPlans is a period [start, end) with plans and stretches, whose windows
// are each stretch's for each meter its plan prices (the stretches must not
// overlap on a meter), and next from end on.
func onPlans(start, end time.Time, floor *time.Time, plans map[string]PriceSet, stretches []Stretch, next PriceSet) OpenPeriod {
	p := OpenPeriod{Start: start, End: end, Floor: floor, Next: []PriceSet{next}, Plans: plans, Stretches: stretches, Windows: map[string][]Window{}}
	for _, st := range stretches {
		for m := range plans[st.PlanID].ByMeter {
			p.Windows[m] = append(p.Windows[m], Window(st))
		}
	}
	return p
}

// TestClassify is the rule, one case per branch.
func TestClassify(t *testing.T) {
	sum := MeterBuckets{Additive: true}
	max := MeterBuckets{}
	// May is the open period; April was invoiced. Started Mar 1.
	may := whole(opMay, opJun, opAt(opApr.AddDate(0, -1, 0)), "m")
	withTrial := may
	withTrial.Floor = opAt(opApr.AddDate(0, 0, 14))
	// A threshold fire billed May 1 to 20, and its invoice is paid.
	watermark := may
	watermark.BilledThrough = opAt(opMay.AddDate(0, 0, 19))
	watermark.PaidThrough = watermark.BilledThrough
	// The same fire, its invoice not paid yet.
	unpaid := watermark
	unpaid.PaidThrough = nil
	// Two fires, May 1 to 10 paid and May 10 to 20 not paid yet.
	partPaid := watermark
	partPaid.PaidThrough = opAt(opMay.AddDate(0, 0, 9))
	ending := may
	ending.Next = nil
	// Started at 11:00 on May 1 with no trial: the first period starts at
	// midnight. A threshold fire billed May 1 00:00 to 20:00.
	firstDay := whole(opMay, opJun, opAt(opMay.Add(11*time.Hour)), "m")
	firstDay.BilledThrough = opAt(opMay.Add(20 * time.Hour))
	firstDay.PaidThrough = firstDay.BilledThrough
	firstDayUnpaid := firstDay
	firstDayUnpaid.PaidThrough = nil
	pendingDrop := may
	pendingDrop.Next = []PriceSet{{}}
	noFloor := may
	noFloor.Floor = nil
	// Another subscription whose open period (a year) still holds April.
	yearly := whole(opApr.AddDate(0, -1, 0), opApr.AddDate(1, -1, 0), opAt(opApr.AddDate(0, -1, 0)), "other")
	// m is priced for model a only, now and from the period end on.
	modelA := PriceSet{ByMeter: map[string][]map[string]string{"m": {{"model": "a"}}}}
	labelled := onPlans(opMay, opJun, may.Floor, map[string]PriceSet{"pln": modelA}, []Stretch{{PlanID: "pln", From: opMay, To: opJun}}, modelA)
	// On plan P (m) until May 15, then on plan Q (n): the close bills m's
	// May 1-15 usage at P's prices.
	swapped := onPlans(opMay, opJun, opAt(opApr), map[string]PriceSet{"P": everyEvent("m"), "Q": everyEvent("n")}, []Stretch{
		{PlanID: "P", From: opMay, To: opMay.AddDate(0, 0, 14)},
		{PlanID: "Q", From: opMay.AddDate(0, 0, 14), To: opJun},
	}, everyEvent("n"))
	// A first-day add (pln2, every event) reaches back to the period start
	// over pln (model a only), whose item left the meter at 01:00. The close
	// gives m's time before 01:00 to pln, the item on the meter then.
	reachBack := labelled
	reachBack.Plans = map[string]PriceSet{"pln": modelA, "pln2": everyEvent("m")}
	reachBack.Stretches = []Stretch{{PlanID: "pln", From: opMay, To: opMay.Add(time.Hour)}, {PlanID: "pln2", From: opMay, To: opJun}}
	reachBack.Windows = map[string][]Window{"m": {{PlanID: "pln", From: opMay, To: opMay.Add(time.Hour)}, {PlanID: "pln2", From: opMay.Add(time.Hour), To: opJun}}}
	// The trial ended at 18:00 on May 1; the first paid period starts at
	// midnight, and m's window at the trial end.
	trialDay := may
	trialDay.TrialEnd = opAt(opMay.Add(18 * time.Hour))
	trialDay.Floor = trialDay.TrialEnd
	trialDay.Windows = map[string][]Window{"m": {{PlanID: "pln", From: *trialDay.TrialEnd, To: opJun}}}
	// A trial that outlasts the period (to June 10): the close only
	// advances the period, and the next one is trial time too.
	longTrial := may
	longTrial.TrialEnd = opAt(opJun.AddDate(0, 0, 9))
	longTrial.Floor = longTrial.TrialEnd
	// A cancel is scheduled for May 11.
	canceling := may
	canceling.Ends = opAt(opMay.AddDate(0, 0, 10))
	canceling.Next = nil
	cancelInGrace := may
	cancelInGrace.Ends = opAt(opJun.Add(time.Hour))
	// Another subscription stopped covering m at 10:00 on May 1.
	covered := may
	covered.Windows = map[string][]Window{"m": {{PlanID: "pln", From: opMay.Add(10 * time.Hour), To: opJun}}}
	// The prices were read before their pins: a price saved now is in force.
	ahead := labelled
	aheadA := modelA
	aheadA.Ahead = true
	ahead.Plans = map[string]PriceSet{"pln": aheadA}
	ahead.Next = []PriceSet{aheadA}

	a := map[string]any{"model": "a"}
	cases := []struct {
		name    string
		at      time.Time
		meter   string
		labels  map[string]any
		periods []OpenPeriod
		mb      MeterBuckets
		want    Class
	}{
		{"open period", opMay.AddDate(0, 0, 3), "m", nil, []OpenPeriod{may}, sum, InOpen},
		{"open period, meter not priced", opMay.AddDate(0, 0, 3), "x", nil, []OpenPeriod{may}, sum, NotOnPlan},
		{"open period, a price takes the labels", opMay.AddDate(0, 0, 3), "m", a, []OpenPeriod{labelled}, sum, InOpen},
		{"open period, no price takes the labels", opMay.AddDate(0, 0, 3), "m", map[string]any{"model": "b"}, []OpenPeriod{labelled}, sum, NoPrice},
		{"open period, no labels", opMay.AddDate(0, 0, 3), "m", nil, []OpenPeriod{labelled}, sum, NoPrice},
		{"no price, before a paid watermark", opMay.AddDate(0, 0, 3), "m", nil, []OpenPeriod{func() OpenPeriod { p := labelled; p.ThresholdWatermark = watermark.ThresholdWatermark; return p }()}, sum, NoPrice},
		{"swapped away: the old plan's stretch", opMay.AddDate(0, 0, 3), "m", nil, []OpenPeriod{swapped}, sum, InOpen},
		{"swapped away: after the swap", opMay.AddDate(0, 0, 20), "m", nil, []OpenPeriod{swapped}, sum, NotOnPlan},
		{"swapped onto: before the swap", opMay.AddDate(0, 0, 3), "n", nil, []OpenPeriod{swapped}, sum, NotOnPlan},
		{"first-day reach-back: the earlier item's window", opMay.Add(time.Minute), "m", nil, []OpenPeriod{reachBack}, sum, NoPrice},
		{"first-day reach-back: the earlier item's window, a price takes it", opMay.Add(time.Minute), "m", a, []OpenPeriod{reachBack}, sum, InOpen},
		{"first-day reach-back: the added item's window", opMay.Add(2 * time.Hour), "m", nil, []OpenPeriod{reachBack}, sum, InOpen},
		{"trial-end day, before the trial end", opMay.Add(10 * time.Hour), "m", nil, []OpenPeriod{trialDay}, sum, Outside},
		{"trial-end day, before the trial end, meter not priced", opMay.Add(10 * time.Hour), "x", nil, []OpenPeriod{trialDay}, sum, Outside},
		{"grace: the trial outlasts the period", opJun.Add(10 * time.Minute), "m", nil, []OpenPeriod{longTrial}, sum, Outside},
		{"trial-end day, after the trial end", opMay.Add(19 * time.Hour), "m", nil, []OpenPeriod{trialDay}, sum, InOpen},
		{"before a scheduled cancel", opMay.AddDate(0, 0, 9), "m", nil, []OpenPeriod{canceling}, sum, InOpen},
		{"after a scheduled cancel", opMay.AddDate(0, 0, 10).Add(20 * time.Minute), "m", nil, []OpenPeriod{canceling}, sum, Outside},
		{"grace: after a cancel scheduled in it", opJun.Add(2 * time.Hour), "m", nil, []OpenPeriod{cancelInGrace}, sum, Outside},
		{"grace: before a cancel scheduled in it", opJun.Add(10 * time.Minute), "m", nil, []OpenPeriod{cancelInGrace}, sum, InOpen},
		{"before another subscription's coverage end", opMay.Add(9 * time.Hour), "m", nil, []OpenPeriod{covered}, sum, Outside},
		{"before another subscription's coverage end, meter not priced", opMay.Add(9 * time.Hour), "x", nil, []OpenPeriod{covered}, sum, NotOnPlan},
		{"after another subscription's coverage end", opMay.Add(11 * time.Hour), "m", nil, []OpenPeriod{covered}, sum, InOpen},
		{"pin ahead: no price takes the labels", opMay.AddDate(0, 0, 3), "m", map[string]any{"model": "b"}, []OpenPeriod{ahead}, sum, InOpen},
		{"pin ahead: meter not priced", opMay.AddDate(0, 0, 3), "x", nil, []OpenPeriod{ahead}, sum, InOpen},
		{"grace, next pin ahead: no price takes the labels", opJun.Add(time.Minute), "m", map[string]any{"model": "b"}, []OpenPeriod{ahead}, sum, InOpen},
		{"invoiced period", opApr.AddDate(0, 0, 20), "m", nil, []OpenPeriod{may}, sum, AfterClose},
		{"invoiced period, labels no price takes", opApr.AddDate(0, 0, 20), "m", map[string]any{"model": "b"}, []OpenPeriod{labelled}, sum, AfterClose},
		{"invoiced period, one instant before the start", opMay.Add(-time.Nanosecond), "m", nil, []OpenPeriod{may}, sum, AfterClose},
		{"the start itself is open", opMay, "m", nil, []OpenPeriod{may}, sum, InOpen},
		// Review round 2 on #1390: an invoiced period is answered as such,
		// whatever the current plans price. Adding a price never bills it.
		{"invoiced period, meter not priced now", opApr.AddDate(0, 0, 20), "x", nil, []OpenPeriod{may}, sum, AfterClose},
		{"before the subscription started", opApr.AddDate(0, -2, 0), "m", nil, []OpenPeriod{may}, sum, Outside},
		{"in the trial", opApr.AddDate(0, 0, 3), "m", nil, []OpenPeriod{withTrial}, sum, Outside},
		{"after the trial, invoiced", opApr.AddDate(0, 0, 20), "m", nil, []OpenPeriod{withTrial}, sum, AfterClose},
		{"no floor known", opApr.AddDate(0, 0, 20), "m", nil, []OpenPeriod{noFloor}, sum, Outside},
		{"max meter, invoiced period", opApr.AddDate(0, 0, 20), "m", nil, []OpenPeriod{may}, max, AfterClose},
		{"before the watermark, sum", opMay.AddDate(0, 0, 5), "m", nil, []OpenPeriod{watermark}, sum, AfterClose},
		{"before the watermark, max (deferred)", opMay.AddDate(0, 0, 5), "m", nil, []OpenPeriod{watermark}, max, InOpen},
		{"after the watermark", opMay.AddDate(0, 0, 25), "m", nil, []OpenPeriod{watermark}, sum, InOpen},
		// Review finding 1 on #1350: an unpaid threshold invoice (open or
		// written off) can still be voided, and then the close bills from
		// the start.
		{"before an unpaid watermark, sum", opMay.AddDate(0, 0, 5), "m", nil, []OpenPeriod{unpaid}, sum, Unsettled},
		{"before an unpaid watermark, max", opMay.AddDate(0, 0, 5), "m", nil, []OpenPeriod{unpaid}, max, InOpen},
		{"before the paid fire of two", opMay.AddDate(0, 0, 5), "m", nil, []OpenPeriod{partPaid}, sum, AfterClose},
		{"after the paid fire, before the unpaid one", opMay.AddDate(0, 0, 12), "m", nil, []OpenPeriod{partPaid}, sum, Unsettled},
		{"after both fires", opMay.AddDate(0, 0, 22), "m", nil, []OpenPeriod{partPaid}, sum, InOpen},
		{"before the watermark, meter not priced", opMay.AddDate(0, 0, 5), "x", nil, []OpenPeriod{watermark}, sum, NotOnPlan},
		{"unsettled here, open in another subscription", opMay.AddDate(0, 0, 5), "m", nil, []OpenPeriod{unpaid, whole(opMay, opJun, opAt(opApr), "m")}, sum, InOpen},
		{"grace: next period", opJun.Add(10 * time.Minute), "m", nil, []OpenPeriod{may}, sum, InOpen},
		{"grace: ends at the period end", opJun.Add(10 * time.Minute), "m", nil, []OpenPeriod{ending}, sum, Outside},
		// Review round 3 on #1390: the first period starts at midnight of
		// the first day, before the start instant, and the close's
		// threshold watermark holds from there.
		{"first day, before the start instant, before a paid watermark", opMay.Add(9 * time.Hour), "m", nil, []OpenPeriod{firstDay}, sum, AfterClose},
		{"first day, before the start instant, before an unpaid watermark", opMay.Add(9 * time.Hour), "m", nil, []OpenPeriod{firstDayUnpaid}, sum, Unsettled},
		// The subscription that prices the meter invoiced April; another one
		// holds April but does not price it.
		{"invoiced here, another subscription holds it unpriced", opApr.AddDate(0, 0, 20), "m", nil, []OpenPeriod{may, yearly}, sum, AfterClose},
		{"no price here, unpriced in another subscription", opMay.AddDate(0, 0, 3), "m", nil, []OpenPeriod{labelled, yearly}, sum, NoPrice},
		{"no subscription", opApr.AddDate(0, 0, 20), "m", nil, nil, sum, Outside},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := classify(c.at, c.meter, c.labels, c.periods, func() (MeterBuckets, error) { return c.mb, nil })
			if err != nil || got != c.want {
				t.Errorf("classify = %v, %v; want %v", got, err, c.want)
			}
		})
	}
}

// TestClassify_AfterThePeriodEndIsNotChecked: an event at or after the
// current period's end is billed by its own period's plans at that period's
// pins. While the period's close has not run, ingest knows neither (the
// close can be behind by more than one period), so it answers no status,
// whatever the plans price at the period end. A trial and a scheduled
// cancel still make it outside_subscription.
func TestClassify_AfterThePeriodEndIsNotChecked(t *testing.T) {
	modelA := PriceSet{ByMeter: map[string][]map[string]string{"m": {{"model": "a"}}}}
	labelled := onPlans(opMay, opJun, opAt(opApr), map[string]PriceSet{"pln": modelA}, []Stretch{{PlanID: "pln", From: opMay, To: opJun}}, modelA)
	drops := onPlans(opMay, opJun, opAt(opApr), map[string]PriceSet{"pln": everyEvent("m")}, []Stretch{{PlanID: "pln", From: opMay, To: opJun}}, PriceSet{})
	for _, c := range []struct {
		name   string
		at     time.Time
		labels map[string]any
		p      OpenPeriod
	}{
		{"grace, no price at the period end takes the labels", opJun.Add(10 * time.Minute), map[string]any{"model": "b"}, labelled},
		{"grace, the plan at the period end drops the meter", opJun.Add(10 * time.Minute), nil, drops},
		{"a later period, the close behind", opJun.AddDate(0, 1, 9), map[string]any{"model": "b"}, labelled},
		{"a later period, a price takes the labels at the period end", opJun.AddDate(0, 1, 9), map[string]any{"model": "a"}, labelled},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, err := classify(c.at, "m", c.labels, []OpenPeriod{c.p}, func() (MeterBuckets, error) { return MeterBuckets{Additive: true}, nil })
			if err != nil || got.BillingStatus() != "" {
				t.Errorf("classify = %v (%q), %v; want no status", got, got.BillingStatus(), err)
			}
		})
	}
}

// TestClassify_ReadsTheMeterOnlyWhenNeeded: an event in an open period
// never costs the meter read.
func TestClassify_ReadsTheMeterOnlyWhenNeeded(t *testing.T) {
	may := whole(opMay, opJun, opAt(opApr), "m")
	read := 0
	if _, err := classify(opMay.AddDate(0, 0, 2), "m", nil, []OpenPeriod{may}, func() (MeterBuckets, error) {
		read++
		return MeterBuckets{}, nil
	}); err != nil || read != 0 {
		t.Errorf("open-period event: meter read %d times, err %v; want 0", read, err)
	}
}

// TestClass_BillingStatus: each class answers with its status, and a row an
// invoice counts or still can carries none.
func TestClass_BillingStatus(t *testing.T) {
	for c, want := range map[Class]string{
		Outside: "outside_subscription", NotOnPlan: "not_on_plan", AfterClose: "after_close",
		NoPrice: "no_price", Unsettled: "", InOpen: "",
	} {
		if got := c.BillingStatus(); got != want {
			t.Errorf("class %d: %q, want %q", c, got, want)
		}
	}
}

// TestTakes is the Go matcher on its own; the real-Postgres corpus test
// (takes_integration_test.go) checks it against @>.
func TestTakes(t *testing.T) {
	cases := []struct {
		match  map[string]string
		labels map[string]any
		want   bool
	}{
		{map[string]string{}, nil, true},
		{map[string]string{}, map[string]any{"a": 1.0}, true},
		{map[string]string{"model": "a"}, map[string]any{"model": "a"}, true},
		{map[string]string{"model": "a"}, map[string]any{"model": "a", "x": "y"}, true},
		{map[string]string{"model": "a", "tier": "1"}, map[string]any{"model": "a"}, false},
		{map[string]string{"model": "a"}, map[string]any{"model": "A"}, false},
		{map[string]string{"model": "a"}, nil, false},
		{map[string]string{"n": "1"}, map[string]any{"n": 1.0}, false},
		{map[string]string{"b": "true"}, map[string]any{"b": true}, false},
		{map[string]string{"z": ""}, map[string]any{"z": nil}, false},
		{map[string]string{"z": ""}, map[string]any{"z": ""}, true},
	}
	for _, c := range cases {
		if got := takes(c.match, c.labels); got != c.want {
			t.Errorf("takes(%v, %v) = %v, want %v", c.match, c.labels, got, c.want)
		}
	}
}

// TestOpenPeriodOf builds the period from a subscription: floor, the
// stretches with their plans' prices, the plan from the period end on, and
// whether it continues.
func TestOpenPeriodOf(t *testing.T) {
	plans := map[string]PriceSet{"pA": everyEvent("m"), "pB": everyEvent("n"), "pOld": everyEvent("o")}
	base := domain.Subscription{
		Status: domain.SubscriptionActive, StartedAt: opAt(opApr), CurrentBillingPeriodStart: opAt(opMay), CurrentBillingPeriodEnd: opAt(opJun),
		Items: []domain.SubscriptionItem{{PlanID: "pA"}},
	}
	stretches := []Stretch{{PlanID: "pOld", From: opMay, To: opMay.AddDate(0, 0, 9)}, {PlanID: "pA", From: opMay.AddDate(0, 0, 9), To: opJun}}
	windows := map[string][]Window{"o": {{PlanID: "pOld", From: opMay, To: opMay.AddDate(0, 0, 9)}}, "m": {{PlanID: "pA", From: opMay.AddDate(0, 0, 9), To: opJun}}}
	p, ok := OpenPeriodOf(base, stretches, windows, plans, plans, ThresholdWatermark{})
	if !ok || len(p.Next) != 1 || p.Next[0].ByMeter["m"] == nil || p.Floor == nil || !p.Floor.Equal(opApr) || p.TrialEnd != nil || p.Ends != nil {
		t.Fatalf("plain: %+v %v", p, ok)
	}
	if len(p.Stretches) != 2 || p.Stretches[0].PlanID != "pOld" || p.Plans["pOld"].ByMeter["o"] == nil || len(p.Windows["m"]) != 1 || p.Windows["m"][0].PlanID != "pA" {
		t.Errorf("stretches = %+v windows = %+v, want pOld then pA with their prices", p.Stretches, p.Windows)
	}

	nextMeters := func(p OpenPeriod) map[string]bool {
		out := map[string]bool{}
		for _, ps := range p.Next {
			for m := range ps.ByMeter {
				out[m] = true
			}
		}
		return out
	}
	pending := base
	pending.Items = []domain.SubscriptionItem{{PlanID: "pA", PendingPlanID: "pB", PendingPlanEffectiveAt: opAt(opJun)}}
	if p, _ := OpenPeriodOf(pending, stretches, windows, plans, plans, ThresholdWatermark{}); nextMeters(p)["m"] || !nextMeters(p)["n"] {
		t.Errorf("pending change at the end: next %v, want n", nextMeters(p))
	}
	later := base
	later.Items = []domain.SubscriptionItem{{PlanID: "pA", PendingPlanID: "pB", PendingPlanEffectiveAt: opAt(opJun.AddDate(0, 1, 0))}}
	if p, _ := OpenPeriodOf(later, stretches, windows, plans, plans, ThresholdWatermark{}); !nextMeters(p)["m"] || nextMeters(p)["n"] {
		t.Errorf("pending change after the end: next %v, want m", nextMeters(p))
	}

	atEnd := base
	atEnd.CancelAtPeriodEnd = true
	if p, _ := OpenPeriodOf(atEnd, stretches, windows, plans, plans, ThresholdWatermark{}); p.Next != nil || p.Ends == nil || !p.Ends.Equal(opJun) {
		t.Errorf("cancel at period end: next %v ends %v, want nil and the period end", p.Next, p.Ends)
	}
	cancelAt := base
	cancelAt.CancelAt = opAt(opJun)
	if p, _ := OpenPeriodOf(cancelAt, stretches, windows, plans, plans, ThresholdWatermark{}); p.Next != nil || p.Ends == nil || !p.Ends.Equal(opJun) {
		t.Errorf("cancel_at at the end: next %v ends %v, want nil and the cancel", p.Next, p.Ends)
	}

	// Review round 2 on #1390: a trialing subscription with a cancel at
	// period end is canceled free at its trial end (ADR-069), so its billed
	// time ends there.
	trialCancel := base
	trialCancel.Status = domain.SubscriptionTrialing
	trialCancel.TrialEndAt = opAt(opMay.Add(18 * time.Hour))
	trialCancel.CancelAtPeriodEnd = true
	if p, _ := OpenPeriodOf(trialCancel, stretches, windows, plans, plans, ThresholdWatermark{}); p.Ends == nil || !p.Ends.Equal(*trialCancel.TrialEndAt) || p.Next != nil {
		t.Errorf("trialing, cancel at period end: ends %v next %v, want the trial end and nil", p.Ends, p.Next)
	} else if c, _ := classify(opMay.Add(19*time.Hour), "m", nil, []OpenPeriod{p}, nil); c != Outside {
		t.Errorf("trialing, cancel at period end: after the trial end = %v, want Outside", c)
	}

	trial := base
	trial.TrialEndAt = opAt(opApr.AddDate(0, 0, 14))
	if p, _ := OpenPeriodOf(trial, stretches, windows, plans, plans, ThresholdWatermark{}); p.Floor == nil || !p.Floor.Equal(*trial.TrialEndAt) || p.TrialEnd == nil || !p.TrialEnd.Equal(*trial.TrialEndAt) {
		t.Errorf("trial: floor %v trial end %v, want the trial end", p.Floor, p.TrialEnd)
	}

	wm := ThresholdWatermark{BilledThrough: opAt(opMay.AddDate(0, 0, 19)), PaidThrough: opAt(opMay.AddDate(0, 0, 9))}
	if p, _ := OpenPeriodOf(base, stretches, windows, plans, plans, wm); p.BilledThrough != wm.BilledThrough || p.PaidThrough != wm.PaidThrough {
		t.Errorf("watermarks: %+v, want %+v", p.ThresholdWatermark, wm)
	}

	canceled := base
	canceled.Status = domain.SubscriptionCanceled
	if _, ok := OpenPeriodOf(canceled, stretches, windows, plans, plans, ThresholdWatermark{}); ok {
		t.Errorf("a canceled subscription has no open period")
	}
}

// TestBucketsOf: a meter's buckets are additive when its one aggregation is
// sum or count (ADR-130 §2); an empty aggregation reads as sum.
func TestBucketsOf(t *testing.T) {
	for agg, want := range map[string]MeterBuckets{
		"sum": {Additive: true}, "count": {Additive: true}, "": {Additive: true},
		"max": {}, "last": {},
	} {
		if got := BucketsOf(domain.Meter{Aggregation: agg}); got != want {
			t.Errorf("%q meter: %+v, want %+v", agg, got, want)
		}
	}
}

// stepLog records reads and writes in order.
type stepLog struct{ steps []string }

// orderReader answers every event as after-close, logging its reads.
type orderReader struct {
	log *stepLog
	err error
}

func (r *orderReader) OpenPeriods(context.Context, string, string) ([]OpenPeriod, error) {
	r.log.steps = append(r.log.steps, "read")
	if r.err != nil {
		return nil, r.err
	}
	return []OpenPeriod{whole(opMay, opJun, opAt(opApr), "mtr_1")}, nil
}

func (r *orderReader) MeterBucketsTx(context.Context, *sql.Tx, string) (MeterBuckets, error) {
	return MeterBuckets{Additive: true}, nil
}

// orderStore logs its writes.
type orderStore struct {
	*memStore
	log *stepLog
}

func (s orderStore) IngestAudited(ctx context.Context, tenantID string, e domain.UsageEvent, emit func(tx *sql.Tx, out domain.UsageEvent) error) (domain.UsageEvent, error) {
	s.log.steps = append(s.log.steps, "write")
	return s.memStore.IngestAudited(ctx, tenantID, e, emit)
}

func (s orderStore) IngestBatch(ctx context.Context, tenantID string, events []domain.UsageEvent, after func(*sql.Tx, []int) error) ([]int, int, error) {
	s.log.steps = append(s.log.steps, "write")
	return s.memStore.IngestBatch(ctx, tenantID, events, after)
}

// TestIngest_ReadsOpenPeriodsBeforeTheWrite: the order is what keeps the
// flag true (open_period.go). Read after the write, a close that billed the
// row and committed between them would make the row look unbilled.
//
// Mutation check: move the classify call after the store write → the order
// is [write read].
func TestIngest_ReadsOpenPeriodsBeforeTheWrite(t *testing.T) {
	log := &stepLog{}
	svc := NewService(orderStore{newMemStore(), log})
	svc.SetOpenPeriodReader(&orderReader{log: log})
	ts := opApr.AddDate(0, 0, 20)
	ev, err := svc.Ingest(context.Background(), "t1", IngestInput{CustomerID: "cus_1", MeterID: "mtr_1", Quantity: dec(1), IdempotencyKey: "k1", Timestamp: &ts})
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if strings.Join(log.steps, " ") != "read write" {
		t.Errorf("single ingest order = %v, want [read write]", log.steps)
	}
	if ev.BillingStatus != BillingStatusAfterClose {
		t.Errorf("billing_status = %q, want after_close", ev.BillingStatus)
	}

	log.steps = nil
	if _, errs := svc.BatchIngest(context.Background(), "t1", []IngestInput{
		{CustomerID: "cus_1", MeterID: "mtr_1", Quantity: dec(1), IdempotencyKey: "k2", Timestamp: &ts},
	}); len(errs) != 0 {
		t.Fatalf("batch: %v", errs)
	}
	if strings.Join(log.steps, " ") != "read write" {
		t.Errorf("batch order = %v, want [read write]", log.steps)
	}
}

// TestIngest_AfterCloseCountsWrittenRowsOnly: the counter moves for the row
// written, not for its replay, which also carries no billing_status.
func TestIngest_AfterCloseCountsWrittenRowsOnly(t *testing.T) {
	log := &stepLog{}
	svc := NewService(newMemStore())
	svc.SetOpenPeriodReader(&orderReader{log: log})
	ts := opApr.AddDate(0, 0, 20)
	in := IngestInput{CustomerID: "cus_1", MeterID: "mtr_1", Quantity: dec(1), IdempotencyKey: "k-count", Timestamp: &ts}
	c := unbilledEvents.WithLabelValues(BillingStatusAfterClose, string(domain.UsageOriginAPI))
	before := testutil.ToFloat64(c)
	if ev, err := svc.Ingest(context.Background(), "t1", in); err != nil || ev.BillingStatus != BillingStatusAfterClose {
		t.Fatalf("ingest: %+v %v", ev, err)
	}
	ev, err := svc.Ingest(context.Background(), "t1", in)
	if !errors.Is(err, errs.ErrDuplicateKey) || ev.BillingStatus != "" {
		t.Fatalf("replay: %+v %v, want ErrDuplicateKey and no status", ev, err)
	}
	if got := testutil.ToFloat64(c) - before; got != 1 {
		t.Errorf("counter moved by %v, want 1", got)
	}
}

// TestIngest_OpenPeriodReadFailureIs503: a read the store could not answer
// refuses the event at every door, with nothing written, like a failed
// customer or meter lookup.
//
// Mutation check: ignore the read error and ingest → 201/202 and a row.
func TestIngest_OpenPeriodReadFailureIs503(t *testing.T) {
	const event = `{"external_customer_id":"acme","event_name":"api_call","quantity":1,"idempotency_key":"evt_periods","timestamp":"2026-04-20T00:00:00Z"}`
	doors := map[string]func(h *Handler) (func(http.ResponseWriter, *http.Request), string){
		"single": func(h *Handler) (func(http.ResponseWriter, *http.Request), string) { return h.ingest, event },
		"batch": func(h *Handler) (func(http.ResponseWriter, *http.Request), string) {
			return h.batchIngest, "[" + event + "]"
		},
		"backfill": func(h *Handler) (func(http.ResponseWriter, *http.Request), string) { return h.Backfill, event },
	}
	for door, pick := range doors {
		t.Run(door, func(t *testing.T) {
			store := newMemStore()
			svc := NewService(store)
			svc.SetOpenPeriodReader(&orderReader{log: &stepLog{}, err: errLookupInfra})
			fn, body := pick(NewHandler(svc, &stubCustomers{}, &stubMeters{}))
			rr := usageReq(t, fn, body)
			if rr.Code != http.StatusServiceUnavailable || !strings.Contains(rr.Body.String(), `"code":"ingest_unavailable"`) {
				t.Fatalf("status %d body %s, want 503 ingest_unavailable", rr.Code, rr.Body.String())
			}
			if strings.Contains(rr.Body.String(), "connection refused") {
				t.Errorf("driver error leaked: %s", rr.Body.String())
			}
			if len(store.events) != 0 {
				t.Errorf("%d rows written, want 0", len(store.events))
			}
		})
	}
}

// TestIngestCall_AfterCloseCountsWrittenRowsOnly: a LiteLLM call arrives as
// two mappings, of which the store writes one, and in it some rows may be
// deduplicated. Only the rows the store newly wrote, in the mapping it
// wrote, count as after_close: 4 for the split written whole, 1 for the
// pre-split mapping with :input already stored, 0 for a replay.
//
// Mutation checks: count every after-close row of the written mapping
// (ignore Written) → 2 and 4 instead of 1 and 0; count both mappings → 6.
func TestIngestCall_AfterCloseCountsWrittenRowsOnly(t *testing.T) {
	ts := opApr.AddDate(0, 0, 20)
	ev := func(key string, q int64) CallEvent {
		return CallEvent{Quantity: dec(q), IdempotencyKey: key, Timestamp: &ts}
	}
	in := CallInput{
		CustomerID: "cus_1", MeterID: "mtr_1",
		Events:    []CallEvent{ev("c:input", 1000), ev("c:cache_write_5m", 40000), ev("c:cache_write_1h", 60000), ev("c:output", 500)},
		Legacy:    []CallEvent{ev("c:input", 101000), ev("c:output", 500)},
		LegacyKey: "c:input",
	}
	c := unbilledEvents.WithLabelValues(BillingStatusAfterClose, string(domain.UsageOriginAPI))
	for _, tc := range []struct {
		name    string
		legacy  bool
		written []int
		want    int
	}{
		{"split, written whole", false, nil, 4},
		{"pre-split, :input already stored", true, []int{1}, 1},
		{"replay", false, []int{}, 0},
	} {
		store := &heldMemStore{memStore: newMemStore(), callLegacy: tc.legacy, callWritten: tc.written}
		svc := NewService(store)
		svc.SetOpenPeriodReader(&orderReader{log: &stepLog{}})
		before := testutil.ToFloat64(c)
		res, err := svc.IngestCall(context.Background(), "t1", in)
		if err != nil {
			t.Fatalf("%s: ingest call: %v", tc.name, err)
		}
		if res.Unbilled.AfterClose != tc.want {
			t.Errorf("%s: after_close = %d, want %d", tc.name, res.Unbilled.AfterClose, tc.want)
		}
		if got := testutil.ToFloat64(c) - before; got != float64(tc.want) {
			t.Errorf("%s: counter moved by %v, want %d", tc.name, got, tc.want)
		}
	}
}

// TestIngestCall_ReadsOpenPeriodsBeforeTheWrite: as for the other doors, the
// read comes first, and a failed read writes nothing.
//
// Mutation checks: classify after IngestCallResolvingHeld → the order test
// fails; ignore the read error → the call is written.
func TestIngestCall_ReadsOpenPeriodsBeforeTheWrite(t *testing.T) {
	ts := opApr.AddDate(0, 0, 20)
	in := CallInput{CustomerID: "cus_1", MeterID: "mtr_1", Events: []CallEvent{{Quantity: dec(1), IdempotencyKey: "o:input", Timestamp: &ts}}}

	log := &stepLog{}
	svc := NewService(&callOrderStore{heldMemStore: &heldMemStore{memStore: newMemStore()}, log: log})
	svc.SetOpenPeriodReader(&orderReader{log: log})
	if _, err := svc.IngestCall(context.Background(), "t1", in); err != nil {
		t.Fatalf("ingest call: %v", err)
	}
	if strings.Join(log.steps, " ") != "read write" {
		t.Errorf("call order = %v, want [read write]", log.steps)
	}

	log = &stepLog{}
	svc = NewService(&callOrderStore{heldMemStore: &heldMemStore{memStore: newMemStore()}, log: log})
	svc.SetOpenPeriodReader(&orderReader{log: log, err: errLookupInfra})
	if _, err := svc.IngestCall(context.Background(), "t1", in); !errors.Is(err, errIngestUnavailable) {
		t.Fatalf("ingest call with a failed read: %v, want errIngestUnavailable", err)
	}
	if strings.Contains(strings.Join(log.steps, " "), "write") {
		t.Errorf("call order = %v, want no write after a failed read", log.steps)
	}
}

// callOrderStore logs IngestCallResolvingHeld.
type callOrderStore struct {
	*heldMemStore
	log *stepLog
}

func (s *callOrderStore) IngestCallResolvingHeld(ctx context.Context, tenantID string, call StoreCall, after func(*sql.Tx, CallResult) error) (CallResult, error) {
	s.log.steps = append(s.log.steps, "write")
	return s.heldMemStore.IngestCallResolvingHeld(ctx, tenantID, call, after)
}

// heldOrderStore logs every write of the held store's ingest doors and of
// the plain ones.
type heldOrderStore struct {
	*heldMemStore
	log *stepLog
}

func (s *heldOrderStore) IngestAudited(ctx context.Context, tenantID string, e domain.UsageEvent, emit func(tx *sql.Tx, out domain.UsageEvent) error) (domain.UsageEvent, error) {
	s.log.steps = append(s.log.steps, "write")
	return s.heldMemStore.IngestAudited(ctx, tenantID, e, emit)
}

func (s *heldOrderStore) IngestBatch(ctx context.Context, tenantID string, events []domain.UsageEvent, after func(*sql.Tx, []int) error) ([]int, int, error) {
	s.log.steps = append(s.log.steps, "write")
	return s.heldMemStore.IngestBatch(ctx, tenantID, events, after)
}

func (s *heldOrderStore) IngestResolvingHeld(ctx context.Context, tenantID string, ev domain.UsageEvent, after func(*sql.Tx, domain.UsageEvent) error) (domain.UsageEvent, error) {
	s.log.steps = append(s.log.steps, "write")
	return s.heldMemStore.IngestResolvingHeld(ctx, tenantID, ev, after)
}

func (s *heldOrderStore) IngestCallResolvingHeld(ctx context.Context, tenantID string, call StoreCall, after func(*sql.Tx, CallResult) error) (CallResult, error) {
	s.log.steps = append(s.log.steps, "write")
	return s.heldMemStore.IngestCallResolvingHeld(ctx, tenantID, call, after)
}

// aggOrderReader answers a period whose paid threshold invoice billed
// through after the event, so the class depends on the meter's aggregation,
// and logs its reads.
type aggOrderReader struct{ log *stepLog }

func (r *aggOrderReader) OpenPeriods(context.Context, string, string) ([]OpenPeriod, error) {
	r.log.steps = append(r.log.steps, "periods")
	wm := opAt(opMay.AddDate(0, 0, 19))
	p := whole(opMay, opJun, opAt(opApr), "mtr_1")
	p.ThresholdWatermark = ThresholdWatermark{BilledThrough: wm, PaidThrough: wm}
	return []OpenPeriod{p}, nil
}

func (r *aggOrderReader) MeterBucketsTx(context.Context, *sql.Tx, string) (MeterBuckets, error) {
	r.log.steps = append(r.log.steps, "meter")
	return MeterBuckets{Additive: true}, nil
}

// TestIngest_ReadsTheAggregationAfterTheWrite: at every door the open
// periods are read before the write and the meter's aggregation after it,
// on the write's transaction (open_period.go). Read before the write, an
// aggregation PATCH that commits in between makes the flag follow the old
// aggregation. The flag still follows what was read.
//
// Mutation check: read the meter before the write again → the order is
// [periods meter write].
func TestIngest_ReadsTheAggregationAfterTheWrite(t *testing.T) {
	ts := opMay.AddDate(0, 0, 5)
	in := func(key string) IngestInput {
		return IngestInput{CustomerID: "cus_1", MeterID: "mtr_1", Quantity: dec(1), IdempotencyKey: key, Timestamp: &ts}
	}
	doors := map[string]func(svc *Service) (int, error){
		"single": func(svc *Service) (int, error) {
			ev, err := svc.Ingest(context.Background(), "t1", in("k-single"))
			return boolInt(ev.BillingStatus == BillingStatusAfterClose), err
		},
		"batch": func(svc *Service) (int, error) {
			res, errs := svc.BatchIngest(context.Background(), "t1", []IngestInput{in("k-batch-1"), in("k-batch-2")})
			if len(errs) > 0 {
				return 0, errs[0]
			}
			return res.Unbilled.AfterClose, nil
		},
		"backfill": func(svc *Service) (int, error) {
			ev, err := svc.Backfill(context.Background(), "t1", in("k-backfill"))
			return boolInt(ev.BillingStatus == BillingStatusAfterClose), err
		},
		"litellm event": func(svc *Service) (int, error) {
			ev, err := svc.IngestResolvingHeld(context.Background(), "t1", in("k-held"))
			return boolInt(ev.BillingStatus == BillingStatusAfterClose), err
		},
		"litellm call": func(svc *Service) (int, error) {
			res, err := svc.IngestCall(context.Background(), "t1", CallInput{CustomerID: "cus_1", MeterID: "mtr_1", Events: []CallEvent{
				{Quantity: dec(1), IdempotencyKey: "c:input", Timestamp: &ts}, {Quantity: dec(1), IdempotencyKey: "c:output", Timestamp: &ts},
			}})
			return res.Unbilled.AfterClose, err
		},
	}
	want := map[string]int{"single": 1, "batch": 2, "backfill": 1, "litellm event": 1, "litellm call": 2}
	for name, run := range doors {
		t.Run(name, func(t *testing.T) {
			log := &stepLog{}
			svc := NewService(&heldOrderStore{heldMemStore: &heldMemStore{memStore: newMemStore()}, log: log})
			svc.SetOpenPeriodReader(&aggOrderReader{log: log})
			flagged, err := run(svc)
			if err != nil {
				t.Fatalf("ingest: %v", err)
			}
			if got := strings.Join(log.steps, " "); got != "periods write meter" {
				t.Errorf("order = [%s], want [periods write meter]", got)
			}
			if flagged != want[name] {
				t.Errorf("after_close = %d, want %d", flagged, want[name])
			}
		})
	}
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
