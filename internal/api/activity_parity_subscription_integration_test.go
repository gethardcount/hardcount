package api

import (
	"fmt"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/platform/activity"
	"github.com/sagarsuperuser/velox/internal/platform/clock"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

// TestActivityParity_SubscriptionOperatorScenarios is the activity line
// standard's parity test for the subscription page (ADR-128, PR 4): one
// operator story, told twice through the real router and database — for a
// customer on the real clock and for one pinned to a test clock — must
// produce the same lines in the same order. A test-clock line is the
// real-clock line plus one Recorded time; nothing else may differ.
//
// The story: a trial is extended and ended by hand; the plan changes now,
// a plan change is scheduled for renewal and then canceled; a cancellation
// is scheduled and undone; collection is paused and resumed; spend
// thresholds are set and removed; an item is added, its quantity changed,
// and removed. The plans bill in arrears, so no change makes a proration
// invoice and the story needs no payment method.
//
// Dates inside a line differ between the arms (a month after Oct 19 is not
// a month after Feb 1), so each one is checked against the instant it names
// in its own arm: the trial end the operator asked for, or the current
// period's end.
func TestActivityParity_SubscriptionOperatorScenarios(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ts := httptest.NewServer(NewServer(db, clock.Real(), nil))
	defer ts.Close()

	tenantID := testutil.CreateTestTenant(t, db, "Subscription Parity Co")
	key := createAPIKeyWithMode(t, db, tenantID, false)
	call := func(method, path string, req flowReq) flowResp {
		t.Helper()
		req.bearer = key
		return flowDo(t, ts, method, path, req)
	}

	plan := func(code, name string, cents int) string {
		return flowStr(call("POST", "/v1/plans", flowReq{body: map[string]any{
			"code": code, "name": name, "currency": "USD", "billing_interval": "monthly",
			"base_amount_cents": cents, "base_bill_timing": "in_arrears",
		}}).want(t, 201, "create plan "+name).body, "id")
	}
	starter := plan("parity-starter", "Parity Starter", 1000)
	pro := plan("parity-pro", "Parity Pro", 2000)
	basic := plan("parity-basic", "Parity Basic", 500)
	extra := plan("parity-extra", "Parity Extra", 300)

	const frozen = "2027-02-01T04:30:00Z"
	clockID := flowStr(call("POST", "/v1/test-clocks", flowReq{body: map[string]any{
		"name": "subscription parity", "frozen_time": frozen,
	}}).want(t, 201, "create test clock").body, "id")

	type arm struct {
		name    string
		clockID string
	}
	type told struct {
		lines []map[string]any
		names map[string]time.Time // the instants a line's dates may name
	}
	story := func(a arm) told {
		t.Helper()
		cust := map[string]any{"external_id": "sub-parity-" + a.name, "display_name": "Sub Parity " + a.name, "email": "billing@parity.test"}
		if a.clockID != "" {
			cust["test_clock_id"] = a.clockID
		}
		customerID := flowStr(call("POST", "/v1/customers", flowReq{body: cust}).want(t, 201, a.name+": create customer").body, "id")
		sub := call("POST", "/v1/subscriptions", flowReq{body: map[string]any{
			"code": "sub-parity-" + a.name, "display_name": "Sub Parity " + a.name, "customer_id": customerID,
			"items": []map[string]any{{"plan_id": starter, "quantity": 1}}, "billing_time": "anniversary",
			"trial_days": 14,
		}}).want(t, 201, a.name+": create subscription").body
		subID := flowStr(sub, "id")
		items, _ := sub["items"].([]any)
		if len(items) != 1 {
			t.Fatalf("%s: subscription has %d items, want 1", a.name, len(items))
		}
		itemID := flowStr(items[0].(map[string]any), "id")
		trialEndAt, err := time.Parse(time.RFC3339, flowStr(sub, "trial_end_at"))
		if err != nil {
			t.Fatalf("%s: trial_end_at %q: %v", a.name, flowStr(sub, "trial_end_at"), err)
		}
		newTrialEnd := trialEndAt.Add(7 * 24 * time.Hour).UTC()
		base := "/v1/subscriptions/" + subID

		call("POST", base+"/extend-trial", flowReq{body: map[string]any{"trial_end": newTrialEnd.Format(time.RFC3339)}}).want(t, 200, a.name+": extend trial")
		call("POST", base+"/end-trial", flowReq{}).want(t, 200, a.name+": end trial")
		call("PATCH", base+"/items/"+itemID, flowReq{body: map[string]any{"new_plan_id": pro, "immediate": true}}).want(t, 200, a.name+": change plan now")
		call("PATCH", base+"/items/"+itemID, flowReq{body: map[string]any{"new_plan_id": basic, "immediate": false}}).want(t, 200, a.name+": schedule plan change")
		call("DELETE", base+"/items/"+itemID+"/pending-change", flowReq{}).want(t, 200, a.name+": cancel scheduled plan change")
		call("POST", base+"/schedule-cancel", flowReq{body: map[string]any{"at_period_end": true}}).want(t, 200, a.name+": schedule cancel")
		call("DELETE", base+"/scheduled-cancel", flowReq{}).want(t, 200, a.name+": undo scheduled cancel")
		call("PUT", base+"/pause-collection", flowReq{body: map[string]any{"behavior": "keep_as_draft"}}).want(t, 200, a.name+": pause collection")
		call("DELETE", base+"/pause-collection", flowReq{}).want(t, 200, a.name+": resume collection")
		call("PUT", base+"/billing-thresholds", flowReq{body: map[string]any{"amount_gte": 50000}}).want(t, 200, a.name+": set spend thresholds")
		call("DELETE", base+"/billing-thresholds", flowReq{}).want(t, 200, a.name+": remove spend thresholds")
		added := call("POST", base+"/items", flowReq{body: map[string]any{"plan_id": extra, "quantity": 2}}).want(t, 201, a.name+": add item").body
		addedID := flowStr(added, "id")
		call("PATCH", base+"/items/"+addedID, flowReq{body: map[string]any{"quantity": 3}}).want(t, 200, a.name+": change quantity")
		call("DELETE", base+"/items/"+addedID, flowReq{}).want(t, 204, a.name+": remove item")

		after := call("GET", base, flowReq{}).want(t, 200, a.name+": read subscription").body
		periodEnd, err := time.Parse(time.RFC3339, flowStr(after, "current_billing_period_end"))
		if err != nil {
			t.Fatalf("%s: current_billing_period_end %q: %v", a.name, flowStr(after, "current_billing_period_end"), err)
		}
		events := flowList(call("GET", base+"/timeline", flowReq{}).want(t, 200, a.name+": timeline").body, "events")
		return told{
			lines: linesOf(t, events),
			names: map[string]time.Time{"<trial end>": newTrialEnd, "<period end>": periodEnd},
		}
	}

	realArm := story(arm{name: "real"})
	testArm := story(arm{name: "test", clockID: clockID})

	if len(realArm.lines) < 10 {
		t.Fatalf("%d rows on the real clock, want the whole story", len(realArm.lines))
	}
	for i, l := range testArm.lines {
		if l["recorded_at"] == nil {
			t.Errorf("test-clock row %d (%v) has no recorded_at", i, l["title"])
		}
		if at, _ := time.Parse(time.RFC3339Nano, fmt.Sprint(l["occurred_at"])); at.UTC().Format(time.RFC3339) != frozen {
			t.Errorf("test-clock row %d (%v) occurred at %v, want the frozen %s", i, l["title"], l["occurred_at"], frozen)
		}
	}
	for i, l := range realArm.lines {
		if l["recorded_at"] != nil {
			t.Errorf("real-clock row %d (%v) carries recorded_at %v", i, l["title"], l["recorded_at"])
		}
	}

	want := []string{
		"Subscription created | by API key",
		"Trial extended | Trial now ends <trial end> · by API key",
		"Trial ended early | Subscription is now active · by API key",
		"Plan changed | Parity Starter → Parity Pro · by API key",
		"Plan change scheduled | Parity Pro → Parity Basic at renewal on <period end> · by API key",
		"Scheduled plan change canceled | by API key",
		"Cancellation scheduled | At the end of the current period (<period end>) · by API key",
		"Scheduled cancellation cleared | by API key",
		"Collection paused | Invoices are created but not charged until collection resumes · by API key",
		"Collection resumed | by API key",
		"Spend thresholds set | $500.00 total · by API key",
		"Spend thresholds removed | by API key",
		"Item added | Parity Extra · Quantity 2 · by API key",
		"Quantity changed | Parity Extra · 2 → 3 · by API key",
		"Item removed | Parity Extra · by API key",
	}
	realSum, testSum := subscriptionSummaries(realArm.lines, realArm.names), subscriptionSummaries(testArm.lines, testArm.names)
	if !reflect.DeepEqual(realSum, testSum) {
		t.Errorf("the two clocks tell different stories\nreal: %q\ntest: %q", realSum, testSum)
	}
	if !reflect.DeepEqual(realSum, want) {
		t.Errorf("subscription lines\n got: %q\nwant: %q", realSum, want)
	}
	// Beyond the words: kinds, tones and the parts' shapes are equal too.
	if rs, tst := normalizeLines(namedTimes(realArm.lines, realArm.names)), normalizeLines(namedTimes(testArm.lines, testArm.names)); !reflect.DeepEqual(rs, tst) {
		t.Errorf("the two clocks' lines differ\nreal: %s\ntest: %s", mustJSON(rs), mustJSON(tst))
	}
}

// namedTimes replaces each time part that equals one of the arm's named
// instants (to the second) with that name, as a text part.
func namedTimes(lines []map[string]any, names map[string]time.Time) []map[string]any {
	out := make([]map[string]any, 0, len(lines))
	for _, l := range lines {
		c := map[string]any{}
		for k, v := range l {
			c[k] = v
		}
		detail, _ := l["detail"].([]any)
		segs := make([]any, 0, len(detail))
		for _, s := range detail {
			parts, _ := s.([]any)
			seg := make([]any, 0, len(parts))
			for _, p := range parts {
				pm, _ := p.(map[string]any)
				if pm["time"] != nil {
					if name := instantName(fmt.Sprint(pm["time"]), names); name != "" {
						seg = append(seg, map[string]any{"text": name})
						continue
					}
				}
				seg = append(seg, p)
			}
			segs = append(segs, seg)
		}
		c["detail"] = segs
		out = append(out, c)
	}
	return out
}

func instantName(v string, names map[string]time.Time) string {
	at, err := time.Parse(time.RFC3339Nano, v)
	if err != nil {
		return ""
	}
	for name, want := range names {
		if at.UTC().Truncate(time.Second).Equal(want.UTC().Truncate(time.Second)) {
			return name
		}
	}
	return ""
}

// subscriptionSummaries renders lines as "Title | meta · actor", with money
// formatted, links by their label and dates by the instant they name.
func subscriptionSummaries(lines []map[string]any, names map[string]time.Time) []string {
	var out []string
	for _, l := range namedTimes(lines, names) {
		var meta []string
		detail, _ := l["detail"].([]any)
		for _, s := range detail {
			parts, _ := s.([]any)
			var b strings.Builder
			for _, p := range parts {
				pm, _ := p.(map[string]any)
				switch {
				case pm["money"] != nil:
					m, _ := pm["money"].(map[string]any)
					cents, _ := m["cents"].(float64)
					cur, _ := m["currency"].(string)
					b.WriteString(activity.FormatMoney(activity.Money{Cents: int64(cents), Currency: cur}))
				case pm["link"] != nil:
					link, _ := pm["link"].(map[string]any)
					fmt.Fprint(&b, link["label"])
				case pm["time"] != nil:
					b.WriteString("<unnamed time " + fmt.Sprint(pm["time"]) + ">")
				default:
					fmt.Fprint(&b, pm["text"])
				}
			}
			meta = append(meta, b.String())
		}
		if a, ok := l["actor"].(map[string]any); ok {
			switch a["kind"] {
			case "api_key":
				meta = append(meta, "by API key")
			case "person":
				meta = append(meta, "by person")
			case "automatic":
				meta = append(meta, "Automatic")
			default:
				meta = append(meta, fmt.Sprint(a["kind"]))
			}
		}
		out = append(out, fmt.Sprint(l["title"])+" | "+strings.Join(meta, " · "))
	}
	return out
}
