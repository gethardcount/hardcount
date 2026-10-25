package billing

// The same-build differential of the one rater (ADR-130 PR-1; design of
// record §4.17). This file keeps the four rating loops as they were before
// rate.go replaced them, renamed legacy*, and checks the new rater against
// them on every rating call the package's tests make: the period close, the
// immediate cancel and the cross-interval swap, the preview (close mirror and
// flat window, which is also the spend-threshold evaluation) and the live
// balance. TestMain sets rateTap, so each of the four entry points hands its
// call to rateDifferential, which rates it through the new code and through
// the old loops with the same inputs and compares the results field by field:
// lines (quantities, amounts, descriptions, rule-version stamps, windows),
// subtotals, the threshold watermark, the unpriced and below-zero records,
// warnings and the live total. Any difference fails the test binary.
//
// A call whose new result moves between two runs is a difference too: the
// rater must give one answer for one input. The only exceptions are the tests
// in movesBetweenRuns, whose fakes change their answer after the first call
// on purpose.
//
// Set VELOX_RATE_DIFF_REPORT=<file> to write which entry points and pricing
// shapes the run compared, and in which tests.
//
// Everything below the "Old rating loops" line is the code that origin/main
// 672eb6d5 ran, cut verbatim from engine.go, preview.go, live_balance.go and
// usage_ladder.go, with only the names changed and three edits, each made in
// the new code too. The three loops over a map of meters (the period close,
// the cancel, the live balance) walk the meters in id order, and the
// close-mirror preview adds the meters only a segment plan carries by plan id.
// The old code walked both maps in map order, which is random, so each is one
// order it could take; it makes the line order, and the error text of a call
// that fails on every meter, the same in both. The third edit is #1371's,
// merged from main after the cut: the period close, the cancel and the
// close-mirror preview skip the in_arrears base only behind a threshold
// invoice that carries one (wm.billedBase).
// Shared helpers this PR does not change (the window builders, the base-fee
// lines, the usage reads, the watermark, line descriptions) are not copied.
// A fourth edit came with checked money math: the shared base-fee helpers
// and computePreviewTotals return an error now, which the old loops pass on.
// The old loops' own sums stay unchecked, so a call the new code refuses
// with domain.ErrAmountOverflow (an amount past int64, which the old loops
// wrapped) is not compared; overflowRefusals names those calls.
// PR-4 deletes this file with the rating-rule tables.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"os"
	"reflect"
	"runtime"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/errs"
	"github.com/sagarsuperuser/velox/internal/usage"
)

func TestMain(m *testing.M) {
	d := &rateDifferential{feats: map[string]map[string]*featTally{}, calls: map[string]int{}}
	rateTap = d
	code := m.Run()
	if d.report(os.Stderr) && code == 0 {
		code = 1
	}
	os.Exit(code)
}

// featTally counts the comparisons that exercised one feature, and names a
// few of the tests that did.
type featTally struct {
	n     int
	tests map[string]struct{}
}

type rateDifferential struct {
	mu       sync.Mutex
	calls    map[string]int
	feats    map[string]map[string]*featTally
	diffs    []string
	unstable []string // entry and test of each call of a movesBetweenRuns test whose new result moved
	memo     []string // entry and test of each call skipped under an aggregation memo
	overflow []string // entry and test of each call the new code refused with ErrAmountOverflow
}

// refusedOverflow reports whether the new code refused the call with
// domain.ErrAmountOverflow, and records it: the old loops wrapped such
// sums, so there is nothing to compare.
func (d *rateDifferential) refusedOverflow(entry string, err error) bool {
	if !errors.Is(err, domain.ErrAmountOverflow) {
		return false
	}
	test := currentTest()
	d.mu.Lock()
	defer d.mu.Unlock()
	d.overflow = append(d.overflow, entry+" in "+test)
	return true
}

// OverflowRefusals is how many calls the differential left uncompared
// because the new code refused them with ErrAmountOverflow.
func OverflowRefusals() int {
	d, ok := rateTap.(*rateDifferential)
	if !ok {
		return 0
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.overflow)
}

// noTap is the context the differential rates on, so its own calls are not
// tapped again.
func noTap(ctx context.Context) context.Context { return context.WithValue(ctx, rateTapOff{}, true) }

// underMemo reports whether ctx carries the public cost dashboard's
// aggregation memo (usage.WithAggregationMemo returns the one already on ctx,
// a new one otherwise). A second rating under it would count as memo hits,
// which TestLiveBalance_DashboardAggregatesOncePerMeter asserts on, so such a
// call is rated once, by the new code, and counted as skipped.
func underMemo(ctx context.Context) bool {
	_, a := usage.WithAggregationMemo(ctx)
	_, b := usage.WithAggregationMemo(ctx)
	return a == b
}

func (d *rateDifferential) skipMemo(entry string) {
	test := currentTest()
	d.mu.Lock()
	defer d.mu.Unlock()
	d.memo = append(d.memo, entry+" in "+test)
}

// RateDifferentialCompared is how many calls of entry the differential has
// compared so far (rate_shapes_integration_test.go asserts its calls were).
func RateDifferentialCompared(entry string) int {
	d, ok := rateTap.(*rateDifferential)
	if !ok {
		return 0
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.calls[entry]
}

// --- the four entry points --------------------------------------------------

func (d *rateDifferential) buildLineItems(ctx context.Context, e *Engine, sub domain.Subscription, now, periodStart, periodEnd time.Time, plans map[string]domain.Plan, cur string, loc *time.Location, unpriced unpricedUsage, belowZero netNegativeUsage) ([]domain.InvoiceLineItem, int64, *time.Time, error) {
	if underMemo(ctx) {
		d.skipMemo("period close")
		return e.buildLineItems(noTap(ctx), sub, now, periodStart, periodEnd, plans, cur, loc, unpriced, belowZero)
	}
	ctx = noTap(ctx)
	given := clonePlans(plans)
	type result struct {
		lines    []domain.InvoiceLineItem
		subtotal int64
		bt       *time.Time
		u        unpricedUsage
		b        netNegativeUsage
		err      error
	}
	run := func(p map[string]domain.Plan) (string, result) {
		r := result{u: unpricedUsage{}, b: netNegativeUsage{}}
		r.lines, r.subtotal, r.bt, r.err = e.buildLineItems(ctx, sub, now, periodStart, periodEnd, p, cur, loc, r.u, r.b)
		return renderClose(r.lines, r.subtotal, r.bt, r.err, r.u, r.b), r
	}
	// The new code rates on the caller's map, which it hydrates with segment
	// plans as before.
	got, res := run(plans)
	if d.refusedOverflow("period close", res.err) {
		mergeLeftOut(unpriced, belowZero, res.u, res.b)
		return res.lines, res.subtotal, res.bt, res.err
	}
	want := guard(func() string {
		u2, b2 := unpricedUsage{}, netNegativeUsage{}
		l2, s2, bt2, err2 := e.legacyBuildLineItems(ctx, sub, now, periodStart, periodEnd, clonePlans(given), cur, loc, u2, b2)
		return renderClose(l2, s2, bt2, err2, u2, b2)
	})
	d.compare("period close", sub, got, want, func() string { s, _ := run(clonePlans(given)); return s })
	d.tally("period close", sub, closeFeatures(ctx, e, sub, res.lines, res.bt, res.u, res.b))
	mergeLeftOut(unpriced, belowZero, res.u, res.b)
	return res.lines, res.subtotal, res.bt, res.err
}

func (d *rateDifferential) buildCancelLineItems(ctx context.Context, e *Engine, sub domain.Subscription, wm thresholdWatermark, plans map[string]domain.Plan, cur string, periodStart, canceledAt time.Time, loc *time.Location, unpriced unpricedUsage, belowZero netNegativeUsage, suffix string) ([]domain.InvoiceLineItem, int64, error) {
	if underMemo(ctx) {
		d.skipMemo("immediate cancel or swap")
		return e.buildCancelLineItems(noTap(ctx), sub, wm, plans, cur, periodStart, canceledAt, loc, unpriced, belowZero, suffix)
	}
	ctx = noTap(ctx)
	given := clonePlans(plans)
	type result struct {
		lines    []domain.InvoiceLineItem
		subtotal int64
		u        unpricedUsage
		b        netNegativeUsage
		err      error
	}
	run := func(p map[string]domain.Plan) (string, result) {
		r := result{u: unpricedUsage{}, b: netNegativeUsage{}}
		r.lines, r.subtotal, r.err = e.buildCancelLineItems(ctx, sub, wm, p, cur, periodStart, canceledAt, loc, r.u, r.b, suffix)
		return renderClose(r.lines, r.subtotal, nil, r.err, r.u, r.b), r
	}
	got, res := run(plans)
	if d.refusedOverflow("immediate cancel or swap", res.err) {
		mergeLeftOut(unpriced, belowZero, res.u, res.b)
		return res.lines, res.subtotal, res.err
	}
	want := guard(func() string {
		u2, b2 := unpricedUsage{}, netNegativeUsage{}
		l2, s2, err2 := e.legacyBuildCancelLineItems(ctx, sub, wm, clonePlans(given), cur, periodStart, canceledAt, loc, u2, b2, suffix)
		return renderClose(l2, s2, nil, err2, u2, b2)
	})
	entry := "immediate cancel"
	if suffix == swapUsageSuffix {
		entry = "cross-interval swap"
	}
	d.compare(entry, sub, got, want, func() string { s, _ := run(clonePlans(given)); return s })
	d.tally(entry, sub, closeFeatures(ctx, e, sub, res.lines, wm.billedThrough, res.u, res.b))
	mergeLeftOut(unpriced, belowZero, res.u, res.b)
	return res.lines, res.subtotal, res.err
}

func (d *rateDifferential) previewWindow(ctx context.Context, e *Engine, sub domain.Subscription, periodStart, periodEnd time.Time, asClose bool) (PreviewResult, error) {
	entry := "preview, flat window"
	switch {
	case asClose:
		entry = "preview, close mirror"
	case callerIs("evaluateThresholds"):
		entry = "spend-threshold evaluation"
	}
	if underMemo(ctx) {
		d.skipMemo(entry)
		return e.previewWindow(noTap(ctx), sub, periodStart, periodEnd, asClose)
	}
	ctx = noTap(ctx)
	run := func() (string, PreviewResult, error) {
		res, err := e.previewWindow(ctx, sub, periodStart, periodEnd, asClose)
		return renderPreview(res, err), res, err
	}
	got, res, err := run()
	if d.refusedOverflow(entry, err) {
		return res, err
	}
	want := guard(func() string {
		res2, err2 := e.legacyPreviewWindow(ctx, sub, periodStart, periodEnd, asClose)
		return renderPreview(res2, err2)
	})
	d.compare(entry, sub, got, want, func() string { s, _, _ := run(); return s })
	d.tally(entry, sub, previewFeatures(ctx, e, sub, periodStart, periodEnd, res, asClose))
	return res, err
}

func (d *rateDifferential) unbilledUsageCents(ctx context.Context, e *Engine, sub domain.Subscription) (int64, string, error) {
	if underMemo(ctx) {
		d.skipMemo("live balance")
		return e.unbilledUsageCents(noTap(ctx), sub)
	}
	ctx = noTap(ctx)
	run := func() (string, int64, string, error) {
		total, cur, err := e.unbilledUsageCents(ctx, sub)
		return fmt.Sprintf("total=%d currency=%q err=%s", total, cur, errText(err)), total, cur, err
	}
	got, total, cur, err := run()
	if d.refusedOverflow("live balance", err) {
		return total, cur, err
	}
	want := guard(func() string {
		t2, c2, err2 := e.legacyUnbilledUsageCents(ctx, sub)
		return fmt.Sprintf("total=%d currency=%q err=%s", t2, c2, errText(err2))
	})
	d.compare("live balance", sub, got, want, func() string { s, _, _, _ := run(); return s })
	feats := []string{"every call"}
	if total != 0 {
		feats = append(feats, "usage priced")
	}
	if sub.TestClockID != "" {
		feats = append(feats, "test clock")
	}
	if sub.CurrentBillingPeriodStart != nil && sub.CurrentBillingPeriodEnd != nil {
		if wm, werr := e.loadThresholdWatermark(ctx, sub.TenantID, sub.ID, *sub.CurrentBillingPeriodStart, *sub.CurrentBillingPeriodEnd); werr == nil && wm.exists() {
			feats = append(feats, "after a threshold invoice (ladder, deferral)")
		}
	}
	d.tally("live balance", sub, feats)
	return total, cur, err
}

// --- comparison ---------------------------------------------------------------

// guard runs the old loop, turning a panic into a result that cannot match.
func guard(f func() string) (out string) {
	defer func() {
		if r := recover(); r != nil {
			out = fmt.Sprintf("legacy panicked: %v", r)
		}
	}()
	return f()
}

func (d *rateDifferential) compare(entry string, sub domain.Subscription, got, want string, again func() string) {
	test := currentTest()
	d.mu.Lock()
	d.calls[entry]++
	d.mu.Unlock()
	if got == want {
		return
	}
	if second := again(); second != got {
		d.mu.Lock()
		defer d.mu.Unlock()
		if _, ok := movesBetweenRuns[test]; ok {
			d.unstable = append(d.unstable, entry+" in "+test)
			return
		}
		d.diffs = append(d.diffs, fmt.Sprintf("%s in %s (subscription %s): the new result moved between two runs:\n%s", entry, test, sub.ID, lineDiff(got, second)))
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.diffs = append(d.diffs, fmt.Sprintf("%s in %s (subscription %s):\n%s", entry, test, sub.ID, lineDiff(want, got)))
}

// movesBetweenRuns names the tests whose fakes answer the first call
// differently from the next ones, and why. Their calls are not compared.
var movesBetweenRuns = map[string]string{
	"TestRunCycle_MeterReadBlipFailsTheClose": "its pricing fake fails the first read of one meter only",
	"TestCloser_ReadsEachMeterOnce":           "its pricing fake changes one meter's aggregation after the first read",
}

func (d *rateDifferential) tally(entry string, sub domain.Subscription, feats []string) {
	test := currentTest()
	d.mu.Lock()
	defer d.mu.Unlock()
	byFeat := d.feats[entry]
	if byFeat == nil {
		byFeat = map[string]*featTally{}
		d.feats[entry] = byFeat
	}
	seen := map[string]bool{}
	for _, f := range feats {
		if seen[f] {
			continue // one call counts once per feature
		}
		seen[f] = true
		t := byFeat[f]
		if t == nil {
			t = &featTally{tests: map[string]struct{}{}}
			byFeat[f] = t
		}
		t.n++
		t.tests[test] = struct{}{}
	}
}

// report prints every difference and, when asked, the coverage. It reports
// whether there was a difference.
func (d *rateDifferential) report(w io.Writer) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if path := os.Getenv("VELOX_RATE_DIFF_REPORT"); path != "" {
		if f, err := os.Create(path); err == nil {
			d.writeCoverage(f)
			_ = f.Close()
		}
	}
	if len(d.diffs) == 0 {
		return false
	}
	_, _ = fmt.Fprintf(w, "\nRATE DIFFERENTIAL: %d call(s) where the one rater and the old loops disagree (ADR-130 PR-1):\n\n", len(d.diffs))
	for _, s := range d.diffs {
		_, _ = fmt.Fprintf(w, "--- %s\n\n", s)
	}
	return true
}

func (d *rateDifferential) writeCoverage(w io.Writer) {
	entries := make([]string, 0, len(d.calls))
	for e := range d.calls {
		entries = append(entries, e)
	}
	sort.Strings(entries)
	_, _ = fmt.Fprintf(w, "| Path | Feature | Calls compared | Tests (up to 4 of N) |\n|---|---|---|---|\n")
	for _, e := range entries {
		feats := make([]string, 0, len(d.feats[e]))
		for f := range d.feats[e] {
			feats = append(feats, f)
		}
		sort.Strings(feats)
		for _, f := range feats {
			t := d.feats[e][f]
			names := make([]string, 0, len(t.tests))
			for n := range t.tests {
				names = append(names, n)
			}
			sort.Strings(names)
			shown := names
			if len(shown) > 4 {
				shown = shown[:4]
			}
			_, _ = fmt.Fprintf(w, "| %s | %s | %d | %s (%d) |\n", e, f, t.n, strings.Join(shown, ", "), len(names))
		}
	}
	_, _ = fmt.Fprintf(w, "\nCalls compared: %v. Differences: %d.\n", d.calls, len(d.diffs))
	_, _ = fmt.Fprintf(w, "Not compared (a movesBetweenRuns test; the new result moved between two runs): %d %v\n", len(d.unstable), d.unstable)
	_, _ = fmt.Fprintf(w, "Skipped under the cost dashboard's aggregation memo: %d %v\n", len(d.memo), d.memo)
	_, _ = fmt.Fprintf(w, "Not compared (refused with ErrAmountOverflow; the old loops wrapped): %d %v\n", len(d.overflow), d.overflow)
}

// lineDiff shows the lines of the two renderings that differ.
func lineDiff(want, got string) string {
	w, g := strings.Split(want, "\n"), strings.Split(got, "\n")
	inW := map[string]int{}
	for _, l := range w {
		inW[l]++
	}
	inG := map[string]int{}
	for _, l := range g {
		inG[l]++
	}
	var b strings.Builder
	for _, l := range w {
		if inG[l] > 0 {
			inG[l]--
			continue
		}
		fmt.Fprintf(&b, "  old: %s\n", l)
	}
	for _, l := range g {
		if inW[l] > 0 {
			inW[l]--
			continue
		}
		fmt.Fprintf(&b, "  new: %s\n", l)
	}
	if b.Len() == 0 {
		return "  (same lines, different order)\n  old:\n" + want + "\n  new:\n" + got
	}
	return b.String()
}

// currentTest names the test function on the stack, or "a test goroutine".
func currentTest() string {
	pcs := make([]uintptr, 64)
	n := runtime.Callers(2, pcs)
	frames := runtime.CallersFrames(pcs[:n])
	for {
		f, more := frames.Next()
		name := f.Function[strings.LastIndex(f.Function, "/")+1:]
		if i := strings.Index(name, ".Test"); i >= 0 {
			name = name[i+1:]
			if j := strings.Index(name, "."); j >= 0 {
				name = name[:j]
			}
			return name
		}
		if !more {
			return "a test goroutine"
		}
	}
}

// callerIs reports whether fn is on the stack.
func callerIs(fn string) bool {
	pcs := make([]uintptr, 64)
	n := runtime.Callers(2, pcs)
	frames := runtime.CallersFrames(pcs[:n])
	for {
		f, more := frames.Next()
		if strings.HasSuffix(f.Function, "."+fn) {
			return true
		}
		if !more {
			return false
		}
	}
}

// --- rendering ----------------------------------------------------------------

// The renderings are exact: every field of every line, decimals by
// coefficient and exponent (so 10 and 10.0 differ), times to the nanosecond
// with their zone. Lines, preview warnings and left-out buckets are compared
// in the order the call returned them: the closers and the live balance walk
// meters in id order, the previews in the plan's meter order, in the old code
// and the new alike.

func renderClose(lines []domain.InvoiceLineItem, subtotal int64, billedThrough *time.Time, err error, u unpricedUsage, b netNegativeUsage) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "subtotal=%d\nbilled_through=%s\nerr=%s\n", subtotal, canon(reflect.ValueOf(billedThrough)), errText(err))
	sb.WriteString(renderInOrder(len(lines), func(i int) string {
		return canon(reflect.ValueOf(lines[i]))
	}))
	sb.WriteString(renderUnpriced(u))
	sb.WriteString(renderBelowZero(b))
	return sb.String()
}

func renderPreview(res PreviewResult, err error) string {
	res.GeneratedAt = time.Time{} // the clock reading, not a rating result
	lines := res.Lines
	res.Lines = nil
	warnings := res.Warnings
	res.Warnings = nil
	var sb strings.Builder
	fmt.Fprintf(&sb, "err=%s\nheader=%s\n", errText(err), canon(reflect.ValueOf(res)))
	sb.WriteString(renderInOrder(len(lines), func(i int) string {
		return canon(reflect.ValueOf(lines[i]))
	}))
	for _, w := range warnings {
		fmt.Fprintf(&sb, "warning=%s\n", w)
	}
	for _, lo := range res.leftOut {
		fmt.Fprintf(&sb, "left_out=%s\n", canon(reflect.ValueOf(lo)))
	}
	fmt.Fprintf(&sb, "floors.trial_end=%s\n", canon(reflect.ValueOf(res.floors.trialEnd)))
	meters := make([]string, 0, len(res.floors.coverageEnds))
	for m := range res.floors.coverageEnds {
		meters = append(meters, m)
	}
	sort.Strings(meters)
	for _, m := range meters {
		fmt.Fprintf(&sb, "floors.coverage_end[%s]=%s\n", m, canon(reflect.ValueOf(res.floors.coverageEnds[m])))
	}
	return sb.String()
}

// renderInOrder renders n lines in their order.
func renderInOrder(n int, line func(i int) string) string {
	var sb strings.Builder
	for i := 0; i < n; i++ {
		fmt.Fprintf(&sb, "line[%d]=%s\n", i, line(i))
	}
	return sb.String()
}

func renderUnpriced(u unpricedUsage) string {
	keys := make([]string, 0, len(u))
	for k := range u {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var sb strings.Builder
	for _, k := range keys {
		fmt.Fprintf(&sb, "unpriced[%s]=%s\n", k, canon(reflect.ValueOf(u[k])))
	}
	return sb.String()
}

func renderBelowZero(b netNegativeUsage) string {
	keys := make([]string, 0, len(b))
	for k := range b {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var sb strings.Builder
	for _, k := range keys {
		m := b[k]
		fmt.Fprintf(&sb, "below_zero[%s]=key:%s name:%s qty:%s from:%s to:%s\n", k, m.key, m.name,
			canon(reflect.ValueOf(m.qty)), canon(reflect.ValueOf(m.from)), canon(reflect.ValueOf(m.to)))
	}
	return sb.String()
}

func errText(err error) string {
	if err == nil {
		return "<nil>"
	}
	return err.Error()
}

var (
	decimalType = reflect.TypeOf(decimal.Decimal{})
	timeType    = reflect.TypeOf(time.Time{})
)

// canon renders v exactly. It refuses an unexported field rather than skip
// it: a field it cannot read would be a field the differential does not
// compare.
func canon(v reflect.Value) string {
	switch {
	case !v.IsValid():
		return "<invalid>"
	case v.Type() == decimalType:
		d := v.Interface().(decimal.Decimal)
		return fmt.Sprintf("%s(e%d)", d.Coefficient().String(), d.Exponent())
	case v.Type() == timeType:
		t := v.Interface().(time.Time)
		return t.Format(time.RFC3339Nano) + "@" + t.Location().String()
	}
	switch v.Kind() {
	case reflect.Pointer, reflect.Interface:
		if v.IsNil() {
			return "nil"
		}
		return "&" + canon(v.Elem())
	case reflect.Struct:
		var sb strings.Builder
		sb.WriteString("{")
		for i := 0; i < v.NumField(); i++ {
			f := v.Type().Field(i)
			if !f.IsExported() {
				if v.Type() == reflect.TypeOf(PreviewResult{}) {
					continue // leftOut and floors are rendered by renderPreview
				}
				panic(fmt.Sprintf("canon: unexported field %s.%s", v.Type(), f.Name))
			}
			fmt.Fprintf(&sb, "%s:%s ", f.Name, canon(v.Field(i)))
		}
		sb.WriteString("}")
		return sb.String()
	case reflect.Map:
		if v.IsNil() {
			return "nilmap"
		}
		keys := v.MapKeys()
		parts := make([]string, 0, len(keys))
		for _, k := range keys {
			parts = append(parts, canon(k)+"="+canon(v.MapIndex(k)))
		}
		sort.Strings(parts)
		return "map[" + strings.Join(parts, ",") + "]"
	case reflect.Slice:
		if v.IsNil() {
			return "nilslice"
		}
		parts := make([]string, v.Len())
		for i := range parts {
			parts[i] = canon(v.Index(i))
		}
		return "[" + strings.Join(parts, ",") + "]"
	case reflect.Array:
		parts := make([]string, v.Len())
		for i := range parts {
			parts[i] = canon(v.Index(i))
		}
		return "[" + strings.Join(parts, ",") + "]"
	case reflect.String:
		return fmt.Sprintf("%q", v.String())
	default:
		return fmt.Sprintf("%v", v.Interface())
	}
}

func clonePlans(p map[string]domain.Plan) map[string]domain.Plan {
	out := make(map[string]domain.Plan, len(p))
	for k, v := range p {
		out[k] = v
	}
	return out
}

// mergeLeftOut adds what the new code left out to the caller's records, as it
// would have added them itself.
func mergeLeftOut(unpriced unpricedUsage, belowZero netNegativeUsage, u unpricedUsage, b netNegativeUsage) {
	for m, q := range u {
		unpriced.add(m, q)
	}
	for m, n := range b {
		belowZero.add(m, n.key, n.name, n.qty, n.from, n.to)
	}
}

// --- what each call exercised ---------------------------------------------------

// closeFeatures names the pricing shapes a closer's call exercised, for the
// coverage report. Reads here are best effort: a failed read names nothing.
func closeFeatures(ctx context.Context, e *Engine, sub domain.Subscription, lines []domain.InvoiceLineItem, billedThrough *time.Time, u unpricedUsage, b netNegativeUsage) []string {
	feats := []string{"every call"}
	perMeterWindows := map[string]map[time.Time]struct{}{}
	for _, l := range lines {
		if l.LineType != domain.LineTypeUsage {
			continue
		}
		feats = append(feats, "usage priced", "mode "+l.PricingMode)
		if l.NominalUnitAmountDecimal != nil && !l.NominalUnitAmountDecimal.IsInteger() {
			feats = append(feats, "decimal rate")
		}
		if !l.QuantityDecimal.IsInteger() {
			feats = append(feats, "fractional quantity")
		}
		if billedThrough != nil && l.BillingPeriodStart != nil && l.BillingPeriodStart.Before(*billedThrough) {
			feats = append(feats, "full-window pass (deferred max/last)")
		}
		if billedThrough != nil && l.PricingMode == string(domain.PricingGraduated) {
			feats = append(feats, "graduated price after a threshold invoice (ladder)")
		}
		if l.BillingPeriodStart != nil {
			if perMeterWindows[l.MeterID] == nil {
				perMeterWindows[l.MeterID] = map[time.Time]struct{}{}
			}
			perMeterWindows[l.MeterID][*l.BillingPeriodStart] = struct{}{}
		}
		feats = append(feats, ruleFeatures(ctx, e, sub, l.MeterID, l.RatingRuleVersionID)...)
	}
	for _, ws := range perMeterWindows {
		if len(ws) > 1 {
			feats = append(feats, "one meter over several windows (mid-period change)")
		}
	}
	if billedThrough != nil {
		feats = append(feats, "after a threshold invoice")
	}
	if len(u) > 0 {
		feats = append(feats, "unpriced usage left out")
	}
	if len(b) > 0 {
		feats = append(feats, "usage below zero left out")
	}
	return append(feats, subFeatures(sub)...)
}

func previewFeatures(ctx context.Context, e *Engine, sub domain.Subscription, periodStart, periodEnd time.Time, res PreviewResult, asClose bool) []string {
	feats := []string{"every call"}
	for _, l := range res.Lines {
		if l.LineType != "usage" {
			continue
		}
		feats = append(feats, "usage priced", "mode "+l.PricingMode)
		if l.NominalUnitAmountDecimal != nil && !l.NominalUnitAmountDecimal.IsInteger() {
			feats = append(feats, "decimal rate")
		}
		if !l.Quantity.IsInteger() {
			feats = append(feats, "fractional quantity")
		}
		if nonAdditive(l.Aggregation) {
			feats = append(feats, "max/last bucket")
		}
		feats = append(feats, ruleFeatures(ctx, e, sub, l.MeterID, l.RatingRuleVersionID)...)
	}
	for _, lo := range res.leftOut {
		if lo.BelowZero {
			feats = append(feats, "usage below zero left out")
		} else {
			feats = append(feats, "unpriced usage left out")
		}
	}
	if asClose {
		if wm, err := e.loadThresholdWatermark(ctx, sub.TenantID, sub.ID, periodStart, periodEnd); err == nil && wm.exists() {
			feats = append(feats, "after a threshold invoice (ladder)")
		}
	}
	return append(feats, subFeatures(sub)...)
}

// ruleFeatures names how a priced line's meter is priced: by pricing rules
// (multi-dimension) or its own binding, and whether the customer has an
// override or the period resolved a version other than the line's pin.
func ruleFeatures(ctx context.Context, e *Engine, sub domain.Subscription, meterID, versionID string) []string {
	var feats []string
	if e.pricing == nil {
		return nil
	}
	if rules, err := e.pricing.ListMeterPricingRulesByMeter(ctx, sub.TenantID, meterID); err == nil {
		if len(rules) > 0 {
			feats = append(feats, "pricing rules (multi-dimension)")
		} else {
			feats = append(feats, "meter's own rating rule")
		}
	}
	if v, err := e.pricing.GetRatingRule(ctx, sub.TenantID, versionID); err == nil {
		if sub.CurrentBillingPeriodStart != nil {
			if _, oerr := e.pricing.GetOverrideByKeyAsOf(ctx, sub.TenantID, sub.CustomerID, v.RuleKey, *sub.CurrentBillingPeriodStart); oerr == nil {
				feats = append(feats, "customer price override")
			}
		}
		if v.Version > 1 {
			feats = append(feats, "rule version > 1 (price changed)")
		}
	}
	return feats
}

// subFeatures reads nothing: tests count some reads (settings) and fail the
// Nth, so the tally must not add one.
func subFeatures(sub domain.Subscription) []string {
	var feats []string
	if sub.TestClockID != "" {
		feats = append(feats, "test clock")
	}
	return feats
}

// ============================================================================
// Old rating loops: origin/main 672eb6d5, verbatim but for the names.
// ============================================================================

func (e *Engine) legacyBuildLineItems(ctx context.Context, sub domain.Subscription, now, periodStart, periodEnd time.Time, plans map[string]domain.Plan, invoiceCurrency string, billingLoc *time.Location, unpriced unpricedUsage, belowZero netNegativeUsage) ([]domain.InvoiceLineItem, int64, *time.Time, error) {
	var lineItems []domain.InvoiceLineItem
	subtotal := int64(0)

	// At cycle close, check whether the scheduled cancel will fire at
	// this boundary. If yes, in_advance base lines for the UPCOMING
	// period MUST be skipped — the sub is about to terminate, the
	// customer won't consume the upcoming period, and emitting the
	// line would overcharge by one full prepayment. The cycle-close
	// invoice still bills usage from the just-elapsed period
	// (in-arrears for usage is always correct) and any in_arrears
	// base for the just-elapsed period. commitPeriodClose then fires the
	// scheduled cancel in the same tx as this invoice (ADR-115).
	// (audit finding flagged during the 2026-05-18 cancel-flow walk
	// through; same bug class would re-appear if hard-pause is ever
	// re-added — paused subs at pause-activation should likewise
	// skip the next-period in_advance base line.)
	terminalCycleClose := shouldFireScheduledCancel(sub, periodEnd, now)

	// Threshold-fired invoice for THIS cycle: when fireThreshold ran
	// mid-cycle (the period keeps its anchor), that invoice already billed
	// (and typically charged) usage through its billing_period_end. It bills
	// no base fee: the close bills the period's base as if no fire happened
	// (a fire made before that change carried the full in_arrears base, and
	// wm.billedBase skips it here). Without this watermark the cycle close
	// re-billed the usage from periodStart — every threshold cycle
	// double-charged. Keyed on the invoice (ground truth), not the
	// mutable BillingThresholds config, so an operator removing
	// thresholds mid-cycle doesn't resurrect the double-bill.
	// errs.ErrNotFound = no threshold fire = zero behavior change; any
	// other error aborts this sub's cycle (billing blind here risks a
	// double charge — feedback_no_silent_fallbacks). The instant is
	// returned to the caller: commitPeriodClose re-reads it on the closer
	// tx and refuses to commit lines built against a different watermark
	// (ADR-115 — a fire landing between here and the commit).
	wm, err := e.loadThresholdWatermark(ctx, sub.TenantID, sub.ID, periodStart, periodEnd)
	if err != nil {
		return nil, 0, nil, err
	}
	thresholdBilledThrough := wm.billedThrough

	// ADR-101 segment source: billing_intervals — policy-applied item
	// lifetimes recorded at write time; the read is a dumb
	// interval×window intersection (Phase 4 removed the fact-log
	// interpretation). Drives segment-aware base-fee billing (Lago /
	// Chargebee / Orb shape): a mid-period plan or quantity change
	// bills one line per segment at the segment's own plan + qty
	// rate × duration fraction; no mid-period changes → one
	// full-period line.
	segsByItem, err := e.windowSegments(ctx, sub, periodStart, periodEnd)
	if err != nil {
		return nil, 0, nil, fmt.Errorf("resolve segments: %w", err)
	}
	// Hydrate any plans referenced only by an interval segment (items
	// removed mid-period, pre-swap plans not on current items). Plans
	// already loaded for current items are skipped.
	//
	// Failure propagates to the per-sub error handler. Pre-fix the
	// segment under a failed plan lookup would be silently dropped from
	// the invoice, undercharging the customer; per
	// feedback_no_silent_fallbacks the engine fails the sub's cycle
	// rather than guess.
	for _, segs := range segsByItem {
		for _, seg := range segs {
			if seg.planID == "" {
				continue
			}
			if _, ok := plans[seg.planID]; ok {
				continue
			}
			pl, err := e.pricing.GetPlan(ctx, sub.TenantID, seg.planID)
			if err != nil {
				return nil, 0, nil, fmt.Errorf("get segment plan %s: %w", seg.planID, err)
			}
			plans[seg.planID] = pl
		}
	}
	// Every meter the invoice bills: the meters of the current items' plans
	// and of the plans hydrated above. Segment-aware usage needs every meter
	// that was active at ANY point during the period: a meter that existed
	// on plan_A (now swapped out or removed) bills its [seg.start, seg.end)
	// window.
	meters, err := e.planMeters(ctx, sub.TenantID, plans)
	if err != nil {
		return nil, 0, nil, err
	}

	// Base-fee lines: segment-aware for in_arrears, one upcoming-period
	// line for in_advance. closeBaseFeeLines is shared with the operator
	// preview (previewCycle) so the estimate and the invoice cannot drift.
	baseLines, err := closeBaseFeeLines(sub, segsByItem, plans, periodStart, periodEnd, invoiceCurrency, billingLoc, terminalCycleClose, wm.billedBase())
	if err != nil {
		return nil, 0, nil, err
	}
	for _, bl := range baseLines {
		lineItems = append(lineItems, bl.line)
		subtotal += bl.line.AmountCents
	}

	// Segment-aware usage billing (Orb shape): each meter is billed
	// once per disjoint [start, end) range during which it was
	// active on the sub. For subs with no mid-period changes, every
	// meter has exactly one full-period interval — output collapses
	// to the pre-segment-aware single-line behavior. For subs where
	// a mid-period plan change adds/removes meters, each meter
	// bills only for the time it was actually on the sub.

	// The windows each meter is billed over, from item segments and
	// removed-item segments (meterUsageIntervals; shared with every other
	// closer and the live credit read so they cannot drift).
	floors, err := e.loadUsageFloors(ctx, sub, periodStart, periodEnd)
	if err != nil {
		return nil, 0, nil, err
	}
	meterIntervals, meterIntervalsFull := meterUsageIntervals(sub, segsByItem, plans, periodStart, periodEnd, thresholdBilledThrough, floors)

	// Usage is read once per window (one read covers every meter for a
	// window; a pricing-rule meter reads its buckets per window).
	reads := newWindowUsage(e, sub, meters)

	// Union of meters with billable windows. When a watermark exists, a
	// meter whose clamped intervals vanished entirely (all usage inside the
	// fire's window) can still owe a full-window bill for a deferred
	// non-additive bucket — so iterate the full map's keys too.
	meterIDSet := make(map[string]struct{}, len(meterIntervals))
	for mid := range meterIntervals {
		meterIDSet[mid] = struct{}{}
	}
	for mid := range meterIntervalsFull {
		meterIDSet[mid] = struct{}{}
	}
	// A meterWindow is one aggregation pass: the clamped pass
	// (nonAdditiveOnly=false) bills every bucket EXCEPT deferred
	// non-additive ones; the full pass (nonAdditiveOnly=true, only when a
	// watermark exists) bills ONLY deferred non-additive buckets over the
	// unclamped window.
	type meterWindow struct {
		iv              usageInterval
		nonAdditiveOnly bool
	}
	// One tier ladder per usage bucket for the whole close, continuing from
	// what a threshold fire already billed (P21, ADR-066 amendment).
	ladder := newLegacyUsageLadder(wm)
	for _, meterID := range slices.Sorted(maps.Keys(meterIDSet)) {
		meter, err := e.pricing.GetMeter(ctx, sub.TenantID, meterID)
		if err != nil {
			return nil, 0, nil, fmt.Errorf("get meter %s: %w", meterID, err)
		}

		// Does this meter price via dimension-match pricing rules (the
		// multi-dim shape: e.g. the AI `tokens` meter carries one rule per
		// {model, token_type})? If so we MUST use the same
		// AggregateClaims + per-bucket pricing path that preview and
		// the threshold scan use — a single meter-linked rating rule can't
		// express N rates, so the legacy single-rule branch below would skip
		// the meter and emit a $0 invoice (the wedge breakage, ADR-044).
		// rateMeterWindow is the mirror of this loop: same path → preview ==
		// invoice. Meters with no pricing rules keep the single-rule path.
		pricingRules, err := e.pricing.ListMeterPricingRulesByMeter(ctx, sub.TenantID, meterID)
		if err != nil {
			return nil, 0, nil, fmt.Errorf("list pricing rules for meter %s: %w", meterID, err)
		}
		rulesByID := make(map[string]domain.MeterPricingRule, len(pricingRules))
		for _, r := range pricingRules {
			rulesByID[r.ID] = r
		}
		if err := e.legacyCheckLadderSeeds(ctx, sub, meterID, pricingRules, periodStart, ladder); err != nil {
			return nil, 0, nil, err
		}

		windows := make([]meterWindow, 0, 4)
		for _, iv := range mergeUsageIntervals(meterIntervals[meterID]) {
			windows = append(windows, meterWindow{iv: iv})
		}
		if thresholdBilledThrough != nil {
			for _, iv := range mergeUsageIntervals(meterIntervalsFull[meterID]) {
				windows = append(windows, meterWindow{iv: iv, nonAdditiveOnly: true})
			}
		}
		for _, w := range windows {
			iv := w.iv
			ivStart := iv.start
			ivEnd := iv.end

			// ---- Multi-dim path: one billable bucket per claimed rule. ----
			if len(pricingRules) > 0 {
				aggs, err := reads.ruleBuckets(ctx, meterID, meter, pricingRules, iv)
				if err != nil {
					return nil, 0, nil, err
				}
				for _, agg := range aggs {
					qty := agg.Quantity
					if qty.IsZero() {
						continue
					}
					ratingRuleID := agg.RatingRuleVersionID
					if ratingRuleID == "" {
						ratingRuleID = meter.RatingRuleVersionID
					}
					// ADR-066 §4 pass routing, per RULE BUCKET: a deferred
					// non-additive bucket (max/last with NO line on the
					// watermark fire) bills ONLY on the full-window pass —
					// billing it from the clamped window drops pre-fire peaks,
					// and billing it on both passes double-bills. Every other
					// bucket (additive, or non-additive that already rode the
					// fire — the accepted partial-window degradation) bills
					// ONLY on the clamped pass. No watermark ⇒ deferred is
					// always false and no full pass exists: unchanged.
					// It runs before the unpriced check below so an unpriced
					// bucket is reported on the one pass that would have
					// billed it: the full pass re-reads the whole period, and
					// reporting there too counted the pre-fire usage that the
					// threshold invoice already reported (#981).
					if wm.deferredBucket(meter.Aggregation, meterID, ratingRuleID) != w.nonAdditiveOnly {
						continue
					}
					// Corrections larger than the bucket's usage in this
					// window: it bills 0 and the invoice is still built
					// (#1193). After the pass routing, so it is reported on
					// one pass only; before the unpriced check, as preview.
					if qty.IsNegative() {
						slog.Warn("cycle: usage below zero after corrections — billed as 0",
							"meter", meter.EventName, "quantity", qty.String(),
							"subscription_id", sub.ID)
						belowZero.add(meterID, meter.EventName, meter.Name, qty, ivStart, ivEnd)
						continue
					}
					if ratingRuleID == "" {
						// Usage that matched no rule and the meter has no
						// default binding — bill nothing, but loudly (a
						// silent skip would hide unbilled revenue).
						slog.Warn("cycle: usage matched no rating rule and meter has no default binding — not billed",
							"meter", meter.EventName, "quantity", qty.String(),
							"subscription_id", sub.ID)
						unpriced.add(meterID, qty)
						continue
					}
					rule, err := e.legacyResolveRatedRule(ctx, sub.TenantID, sub.CustomerID, ratingRuleID, periodStart)
					if err != nil {
						return nil, 0, nil, fmt.Errorf("resolve pricing for meter %s rule %s: %w", meterID, ratingRuleID, err)
					}
					amount, err := ladder.price(rule, meterID, meterID+"|"+agg.RuleID, meter.Aggregation, qty)
					if err != nil {
						return nil, 0, nil, fmt.Errorf("compute amount for meter %s rule %s: %w", meterID, ratingRuleID, err)
					}
					unitAmount := decimal.NewFromInt(amount).Div(qty).RoundBank(0).IntPart()
					lineItems = append(lineItems, domain.InvoiceLineItem{
						LineType:                 domain.LineTypeUsage,
						MeterID:                  meterID,
						MeterUnit:                meter.Unit,
						Description:              usageLineDescription(meter, rulesByID[agg.RuleID]),
						Quantity:                 qty.IntPart(),
						QuantityDecimal:          qty,
						UnitAmountCents:          unitAmount,
						AmountCents:              amount,
						TotalAmountCents:         amount,
						Currency:                 invoiceCurrency,
						PricingMode:              string(rule.Mode),
						RatingRuleVersionID:      rule.ID,
						NominalUnitAmountDecimal: nominalRate(rule),
						BillingPeriodStart:       &ivStart,
						BillingPeriodEnd:         &ivEnd,
					})
					subtotal += amount
				}
				continue
			}

			// ---- Single-rule path (pre-multi-dim meters). ----
			// Same ADR-066 §4 pass routing as the multi-dim branch, at meter
			// granularity (a single-rule meter has exactly one bucket, whose
			// mode is the meter's own aggregation).
			if wm.deferredBucket(meter.Aggregation, meterID, meter.RatingRuleVersionID) != w.nonAdditiveOnly {
				continue
			}
			quantity, err := reads.meterTotal(ctx, meterID, iv)
			if err != nil {
				return nil, 0, nil, err
			}
			if quantity.IsZero() {
				continue
			}
			// Below zero after corrections: bills 0, the invoice is still
			// built (#1193). Same rule and order as the multi-dim branch.
			if quantity.IsNegative() {
				slog.Warn("cycle: usage below zero after corrections — billed as 0",
					"meter", meter.EventName, "quantity", quantity.String(),
					"subscription_id", sub.ID)
				belowZero.add(meterID, meter.EventName, meter.Name, quantity, ivStart, ivEnd)
				continue
			}

			if meter.RatingRuleVersionID == "" {
				// Unpriced meter → its usage can't be rated. Never silent
				// (ADR-096): the plan-attach guard blocks this at authoring,
				// so reaching it means a rule was unbound out from under a
				// live sub. Loud log, no revenue billed — matches the
				// multi-dim sibling above.
				slog.Warn("cycle: usage on a meter with no rating rule — not billed (unpriced meter on plan; ADR-096)",
					"meter", meter.EventName, "quantity", quantity.String(), "subscription_id", sub.ID)
				unpriced.add(meterID, quantity)
				continue
			}
			rule, err := e.legacyResolveRatedRule(ctx, sub.TenantID, sub.CustomerID, meter.RatingRuleVersionID, periodStart)
			if err != nil {
				return nil, 0, nil, fmt.Errorf("resolve pricing for meter %s: %w", meterID, err)
			}
			amount, err := ladder.price(rule, meterID, meterID+"|", meter.Aggregation, quantity)
			if err != nil {
				return nil, 0, nil, fmt.Errorf("compute amount for meter %s: %w", meterID, err)
			}
			unitAmount := decimal.NewFromInt(amount).Div(quantity).RoundBank(0).IntPart()

			lineItems = append(lineItems, domain.InvoiceLineItem{
				LineType:                 domain.LineTypeUsage,
				MeterID:                  meterID,
				MeterUnit:                meter.Unit,
				Description:              fmt.Sprintf("%s (%s)", meter.Name, meter.Unit),
				Quantity:                 quantity.IntPart(),
				QuantityDecimal:          quantity,
				UnitAmountCents:          unitAmount,
				AmountCents:              amount,
				TotalAmountCents:         amount,
				Currency:                 invoiceCurrency,
				PricingMode:              string(rule.Mode),
				RatingRuleVersionID:      rule.ID,
				NominalUnitAmountDecimal: nominalRate(rule),
				BillingPeriodStart:       &ivStart,
				BillingPeriodEnd:         &ivEnd,
			})
			subtotal += amount
		}
	}

	return lineItems, subtotal, thresholdBilledThrough, nil
}

func (e *Engine) legacyResolveRatedRule(ctx context.Context, tenantID, customerID, pinnedRuleVersionID string, periodStart time.Time) (domain.RatingRuleVersion, error) {
	linked, err := e.pricing.GetRatingRule(ctx, tenantID, pinnedRuleVersionID)
	if err != nil {
		return domain.RatingRuleVersion{}, fmt.Errorf("get rating rule %s: %w", pinnedRuleVersionID, err)
	}
	rule, err := e.pricing.GetRuleByKeyAsOf(ctx, tenantID, linked.RuleKey, periodStart)
	if err != nil {
		if !errors.Is(err, errs.ErrNotFound) {
			return domain.RatingRuleVersion{}, fmt.Errorf("resolve rule %q as of %s: %w", linked.RuleKey, periodStart.Format(time.RFC3339), err)
		}
		// Key has no ACTIVE versions (all archived) but a binding still
		// points at one — rate at the pinned version rather than
		// refusing to bill usage the meter admitted.
		rule = linked
	}
	override, err := e.pricing.GetOverrideByKeyAsOf(ctx, tenantID, customerID, rule.RuleKey, periodStart)
	if err == nil {
		return override.ApplyTo(rule), nil
	}
	if errors.Is(err, errs.ErrNotFound) {
		return rule, nil
	}
	return domain.RatingRuleVersion{}, fmt.Errorf("resolve override for rule %q: %w", rule.RuleKey, err)
}

func (e *Engine) legacyBuildCancelLineItems(ctx context.Context, sub domain.Subscription, wm thresholdWatermark, plans map[string]domain.Plan, invoiceCurrency string, periodStart, canceledAt time.Time, billingLoc *time.Location, unpriced unpricedUsage, belowZero netNegativeUsage, usageSuffix string) ([]domain.InvoiceLineItem, int64, error) {
	// Build base lines: segment-aware in_arrears billing over the
	// partial period [periodStart, canceledAt]. in_advance items are
	// explicitly skipped — their base for the just-canceled period
	// was already paid at period start (BillOnCreate or prior cycle),
	// and the unused portion is relieved by BillOnCancelDraftsTx.
	//
	// Segment-aware: if the customer changed plan or quantity (or
	// added/removed an item) between periodStart and canceledAt, each
	// segment is billed at its own rate × (segment_days / cycle_days).
	// No mid-period changes → single segment from periodStart to
	// canceledAt, matching the pre-segment-aware single-line behavior.
	//
	// Fail-loud parity with buildLineItems (2026-07-10 design review):
	// pre-fix the error was swallowed (`_ =`), so a transient DB failure
	// here silently billed a changed sub's FINAL invoice as a single
	// full-window line at the current plan's rate — mis-billing with no
	// operator signal. Per feedback_no_silent_fallbacks the cancel bill
	// fails instead; the operator's cancel retries.
	// ADR-101 segment source for the cancel window — same seam as the
	// cycle path, window-ended at the cancel instant. The interval read
	// sees pre-cancel state (the close-all seal rides the cancel tx,
	// invisible to this read), which clips identically to
	// [periodStart, canceledAt] — open intervals end at the window edge
	// either way.
	segsByItem, err := e.windowSegments(ctx, sub, periodStart, canceledAt)
	if err != nil {
		return nil, 0, fmt.Errorf("resolve segments on cancel: %w", err)
	}
	for _, segs := range segsByItem {
		for _, seg := range segs {
			if seg.planID == "" {
				continue
			}
			if _, ok := plans[seg.planID]; ok {
				continue
			}
			pl, err := e.pricing.GetPlan(ctx, sub.TenantID, seg.planID)
			if err != nil {
				return nil, 0, fmt.Errorf("get segment plan %s on cancel: %w", seg.planID, err)
			}
			plans[seg.planID] = pl
		}
	}
	itemsByID := map[string]domain.SubscriptionItem{}
	for _, it := range sub.Items {
		itemsByID[it.ID] = it
	}

	lineItems := make([]domain.InvoiceLineItem, 0, len(sub.Items))
	subtotal := int64(0)
	periodDays := roundDays(canceledAt.Sub(periodStart))

	// The in_arrears base is billed here, prorated to the cancel, whether or
	// not a threshold fired: a fire bills usage only. A fire made before that
	// change carried the full base (wm.billedBase), so it is skipped then —
	// the same skip billOnePeriod applies.
	if !wm.billedBase() {
		// Pass 1: items currently on the sub at cancel time.
		for _, it := range sub.Items {
			plan := plans[it.PlanID]
			if plan.BaseBillTiming == domain.BillInAdvance {
				continue
			}
			segments := segsByItem[it.ID]
			for _, seg := range segments {
				segPlan, ok := plans[seg.planID]
				if !ok || segPlan.BaseAmountCents <= 0 || segPlan.BaseBillTiming == domain.BillInAdvance {
					continue
				}
				if err := emitBaseSegmentLine(seg, segPlan, periodStart, periodDays, invoiceCurrency, billingLoc, sub.BillingTime, &lineItems, &subtotal); err != nil {
					return nil, 0, err
				}
			}
		}

		// Pass 2: items removed between periodStart and canceledAt (in the
		// segment map but not on sub.Items now). Bill their pre-remove
		// segments at the respective plans' rates.
		for itemID, segments := range segsByItem {
			if _, stillPresent := itemsByID[itemID]; stillPresent {
				continue
			}
			for _, seg := range segments {
				segPlan, ok := plans[seg.planID]
				if !ok || segPlan.BaseAmountCents <= 0 || segPlan.BaseBillTiming == domain.BillInAdvance {
					continue
				}
				if err := emitBaseSegmentLine(seg, segPlan, periodStart, periodDays, invoiceCurrency, billingLoc, sub.BillingTime, &lineItems, &subtotal); err != nil {
					return nil, 0, err
				}
			}
		}
	}

	// Build usage lines: aggregate every meter referenced by every
	// plan that was active at any point in [periodStart, canceledAt].
	// Same aggregation surface the cycle path uses; only the period
	// boundaries differ. Iterates the full `plans` map (current items'
	// plans + hydrated change-log plans) so segment-aware billing
	// picks up meters that existed on now-removed plans.
	meters, err := e.planMeters(ctx, sub.TenantID, plans)
	if err != nil {
		return nil, 0, fmt.Errorf("cancel: %w", err)
	}

	// Segment-aware usage billing over the partial period
	// [periodStart, canceledAt]: the period close's own window builder
	// (meterUsageIntervals), ended at the cancel instant, with the dual
	// clamped/full maps of the threshold watermark protocol (ADR-066 §4) and
	// the usage floors.
	floors, err := e.loadUsageFloors(ctx, sub, periodStart, canceledAt)
	if err != nil {
		return nil, 0, err
	}
	meterIntervals, meterIntervalsFull := meterUsageIntervals(sub, segsByItem, plans, periodStart, canceledAt, wm.billedThrough, floors)

	// The lines read usage through reads, once per window.
	reads := newWindowUsage(e, sub, meters)

	// Union of meters with billable windows (a meter fully clamped away can
	// still owe a full-window bill for a deferred non-additive bucket) +
	// the clamped/full pass work-list — same shape as billOnePeriod.
	cancelMeterIDSet := make(map[string]struct{}, len(meterIntervals))
	for mid := range meterIntervals {
		cancelMeterIDSet[mid] = struct{}{}
	}
	for mid := range meterIntervalsFull {
		cancelMeterIDSet[mid] = struct{}{}
	}
	type meterWindow struct {
		iv              usageInterval
		nonAdditiveOnly bool
	}
	// Same per-bucket tier ladder as the cycle close (P21, ADR-066 amendment).
	ladder := newLegacyUsageLadder(wm)
	for _, meterID := range slices.Sorted(maps.Keys(cancelMeterIDSet)) {
		// Does this meter price via dimension-match pricing rules (the
		// multi-dim shape: e.g. the AI `tokens` meter, one rule per
		// {model, token_type})? Same fork billOnePeriod / preview / the
		// threshold scan take — a single meter-linked rating rule can't
		// express N rates, so for these meters RatingRuleVersionID is
		// empty and the single-rule branch below would silently skip
		// them. Pre-fix this fork was MISSING here: a mid-period
		// immediate cancel emitted $0 for all token usage (the ADR-044
		// wedge breakage, on the cancel path).
		pricingRules, err := e.pricing.ListMeterPricingRulesByMeter(ctx, sub.TenantID, meterID)
		if err != nil {
			return nil, 0, fmt.Errorf("list pricing rules for meter %s on cancel: %w", meterID, err)
		}
		if err := e.legacyCheckLadderSeeds(ctx, sub, meterID, pricingRules, periodStart, ladder); err != nil {
			return nil, 0, err
		}

		windows := make([]meterWindow, 0, 4)
		for _, iv := range mergeUsageIntervals(meterIntervals[meterID]) {
			windows = append(windows, meterWindow{iv: iv})
		}
		if wm.exists() {
			for _, iv := range mergeUsageIntervals(meterIntervalsFull[meterID]) {
				windows = append(windows, meterWindow{iv: iv, nonAdditiveOnly: true})
			}
		}

		// ---- Multi-dim path: one billable bucket per claimed rule. ----
		if len(pricingRules) > 0 {
			meter, err := e.pricing.GetMeter(ctx, sub.TenantID, meterID)
			if err != nil {
				return nil, 0, fmt.Errorf("get meter %s on cancel: %w", meterID, err)
			}
			rulesByID := make(map[string]domain.MeterPricingRule, len(pricingRules))
			for _, r := range pricingRules {
				rulesByID[r.ID] = r
			}
			for _, mw := range windows {
				iv := mw.iv
				ivStart := iv.start
				ivEnd := iv.end
				aggs, err := reads.ruleBuckets(ctx, meterID, meter, pricingRules, iv)
				if err != nil {
					return nil, 0, fmt.Errorf("cancel: %w", err)
				}
				for _, agg := range aggs {
					qty := agg.Quantity
					if qty.IsZero() {
						continue
					}
					ratingRuleID := agg.RatingRuleVersionID
					if ratingRuleID == "" {
						ratingRuleID = meter.RatingRuleVersionID
					}
					// ADR-066 §4 pass routing — see billOnePeriod. Before the
					// unpriced check, as there, so an unpriced bucket is
					// reported on one pass only.
					if wm.deferredBucket(meter.Aggregation, meterID, ratingRuleID) != mw.nonAdditiveOnly {
						continue
					}
					// Below zero after corrections: bills 0 (#1193) — see
					// billOnePeriod's multi-dim branch.
					if qty.IsNegative() {
						slog.Warn("cancel: usage below zero after corrections — billed as 0",
							"meter", meter.EventName, "quantity", qty.String(),
							"subscription_id", sub.ID)
						belowZero.add(meterID, meter.EventName, meter.Name, qty, ivStart, ivEnd)
						continue
					}
					if ratingRuleID == "" {
						// Usage that matched no rule and the meter has no
						// default binding — bill nothing, but loudly (a
						// silent skip would hide unbilled revenue).
						slog.Warn("cancel: usage matched no rating rule and meter has no default binding — not billed",
							"meter", meter.EventName, "quantity", qty.String(),
							"subscription_id", sub.ID)
						unpriced.add(meterID, qty)
						continue
					}
					rule, err := e.legacyResolveRatedRule(ctx, sub.TenantID, sub.CustomerID, ratingRuleID, periodStart)
					if err != nil {
						return nil, 0, fmt.Errorf("resolve pricing for meter %s rule %s on cancel: %w", meterID, ratingRuleID, err)
					}
					amount, err := ladder.price(rule, meterID, meterID+"|"+agg.RuleID, meter.Aggregation, qty)
					if err != nil {
						return nil, 0, fmt.Errorf("compute amount for meter %s rule %s on cancel: %w", meterID, ratingRuleID, err)
					}
					unitAmount := decimal.NewFromInt(amount).Div(qty).RoundBank(0).IntPart()
					lineItems = append(lineItems, domain.InvoiceLineItem{
						LineType:                 domain.LineTypeUsage,
						MeterID:                  meterID,
						MeterUnit:                meter.Unit,
						Description:              usageLineDescription(meter, rulesByID[agg.RuleID]) + usageSuffix,
						Quantity:                 qty.IntPart(),
						QuantityDecimal:          qty,
						UnitAmountCents:          unitAmount,
						AmountCents:              amount,
						TotalAmountCents:         amount,
						Currency:                 invoiceCurrency,
						PricingMode:              string(rule.Mode),
						RatingRuleVersionID:      rule.ID,
						NominalUnitAmountDecimal: nominalRate(rule),
						BillingPeriodStart:       &ivStart,
						BillingPeriodEnd:         &ivEnd,
					})
					subtotal += amount
				}
			}
			continue
		}

		// ---- Single-rule path (pre-multi-dim meters). ----
		for _, mw := range windows {
			iv := mw.iv
			meter, err := e.pricing.GetMeter(ctx, sub.TenantID, meterID)
			if err != nil {
				return nil, 0, fmt.Errorf("get meter %s on cancel: %w", meterID, err)
			}
			// ADR-066 §4 pass routing at meter granularity — see billOnePeriod.
			if wm.deferredBucket(meter.Aggregation, meterID, meter.RatingRuleVersionID) != mw.nonAdditiveOnly {
				continue
			}
			quantity, err := reads.meterTotal(ctx, meterID, iv)
			if err != nil {
				return nil, 0, fmt.Errorf("cancel: %w", err)
			}
			if quantity.IsZero() {
				continue
			}
			// Below zero after corrections: bills 0 (#1193).
			if quantity.IsNegative() {
				slog.Warn("cancel: usage below zero after corrections — billed as 0",
					"meter", meter.EventName, "quantity", quantity.String(), "subscription_id", sub.ID)
				belowZero.add(meterID, meter.EventName, meter.Name, quantity, iv.start, iv.end)
				continue
			}
			if meter.RatingRuleVersionID == "" {
				// Unpriced meter → its usage can't be rated. Never silent
				// (ADR-096); see the cycle-path sibling. Loud log, no revenue
				// billed.
				slog.Warn("cancel: usage on a meter with no rating rule — not billed (unpriced meter on plan; ADR-096)",
					"meter", meter.EventName, "quantity", quantity.String(), "subscription_id", sub.ID)
				unpriced.add(meterID, quantity)
				continue
			}
			rule, err := e.legacyResolveRatedRule(ctx, sub.TenantID, sub.CustomerID, meter.RatingRuleVersionID, periodStart)
			if err != nil {
				return nil, 0, fmt.Errorf("resolve pricing for meter %s on cancel: %w", meterID, err)
			}
			amount, err := ladder.price(rule, meterID, meterID+"|", meter.Aggregation, quantity)
			if err != nil {
				return nil, 0, fmt.Errorf("compute amount for meter %s on cancel: %w", meterID, err)
			}
			unitAmount := decimal.NewFromInt(amount).Div(quantity).RoundBank(0).IntPart()
			ivStart := iv.start
			ivEnd := iv.end
			lineItems = append(lineItems, domain.InvoiceLineItem{
				LineType:                 domain.LineTypeUsage,
				MeterID:                  meterID,
				MeterUnit:                meter.Unit,
				Description:              fmt.Sprintf("%s (%s)", meter.Name, meter.Unit) + usageSuffix,
				Quantity:                 quantity.IntPart(),
				QuantityDecimal:          quantity,
				UnitAmountCents:          unitAmount,
				AmountCents:              amount,
				TotalAmountCents:         amount,
				Currency:                 invoiceCurrency,
				PricingMode:              string(rule.Mode),
				RatingRuleVersionID:      rule.ID,
				NominalUnitAmountDecimal: nominalRate(rule),
				BillingPeriodStart:       &ivStart,
				BillingPeriodEnd:         &ivEnd,
			})
			subtotal += amount
		}
	}
	return lineItems, subtotal, nil
}

func (e *Engine) legacyPreviewWindow(ctx context.Context, sub domain.Subscription, periodStart, periodEnd time.Time, asClose bool) (PreviewResult, error) {
	plans := make(map[string]domain.Plan, len(sub.Items))
	planNames := make([]string, 0, len(sub.Items))
	var allMeterIDs []string
	seenMeter := make(map[string]struct{})
	for _, it := range sub.Items {
		if _, ok := plans[it.PlanID]; ok {
			continue
		}
		pl, err := e.pricing.GetPlan(ctx, sub.TenantID, it.PlanID)
		if err != nil {
			return PreviewResult{}, fmt.Errorf("get plan %s: %w", it.PlanID, err)
		}
		plans[it.PlanID] = pl
		planNames = append(planNames, pl.Name)
		for _, mid := range pl.MeterIDs {
			if _, ok := seenMeter[mid]; ok {
				continue
			}
			seenMeter[mid] = struct{}{}
			allMeterIDs = append(allMeterIDs, mid)
		}
	}

	// ADR-031: when ANY item is in_advance, the cycle invoice's
	// HEADER period shifts to the upcoming period (engine.billOnePeriod
	// does the same). Preview must mirror this so the operator's
	// "what does the next invoice look like" answer matches what
	// actually lands. Totals are unchanged either way — only the
	// period label differs.
	invoiceStart, invoiceEnd := periodStart, periodEnd
	for _, it := range sub.Items {
		if plans[it.PlanID].BaseBillTiming == domain.BillInAdvance {
			loc, err := e.tenantLocation(ctx, sub.TenantID)
			if err != nil {
				return PreviewResult{}, err
			}
			invoiceStart = periodEnd
			invoiceEnd = domain.NextBillingPeriodEnd(periodEnd, sub.BillingTime, plans[it.PlanID].BillingInterval, loc, sub.BillingAnchorDay)
			break
		}
	}

	result := PreviewResult{
		CustomerID:         sub.CustomerID,
		SubscriptionID:     sub.ID,
		PlanName:           strings.Join(planNames, ", "),
		BillingPeriodStart: invoiceStart,
		BillingPeriodEnd:   invoiceEnd,
		Lines:              []PreviewLine{},
		Warnings:           []string{},
		GeneratedAt:        e.clock.Now(ctx),
	}

	if asClose {
		return e.legacyPreviewAsClose(ctx, sub, plans, allMeterIDs, periodStart, periodEnd, result)
	}

	// Base-fee lines from each subscription item's plan. Quantity here is
	// the item's seat / count (always integer); we still expose it as a
	// decimal for shape uniformity.
	//
	// in_advance items bill the FULL upcoming base (no proration —
	// preview at cycle close, partial-period creation already settled
	// by BillOnCreate). in_arrears items bill the period being previewed.
	for _, it := range sub.Items {
		plan := plans[it.PlanID]
		if plan.BaseAmountCents <= 0 {
			continue
		}
		baseFee := plan.BaseAmountCents * it.Quantity
		desc := fmt.Sprintf("%s - base fee (qty %d)", plan.Name, it.Quantity)
		if plan.BaseBillTiming == domain.BillInAdvance {
			desc = fmt.Sprintf("%s - base fee (qty %d, in advance for upcoming period)", plan.Name, it.Quantity)
		}
		result.Lines = append(result.Lines, PreviewLine{
			LineType:        "base_fee",
			Description:     desc,
			Currency:        plan.Currency,
			Quantity:        decimal.NewFromInt(it.Quantity),
			UnitAmountCents: plan.BaseAmountCents,
			AmountCents:     baseFee,
			PlanID:          plan.ID,
			BaseBillTiming:  plan.BaseBillTiming,
		})
	}

	// Usage lines per (meter, rule). The priority+claim LATERAL JOIN inside
	// AggregateClaims is the canonical pricing identity — the
	// cycle scan calls the same path, so a multi-dim tenant's preview
	// matches what its actual invoice will be.
	//
	// A rateMeterWindow ERROR aborts the whole preview (ADR-070,
	// no-silent-fallbacks). Only a meter with no rule binding at all and a
	// net-negative bucket (usage corrections) are soft cases, surfaced as
	// warnings from INSIDE rateMeterWindow. A missing pinned rule, a rating
	// failure and store or resolution errors all reach this err, and a
	// partial preview built from those is a lie — worse, the threshold
	// scan PERSISTS these lines as a fire invoice, and a silently
	// dropped meter's pre-fire usage would then be billed by nobody:
	// the cycle close clamps every meter's window to the fire
	// watermark, deferral only exists for non-additive buckets. The
	// pre-ADR-070 warning-degradation here defeated rateMeterWindow's own
	// fail-loud contract for exactly the money path it matters on.
	//
	// Each meter's window opens at its usage floor (trial end, another
	// subscription's coverage end; usageFloors), the same start the closers
	// use, while rules stay pinned at the period start (ADR-070). The window
	// is flat, one start per meter: a meter that left this subscription,
	// covered another one and came back opens after that other coverage, so
	// the stretch before it left is not rated here (an accepted residual
	// edge, ADR-012 amendment 2026-10-22).
	floors, err := e.loadUsageFloors(ctx, sub, periodStart, periodEnd)
	if err != nil {
		return PreviewResult{}, err
	}
	result.floors = floors
	for _, meterID := range allMeterIDs {
		from := floors.start(meterID, periodStart, periodEnd)
		if !from.Before(periodEnd) {
			continue
		}
		meterLines, warnings, leftOut, err := e.legacyRateMeterWindow(ctx, sub.TenantID, sub.CustomerID, meterID, from, periodEnd, periodStart, nil)
		if err != nil {
			return PreviewResult{}, fmt.Errorf("preview meter %q: %w", meterID, err)
		}
		result.Lines = append(result.Lines, meterLines...)
		result.Warnings = append(result.Warnings, warnings...)
		result.leftOut = append(result.leftOut, leftOut...)
	}

	// Estimate-scope warning (ADR-045): create_preview is a full-period
	// estimate that does NOT replicate the cycle's mid-period segment
	// proration. Rather than misstating a subscription that changed
	// mid-period, it says so on the warnings channel.
	//
	// Segment proration: a mid-LIFE change within [periodStart, periodEnd]
	// makes the cycle bill base fees and usage per segment (each at its own
	// rate × duration), which the full-period estimate flattens. Only plan
	// swaps, quantity changes, and item removals qualify — the 'add' rows
	// include every item's INITIAL creation (emitted by the subscription_items
	// insert trigger, migration 0029), which is not a mid-period change and
	// would false-positive the warning on every sub in its first period. Since
	// the ADR-012 amendment (2026-07-26) the interval writers agree:
	// a period-start-day 'add' opens its interval at periodStart (write-time
	// day-grade, ADR-101), so a first period bills the full base and this filter matches the money. Same
	// [periodStart, periodEnd] window the cycle's segment-aware billing keys
	// off. e.subs is always wired in production and the create_preview path;
	// the nil-guard only spares narrow subs-less unit tests (the warning is
	// advisory, never a number).
	if e.subs != nil {
		itemChanges, err := e.subs.ListItemChangesInPeriod(ctx, sub.TenantID, sub.ID, periodStart, periodEnd)
		if err != nil {
			return PreviewResult{}, fmt.Errorf("preview: list item changes: %w", err)
		}
		midLifeChange := false
		for _, c := range itemChanges {
			if c.ChangeType != "add" { // plan | quantity | remove
				midLifeChange = true
				break
			}
		}
		if midLifeChange {
			result.Warnings = append(result.Warnings,
				"Estimate excludes mid-period proration; the plan, quantity, or item set changed during this period, so the actual invoice may rate base fees and usage per segment.")
		}
	}

	if result.Totals, err = computePreviewTotals(result.Lines); err != nil {
		return PreviewResult{}, err
	}
	return result, nil
}

func (e *Engine) legacyPreviewAsClose(ctx context.Context, sub domain.Subscription, plans map[string]domain.Plan, meterOrder []string, periodStart, periodEnd time.Time, result PreviewResult) (PreviewResult, error) {
	loc, err := e.tenantLocation(ctx, sub.TenantID)
	if err != nil {
		return PreviewResult{}, err
	}
	currency, err := e.invoiceCurrency(ctx, sub, plans[sub.Items[0].PlanID].Currency)
	if err != nil {
		return PreviewResult{}, err
	}
	if sub.Status == domain.SubscriptionTrialing && trialCancelsFree(sub) {
		// handleTrialState cancels free at trial end: no invoice.
		result.Warnings = append(result.Warnings, "A cancel is scheduled for the end of the trial, so no invoice will be issued.")
		if result.Totals, err = computePreviewTotals(result.Lines); err != nil {
			return PreviewResult{}, err
		}
		return result, nil
	}
	if sub.Status == domain.SubscriptionTrialing {
		if res, ok, err := e.previewTrialEnd(ctx, sub, plans, meterOrder, periodStart, periodEnd, loc, result); ok || err != nil {
			return res, err
		}
	}
	wm, err := e.loadThresholdWatermark(ctx, sub.TenantID, sub.ID, periodStart, periodEnd)
	if err != nil {
		return PreviewResult{}, err
	}
	var segsByItem map[string][]baseSegment
	if sub.Status == domain.SubscriptionTrialing {
		segsByItem = trialConversionSegments(sub, periodStart, periodEnd)
	} else {
		segsByItem, err = e.windowSegments(ctx, sub, periodStart, periodEnd)
		if err != nil {
			return PreviewResult{}, fmt.Errorf("preview: resolve segments: %w", err)
		}
	}
	// Plans referenced only by a segment (a removed item, a pre-swap plan)
	// bill their segment too — same hydration as buildLineItems.
	for _, segs := range segsByItem {
		for _, seg := range segs {
			if seg.planID == "" {
				continue
			}
			if _, ok := plans[seg.planID]; ok {
				continue
			}
			pl, err := e.pricing.GetPlan(ctx, sub.TenantID, seg.planID)
			if err != nil {
				return PreviewResult{}, fmt.Errorf("get segment plan %s: %w", seg.planID, err)
			}
			plans[seg.planID] = pl
			result.PlanName += ", " + pl.Name
		}
	}

	// The close runs at periodEnd; a scheduled cancel firing there skips
	// the upcoming in_advance base.
	terminal := shouldFireScheduledCancel(sub, periodEnd, periodEnd)
	baseLines, err := closeBaseFeeLines(sub, segsByItem, plans, periodStart, periodEnd, currency, loc, terminal, wm.billedBase())
	if err != nil {
		return PreviewResult{}, err
	}
	for _, bl := range baseLines {
		result.Lines = append(result.Lines, PreviewLine{
			LineType:        "base_fee",
			Description:     bl.line.Description,
			Currency:        bl.line.Currency,
			Quantity:        decimal.NewFromInt(bl.line.Quantity),
			UnitAmountCents: bl.line.UnitAmountCents,
			AmountCents:     bl.line.AmountCents,
			PlanID:          bl.plan.ID,
			BaseBillTiming:  bl.plan.BaseBillTiming,
		})
	}

	// Usage: each meter over the windows a plan carrying it was on the
	// subscription, from its usage floor, clamped to the threshold watermark
	// — the close's meterUsageIntervals. Meters only a segment plan carries come after the
	// current plans' meters.
	order := append([]string(nil), meterOrder...)
	seen := make(map[string]struct{}, len(order))
	for _, mid := range order {
		seen[mid] = struct{}{}
	}
	for _, planID := range slices.Sorted(maps.Keys(plans)) {
		pl := plans[planID]
		for _, mid := range pl.MeterIDs {
			if _, ok := seen[mid]; !ok {
				seen[mid] = struct{}{}
				order = append(order, mid)
			}
		}
	}
	floors, err := e.loadUsageFloors(ctx, sub, periodStart, periodEnd)
	if err != nil {
		return PreviewResult{}, err
	}
	meterIntervals, _ := meterUsageIntervals(sub, segsByItem, plans, periodStart, periodEnd, wm.billedThrough, floors)
	// The tier ladder continues from a threshold fire, as in the close.
	ladder := newLegacyUsageLadder(wm)
	for _, meterID := range order {
		price := func(rule domain.RatingRuleVersion, bucketKey string, aggregation string, qty decimal.Decimal) (int64, error) {
			return ladder.price(rule, meterID, bucketKey, aggregation, qty)
		}
		for _, iv := range mergeUsageIntervals(meterIntervals[meterID]) {
			lines, warnings, leftOut, err := e.legacyRateMeterWindow(ctx, sub.TenantID, sub.CustomerID, meterID, iv.start, iv.end, periodStart, price)
			if err != nil {
				return PreviewResult{}, fmt.Errorf("preview meter %q: %w", meterID, err)
			}
			for i := range lines {
				lines[i].Currency = currency
			}
			result.Lines = append(result.Lines, lines...)
			result.Warnings = append(result.Warnings, warnings...)
			result.leftOut = append(result.leftOut, leftOut...)
		}
	}

	if wm.billedThrough != nil {
		result.Warnings = append(result.Warnings, fmt.Sprintf(
			"A spend-threshold invoice already billed this period through %s; this estimate shows only what is left. Tiered prices and max or last-value meters may differ on the actual invoice.",
			wm.billedThrough.In(loc).Format("Jan 2, 2006 15:04 MST")))
	}

	if result.Totals, err = computePreviewTotals(result.Lines); err != nil {
		return PreviewResult{}, err
	}
	return result, nil
}

type legacyBucketPricer func(rule domain.RatingRuleVersion, bucketKey string, aggregation string, qty decimal.Decimal) (int64, error)

func (e *Engine) legacyRateMeterWindow(ctx context.Context, tenantID, customerID, meterID string, from, to, pinAt time.Time, price legacyBucketPricer) ([]PreviewLine, []string, []leftOutBucket, error) {
	meter, err := e.pricing.GetMeter(ctx, tenantID, meterID)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("get meter: %w", err)
	}

	rules, err := e.pricing.ListMeterPricingRulesByMeter(ctx, tenantID, meterID)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("list pricing rules: %w", err)
	}
	aggs, err := e.usage.AggregateClaims(ctx, tenantID, customerID, meterID, meter.Aggregation, domain.ClaimsOfRules(rules), from, to)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("aggregate by pricing rules: %w", err)
	}
	rulesByID := make(map[string]domain.MeterPricingRule, len(rules))
	for _, rule := range rules {
		rulesByID[rule.ID] = rule
	}

	var lines []PreviewLine
	var warnings []string
	var leftOut []leftOutBucket

	for _, agg := range aggs {
		if agg.Quantity.IsZero() {
			continue
		}
		// Ingest accepts negative corrections, so a bucket's period-to-date
		// total can be negative until new usage overtakes it. That is a
		// legitimate state, not a pricing error: it contributes nothing to
		// a preview or a threshold, with a warning — the rule every invoice
		// path follows (net_negative.go, #1193). (Pricing it would fail —
		// ComputeAmountCents refuses negative quantities.)
		if agg.Quantity.IsNegative() {
			warnings = append(warnings, fmt.Sprintf("meter %q: net-negative usage so far (%s) after corrections — counted as 0", meter.EventName, agg.Quantity.String()))
			leftOut = append(leftOut, leftOutBucket{MeterID: meterID, EventName: meter.EventName, MeterName: meter.Name, Aggregation: meter.Aggregation, Quantity: agg.Quantity, BelowZero: true})
			continue
		}

		ratingRuleID := agg.RatingRuleVersionID
		if ratingRuleID == "" {
			ratingRuleID = meter.RatingRuleVersionID
		}
		if ratingRuleID == "" {
			// The quantity is what the operator needs to bill it by hand
			// (a one-off invoice), so the warning carries it (#981).
			warnings = append(warnings, fmt.Sprintf("meter %q: %s %s matched no pricing rule and the meter has no default price — not billed", meter.EventName, agg.Quantity.String(), meter.Unit))
			leftOut = append(leftOut, leftOutBucket{MeterID: meterID, EventName: meter.EventName, MeterName: meter.Name, Aggregation: meter.Aggregation, Quantity: agg.Quantity})
			continue
		}

		// Same resolution rule as the cycle scan and cancel finalize
		// (resolveRatedRule; ADR-070 pin-at-period-start): version and
		// override in force at the period open — pinAt, the period start
		// for every caller, including the threshold scan (which persists
		// these lines as a real invoice). This is
		// what makes preview == invoice hold across a mid-period
		// publish, and a threshold fire agree with the later close.
		// Both failures abort, as the cycle close does for the same rule: a
		// threshold fire persists these lines as a real invoice, and a
		// skipped line under-billed the fire (the bucket was then not on
		// the watermark either). A missing PINNED version is not "not set" —
		// usage is bound to it — so it fails like any other error.
		ratingRule, err := e.legacyResolveRatedRule(ctx, tenantID, customerID, ratingRuleID, pinAt)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("resolve pricing for meter %q rule %q: %w", meter.EventName, ratingRuleID, err)
		}

		var amount int64
		if price != nil {
			amount, err = price(ratingRule, meterID+"|"+agg.RuleID, meter.Aggregation, agg.Quantity)
		} else {
			amount, err = domain.ComputeAmountCents(ratingRule, agg.Quantity)
		}
		if err != nil {
			return nil, nil, nil, fmt.Errorf("compute amount for meter %q rule %q: %w", meter.EventName, ratingRule.RuleKey, err)
		}

		// Unit amount is amount / quantity, rounded to nearest cent. The
		// IsZero short-circuit above guarantees the divisor is non-zero.
		unitAmount := decimal.NewFromInt(amount).Div(agg.Quantity).RoundBank(0).IntPart()

		var dimMatch map[string]any
		if agg.RuleID != "" {
			if rule, ok := rulesByID[agg.RuleID]; ok && len(rule.DimensionMatch) > 0 {
				dimMatch = rule.DimensionMatch
			}
		}

		// Use the exact description the cycle scan persists (meter + dimension
		// match, e.g. "Tokens (claude-3.5-sonnet · input)") so a preview line
		// reads identically to the invoice_line_item it predicts. The quantity
		// lives in the Quantity field, not the description.
		desc := usageLineDescription(meter, rulesByID[agg.RuleID])

		lines = append(lines, PreviewLine{
			LineType:                 "usage",
			Description:              desc,
			MeterID:                  meterID,
			MeterUnit:                meter.Unit,
			RatingRuleVersionID:      ratingRule.ID,
			RuleKey:                  ratingRule.RuleKey,
			DimensionMatch:           dimMatch,
			Currency:                 ratingRule.Currency,
			Quantity:                 agg.Quantity,
			UnitAmountCents:          unitAmount,
			AmountCents:              amount,
			PricingMode:              string(ratingRule.Mode),
			Aggregation:              meter.Aggregation,
			NominalUnitAmountDecimal: nominalRate(ratingRule),
		})
	}

	return lines, warnings, leftOut, nil
}

func (e *Engine) legacyUnbilledUsageCents(ctx context.Context, sub domain.Subscription) (int64, string, error) {
	if sub.CurrentBillingPeriodStart == nil || sub.CurrentBillingPeriodEnd == nil {
		return 0, "", nil
	}
	periodStart, periodEnd := *sub.CurrentBillingPeriodStart, *sub.CurrentBillingPeriodEnd

	wm, err := e.loadThresholdWatermark(ctx, sub.TenantID, sub.ID, periodStart, periodEnd)
	if err != nil {
		return 0, "", err
	}

	plans := map[string]domain.Plan{}
	loadPlan := func(planID string) error {
		if _, ok := plans[planID]; ok || planID == "" {
			return nil
		}
		plan, err := e.pricing.GetPlan(ctx, sub.TenantID, planID)
		if err != nil {
			return fmt.Errorf("get plan %s: %w", planID, err)
		}
		plans[planID] = plan
		return nil
	}
	for _, it := range sub.Items {
		if err := loadPlan(it.PlanID); err != nil {
			return 0, "", err
		}
	}
	// A trialing subscription has no billing intervals yet: conversion opens
	// one per item at the stored period start. With no segments,
	// meterUsageIntervals gives each current item the whole period, which is
	// what those intervals will say.
	var segsByItem map[string][]baseSegment
	if sub.Status != domain.SubscriptionTrialing {
		segsByItem, err = e.windowSegments(ctx, sub, periodStart, periodEnd)
		if err != nil {
			return 0, "", fmt.Errorf("resolve segments: %w", err)
		}
		for _, segs := range segsByItem {
			for _, seg := range segs {
				if err := loadPlan(seg.planID); err != nil {
					return 0, "", err
				}
			}
		}
	}
	floors, err := e.loadUsageFloors(ctx, sub, periodStart, periodEnd)
	if err != nil {
		return 0, "", err
	}
	clamped, full := meterUsageIntervals(sub, segsByItem, plans, periodStart, periodEnd, wm.billedThrough, floors)
	meterIDs := map[string]struct{}{}
	for mid := range clamped {
		meterIDs[mid] = struct{}{}
	}
	for mid := range full {
		meterIDs[mid] = struct{}{}
	}

	ladder := newLegacyUsageLadder(wm)
	var total int64
	for _, meterID := range slices.Sorted(maps.Keys(meterIDs)) {
		price := func(rule domain.RatingRuleVersion, bucketKey string, aggregation string, qty decimal.Decimal) (int64, error) {
			return ladder.price(rule, meterID, bucketKey, aggregation, qty)
		}
		// sum adds the lines of one pass over the given windows: the clamped
		// pass takes every bucket except those the fire deferred, the full
		// pass only those. Rules are pinned at the period start (ADR-070).
		sum := func(windows []usageInterval, deferred bool) error {
			for _, iv := range mergeUsageIntervals(windows) {
				lines, _, _, err := e.legacyRateMeterWindow(ctx, sub.TenantID, sub.CustomerID, meterID, iv.start, iv.end, periodStart, price)
				if err != nil {
					return fmt.Errorf("rate meter %s [%v, %v): %w", meterID, iv.start, iv.end, err)
				}
				for _, l := range lines {
					if wm.deferredBucket(l.Aggregation, meterID, l.RatingRuleVersionID) == deferred {
						total += l.AmountCents
					}
				}
			}
			return nil
		}
		if err := sum(clamped[meterID], false); err != nil {
			return 0, "", err
		}

		// Full-window pass. Skipped for a meter that cannot have a deferred
		// bucket, so that after a fire a sum-only meter costs one aggregation
		// over the short post-fire window instead of a second one over the
		// whole period.
		if !wm.exists() {
			continue
		}
		nonAdditive, err := e.meterHasNonAdditiveBucket(ctx, sub.TenantID, meterID)
		if err != nil {
			return 0, "", err
		}
		if !nonAdditive {
			continue
		}
		if err := sum(full[meterID], true); err != nil {
			return 0, "", err
		}
	}
	planCurrency := ""
	if len(sub.Items) > 0 {
		planCurrency = plans[sub.Items[0].PlanID].Currency
	}
	return total, planCurrency, nil
}

type legacyUsageLadder struct {
	wm  thresholdWatermark
	on  bool
	qty map[string]decimal.Decimal // bucket key → quantity priced so far this period
	amt map[string]int64           // bucket key → amount billed so far this period
}

// newUsageLadder builds the closer's ladder. It is on whenever this period
// has a threshold fire to continue from.
func newLegacyUsageLadder(wm thresholdWatermark) *legacyUsageLadder {
	return &legacyUsageLadder{wm: wm, on: wm.exists(), qty: map[string]decimal.Decimal{}, amt: map[string]int64{}}
}

// continues reports whether a bucket priced under rule continues a ladder.
func (l *legacyUsageLadder) continues(rule domain.RatingRuleVersion, aggregation string) bool {
	return l.on && !nonAdditive(aggregation) && rule.Mode != domain.PricingPerUnit
}

// price returns the amount for qty more units of the bucket under rule.
// bucketKey identifies the closer's bucket (meter + pricing rule); rule is
// the RESOLVED rule, whose id is how the fire's line is found.
func (l *legacyUsageLadder) price(rule domain.RatingRuleVersion, meterID, bucketKey string, aggregation string, qty decimal.Decimal) (int64, error) {
	if !l.continues(rule, aggregation) {
		return domain.ComputeAmountCents(rule, qty)
	}
	if _, seen := l.qty[bucketKey]; !seen {
		seed, err := legacyBilledUsageQty(l.wm, meterID, rule.ID)
		if err != nil {
			return 0, err
		}
		seedAmt, err := domain.ComputeAmountCents(rule, seed)
		if err != nil {
			return 0, err
		}
		l.qty[bucketKey], l.amt[bucketKey] = seed, seedAmt
	}
	cumulative := l.qty[bucketKey].Add(qty)
	total, err := domain.ComputeAmountCents(rule, cumulative)
	if err != nil {
		return 0, err
	}
	amount := max(total-l.amt[bucketKey], 0)
	l.qty[bucketKey] = cumulative
	l.amt[bucketKey] += amount
	return amount, nil
}

func legacyBilledUsageQty(w thresholdWatermark, meterID, ratingRuleVersionID string) (decimal.Decimal, error) {
	var (
		qty   decimal.Decimal
		found bool
	)
	for _, li := range w.lines {
		if li.LineType != domain.LineTypeUsage || li.MeterID != meterID || li.RatingRuleVersionID != ratingRuleVersionID {
			continue
		}
		if found {
			return decimal.Zero, fmt.Errorf("threshold invoice has two usage lines for meter %s rating rule %s — cannot tell which bucket each belongs to, so the close cannot continue its tiers; give each pricing rule its own rating rule", meterID, ratingRuleVersionID)
		}
		found = true
		qty = li.QuantityDecimal
		if qty.IsZero() {
			qty = decimal.NewFromInt(li.Quantity) // lines written before quantity_decimal
		}
	}
	return qty, nil
}

func (e *Engine) legacyCheckLadderSeeds(ctx context.Context, sub domain.Subscription, meterID string, pricingRules []domain.MeterPricingRule, periodStart time.Time, ladder *legacyUsageLadder) error {
	wm := ladder.wm
	if !ladder.on || len(pricingRules) == 0 || !wm.billsMeter(meterID) {
		return nil
	}
	meter, err := e.pricing.GetMeter(ctx, sub.TenantID, meterID)
	if err != nil {
		return fmt.Errorf("get meter %s: %w", meterID, err)
	}
	pinned := make([]string, 0, len(pricingRules)+1)
	if meter.RatingRuleVersionID != "" {
		pinned = append(pinned, meter.RatingRuleVersionID)
	}
	for _, r := range pricingRules {
		pinned = append(pinned, r.RatingRuleVersionID)
	}
	seen := map[string]int{}
	for _, id := range pinned {
		rule, err := e.legacyResolveRatedRule(ctx, sub.TenantID, sub.CustomerID, id, periodStart)
		if err != nil {
			return fmt.Errorf("resolve pricing for meter %s rule %s: %w", meterID, id, err)
		}
		if rule.Mode == domain.PricingPerUnit {
			continue
		}
		seen[rule.ID]++
		if seen[rule.ID] == 2 {
			q, err := legacyBilledUsageQty(wm, meterID, rule.ID)
			if err != nil {
				return err
			}
			if !q.IsZero() {
				return fmt.Errorf("meter %s: two of its pricing buckets resolve to rating rule %s, which the threshold invoice billed — the close cannot tell whose tiers to continue; give each pricing rule its own rating rule", meterID, rule.ID)
			}
		}
	}
	return nil
}
