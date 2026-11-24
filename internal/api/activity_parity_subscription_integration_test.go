package api

import (
	"fmt"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/platform/activity"
	"github.com/gethardcount/hardcount/internal/platform/clock"
	"github.com/gethardcount/hardcount/internal/testutil"
)

// TestActivityParity_SubscriptionOperatorScenarios tells one operator story
// through the real router and database and checks the subscription page's
// lines (ADR-128, PR 4): the standard's words, in the order they happened,
// with no Recorded time.
//
// The story: a trial is extended and ended by hand; the plan changes now,
// a plan change is scheduled for renewal and then canceled; a cancellation
// is scheduled and undone; collection is paused and resumed; spend
// thresholds are set and removed; an item is added, its quantity changed,
// and removed. The plans bill in arrears, so no change makes a proration
// invoice and the story needs no payment method.
//
// Each date inside a line is checked against the instant it names: the
// trial end the operator asked for, or the current period's end.
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

	customerID := flowStr(call("POST", "/v1/customers", flowReq{body: map[string]any{
		"external_id": "sub-parity-real", "display_name": "Sub Parity real", "email": "billing@parity.test",
	}}).want(t, 201, "create customer").body, "id")
	sub := call("POST", "/v1/subscriptions", flowReq{body: map[string]any{
		"code": "sub-parity-real", "display_name": "Sub Parity real", "customer_id": customerID,
		"items": []map[string]any{{"plan_id": starter, "quantity": 1}}, "billing_time": "anniversary",
		"trial_days": 14,
	}}).want(t, 201, "create subscription").body
	subID := flowStr(sub, "id")
	items, _ := sub["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("subscription has %d items, want 1", len(items))
	}
	itemID := flowStr(items[0].(map[string]any), "id")
	trialEndAt, err := time.Parse(time.RFC3339, flowStr(sub, "trial_end_at"))
	if err != nil {
		t.Fatalf("trial_end_at %q: %v", flowStr(sub, "trial_end_at"), err)
	}
	newTrialEnd := trialEndAt.Add(7 * 24 * time.Hour).UTC()
	base := "/v1/subscriptions/" + subID

	call("POST", base+"/extend-trial", flowReq{body: map[string]any{"trial_end": newTrialEnd.Format(time.RFC3339)}}).want(t, 200, "extend trial")
	call("POST", base+"/end-trial", flowReq{}).want(t, 200, "end trial")
	call("PATCH", base+"/items/"+itemID, flowReq{body: map[string]any{"new_plan_id": pro, "immediate": true}}).want(t, 200, "change plan now")
	call("PATCH", base+"/items/"+itemID, flowReq{body: map[string]any{"new_plan_id": basic, "immediate": false}}).want(t, 200, "schedule plan change")
	call("DELETE", base+"/items/"+itemID+"/pending-change", flowReq{}).want(t, 200, "cancel scheduled plan change")
	call("POST", base+"/schedule-cancel", flowReq{body: map[string]any{"at_period_end": true}}).want(t, 200, "schedule cancel")
	call("DELETE", base+"/scheduled-cancel", flowReq{}).want(t, 200, "undo scheduled cancel")
	call("PUT", base+"/pause-collection", flowReq{body: map[string]any{"behavior": "keep_as_draft"}}).want(t, 200, "pause collection")
	call("DELETE", base+"/pause-collection", flowReq{}).want(t, 200, "resume collection")
	call("PUT", base+"/billing-thresholds", flowReq{body: map[string]any{"amount_gte": 50000}}).want(t, 200, "set spend thresholds")
	call("DELETE", base+"/billing-thresholds", flowReq{}).want(t, 200, "remove spend thresholds")
	added := call("POST", base+"/items", flowReq{body: map[string]any{"plan_id": extra, "quantity": 2}}).want(t, 201, "add item").body
	addedID := flowStr(added, "id")
	call("PATCH", base+"/items/"+addedID, flowReq{body: map[string]any{"quantity": 3}}).want(t, 200, "change quantity")
	call("DELETE", base+"/items/"+addedID, flowReq{}).want(t, 204, "remove item")

	after := call("GET", base, flowReq{}).want(t, 200, "read subscription").body
	periodEnd, err := time.Parse(time.RFC3339, flowStr(after, "current_billing_period_end"))
	if err != nil {
		t.Fatalf("current_billing_period_end %q: %v", flowStr(after, "current_billing_period_end"), err)
	}
	lines := linesOf(t, flowList(call("GET", base+"/timeline", flowReq{}).want(t, 200, "timeline").body, "events"))
	names := map[string]time.Time{"<trial end>": newTrialEnd, "<period end>": periodEnd}

	if len(lines) < 10 {
		t.Fatalf("%d rows, want the whole story", len(lines))
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
		"Spend thresholds set | $500.00 usage total · by API key",
		"Spend thresholds removed | by API key",
		"Item added | Parity Extra · Quantity 2 · by API key",
		"Quantity changed | Parity Extra · 2 → 3 · by API key",
		"Item removed | Parity Extra · by API key",
	}
	if got := subscriptionSummaries(lines, names); !reflect.DeepEqual(got, want) {
		t.Errorf("subscription lines\n got: %q\nwant: %q", got, want)
	}
}

// namedTimes replaces each time part that equals one of the named
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
