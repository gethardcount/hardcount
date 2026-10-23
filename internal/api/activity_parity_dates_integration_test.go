package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"sort"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/payment"
	"github.com/sagarsuperuser/velox/internal/platform/clock"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

// Facts: dates (ADR-128 §5, PR 7). A line Velox writes at an instant the
// contract names is dated at that instant, on both clocks. Each scenario uses
// the playbook's "one advance, many boundaries" shape: the test clock is
// advanced to two days past the boundary, so a row dated at the advance
// instead of its named instant shows up as two days late.

// datesRig is the real router on a real database, one tenant and a monthly
// in_advance plan.
type datesRig struct {
	t        *testing.T
	db       *postgres.DB
	tenantID string
	srv      *Server
	ts       *httptest.Server
	call     func(method, path string, req flowReq) flowResp
	planID   string
}

func newDatesRig(t *testing.T, name string) *datesRig {
	t.Helper()
	db := testutil.SetupTestDB(t)
	srv := NewServer(db, clock.Real(), nil)
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	tenantID := testutil.CreateTestTenant(t, db, name)
	key := createAPIKeyWithMode(t, db, tenantID, false)
	r := &datesRig{t: t, db: db, tenantID: tenantID, srv: srv, ts: ts}
	r.call = func(method, path string, req flowReq) flowResp {
		t.Helper()
		req.bearer = key
		return flowDo(t, ts, method, path, req)
	}
	r.planID = flowStr(r.call("POST", "/v1/plans", flowReq{body: map[string]any{
		"code": "dates-pro", "name": "Dates Pro", "currency": "USD", "billing_interval": "monthly",
		"base_amount_cents": 4900, "base_bill_timing": "in_advance",
	}}).want(t, 201, "create plan").body, "id")
	return r
}

// newClock creates a test clock frozen at frozen.
func (r *datesRig) newClock(name string, frozen time.Time) string {
	r.t.Helper()
	return flowStr(r.call("POST", "/v1/test-clocks", flowReq{body: map[string]any{
		"name": name, "frozen_time": frozen.Format(time.RFC3339),
	}}).want(r.t, 201, "create test clock "+name).body, "id")
}

// advance moves the clock to target. The test server has no catchup queue,
// so the catchup runs inside the request; the clock is ready on return.
func (r *datesRig) advance(clockID string, target time.Time) {
	r.t.Helper()
	r.call("POST", "/v1/test-clocks/"+clockID+"/advance", flowReq{body: map[string]any{
		"frozen_time": target.Format(time.RFC3339),
	}}).want(r.t, 200, "advance clock")
	if status := flowStr(r.call("GET", "/v1/test-clocks/"+clockID, flowReq{}).want(r.t, 200, "read clock").body, "status"); status != "ready" {
		r.t.Fatalf("clock status after the advance = %q, want ready", status)
	}
}

// trialSub creates a customer (pinned to clockID when set) and a subscription
// with a 14-day trial on the in_advance plan. It returns the subscription id
// and its trial end.
func (r *datesRig) trialSub(key, clockID string) (string, time.Time) {
	r.t.Helper()
	cust := map[string]any{"external_id": "dates-" + key, "display_name": "Dates " + key, "email": "billing@dates.test"}
	if clockID != "" {
		cust["test_clock_id"] = clockID
	}
	customerID := flowStr(r.call("POST", "/v1/customers", flowReq{body: cust}).want(r.t, 201, key+": create customer").body, "id")
	sub := r.call("POST", "/v1/subscriptions", flowReq{body: map[string]any{
		"code": "dates-" + key, "display_name": "Dates " + key, "customer_id": customerID,
		"items": []map[string]any{{"plan_id": r.planID, "quantity": 1}}, "billing_time": "anniversary",
		"trial_days": 14,
	}}).want(r.t, 201, key+": create subscription").body
	trialEnd, err := time.Parse(time.RFC3339, flowStr(sub, "trial_end_at"))
	if err != nil {
		r.t.Fatalf("%s: trial_end_at %q: %v", key, flowStr(sub, "trial_end_at"), err)
	}
	return flowStr(sub, "id"), trialEnd.UTC()
}

// subLine returns the subscription timeline's one line titled title.
func (r *datesRig) subLine(subID, title string) map[string]any {
	r.t.Helper()
	events := flowList(r.call("GET", "/v1/subscriptions/"+subID+"/timeline", flowReq{}).want(r.t, 200, "subscription timeline").body, "events")
	var found []map[string]any
	for _, l := range linesOf(r.t, events) {
		if l["title"] == title {
			found = append(found, l)
		}
	}
	if len(found) != 1 {
		r.t.Fatalf("subscription timeline has %d %q lines, want 1: %s", len(found), title, mustJSON(linesOf(r.t, events)))
	}
	return found[0]
}

// invoices returns the subscription's invoices, oldest created first.
func (r *datesRig) invoices(subID string) []map[string]any {
	r.t.Helper()
	invs := flowList(r.call("GET", "/v1/invoices?subscription_id="+subID+"&limit=100", flowReq{}).want(r.t, 200, "list invoices").body, "data")
	sort.SliceStable(invs, func(i, j int) bool { return at(r.t, invs[i], "created_at").Before(at(r.t, invs[j], "created_at")) })
	return invs
}

// dayOne returns the subscription's one day-1 (subscription_create) invoice.
func (r *datesRig) dayOne(subID string) map[string]any {
	r.t.Helper()
	var found []map[string]any
	for _, inv := range r.invoices(subID) {
		if inv["billing_reason"] == "subscription_create" {
			found = append(found, inv)
		}
	}
	if len(found) != 1 {
		r.t.Fatalf("subscription %s has %d day-1 invoices, want 1", subID, len(found))
	}
	return found[0]
}

// invoiceLine returns the invoice timeline's one line titled title.
func (r *datesRig) invoiceLine(invoiceID, title string) map[string]any {
	r.t.Helper()
	events := flowList(r.call("GET", "/v1/invoices/"+invoiceID+"/payment-timeline", flowReq{}).want(r.t, 200, "invoice timeline").body, "events")
	var found []map[string]any
	for _, l := range linesOf(r.t, events) {
		if l["title"] == title {
			found = append(found, l)
		}
	}
	if len(found) != 1 {
		r.t.Fatalf("invoice timeline has %d %q lines, want 1: %s", len(found), title, mustJSON(linesOf(r.t, events)))
	}
	return found[0]
}

// at parses m[key] as an RFC 3339 instant, failing when it is not one.
func at(t *testing.T, m map[string]any, key string) time.Time {
	t.Helper()
	v, err := time.Parse(time.RFC3339Nano, fmt.Sprint(m[key]))
	if err != nil {
		t.Fatalf("%s = %v: %v", key, m[key], err)
	}
	return v.UTC()
}

// wantAt fails unless m[key] is want, to the second.
func wantAt(t *testing.T, what string, m map[string]any, key string, want time.Time) {
	t.Helper()
	if got := at(t, m, key); !got.Truncate(time.Second).Equal(want.Truncate(time.Second)) {
		t.Errorf("%s %s = %v, want %v (off by %v)", what, key, got, want, got.Sub(want))
	}
}

// TestActivityDates_TrialRunsOut is B12 / P-3: a trial that runs out on a test
// clock is dated at trial_end_at, not at the advance that found it, and so is
// the day-1 invoice it issues. Before the fix the catchup ran the flip at the
// clock's frozen_time: "Trial ended", the invoice's issue date, its due date
// and its "Invoice finalized" line all read two days late, and a day-1
// invoice swept by a longer advance sorted after the cycle invoice it
// preceded.
//
// Mutation checks: drop the WithSim in ProcessExpiredTrialsForClock and the
// line assertions fail; read the clock instead of startAt in
// buildOnCreateInvoice and the invoice assertions fail while the "Trial
// ended" line still passes (TestTrialExpiry_TestClock_ChargeNotBeforeIssue in
// internal/billing catches the paid-before-issued split that leaves).
func TestActivityDates_TrialRunsOut(t *testing.T) {
	r := newDatesRig(t, "Activity Dates Trial")
	frozen := time.Date(2027, 2, 1, 4, 30, 0, 0, time.UTC)

	t.Run("test clock: two days past trial end", func(t *testing.T) {
		clockID := r.newClock("trial ends", frozen)
		subID, trialEnd := r.trialSub("trial", clockID)
		r.advance(clockID, trialEnd.Add(48*time.Hour))

		wantAt(t, `"Trial ended" line`, r.subLine(subID, "Trial ended"), "occurred_at", trialEnd)
		inv := r.dayOne(subID)
		wantAt(t, "day-1 invoice", inv, "issued_at", trialEnd)
		wantAt(t, "day-1 invoice", inv, "created_at", trialEnd)
		netDays := int(flowInt(inv, "net_payment_term_days"))
		wantAt(t, "day-1 invoice", inv, "due_at", trialEnd.AddDate(0, 0, netDays))
		wantAt(t, `day-1 "Invoice finalized" line`, r.invoiceLine(flowStr(inv, "id"), "Invoice finalized"), "occurred_at", trialEnd)
		wantAt(t, `day-1 "Invoice created" line`, r.invoiceLine(flowStr(inv, "id"), "Invoice created"), "occurred_at", trialEnd)
		if inv["paid_at"] != nil && at(t, inv, "paid_at").Before(at(t, inv, "issued_at")) {
			t.Errorf("day-1 invoice paid_at %v is before its issued_at %v", inv["paid_at"], inv["issued_at"])
		}
	})

	t.Run("test clock: one advance past the trial end and the first renewal", func(t *testing.T) {
		clockID := r.newClock("trial and renewal", frozen)
		subID, trialEnd := r.trialSub("renewal", clockID)
		r.advance(clockID, trialEnd.AddDate(0, 1, 2))

		invs := r.invoices(subID)
		if len(invs) < 2 {
			t.Fatalf("%d invoices after an advance past the trial end and the first renewal, want the day-1 and a cycle invoice", len(invs))
		}
		// Oldest first: the day-1 invoice the trial end issued comes before
		// the renewal that followed it.
		if invs[0]["billing_reason"] != "subscription_create" {
			t.Errorf("first invoice by created_at is %v (created %v), want the day-1 invoice: %s",
				invs[0]["billing_reason"], invs[0]["created_at"], mustJSON(invs))
		}
		wantAt(t, "day-1 invoice", r.dayOne(subID), "issued_at", trialEnd)
		wantAt(t, `"Trial ended" line`, r.subLine(subID, "Trial ended"), "occurred_at", trialEnd)
	})

	t.Run("test clock: a cancellation due at trial end", func(t *testing.T) {
		clockID := r.newClock("trial cancel", frozen)
		subID, trialEnd := r.trialSub("cancel", clockID)
		r.call("POST", "/v1/subscriptions/"+subID+"/schedule-cancel", flowReq{body: map[string]any{"at_period_end": true}}).want(t, 200, "schedule cancel at trial end")
		r.advance(clockID, trialEnd.Add(48*time.Hour))

		wantAt(t, `"Subscription canceled" line`, r.subLine(subID, "Subscription canceled"), "occurred_at", trialEnd)
		if invs := r.invoices(subID); len(invs) != 0 {
			t.Errorf("a trial canceled at its end issued %d invoices, want none", len(invs))
		}
	})

	t.Run("real clock: one expiry tick", func(t *testing.T) {
		subID, trialEnd := r.trialSub("wall", "")
		// Move the trial, and the period that follows it, so it ended a
		// minute ago.
		shift := time.Now().UTC().Add(-time.Minute).Sub(trialEnd)
		admin := testutil.AdminPool(t)
		if _, err := admin.Exec(`UPDATE subscriptions SET
			trial_start_at = trial_start_at + $1 * interval '1 microsecond',
			trial_end_at = trial_end_at + $1 * interval '1 microsecond',
			current_billing_period_start = current_billing_period_start + $1 * interval '1 microsecond',
			current_billing_period_end = current_billing_period_end + $1 * interval '1 microsecond',
			next_billing_at = next_billing_at + $1 * interval '1 microsecond'
			WHERE id = $2`, shift.Microseconds(), subID); err != nil {
			t.Fatalf("move the trial end: %v", err)
		}
		trialEnd = trialEnd.Add(shift).Truncate(time.Microsecond)

		before := time.Now().UTC()
		if n, errs := r.srv.SubscriptionSvc.ProcessExpiredTrials(postgres.WithLivemode(context.Background(), false), 100); n != 1 || len(errs) != 0 {
			t.Fatalf("trial expiry tick: flipped=%d errs=%v, want 1 and none", n, errs)
		}
		after := time.Now().UTC()

		line := r.subLine(subID, "Trial ended")
		if got := at(t, line, "occurred_at"); got.Before(before.Add(-time.Second)) || got.After(after.Add(time.Second)) {
			t.Errorf(`"Trial ended" line at %v, want within the tick (%v – %v)`, got, before, after)
		}
		if line["recorded_at"] != nil {
			t.Errorf("real-clock line carries recorded_at %v", line["recorded_at"])
		}
		wantAt(t, "day-1 invoice", r.dayOne(subID), "issued_at", trialEnd)
	})
}

// subWithItem creates a customer (pinned to clockID when set) and an active
// subscription on planID, returning the subscription and its one item.
func (r *datesRig) subWithItem(key, clockID, planID string) (string, string) {
	r.t.Helper()
	cust := map[string]any{"external_id": "dates-" + key, "display_name": "Dates " + key, "email": "billing@dates.test"}
	if clockID != "" {
		cust["test_clock_id"] = clockID
	}
	customerID := flowStr(r.call("POST", "/v1/customers", flowReq{body: cust}).want(r.t, 201, key+": create customer").body, "id")
	sub := r.call("POST", "/v1/subscriptions", flowReq{body: map[string]any{
		"code": "dates-" + key, "display_name": "Dates " + key, "customer_id": customerID,
		"items": []map[string]any{{"plan_id": planID, "quantity": 1}}, "billing_time": "anniversary",
	}}).want(r.t, 201, key+": create subscription").body
	items, _ := sub["items"].([]any)
	if len(items) != 1 {
		r.t.Fatalf("%s: %d items, want 1", key, len(items))
	}
	return flowStr(sub, "id"), flowStr(items[0].(map[string]any), "id")
}

// finalizeRowMeta returns the metadata of the invoice's one finalize audit row.
func (r *datesRig) finalizeRowMeta(invoiceID string) map[string]any {
	r.t.Helper()
	admin := testutil.AdminPool(r.t)
	var raw []byte
	if err := admin.QueryRow(`SELECT metadata FROM audit_log WHERE resource_type = 'invoice' AND resource_id = $1 AND action = 'finalize'`, invoiceID).Scan(&raw); err != nil {
		r.t.Fatalf("finalize row of invoice %s: %v", invoiceID, err)
	}
	m := map[string]any{}
	if err := json.Unmarshal(raw, &m); err != nil {
		r.t.Fatalf("finalize row metadata %s: %v", raw, err)
	}
	return m
}

// TestActivityDates_BornFinalized is P-1: the finalize audit row of an
// invoice Velox created already finalized says so (born_finalized: true), so
// the "Invoice finalized" line can say why the invoice exists. Writers: the
// day-1 invoice, the proration invoice of an immediate upgrade, the cycle
// close and the final invoice of an immediate cancel. The spend threshold's
// row is checked in internal/billing; a draft finalized by the operator or by
// the tax retry never carries the flag (internal/invoice).
func TestActivityDates_BornFinalized(t *testing.T) {
	r := newDatesRig(t, "Activity Dates Born Finalized")
	plan := func(code string, cents int, timing string) string {
		return flowStr(r.call("POST", "/v1/plans", flowReq{body: map[string]any{
			"code": code, "name": code, "currency": "USD", "billing_interval": "monthly",
			"base_amount_cents": cents, "base_bill_timing": timing,
		}}).want(t, 201, "create plan "+code).body, "id")
	}
	bigger := plan("dates-bigger", 9900, "in_advance")
	arrears := plan("dates-arrears", 3000, "in_arrears")
	frozen := time.Date(2027, 2, 1, 4, 30, 0, 0, time.UTC)

	// Day-1 and proration, on the real clock.
	upSub, upItem := r.subWithItem("upgrade", "", r.planID)
	// An unpaid period invoice blocks a plan change: pay the day-1 invoice
	// offline first.
	r.call("POST", "/v1/invoices/"+flowStr(r.dayOne(upSub), "id")+"/record-payment", flowReq{body: map[string]any{"note": "Wire"}}).want(t, 200, "pay day-1 offline")
	r.call("PATCH", "/v1/subscriptions/"+upSub+"/items/"+upItem, flowReq{body: map[string]any{"new_plan_id": bigger, "immediate": true}}).want(t, 200, "upgrade now")

	// The cycle close, on a test clock.
	cycleClock := r.newClock("cycle close", frozen)
	cycleSub, _ := r.subWithItem("cycle", cycleClock, r.planID)
	r.advance(cycleClock, frozen.AddDate(0, 1, 2))

	// The final invoice of an immediate cancel, ten days into an arrears period.
	cancelClock := r.newClock("cancel final", frozen)
	cancelSub, _ := r.subWithItem("cancel", cancelClock, arrears)
	r.advance(cancelClock, frozen.AddDate(0, 0, 10))
	r.call("POST", "/v1/subscriptions/"+cancelSub+"/cancel", flowReq{}).want(t, 200, "cancel now")

	seen := map[string]int{}
	for _, subID := range []string{upSub, cycleSub, cancelSub} {
		for _, inv := range r.invoices(subID) {
			reason := fmt.Sprint(inv["billing_reason"])
			seen[reason]++
			if inv["status"] == "draft" {
				t.Errorf("%s invoice %s is a draft, want finalized", reason, inv["id"])
				continue
			}
			if v := r.finalizeRowMeta(flowStr(inv, "id"))["born_finalized"]; v != true {
				t.Errorf("%s invoice %s: finalize row born_finalized = %v, want true", reason, inv["id"], v)
			}
		}
	}
	for _, want := range []string{"subscription_create", "subscription_update", "subscription_cycle", "subscription_cancel"} {
		if seen[want] == 0 {
			t.Errorf("no %s invoice among %v: the scenario did not reach that writer", want, seen)
		}
	}
}

// TestActivityDates_HostedPageAttempts is P-2 (ADR-128 decision 7, reversing
// ADR-102's wall-clock rule): a charge attempt the settle path records for a
// PaymentIntent Velox did not make (the hosted payment page) is dated on the
// invoice's test clock, like any other act on a pinned invoice. Writes go
// through the production payment adapter's settle (the webhook's entry
// point, on a ctx with no clock) and the real invoice store; reads through
// the real router.
//
// Collision: a Velox-made dunning retry whose failure webhook commits before
// the charge call records its attempt. The early report stamps the invoice's
// clock (frozen_time); the retry's own write carries its slot and must win,
// because the writer that made the attempt owns its facts (the trigger_source
// rule). Mutation check: keep COALESCE(existing, new) for sim_effective_at in
// RecordChargeAttempt and the collision arm fails.
func TestActivityDates_HostedPageAttempts(t *testing.T) {
	hr := newHardDeclineRig(t, "Activity Dates Hosted")
	srv := NewServer(hr.db, clock.Real(), nil)
	ts := httptest.NewServer(srv)
	defer ts.Close()
	hr.charger.SetResolver(srv.BillingEngine)
	key := createAPIKeyWithMode(t, hr.db, hr.tenantID, false)
	call := func(method, path string, req flowReq) flowResp {
		t.Helper()
		req.bearer = key
		return flowDo(t, ts, method, path, req)
	}
	webhook := postgres.WithLivemode(context.Background(), false) // a webhook carries no clock

	frozen := time.Date(2027, 2, 1, 4, 30, 0, 0, time.UTC)
	clockID := flowStr(call("POST", "/v1/test-clocks", flowReq{body: map[string]any{
		"name": "hosted attempts", "frozen_time": frozen.Format(time.RFC3339),
	}}).want(t, 201, "create test clock").body, "id")

	newInvoice := func(key, clockID string) string {
		t.Helper()
		cust := map[string]any{"external_id": "hosted-" + key, "display_name": "Hosted " + key, "email": "billing@hosted.test"}
		if clockID != "" {
			cust["test_clock_id"] = clockID
		}
		customerID := flowStr(call("POST", "/v1/customers", flowReq{body: cust}).want(t, 201, key+": create customer").body, "id")
		id := flowStr(call("POST", "/v1/invoices", flowReq{body: map[string]any{
			"customer_id": customerID,
			"line_items":  []map[string]any{{"description": "Hosted " + key, "quantity": 1, "unit_amount_cents": 4900}},
		}}).want(t, 201, key+": create invoice").body, "id")
		call("POST", "/v1/invoices/"+id+"/finalize", flowReq{}).want(t, 200, key+": finalize")
		return id
	}
	attempt := func(invoiceID, pi string) domain.InvoiceChargeAttempt {
		t.Helper()
		atts, err := hr.invoices.ListChargeAttemptsByInvoice(hr.ctx, hr.tenantID, invoiceID)
		if err != nil {
			t.Fatalf("list attempts: %v", err)
		}
		for _, a := range atts {
			if a.StripePaymentIntentID == pi {
				return a
			}
		}
		t.Fatalf("no attempt for %s among %+v", pi, atts)
		return domain.InvoiceChargeAttempt{}
	}
	hostedFailure := func(invoiceID, pi, purpose string) {
		t.Helper()
		if err := hr.charger.SettleFailed(webhook, hr.tenantID, hr.invoice(invoiceID), payment.PaymentFailure{
			PaymentIntentID: pi, Purpose: purpose, Message: "Your card was declined.", DeclineCode: "generic_decline",
		}, payment.SourceWebhook); err != nil {
			t.Fatalf("settle failed %s: %v", pi, err)
		}
	}

	t.Run("test clock: a hosted-page payment fails", func(t *testing.T) {
		id := newInvoice("fail", clockID)
		before := time.Now().UTC()
		hostedFailure(id, "pi_hosted_fail", payment.PurposeHostedInvoicePay)
		after := time.Now().UTC()

		a := attempt(id, "pi_hosted_fail")
		if a.SimEffectiveAt == nil || !a.SimEffectiveAt.Equal(frozen) {
			t.Errorf("attempt sim_effective_at = %v, want the invoice's clock %v", a.SimEffectiveAt, frozen)
		}
		events := flowList(call("GET", "/v1/invoices/"+id+"/payment-timeline", flowReq{}).want(t, 200, "timeline").body, "events")
		lines := linesOf(t, events)
		var titles []string
		for _, l := range lines {
			titles = append(titles, fmt.Sprint(l["title"]))
		}
		if len(lines) == 0 || lines[len(lines)-1]["title"] != "Payment failed" {
			t.Fatalf("timeline %q, want the failed payment last, after the invoice's own rows", titles)
		}
		failed := lines[len(lines)-1]
		wantAt(t, `"Payment failed" line`, failed, "occurred_at", frozen)
		if failed["recorded_at"] == nil {
			t.Fatalf(`"Payment failed" line has no recorded_at`)
		}
		if rec := at(t, failed, "recorded_at"); rec.Before(before.Add(-time.Second)) || rec.After(after.Add(time.Second)) {
			t.Errorf(`"Payment failed" recorded_at %v, want the real time of the settle (%v – %v)`, rec, before, after)
		}
	})

	t.Run("test clock: a hosted-page payment succeeds", func(t *testing.T) {
		id := newInvoice("pay", clockID)
		if err := hr.charger.SettleSucceeded(webhook, hr.tenantID, hr.invoice(id), "pi_hosted_ok", 4900, payment.SourceWebhook); err != nil {
			t.Fatalf("settle succeeded: %v", err)
		}
		inv := hr.invoice(id)
		if inv.PaidAt == nil {
			t.Fatalf("invoice status %s with no paid_at, want paid", inv.Status)
		}
		a := attempt(id, "pi_hosted_ok")
		if a.SimEffectiveAt == nil || !a.SimEffectiveAt.Equal(*inv.PaidAt) {
			t.Errorf("attempt sim_effective_at = %v, want the invoice's paid_at %v", a.SimEffectiveAt, inv.PaidAt)
		}
	})

	t.Run("test clock: a dunning retry's failure webhook beats its attempt write", func(t *testing.T) {
		id := newInvoice("collide", clockID)
		slot := frozen.Add(-36 * time.Hour) // the retry's contracted instant, inside the advance
		hostedFailure(id, "pi_retry_collide", "dunning_retry")
		if err := hr.invoices.RecordChargeAttempt(hr.ctx, hr.tenantID, domain.InvoiceChargeAttempt{
			InvoiceID: id, StripePaymentIntentID: "pi_retry_collide", Trigger: domain.ChargeTriggerDunningRetry,
			Outcome: domain.ChargeAttemptFailed, AmountCents: 4900, OccurredAt: time.Now().UTC(), SimEffectiveAt: &slot,
		}); err != nil {
			t.Fatalf("record attempt: %v", err)
		}
		a := attempt(id, "pi_retry_collide")
		if a.Trigger != domain.ChargeTriggerDunningRetry {
			t.Errorf("attempt trigger = %s, want dunning_retry", a.Trigger)
		}
		if a.SimEffectiveAt == nil || !a.SimEffectiveAt.Equal(slot) {
			t.Errorf("attempt sim_effective_at = %v, want the retry's slot %v (the report that arrived first stamped the clock's frozen_time)", a.SimEffectiveAt, slot)
		}
	})

	t.Run("real clock: a hosted-page payment fails", func(t *testing.T) {
		id := newInvoice("wall", "")
		hostedFailure(id, "pi_hosted_wall", payment.PurposeHostedInvoicePay)
		if a := attempt(id, "pi_hosted_wall"); a.SimEffectiveAt != nil {
			t.Errorf("real-clock attempt sim_effective_at = %v, want none", a.SimEffectiveAt)
		}
	})
}
