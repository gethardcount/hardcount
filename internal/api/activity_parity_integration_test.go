package api

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/platform/clock"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

// TestActivityParity_InvoiceOperatorScenarios is the activity line
// standard's parity test for the invoice page (ADR-128): one operator story,
// told twice through the real router and database — for a customer on the
// real clock and for one pinned to a test clock — must produce the same
// lines, in the same order. A test-clock line is the real-clock line plus
// one Recorded time; nothing else may differ.
//
// The story: a one-off invoice is finalized, emailed, reduced by a credit
// note and paid offline; a second one is finalized and voided. Each step is
// the operator's own act, so on the test clock every row is dated at the
// clock's frozen instant and ordered by its real time.
func TestActivityParity_InvoiceOperatorScenarios(t *testing.T) {
	db := testutil.SetupTestDB(t)
	// A configured server queues the invoice email; no dispatcher runs in
	// the test, so both arms read "queued".
	t.Setenv("SMTP_HOST", "smtp.parity-e2e.invalid")
	ts := httptest.NewServer(NewServer(db, clock.Real(), nil))
	defer ts.Close()

	tenantID := testutil.CreateTestTenant(t, db, "Activity Parity Co")
	key := createAPIKeyWithMode(t, db, tenantID, false)
	call := func(method, path string, req flowReq) flowResp {
		t.Helper()
		req.bearer = key
		return flowDo(t, ts, method, path, req)
	}

	const frozen = "2027-02-01T04:30:00Z"
	clockID := flowStr(call("POST", "/v1/test-clocks", flowReq{body: map[string]any{
		"name": "activity parity", "frozen_time": frozen,
	}}).want(t, 201, "create test clock").body, "id")

	type arm struct {
		name    string
		clockID string
	}
	story := func(a arm) (paid, voided []map[string]any) {
		t.Helper()
		cust := map[string]any{
			"external_id": "parity-" + a.name, "display_name": "Parity " + a.name, "email": "billing@parity.test",
		}
		if a.clockID != "" {
			cust["test_clock_id"] = a.clockID
		}
		customerID := flowStr(call("POST", "/v1/customers", flowReq{body: cust}).want(t, 201, a.name+": create customer").body, "id")
		newInvoice := func(what string) string {
			id := flowStr(call("POST", "/v1/invoices", flowReq{body: map[string]any{
				"customer_id": customerID,
				"line_items":  []map[string]any{{"description": "Parity " + what, "quantity": 1, "unit_amount_cents": 4900}},
			}}).want(t, 201, a.name+": create "+what).body, "id")
			call("POST", "/v1/invoices/"+id+"/finalize", flowReq{}).want(t, 200, a.name+": finalize "+what)
			return id
		}
		timeline := func(id string) []map[string]any {
			body := call("GET", "/v1/invoices/"+id+"/payment-timeline", flowReq{}).want(t, 200, a.name+": timeline").body
			if d, _ := body["degraded"].([]any); len(d) != 0 {
				t.Errorf("%s: degraded lanes %v", a.name, d)
			}
			return flowList(body, "events")
		}

		paidID := newInvoice("paid invoice")
		call("POST", "/v1/invoices/"+paidID+"/send", flowReq{body: map[string]any{"email": "billing@parity.test"}}).want(t, 200, a.name+": email invoice")
		call("POST", "/v1/credit-notes", flowReq{body: map[string]any{
			"invoice_id": paidID, "reason": "Billing error", "auto_issue": true,
			"lines": []map[string]any{{"description": "Billing error", "quantity": 1, "unit_amount_cents": 1000}},
		}}).want(t, 201, a.name+": credit note")
		call("POST", "/v1/invoices/"+paidID+"/record-payment", flowReq{body: map[string]any{"note": "Wire 1042"}}).want(t, 200, a.name+": record payment")

		voidID := newInvoice("voided invoice")
		call("POST", "/v1/invoices/"+voidID+"/void", flowReq{}).want(t, 200, a.name+": void")
		return timeline(paidID), timeline(voidID)
	}

	realPaid, realVoid := story(arm{name: "real"})
	testPaid, testVoid := story(arm{name: "test", clockID: clockID})

	for _, c := range []struct {
		name       string
		real, test []map[string]any
	}{{"paid invoice", realPaid, testPaid}, {"voided invoice", realVoid, testVoid}} {
		realLines, testLines := linesOf(t, c.real), linesOf(t, c.test)
		if len(realLines) < 3 {
			t.Fatalf("%s: %d rows on the real clock, want the whole story", c.name, len(realLines))
		}
		for i, l := range testLines {
			if l["recorded_at"] == nil {
				t.Errorf("%s: test-clock row %d (%v) has no recorded_at", c.name, i, l["title"])
			}
		}
		for i, l := range realLines {
			if l["recorded_at"] != nil {
				t.Errorf("%s: real-clock row %d (%v) carries recorded_at %v", c.name, i, l["title"], l["recorded_at"])
			}
		}
		// The test-clock arm is dated on the clock's frozen calendar.
		for i, l := range testLines {
			if at, _ := time.Parse(time.RFC3339Nano, fmt.Sprint(l["occurred_at"])); at.UTC().Format(time.RFC3339) != frozen {
				t.Errorf("%s: test-clock row %d (%v) occurred at %v, want the frozen %s", c.name, i, l["title"], l["occurred_at"], frozen)
			}
		}
		rs, tst := normalizeLines(realLines), normalizeLines(testLines)
		if !reflect.DeepEqual(rs, tst) {
			t.Errorf("%s: the two clocks tell different stories\nreal: %s\ntest: %s", c.name, mustJSON(rs), mustJSON(tst))
		}
	}

	// The story reads as the standard words it, in the order it happened.
	for _, c := range []struct {
		name   string
		events []map[string]any
		want   []string
	}{
		{"paid invoice", realPaid, []string{
			"Invoice created | by API key",
			"Invoice finalized · 4900 | by API key",
			"Invoice email queued | To billing@parity.test",
			"Credit note issued · 1000 | <number> · Billing error · by API key",
			"Offline payment received · 3900 | Wire 1042 · by API key",
		}},
		{"voided invoice", realVoid, []string{
			"Invoice created | by API key",
			"Invoice finalized · 4900 | by API key",
			"Invoice voided · 4900 | by API key",
		}},
	} {
		if got := summaries(normalizeLines(linesOf(t, c.events))); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s lines\n got: %q\nwant: %q", c.name, got, c.want)
		}
	}
}

// linesOf returns each row's `line`, failing on a row without one.
func linesOf(t *testing.T, events []map[string]any) []map[string]any {
	t.Helper()
	out := make([]map[string]any, 0, len(events))
	for _, e := range events {
		l, ok := e["line"].(map[string]any)
		if !ok {
			t.Fatalf("row %v has no line", e["id"])
		}
		out = append(out, l)
	}
	return out
}

var docNumber = regexp.MustCompile(`\b[A-Z]{2,}-\d{4,}\b`)

// normalizeLines strips what legitimately differs between the two arms: row
// ids, document numbers and object ids, the times on the row, and the API
// key's own name. Dates inside a part become their offset from the row's
// own time.
func normalizeLines(lines []map[string]any) []map[string]any {
	out := make([]map[string]any, 0, len(lines))
	for _, l := range lines {
		occurred, _ := time.Parse(time.RFC3339Nano, fmt.Sprint(l["occurred_at"]))
		n := map[string]any{"kind": l["kind"], "title": l["title"], "tone": l["tone"]}
		if l["amount"] != nil {
			n["amount"] = l["amount"]
		}
		if l["problem"] != nil {
			n["problem"] = docNumber.ReplaceAllString(fmt.Sprint(l["problem"]), "<number>")
		}
		if a, ok := l["actor"].(map[string]any); ok {
			n["actor"] = a["kind"]
		}
		var segs [][]string
		detail, _ := l["detail"].([]any)
		for _, s := range detail {
			parts, _ := s.([]any)
			var seg []string
			for _, p := range parts {
				pm, _ := p.(map[string]any)
				switch {
				case pm["link"] != nil:
					link, _ := pm["link"].(map[string]any)
					seg = append(seg, fmt.Sprintf("link:%v:<number>", link["kind"]))
				case pm["money"] != nil:
					seg = append(seg, fmt.Sprintf("money:%v", pm["money"]))
				case pm["time"] != nil:
					at, _ := time.Parse(time.RFC3339Nano, fmt.Sprint(pm["time"]))
					seg = append(seg, fmt.Sprintf("time:%s", at.Sub(occurred).Round(time.Minute)))
				default:
					seg = append(seg, docNumber.ReplaceAllString(fmt.Sprint(pm["text"]), "<number>"))
				}
			}
			segs = append(segs, seg)
		}
		n["detail"] = segs
		out = append(out, n)
	}
	return out
}

// summaries renders normalized lines as "Title · amount | meta" for a
// readable assertion.
func summaries(lines []map[string]any) []string {
	var out []string
	for _, l := range lines {
		s := fmt.Sprint(l["title"])
		if a, ok := l["amount"].(map[string]any); ok {
			s += fmt.Sprintf(" · %v", a["cents"])
		}
		var meta []string
		for _, seg := range l["detail"].([][]string) {
			text := strings.Join(seg, "")
			if strings.HasPrefix(text, "link:") {
				text = "<number>"
			}
			meta = append(meta, text)
		}
		switch l["actor"] {
		case "api_key":
			meta = append(meta, "by API key")
		case "automatic":
			meta = append(meta, "Automatic")
		case nil:
		default:
			meta = append(meta, fmt.Sprint(l["actor"]))
		}
		out = append(out, s+" | "+strings.Join(meta, " · "))
	}
	return out
}

func mustJSON(v any) string {
	b, _ := json.MarshalIndent(v, "", "  ")
	return string(b)
}
