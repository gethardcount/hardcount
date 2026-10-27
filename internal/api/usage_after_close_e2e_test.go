package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/platform/clock"
)

// Usage that arrives after its period was invoiced (ADR-120 amendment
// 2026-10-22). Velox still stores it and answers 2xx, and still bills it on
// no invoice. What changed is that the caller and the operator are told:
// the single-ingest and backfill 201 carry billing_status "after_close", the
// batch and LiteLLM answers count after_close rows, and
// velox_usage_unbilled_total{reason="after_close", origin} counts them.
//
// The flag must never be false: a flagged event must really be billed on no
// invoice. Each test that flags an event checks the invoices too, and each
// case Velox does still bill (the grace, a trial, a max
// meter a threshold fire deferred) checks the event is not flagged.
//
// The rig is the close-lag rig: the real server over HTTP. April is
// P0 = [Apr 1, May 1) and B = May 1.

const afterCloseMetric = unbilledMetric + `{reason="after_close"}`

// afterCloseCount reads velox_usage_unbilled_total{reason="after_close", origin}.
func afterCloseCount(t *testing.T, origin string) float64 {
	t.Helper()
	return unbilledCount(t, "after_close", origin)
}

// doH is do with the response headers.
func (r *lagRig) doH(method, path string, at time.Time, body any) (int, map[string]any, http.Header) {
	r.t.Helper()
	r.clk.Set(at)
	var buf bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&buf).Encode(body)
	}
	req := httptest.NewRequest(method, path, &buf)
	req = req.WithContext(clock.WithEffectiveNow(context.Background(), at))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", r.auth)
	rec := httptest.NewRecorder()
	r.srv.ServeHTTP(rec, req)
	out := map[string]any{}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if rec.Code >= 400 {
		r.t.Logf("%s %s at %v: %d %s", method, path, at, rec.Code, rec.Body.String())
	}
	return rec.Code, out, rec.Header()
}

// postEvent posts one event of meter `meter` stamped ts with key, reaching
// Velox at arrival, to path (/v1/usage-events or its backfill), and wants
// status `want`. It returns the answer.
func (r *lagRig) postEvent(path, meter, key string, ts, arrival time.Time, qty int64, want int) map[string]any {
	r.t.Helper()
	code, out, _ := r.doH(http.MethodPost, path, arrival, map[string]any{
		"external_customer_id": r.custExt, "event_name": meter, "quantity": qty,
		"timestamp": ts.Format(time.RFC3339), "idempotency_key": key,
	})
	if code != want {
		r.t.Fatalf("POST %s %s at %v: %d %v, want %d", path, key, arrival, code, out, want)
	}
	return out
}

// wantStatus checks an event answer's billing_status: "" means absent.
func wantStatus(t *testing.T, what string, out map[string]any, want string) {
	t.Helper()
	got, present := out["billing_status"]
	switch {
	case want == "" && present:
		t.Errorf("%s: billing_status = %v, want none", what, got)
	case want != "" && got != want:
		t.Errorf("%s: billing_status = %v, want %q", what, got, want)
	}
}

// count reads an integer field of an answer.
func count(out map[string]any, field string) int {
	n, _ := out[field].(float64)
	return int(n)
}

// newMeter creates a meter with aggregation agg; planWith prices it at 1c
// per unit.
func (r *lagRig) newMeter(key, agg string) string {
	r.t.Helper()
	m := r.must(http.MethodPost, "/v1/meters", lagP0, map[string]any{
		"event_name": key, "name": key, "unit": key, "aggregation": agg,
	}, http.StatusCreated)
	return m["id"].(string)
}

// planWith creates a monthly in-arrears plan pricing meters at 1c per unit.
func (r *lagRig) planWith(code string, base int64, meters ...string) string {
	r.t.Helper()
	p := r.must(http.MethodPost, "/v1/plans", lagP0, map[string]any{
		"code": code, "name": code, "currency": "USD", "billing_interval": "monthly",
		"base_amount_cents": base, "base_bill_timing": "in_arrears", "prices": centPrices(meters),
	}, http.StatusCreated)
	return p["id"].(string)
}

// TestUsageAfterClose_SingleIngestAndBackfillAreFlagged: once April's invoice
// is built, an April event is answered 201 with billing_status after_close
// and counted, and the May invoice really leaves it out. Events in the open
// period carry no billing_status. A backfill into April is flagged the same
// way, counted under origin backfill. A replay of a flagged event answers
// 200 with the original row and no billing_status, and is not counted again.
//
// Mutation checks: compare ts <= period end instead of ts < period start →
// the May event is flagged; count before the write → the replay is counted;
// recompute billing_status on the replay → the 200 carries it.
func TestUsageAfterClose_SingleIngestAndBackfillAreFlagged(t *testing.T) {
	skipShort(t)
	r := newLagRigWith(t, "in_arrears", false, WithInvoiceGrace(e2eGrace))
	api0, bf0 := afterCloseCount(t, "api"), afterCloseCount(t, "backfill")

	out := r.postEvent("/v1/usage-events", "calls", "open-apr", lagP0.AddDate(0, 0, 19), lagP0.AddDate(0, 0, 19), 100, http.StatusCreated)
	wantStatus(t, "April event while April is open", out, "")

	if n := r.run(lagB.Add(2 * time.Hour)); n != 1 {
		t.Fatalf("run at B+2h generated %d invoices, want April's", n)
	}

	out = r.postEvent("/v1/usage-events", "calls", "late-apr", lagB.Add(-5*time.Minute), lagB.Add(3*time.Hour), 25, http.StatusCreated)
	wantStatus(t, "April event after April's invoice", out, "after_close")
	if got := afterCloseCount(t, "api") - api0; got != 1 {
		t.Errorf("%s{origin=api} moved by %v, want 1", afterCloseMetric, got)
	}

	out = r.postEvent("/v1/usage-events/backfill", "calls", "bf-apr", lagP0.AddDate(0, 0, 25), lagB.Add(3*time.Hour), 9, http.StatusCreated)
	wantStatus(t, "April backfill after April's invoice", out, "after_close")
	if got := afterCloseCount(t, "backfill") - bf0; got != 1 {
		t.Errorf("%s{origin=backfill} moved by %v, want 1", afterCloseMetric, got)
	}

	out = r.postEvent("/v1/usage-events", "calls", "open-may", lagB.AddDate(0, 0, 9), lagB.AddDate(0, 0, 9), 4, http.StatusCreated)
	wantStatus(t, "May event while May is open", out, "")

	// The replay: the original row, no billing_status, not counted again.
	code, body, hdr := r.doH(http.MethodPost, "/v1/usage-events", lagB.AddDate(0, 0, 10), map[string]any{
		"external_customer_id": r.custExt, "event_name": "calls", "quantity": 25,
		"timestamp": lagB.Add(-5 * time.Minute).Format(time.RFC3339), "idempotency_key": "late-apr",
	})
	if code != http.StatusOK || hdr.Get("Idempotent-Replayed") != "true" {
		t.Fatalf("replay: %d %v Idempotent-Replayed=%q, want 200 and the header", code, body, hdr.Get("Idempotent-Replayed"))
	}
	wantStatus(t, "replay of a flagged event", body, "")
	if got := afterCloseCount(t, "api") - api0; got != 1 {
		t.Errorf("%s{origin=api} moved by %v after the replay, want still 1", afterCloseMetric, got)
	}

	// The flag is true: no invoice bills the flagged rows.
	r.run(lagP2.Add(2 * time.Hour))
	invs := r.invoices()
	if got := r.cycleInvoice(invs, lagP0).sum("usage"); got != 100 {
		r.logInvoices(invs)
		t.Errorf("April usage = %dc, want 100c", got)
	}
	if got := r.cycleInvoice(invs, lagB).sum("usage"); got != 4 {
		r.logInvoices(invs)
		t.Errorf("May usage = %dc, want 4c: only May's event, not the flagged April rows", got)
	}
	if got := usageBilled(invs); got != 104 {
		r.logInvoices(invs)
		t.Errorf("usage on all invoices = %dc, want 104c: the flagged 25 + 9 are billed nowhere", got)
	}
}

// TestUsageAfterClose_BatchCountsInsertedRowsOnly: a batch with a new April
// event, a replay of an already-stored April event and a May event, sent
// after April's invoice, answers after_close 1: the replay is not counted.
//
// Mutation check: count prepared rows instead of inserted ones → 2.
func TestUsageAfterClose_BatchCountsInsertedRowsOnly(t *testing.T) {
	skipShort(t)
	r := newLagRigWith(t, "in_arrears", false, WithInvoiceGrace(e2eGrace))
	r.postEvent("/v1/usage-events", "calls", "apr-1", lagP0.AddDate(0, 0, 19), lagP0.AddDate(0, 0, 19), 100, http.StatusCreated)
	r.run(lagB.Add(2 * time.Hour))
	api0 := afterCloseCount(t, "api")

	ev := func(key string, ts time.Time, qty int64) map[string]any {
		return map[string]any{
			"external_customer_id": r.custExt, "event_name": "calls", "quantity": qty,
			"timestamp": ts.Format(time.RFC3339), "idempotency_key": key,
		}
	}
	batch := []map[string]any{
		ev("apr-late", lagB.Add(-time.Minute), 3),
		ev("apr-1", lagP0.AddDate(0, 0, 19), 100),
		ev("may-1", lagB.Add(time.Hour), 4),
	}
	code, out, _ := r.doH(http.MethodPost, "/v1/usage-events/batch", lagB.Add(3*time.Hour), batch)
	if code != http.StatusCreated {
		t.Fatalf("batch: %d %v, want 201", code, out)
	}
	if count(out, "ingested") != 2 || count(out, "deduplicated") != 1 || count(out, "after_close") != 1 {
		t.Errorf("batch answer = %v, want ingested 2, deduplicated 1, after_close 1", out)
	}
	if got := afterCloseCount(t, "api") - api0; got != 1 {
		t.Errorf("%s{origin=api} moved by %v, want 1", afterCloseMetric, got)
	}

	// A replay of the whole batch writes nothing and flags nothing.
	code, out, _ = r.doH(http.MethodPost, "/v1/usage-events/batch", lagB.Add(4*time.Hour), batch)
	if code != http.StatusCreated || count(out, "ingested") != 0 || count(out, "after_close") != 0 {
		t.Errorf("batch replay: %d %v, want 201 with ingested 0 and after_close 0", code, out)
	}
	if got := afterCloseCount(t, "api") - api0; got != 1 {
		t.Errorf("%s{origin=api} moved by %v after the replay, want still 1", afterCloseMetric, got)
	}
}

// litellmCall is one LiteLLM spend record for user, ended at end.
func litellmCall(id, user string, end time.Time, prompt, completion int) map[string]any {
	return map[string]any{
		"id": id, "call_type": "completion", "model": "gpt-4o", "custom_llm_provider": "openai",
		"user": user, "endTime": end.Unix(),
		"usage": map[string]any{"prompt_tokens": prompt, "completion_tokens": completion, "total_tokens": prompt + completion},
	}
}

// newTokensRig is the close-lag rig with a "tokens" meter (the LiteLLM
// adapter's meter, 1c per token) on plan T, and no subscription yet.
func newTokensRig(t *testing.T) (*lagRig, string) {
	t.Helper()
	r := newLagRigBare(t, "in_arrears", 0, WithInvoiceGrace(e2eGrace))
	tokens := r.newMeter("tokens", "sum")
	return r, r.planWith("plan_t", 3000, tokens)
}

// TestUsageAfterClose_LiteLLMCountsAfterClose: a LiteLLM call that ended in
// April and arrives after April's invoice answers after_close 2 (its input
// and output rows); its replay answers after_close 0. Neither row is billed.
func TestUsageAfterClose_LiteLLMCountsAfterClose(t *testing.T) {
	skipShort(t)
	r, planT := newTokensRig(t)
	r.newSub("llm", planT, lagP0, nil)
	if s := r.sub(); !s.CurrentBillingPeriodStart.Equal(lagP0) || !s.CurrentBillingPeriodEnd.Equal(lagB) {
		t.Fatalf("period = [%v, %v), want April", s.CurrentBillingPeriodStart, s.CurrentBillingPeriodEnd)
	}
	r.must(http.MethodPost, "/v1/integrations/litellm/spend", lagP0.AddDate(0, 0, 9),
		litellmCall("call-open", r.custExt, lagP0.AddDate(0, 0, 9), 10, 5), http.StatusOK)
	r.run(lagB.Add(2 * time.Hour))
	api0 := afterCloseCount(t, "api")

	late := litellmCall("call-late", r.custExt, lagB.Add(-2*time.Minute), 7, 3)
	out := r.must(http.MethodPost, "/v1/integrations/litellm/spend", lagB.Add(3*time.Hour), late, http.StatusOK)
	if count(out, "accepted") != 2 || count(out, "after_close") != 2 {
		t.Errorf("late call answer = %v, want accepted 2 and after_close 2", out)
	}
	out = r.must(http.MethodPost, "/v1/integrations/litellm/spend", lagB.Add(4*time.Hour), late, http.StatusOK)
	if count(out, "deduplicated") != 2 || count(out, "after_close") != 0 {
		t.Errorf("replay answer = %v, want deduplicated 2 and after_close 0", out)
	}
	if got := afterCloseCount(t, "api") - api0; got != 2 {
		t.Errorf("%s{origin=api} moved by %v, want 2", afterCloseMetric, got)
	}

	r.run(lagP2.Add(2 * time.Hour))
	if got := usageBilled(r.invoices()); got != 15 {
		r.logInvoices(r.invoices())
		t.Errorf("usage on all invoices = %dc, want 15c: the open call only", got)
	}
}

// TestUsageAfterClose_GraceIsNotFlagged: inside April's grace, an April event
// and a May event (stamped after April ended, before April's invoice) are not
// flagged. Both are billed: the April event on April's invoice, the May event
// on May's.
func TestUsageAfterClose_GraceIsNotFlagged(t *testing.T) {
	skipShort(t)
	r := newLagRigWith(t, "in_arrears", false, WithInvoiceGrace(e2eGrace))
	api0 := afterCloseCount(t, "api")
	out := r.postEvent("/v1/usage-events", "calls", "apr-grace", lagB.Add(-10*time.Minute), lagB.Add(20*time.Minute), 40, http.StatusCreated)
	wantStatus(t, "April event inside the grace", out, "")
	out = r.postEvent("/v1/usage-events", "calls", "may-grace", lagB.Add(10*time.Minute), lagB.Add(25*time.Minute), 6, http.StatusCreated)
	wantStatus(t, "May event inside April's grace", out, "")
	if got := afterCloseCount(t, "api") - api0; got != 0 {
		t.Errorf("%s{origin=api} moved by %v, want 0", afterCloseMetric, got)
	}

	r.run(lagB.Add(2 * time.Hour))
	r.run(lagP2.Add(2 * time.Hour))
	invs := r.invoices()
	if got := r.cycleInvoice(invs, lagP0).sum("usage"); got != 40 {
		r.logInvoices(invs)
		t.Errorf("April usage = %dc, want 40c", got)
	}
	if got := r.cycleInvoice(invs, lagB).sum("usage"); got != 6 {
		r.logInvoices(invs)
		t.Errorf("May usage = %dc, want 6c", got)
	}
}

// TestUsageAfterClose_TrialUsageIsNotFlagged: usage stamped in a trial, before
// the first paid period starts, is free, not missed: it is never after_close.
// It is answered outside_subscription, because no billing period holds it
// (D07). The control: after that period's invoice, an event stamped in it is
// after_close.
//
// Mutation check: take the floor from the subscription's start instead of
// the trial end → the trial event after the first paid invoice is
// after_close.
func TestUsageAfterClose_TrialUsageIsNotFlagged(t *testing.T) {
	skipShort(t)
	r := newLagRigBare(t, "in_arrears", 0)
	r.newTrialEndingAt(uwDay)
	out := r.postEvent("/v1/usage-events", "calls", "trial-1", uwDay.Add(-14*time.Hour), uwDay.Add(-14*time.Hour+time.Minute), 5, http.StatusCreated)
	wantStatus(t, "trial event", out, "outside_subscription")

	r.expireTrials(dayAt(0, 30))
	r.closePeriod(uwDay)
	out = r.postEvent("/v1/usage-events", "calls", "paid-late", dayAt(1, 0), r.sub().CurrentBillingPeriodStart.Add(2*time.Hour), 9, http.StatusCreated)
	wantStatus(t, "paid-period event after its invoice", out, "after_close")
	out = r.postEvent("/v1/usage-events", "calls", "trial-late", uwDay.Add(-13*time.Hour), r.sub().CurrentBillingPeriodStart.Add(2*time.Hour), 5, http.StatusCreated)
	wantStatus(t, "trial event after the first paid invoice", out, "outside_subscription")
}

// TestUsageAfterClose_LastMeterIsFlagged: a last meter bills the latest
// event of each period, as every meter bills its own period (ADR-130 §2).
// So an April reading that arrives after April's invoice is billed on no
// invoice, and the answer says after_close. A last_ever pricing rule, which
// read the latest value however old, was the one exception; it is gone.
// The control: a sum-meter event of the same subscription, same instant.
//
// Mutation check: classify a last meter as never after_close → the seats
// reading is not flagged.
func TestUsageAfterClose_LastMeterIsFlagged(t *testing.T) {
	skipShort(t)
	r := newLagRigBare(t, "in_arrears", 0, WithInvoiceGrace(e2eGrace))
	seats := r.newMeter("seats", "last")
	r.newSub("gauge", r.planWith("plan_g", 3000, r.meterID, seats), lagP0, nil)
	r.postEvent("/v1/usage-events", "seats", "seats-apr10", lagP0.AddDate(0, 0, 9), lagP0.AddDate(0, 0, 9), 5, http.StatusCreated)
	r.run(lagB.Add(2 * time.Hour))

	out := r.postEvent("/v1/usage-events", "seats", "seats-apr30", lagB.Add(-time.Hour), lagB.Add(3*time.Hour), 7, http.StatusCreated)
	wantStatus(t, "last reading stamped in April after April's invoice", out, "after_close")
	out = r.postEvent("/v1/usage-events", "calls", "calls-apr30", lagB.Add(-time.Hour), lagB.Add(3*time.Hour), 2, http.StatusCreated)
	wantStatus(t, "sum event stamped in April after April's invoice", out, "after_close")

	r.run(lagP2.Add(2 * time.Hour))
	invs := r.invoices()
	if got := r.cycleInvoice(invs, lagB).sum("usage"); got != 0 {
		r.logInvoices(invs)
		t.Errorf("May usage = %dc, want 0c: the late April reading belongs to April, whose invoice was built", got)
	}
}

// thresholdFired is April on one subscription pricing a sum meter (calls)
// and a max meter (peak): 100 calls and a peak of 5 on April 10, and a spend
// threshold of 100c with no reset that fires on April 20 and bills April 1
// to 20. It returns the fire instant and the threshold invoice, which is not
// paid yet.
func thresholdFired(t *testing.T) (*lagRig, time.Time, string) {
	t.Helper()
	return thresholdFiredWithCredit(t, 0)
}

// thresholdFiredWithCredit is thresholdFired for a customer granted
// grantCents of credit on April 1, which the fire's collection applies to the
// threshold invoice.
func thresholdFiredWithCredit(t *testing.T, grantCents int64) (*lagRig, time.Time, string) {
	t.Helper()
	r := newLagRigBare(t, "in_arrears", grantCents, WithInvoiceGrace(e2eGrace))
	peak := r.newMeter("peak", "max")
	r.newSub("thr", r.planWith("plan_p", 3000, r.meterID, peak), lagP0, nil)
	r.postEvent("/v1/usage-events", "calls", "calls-apr10", lagP0.AddDate(0, 0, 9), lagP0.AddDate(0, 0, 9), 100, http.StatusCreated)
	r.postEvent("/v1/usage-events", "peak", "peak-apr10", lagP0.AddDate(0, 0, 9), lagP0.AddDate(0, 0, 9), 5, http.StatusCreated)
	fireAt := lagP0.AddDate(0, 0, 19)
	r.must(http.MethodPut, "/v1/subscriptions/"+r.subID+"/billing-thresholds", fireAt.Add(-time.Hour),
		map[string]any{"amount_gte": 100}, http.StatusOK)
	if n := r.scanThresholds(fireAt); n != 1 {
		t.Fatalf("threshold scan fired %d invoices, want 1", n)
	}
	for _, inv := range r.invoices() {
		if inv.reason == string(domain.BillingReasonThreshold) {
			if inv.status == string(domain.InvoicePaid) {
				t.Fatalf("threshold invoice is already paid; the tests need it unpaid")
			}
			return r, fireAt, inv.id
		}
	}
	t.Fatal("no threshold invoice")
	return nil, time.Time{}, ""
}

// TestUsageAfterClose_ThresholdWatermark: the April 20 fire's invoice is
// paid. A sum-meter event stamped April 15 that arrives after that is
// billed nowhere: the fire billed through April 20, the close bills sum
// buckets from there, and a paid invoice can be neither voided nor written
// off. It is flagged. A max-meter reading stamped April 15 is not: the fire
// deferred the max bucket, and April's close bills it over the whole month.
//
// Mutation check: drop the sum/count-only condition → the max reading is
// flagged.
func TestUsageAfterClose_ThresholdWatermark(t *testing.T) {
	skipShort(t)
	r, fireAt, thr := thresholdFired(t)
	r.must(http.MethodPost, "/v1/invoices/"+thr+"/record-payment", fireAt.Add(30*time.Minute), map[string]any{"note": "wire"}, http.StatusOK)

	out := r.postEvent("/v1/usage-events", "calls", "calls-apr15", lagP0.AddDate(0, 0, 14), fireAt.Add(time.Hour), 30, http.StatusCreated)
	wantStatus(t, "sum event before a paid threshold invoice's watermark", out, "after_close")
	out = r.postEvent("/v1/usage-events", "peak", "peak-apr15", lagP0.AddDate(0, 0, 14), fireAt.Add(time.Hour), 9, http.StatusCreated)
	wantStatus(t, "max reading before the threshold watermark", out, "")
	out = r.postEvent("/v1/usage-events", "calls", "calls-apr25", lagP0.AddDate(0, 0, 24), lagP0.AddDate(0, 0, 24), 3, http.StatusCreated)
	wantStatus(t, "sum event after the threshold watermark", out, "")

	r.run(lagB.Add(2 * time.Hour))
	invs := r.invoices()
	if got := usageBilled(invs); got != 100+9+3 {
		r.logInvoices(invs)
		t.Errorf("usage on all invoices = %dc, want 112c: 100 on the fire, the 9 peak and 3 calls at the close; the flagged 30 nowhere", got)
	}
}

// TestUsageAfterClose_ThresholdWatermark_Unpaid: review finding 1 on #1350.
// While the threshold invoice is unpaid it can still be voided, and then
// April's close bills usage from April 1 (#243). So a sum event stamped
// before the watermark that arrives while the invoice is unpaid is not
// flagged: after a void April's invoice bills it, and a flag would have told
// the operator to bill it a second time. If the invoice is paid or written
// off instead, its window stays billed (ADR-066 amendment 2026-10-23), so
// the event is billed nowhere and was not flagged: a missed flag, never a
// wrong one.
//
// Mutation check: flag before the watermark of any threshold invoice, paid
// or not → the event is flagged in all three cases.
func TestUsageAfterClose_ThresholdWatermark_Unpaid(t *testing.T) {
	skipShort(t)
	for _, tc := range []struct {
		action    string
		wantUsage int64 // on April's cycle invoice; the 5 peak is billed there either way
	}{
		{"void", 100 + 30 + 5},
		{"mark-uncollectible", 5},
		{"record-payment", 5},
	} {
		t.Run(tc.action, func(t *testing.T) {
			r, fireAt, thr := thresholdFired(t)
			out := r.postEvent("/v1/usage-events", "calls", "calls-apr15", lagP0.AddDate(0, 0, 14), fireAt.Add(time.Hour), 30, http.StatusCreated)
			wantStatus(t, "sum event before an unpaid threshold invoice's watermark", out, "")

			if code, body, _ := r.doH(http.MethodPost, "/v1/invoices/"+thr+"/"+tc.action, fireAt.Add(2*time.Hour), map[string]any{"note": "wire"}); code >= 300 {
				t.Fatalf("%s the threshold invoice: %d %v", tc.action, code, body)
			}
			r.run(lagB.Add(2 * time.Hour))
			invs := r.invoices()
			if got := r.cycleInvoice(invs, lagP0).sum("usage"); got != tc.wantUsage {
				r.logInvoices(invs)
				t.Errorf("April cycle usage after %s = %dc, want %dc", tc.action, got, tc.wantUsage)
			}
		})
	}
}

// TestUsageAfterClose_ThresholdWatermark_ThresholdsCleared: review finding 2
// on #1350. The close keys the watermark on the fire's invoice, not on the
// subscription's threshold setting. So after the thresholds are removed, a
// sum event stamped before the (paid) fire's watermark is still billed
// nowhere, and it is flagged.
//
// Mutation check: read the watermark only for a subscription that still has
// thresholds → the event is not flagged.
func TestUsageAfterClose_ThresholdWatermark_ThresholdsCleared(t *testing.T) {
	skipShort(t)
	r, fireAt, thr := thresholdFired(t)
	r.must(http.MethodPost, "/v1/invoices/"+thr+"/record-payment", fireAt.Add(10*time.Minute), map[string]any{"note": "wire"}, http.StatusOK)
	r.must(http.MethodDelete, "/v1/subscriptions/"+r.subID+"/billing-thresholds", fireAt.Add(30*time.Minute), nil, http.StatusOK)
	if r.sub().BillingThresholds != nil {
		t.Fatal("thresholds still set after DELETE")
	}

	out := r.postEvent("/v1/usage-events", "calls", "calls-apr15", lagP0.AddDate(0, 0, 14), fireAt.Add(time.Hour), 30, http.StatusCreated)
	wantStatus(t, "sum event before the watermark, thresholds removed", out, "after_close")

	r.run(lagB.Add(2 * time.Hour))
	invs := r.invoices()
	if got := usageBilled(invs); got != 100+5 {
		r.logInvoices(invs)
		t.Errorf("usage on all invoices = %dc, want 105c: 100 calls on the fire, the 5 peak at the close; the flagged 30 nowhere", got)
	}
}

// heldRelease holds one LiteLLM call for the unknown user lag_typo, ended at
// end, then releases it into the rig's customer at at and returns the answer.
func (r *lagRig) heldRelease(end, at time.Time) map[string]any {
	r.t.Helper()
	return r.heldReleaseVia(end, end.Add(time.Minute), at)
}

// heldReleaseVia is heldRelease with the call reaching Velox at arrive.
func (r *lagRig) heldReleaseVia(end, arrive, at time.Time) map[string]any {
	r.t.Helper()
	out := r.must(http.MethodPost, "/v1/integrations/litellm/spend", arrive,
		litellmCall(fmt.Sprintf("held-%d", end.Unix()), "lag_typo", end, 10, 5), http.StatusOK)
	if count(out, "held") != 2 {
		r.t.Fatalf("hold answer = %v, want held 2", out)
	}
	return r.must(http.MethodPost, "/v1/usage-events/held/release", at, map[string]any{
		"external_customer_id": "lag_typo", "customer_id": r.custID,
	}, http.StatusOK)
}

// TestUsageAfterClose_HeldReleaseInTheGrace: rows stamped after April ended
// and released inside April's grace, before April's invoice, belong to May,
// which the subscription prices: in_open_period.
//
// Mutation check: drop the grace branch from the classification → outside.
func TestUsageAfterClose_HeldReleaseInTheGrace(t *testing.T) {
	skipShort(t)
	r, planT := newTokensRig(t)
	r.newSub("held", planT, lagP0, nil)
	out := r.heldRelease(lagB.Add(10*time.Minute), lagB.Add(20*time.Minute))
	if count(out, "released") != 2 || count(out, "in_open_period") != 2 || count(out, "outside_open_period") != 0 {
		t.Errorf("release answer = %v, want released 2, in_open_period 2", out)
	}
}

// TestUsageAfterClose_HeldReleaseInTheGrace_CancelAtPeriodEnd: the same, on a
// subscription that cancels at April's end. May is not billed: outside.
//
// Mutation check: drop the cancel-at-period-end exclusion → in_open_period.
func TestUsageAfterClose_HeldReleaseInTheGrace_CancelAtPeriodEnd(t *testing.T) {
	skipShort(t)
	r, planT := newTokensRig(t)
	r.newSub("held", planT, lagP0, nil)
	r.must(http.MethodPost, "/v1/subscriptions/"+r.subID+"/schedule-cancel", lagP0.AddDate(0, 0, 9), map[string]any{"at_period_end": true}, http.StatusOK)
	out := r.heldRelease(lagB.Add(10*time.Minute), lagB.Add(20*time.Minute))
	if count(out, "released") != 2 || count(out, "outside_open_period") != 2 || count(out, "in_open_period") != 0 {
		t.Errorf("release answer = %v, want released 2, outside_open_period 2", out)
	}
}

// TestUsageAfterClose_HeldReleaseInTheGrace_PendingPlan: the subscription is
// on a plan without the meter, with a change at April's end to plan T, which
// prices it. Rows stamped in May are priced by plan T: in_open_period.
//
// Mutation checks: classify against the current plan only → not_on_plan;
// don't load the pending plan in the release handler → not_on_plan.
func TestUsageAfterClose_HeldReleaseInTheGrace_PendingPlan(t *testing.T) {
	skipShort(t)
	r, planT := newTokensRig(t)
	r.newSub("held", r.planC, lagP0, nil)
	item := r.sub().Items[0].ID
	r.must(http.MethodPatch, "/v1/subscriptions/"+r.subID+"/items/"+item, lagP0.AddDate(0, 0, 9),
		map[string]any{"new_plan_id": planT, "immediate": false}, http.StatusOK)
	if it := r.sub().Items[0]; it.PendingPlanID != planT || it.PendingPlanEffectiveAt == nil || !it.PendingPlanEffectiveAt.Equal(lagB) {
		t.Fatalf("item pending plan = %s at %v, want plan T at %v", it.PendingPlanID, it.PendingPlanEffectiveAt, lagB)
	}
	out := r.heldRelease(lagB.Add(10*time.Minute), lagB.Add(20*time.Minute))
	if count(out, "released") != 2 || count(out, "in_open_period") != 2 || count(out, "not_on_plan") != 0 {
		t.Errorf("release answer = %v, want released 2, in_open_period 2", out)
	}
}

// TestUsageAfterClose_HeldReleaseAroundThresholdWatermark: review finding 3
// on #1350. The release classifies with the same rule as ingest, threshold
// watermark included. A threshold (no reset) fires on April 20 and bills
// April 1 to 20. Rows stamped April 15 released while that invoice is
// unpaid count as pending_threshold_invoice: April's invoice bills them only
// if the threshold invoice is voided before April closes. Once it is paid,
// rows stamped April 16 count as outside_open_period: no invoice bills them.
// Rows stamped April 25 are in_open_period.
//
// Mutation checks: classify the release with no watermark → the April 15
// and 16 rows count in_open_period; flag before an unpaid watermark → the
// April 15 rows count outside_open_period.
func TestUsageAfterClose_HeldReleaseAroundThresholdWatermark(t *testing.T) {
	skipShort(t)
	r, planT := newTokensRig(t)
	r.newSub("held", planT, lagP0, nil)
	r.postEvent("/v1/usage-events", "tokens", "tokens-apr10", lagP0.AddDate(0, 0, 9), lagP0.AddDate(0, 0, 9), 100, http.StatusCreated)
	fireAt := lagP0.AddDate(0, 0, 19)
	r.must(http.MethodPut, "/v1/subscriptions/"+r.subID+"/billing-thresholds", fireAt.Add(-time.Hour),
		map[string]any{"amount_gte": 100}, http.StatusOK)
	if n := r.scanThresholds(fireAt); n != 1 {
		t.Fatalf("threshold scan fired %d invoices, want 1", n)
	}
	var thr string
	for _, inv := range r.invoices() {
		if inv.reason == string(domain.BillingReasonThreshold) {
			thr = inv.id
		}
	}

	out := r.heldReleaseVia(lagP0.AddDate(0, 0, 14), fireAt.Add(10*time.Minute), fireAt.Add(time.Hour))
	if count(out, "released") != 2 || count(out, "pending_threshold_invoice") != 2 || count(out, "in_open_period") != 0 {
		t.Errorf("release before an unpaid watermark = %v, want released 2, pending_threshold_invoice 2", out)
	}

	r.must(http.MethodPost, "/v1/invoices/"+thr+"/record-payment", fireAt.Add(90*time.Minute), map[string]any{"note": "wire"}, http.StatusOK)
	out = r.heldReleaseVia(lagP0.AddDate(0, 0, 15), fireAt.Add(100*time.Minute), fireAt.Add(2*time.Hour))
	if count(out, "released") != 2 || count(out, "outside_open_period") != 2 || count(out, "in_open_period") != 0 {
		t.Errorf("release before a paid watermark = %v, want released 2, outside_open_period 2", out)
	}

	out = r.heldRelease(lagP0.AddDate(0, 0, 24), lagP0.AddDate(0, 0, 24).Add(time.Hour))
	if count(out, "released") != 2 || count(out, "in_open_period") != 2 {
		t.Errorf("release after the watermark = %v, want released 2, in_open_period 2", out)
	}
}

// TestUsageAfterClose_HeldReleaseUnderWrittenOffThresholdInvoice: a
// written-off threshold invoice keeps its window billed (ADR-066 amendment
// 2026-10-23), so April's invoice does not bill rows stamped inside it. It
// can still be voided, so rows released into it count as
// pending_threshold_invoice, not in_open_period. Once it is voided, April's
// invoice bills the window again: rows released into it are in_open_period.
//
// Mutation check: put 'uncollectible' back in the watermark predicate → the
// rows released after the write-off count in_open_period.
func TestUsageAfterClose_HeldReleaseUnderWrittenOffThresholdInvoice(t *testing.T) {
	skipShort(t)
	r, planT := newTokensRig(t)
	r.newSub("held", planT, lagP0, nil)
	r.postEvent("/v1/usage-events", "tokens", "tokens-apr10", lagP0.AddDate(0, 0, 9), lagP0.AddDate(0, 0, 9), 100, http.StatusCreated)
	fireAt := lagP0.AddDate(0, 0, 19)
	r.must(http.MethodPut, "/v1/subscriptions/"+r.subID+"/billing-thresholds", fireAt.Add(-time.Hour),
		map[string]any{"amount_gte": 100}, http.StatusOK)
	if n := r.scanThresholds(fireAt); n != 1 {
		t.Fatalf("threshold scan fired %d invoices, want 1", n)
	}
	var thr string
	for _, inv := range r.invoices() {
		if inv.reason == string(domain.BillingReasonThreshold) {
			thr = inv.id
		}
	}

	r.invoiceAction(thr, "mark-uncollectible", fireAt.Add(10*time.Minute))
	out := r.heldReleaseVia(lagP0.AddDate(0, 0, 14), fireAt.Add(20*time.Minute), fireAt.Add(time.Hour))
	if count(out, "released") != 2 || count(out, "pending_threshold_invoice") != 2 || count(out, "in_open_period") != 0 {
		t.Errorf("release before a written-off watermark = %v, want released 2, pending_threshold_invoice 2", out)
	}

	r.invoiceAction(thr, "void", fireAt.Add(90*time.Minute))
	out = r.heldReleaseVia(lagP0.AddDate(0, 0, 15), fireAt.Add(100*time.Minute), fireAt.Add(2*time.Hour))
	if count(out, "released") != 2 || count(out, "in_open_period") != 2 || count(out, "pending_threshold_invoice") != 0 {
		t.Errorf("release after the void = %v, want released 2, in_open_period 2", out)
	}
}

// anthropicCall is one of the LiteLLM 1.83.9 records captured from a mock
// Anthropic (internal/integrations/litellm/testdata), renamed to id, sent for
// user and ended at end. "cache-write-5m-1h" is 1,000 fresh input tokens,
// 40,000 5-minute and 60,000 1-hour cache writes and 500 output tokens;
// "cache-hit" is a LiteLLM response-cache hit.
func anthropicCall(t *testing.T, name, id, user string, end time.Time) map[string]any {
	t.Helper()
	raw, err := os.ReadFile("../integrations/litellm/testdata/litellm-1.83.9-mock-anthropic-" + name + ".json")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	p := map[string]any{}
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatalf("decode fixture: %v", err)
	}
	at := float64(end.Unix())
	p["id"], p["end_user"] = id, user
	p["startTime"], p["completionStartTime"], p["endTime"] = at-1, at-1, at
	return p
}

// TestUsageAfterClose_LiteLLMCacheWritesFlaggedPerWrittenRow: a call with
// cache writes that ended in April and arrives after April's invoice is
// written split, as four rows (input, the 5m and 1h writes, output), and
// answers after_close 4. Its replay writes nothing and answers 0. No invoice
// bills the four rows.
//
// Mutation checks: count the after-close rows of both mappings → 6 (the two
// pre-split rows on top of the four written); count every after-close row
// of the written mapping, written or deduplicated → the replay answers 4.
func TestUsageAfterClose_LiteLLMCacheWritesFlaggedPerWrittenRow(t *testing.T) {
	skipShort(t)
	r, planT := newTokensRig(t)
	r.newSub("llm", planT, lagP0, nil)
	r.must(http.MethodPost, "/v1/integrations/litellm/spend", lagP0.AddDate(0, 0, 9),
		litellmCall("call-open", r.custExt, lagP0.AddDate(0, 0, 9), 10, 5), http.StatusOK)
	r.run(lagB.Add(2 * time.Hour))
	api0 := afterCloseCount(t, "api")

	late := anthropicCall(t, "cache-write-5m-1h", "cw-late", r.custExt, lagB.Add(-2*time.Minute))
	out := r.must(http.MethodPost, "/v1/integrations/litellm/spend", lagB.Add(3*time.Hour), late, http.StatusOK)
	if count(out, "accepted") != 4 || count(out, "after_close") != 4 {
		t.Errorf("late call answer = %v, want accepted 4 and after_close 4", out)
	}
	if got := afterCloseCount(t, "api") - api0; got != 4 {
		t.Errorf("%s{origin=api} moved by %v, want 4", afterCloseMetric, got)
	}
	out = r.must(http.MethodPost, "/v1/integrations/litellm/spend", lagB.Add(4*time.Hour), late, http.StatusOK)
	if count(out, "accepted") != 0 || count(out, "deduplicated") != 4 || count(out, "after_close") != 0 {
		t.Errorf("replay answer = %v, want deduplicated 4 and after_close 0", out)
	}
	if got := afterCloseCount(t, "api") - api0; got != 4 {
		t.Errorf("%s{origin=api} moved by %v after the replay, want still 4", afterCloseMetric, got)
	}

	r.run(lagP2.Add(2 * time.Hour))
	if got := usageBilled(r.invoices()); got != 15 {
		r.logInvoices(r.invoices())
		t.Errorf("usage on all invoices = %dc, want 15c: the open call only", got)
	}
}

// TestUsageAfterClose_LiteLLMPreSplitCallCountsOldWayRows: a call whose
// :input a release before the cache-write split stored in April, at the old
// quantity (fresh input plus the writes), and whose :output it never stored.
// LiteLLM resends it after April's invoice. Velox writes it the old way: its
// :input is deduplicated and only its :output is written, with no write rows.
// The answer counts after_close 1, the :output. The :input stays billed on
// April's invoice; the :output is billed nowhere.
//
// Mutation checks: count the after-close rows of both mappings → 2; count
// every after-close row of the written mapping, written or deduplicated → 2.
func TestUsageAfterClose_LiteLLMPreSplitCallCountsOldWayRows(t *testing.T) {
	skipShort(t)
	r, planT := newTokensRig(t)
	r.newSub("llm", planT, lagP0, nil)
	end := lagB.Add(-2 * time.Minute)
	out := r.postEvent("/v1/usage-events", "tokens", "ps-1:input", end, end.Add(time.Minute), 101000, http.StatusCreated)
	wantStatus(t, "pre-split :input while April is open", out, "")
	r.run(lagB.Add(2 * time.Hour))
	api0 := afterCloseCount(t, "api")

	call := anthropicCall(t, "cache-write-5m-1h", "ps-1", r.custExt, end)
	out = r.must(http.MethodPost, "/v1/integrations/litellm/spend", lagB.Add(3*time.Hour), call, http.StatusOK)
	if count(out, "accepted") != 1 || count(out, "deduplicated") != 1 || count(out, "after_close") != 1 {
		t.Errorf("resend of a pre-split call = %v, want accepted 1, deduplicated 1 and after_close 1", out)
	}
	if got := afterCloseCount(t, "api") - api0; got != 1 {
		t.Errorf("%s{origin=api} moved by %v, want 1", afterCloseMetric, got)
	}

	r.run(lagP2.Add(2 * time.Hour))
	invs := r.invoices()
	if got := r.cycleInvoice(invs, lagP0).sum("usage"); got != 101000 {
		r.logInvoices(invs)
		t.Errorf("April usage = %dc, want 101000c: the pre-split :input", got)
	}
	if got := usageBilled(invs); got != 101000 {
		r.logInvoices(invs)
		t.Errorf("usage on all invoices = %dc, want 101000c: no write rows on top, the flagged :output nowhere", got)
	}
}

// TestUsageAfterClose_LiteLLMCacheHitCountsNothing: a LiteLLM response-cache
// hit that ended in April and arrives after April's invoice writes nothing,
// so nothing is after_close.
//
// Mutation check: bill cache hits in the mapper → accepted 2, after_close 2.
func TestUsageAfterClose_LiteLLMCacheHitCountsNothing(t *testing.T) {
	skipShort(t)
	r, planT := newTokensRig(t)
	r.newSub("llm", planT, lagP0, nil)
	r.run(lagB.Add(2 * time.Hour))
	api0 := afterCloseCount(t, "api")

	hit := anthropicCall(t, "cache-hit", "hit-late", r.custExt, lagB.Add(-2*time.Minute))
	out := r.must(http.MethodPost, "/v1/integrations/litellm/spend", lagB.Add(3*time.Hour), hit, http.StatusOK)
	if count(out, "accepted") != 0 || count(out, "cache_hits") != 1 || count(out, "after_close") != 0 {
		t.Errorf("late cache hit answer = %v, want accepted 0, cache_hits 1 and after_close 0", out)
	}
	if got := afterCloseCount(t, "api") - api0; got != 0 {
		t.Errorf("%s{origin=api} moved by %v, want 0", afterCloseMetric, got)
	}
}
