package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"sort"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/payment"
	"github.com/gethardcount/hardcount/internal/platform/clock"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/testutil"
)

// Facts: dates (ADR-128 §5, PR 7). A line Hardcount writes at an instant the
// contract names is dated at that instant.

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
	return newDatesRigOn(t, name, clock.Real())
}

// newDatesRigOn is newDatesRig with the server built on clk.
func newDatesRigOn(t *testing.T, name string, clk clock.Clock) *datesRig {
	t.Helper()
	db := testutil.SetupTestDB(t)
	srv := NewServer(db, clk, nil)
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

// trialSub creates a customer and a subscription with a 14-day trial on the
// in_advance plan. It returns the subscription id and its trial end.
func (r *datesRig) trialSub(key string) (string, time.Time) {
	r.t.Helper()
	cust := map[string]any{"external_id": "dates-" + key, "display_name": "Dates " + key, "email": "billing@dates.test"}
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

// TestActivityDates_TrialRunsOut: a trial that runs out is dated at the
// expiry tick, and its day-1 invoice is issued at trial_end_at.
func TestActivityDates_TrialRunsOut(t *testing.T) {
	r := newDatesRig(t, "Activity Dates Trial")

	t.Run("real clock: one expiry tick", func(t *testing.T) {
		subID, trialEnd := r.trialSub("wall")
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

// subWithItem creates a customer and an active subscription on planID,
// returning the subscription and its one item.
func (r *datesRig) subWithItem(key, planID string) (string, string) {
	r.t.Helper()
	cust := map[string]any{"external_id": "dates-" + key, "display_name": "Dates " + key, "email": "billing@dates.test"}
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
// invoice Hardcount created already finalized says so (born_finalized: true), so
// the "Invoice finalized" line can say why the invoice exists. Writers: the
// day-1 invoice, the proration invoice of an immediate upgrade, the cycle
// close and the final invoice of an immediate cancel. The spend threshold's
// row is checked in internal/billing; a draft finalized by the operator or by
// the tax retry never carries the flag (internal/invoice).
func TestActivityDates_BornFinalized(t *testing.T) {
	frozen := time.Date(2027, 2, 1, 4, 30, 0, 0, time.UTC)
	wall := clock.NewFake(frozen)
	r := newDatesRigOn(t, "Activity Dates Born Finalized", wall)
	plan := func(code string, cents int, timing string) string {
		return flowStr(r.call("POST", "/v1/plans", flowReq{body: map[string]any{
			"code": code, "name": code, "currency": "USD", "billing_interval": "monthly",
			"base_amount_cents": cents, "base_bill_timing": timing,
		}}).want(t, 201, "create plan "+code).body, "id")
	}
	bigger := plan("dates-bigger", 9900, "in_advance")
	arrears := plan("dates-arrears", 3000, "in_arrears")

	// Day-1 and proration.
	upSub, upItem := r.subWithItem("upgrade", r.planID)
	// An unpaid period invoice blocks a plan change: pay the day-1 invoice
	// offline first.
	r.call("POST", "/v1/invoices/"+flowStr(r.dayOne(upSub), "id")+"/record-payment", flowReq{body: map[string]any{"note": "Wire"}}).want(t, 200, "pay day-1 offline")
	r.call("PATCH", "/v1/subscriptions/"+upSub+"/items/"+upItem, flowReq{body: map[string]any{"new_plan_id": bigger, "immediate": true}}).want(t, 200, "upgrade now")

	// The final invoice of an immediate cancel, ten days into an arrears
	// period, then the cycle close.
	cycleSub, _ := r.subWithItem("cycle", r.planID)
	cancelSub, _ := r.subWithItem("cancel", arrears)
	wall.Set(frozen.AddDate(0, 0, 10))
	r.call("POST", "/v1/subscriptions/"+cancelSub+"/cancel", flowReq{}).want(t, 200, "cancel now")
	tickAt(t, r.srv, wall, r.tenantID, frozen.AddDate(0, 1, 2))

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

// TestActivityDates_HostedPageAttempts is P-2 (ADR-128 decision 7): a charge
// attempt the settle path records for a PaymentIntent Hardcount did not make
// (the hosted payment page) is dated at the invoice's time, like any other act
// on the invoice. Writes go through the production payment adapter's settle
// (the webhook's entry point, on a ctx with no clock) and the real invoice
// store.
//
// Collision: a Hardcount-made dunning retry whose failure webhook commits before
// the charge call records its attempt. The early report stamps the time it was
// settled at; the retry's own write carries its slot and must win,
// because the writer that made the attempt owns its facts (the trigger_source
// rule). Mutation check: keep the existing effective_at in RecordChargeAttempt
// and the collision arm fails.
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
	// A webhook's time, decided from the invoice it names, as its handler
	// decides it.
	webhook := func(t *testing.T, invoiceID string) context.Context {
		return testutil.DecidedFor(t, srv.BillingEngine, clock.Pin{TenantID: hr.tenantID, InvoiceID: invoiceID})
	}

	newInvoice := func(key string) string {
		t.Helper()
		cust := map[string]any{"external_id": "hosted-" + key, "display_name": "Hosted " + key, "email": "billing@hosted.test"}
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
		if err := hr.charger.SettleFailed(webhook(t, invoiceID), hr.tenantID, hr.invoice(invoiceID), payment.PaymentFailure{
			PaymentIntentID: pi, Purpose: purpose, Message: "Your card was declined.", DeclineCode: "generic_decline",
		}, payment.SourceWebhook); err != nil {
			t.Fatalf("settle failed %s: %v", pi, err)
		}
	}

	t.Run("a hosted-page payment succeeds", func(t *testing.T) {
		id := newInvoice("pay")
		if err := hr.charger.SettleSucceeded(webhook(t, id), hr.tenantID, hr.invoice(id), "pi_hosted_ok", 4900, payment.SourceWebhook); err != nil {
			t.Fatalf("settle succeeded: %v", err)
		}
		inv := hr.invoice(id)
		if inv.PaidAt == nil {
			t.Fatalf("invoice status %s with no paid_at, want paid", inv.Status)
		}
		a := attempt(id, "pi_hosted_ok")
		if !a.EffectiveAt.Equal(*inv.PaidAt) {
			t.Errorf("attempt effective_at = %v, want the invoice's paid_at %v", a.EffectiveAt, inv.PaidAt)
		}
	})

	t.Run("a dunning retry's failure webhook beats its attempt write", func(t *testing.T) {
		id := newInvoice("collide")
		slot := time.Now().UTC().Add(-36 * time.Hour).Truncate(time.Microsecond) // the retry's contracted instant
		hostedFailure(id, "pi_retry_collide", "dunning_retry")
		if err := hr.invoices.RecordChargeAttempt(clock.WithEffectiveNow(hr.ctx, slot), hr.tenantID, domain.InvoiceChargeAttempt{
			InvoiceID: id, StripePaymentIntentID: "pi_retry_collide", Trigger: domain.ChargeTriggerDunningRetry,
			Outcome: domain.ChargeAttemptFailed, AmountCents: 4900, OccurredAt: time.Now().UTC(),
		}); err != nil {
			t.Fatalf("record attempt: %v", err)
		}
		a := attempt(id, "pi_retry_collide")
		if a.Trigger != domain.ChargeTriggerDunningRetry {
			t.Errorf("attempt trigger = %s, want dunning_retry", a.Trigger)
		}
		if !a.EffectiveAt.Equal(slot) {
			t.Errorf("attempt effective_at = %v, want the retry's slot %v (the report that arrived first stamped its own time)", a.EffectiveAt, slot)
		}
	})
}
