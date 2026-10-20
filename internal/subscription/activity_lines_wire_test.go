package subscription

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/auth"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/errs"
)

// TestActivityLines_SubscriptionWire asserts the subscription timeline's rows
// as the dashboard receives them (ADR-128, the activity line standard's PR 4):
// a `line` on every row, keyed by its audit row, with the actor from that row;
// and description / detail / detail_timestamp / error / status filled from it.
// Each case is one line the standard words, or one fix it names: B2 (a trial
// that ran out reads "Trial ended early"), B5 (an applied scheduled plan change
// has a status the dashboard does not know), B9 (raw cents on spend
// thresholds) and B10 (a raw Go error on a failed proration).
func TestActivityLines_SubscriptionWire(t *testing.T) {
	tenantID := "t1"
	store := newMemStore()
	subID, _ := seedSubWithItem(t, store, tenantID, "cus_1", "plan_a")

	base := time.Date(2026, 10, 19, 5, 3, 0, 0, time.UTC)
	trialEnd := time.Date(2026, 11, 9, 5, 3, 0, 0, time.UTC)
	periodEnd := time.Date(2026, 11, 19, 5, 3, 0, 0, time.UTC)
	resumes := time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)
	sim := time.Date(2027, 2, 1, 4, 30, 0, 0, time.UTC)

	type row struct {
		id, action, actorType, actorName string
		meta                             map[string]any
	}
	// The audit store returns newest first; rows are listed oldest first here
	// and reversed below.
	rows := []row{
		{"aud_created", domain.AuditActionCreate, "api_key", "billing-sync", map[string]any{"customer_id": "cus_1"}},
		{"aud_extended", domain.AuditActionUpdate, "user", "alice@example.com", map[string]any{
			"action": "trial_extended", "trial_end": trialEnd.Format(time.RFC3339)}},
		{"aud_thresholds", domain.AuditActionUpdate, "api_key", "billing-sync", map[string]any{
			"action": "billing_thresholds_set", "amount_gte": float64(50000), "item_threshold_count": float64(2), "currency": "USD"}},
		{"aud_cancel_sched", domain.AuditActionUpdate, "user", "alice@example.com", map[string]any{
			"action": "cancel_scheduled", "cancel_at_period_end": true, "status": "active",
			"cancel_effective_at": periodEnd.Format(time.RFC3339)}},
		{"aud_trial_ran_out", domain.AuditActionUpdate, "system", "", map[string]any{
			"action": "trial_ended", "triggered_by": "schedule"}},
		{"aud_trial_ended_by_hand", domain.AuditActionUpdate, "user", "alice@example.com", map[string]any{
			"action": "trial_ended"}},
		{"aud_paused", domain.AuditActionUpdate, "system", "", map[string]any{
			"action": "collection_paused", "triggered_by": "dunning", "behavior": "keep_as_draft"}},
		{"aud_paused_until", domain.AuditActionUpdate, "user", "alice@example.com", map[string]any{
			"action": "collection_paused", "triggered_by": "operator", "resumes_at": resumes.Format(time.RFC3339)}},
		{"aud_resumed", domain.AuditActionUpdate, "system", "", map[string]any{
			"action": "collection_resumed", "triggered_by": "schedule"}},
		{"aud_qty", "subscription.item_updated", "user", "alice@example.com", map[string]any{
			"action": "item_quantity_changed", "plan_id": "plan_a", "old_quantity": float64(3), "quantity": float64(5)}},
		{"aud_qty_legacy", "subscription.item_updated", "user", "alice@example.com", map[string]any{
			"action": "item_quantity_changed", "quantity": float64(5)}},
		{"aud_plan_scheduled", "subscription.item_updated", "user", "alice@example.com", map[string]any{
			"action": "item_plan_changed", "immediate": false, "old_plan_id": "plan_a", "new_plan_id": "plan_b",
			"effective_at": periodEnd.Format(time.RFC3339)}},
		{"aud_plan_applied", "subscription.pending_change_applied", "system", "", map[string]any{
			"old_plan_id": "plan_a", "new_plan_id": "plan_b"}},
		{"aud_proration_failed", "subscription.proration_failed", "user", "alice@example.com", map[string]any{
			"change_type": "add", "plan_id": "plan_b", "error": "pq: deadlock detected in proration insert"}},
		{"aud_crossed", "subscription.threshold_crossed", "system", "", map[string]any{
			"invoice_id": "vlx_inv_1", "invoice_number": "VLX-000123", "amount_cents": float64(5000), "currency": "USD"}},
		{"aud_below_zero", "subscription.net_negative_usage", "system", "", map[string]any{
			"meters": []any{map[string]any{"meter_id": "mtr_1", "meter_key": "api_calls", "meter_name": "API calls", "quantity": "-120"}}}},
		{"aud_canceled", domain.AuditActionCancel, "system", "", map[string]any{
			"canceled_by": "schedule", "prorated_credit_cents": float64(1200), "currency": "USD"}},
		{"aud_trial_end_cancel", domain.AuditActionCancel, "system", "", map[string]any{
			"action": "trial_end_cancel", "triggered_by": "schedule"}},
		{"aud_item_added", domain.AuditActionUpdate, "user", "alice@example.com", map[string]any{
			"action": "item_added", "plan_id": "plan_b", "quantity": float64(3), "proration_type": "invoice",
			"proration_amount_cents": float64(1800), "currency": "USD", "proration_invoice_id": "vlx_inv_gone"}},
		{"aud_unknown", "subscription.some_future_action", "system", "", map[string]any{}},
		{"aud_on_clock", domain.AuditActionUpdate, "user", "alice@example.com", map[string]any{
			"action": "cancel_cleared", "sim_effective_at": sim.Format(time.RFC3339), "test_clock_id": "clk_1"}},
	}
	entries := make([]domain.AuditEntry, 0, len(rows))
	for i := len(rows) - 1; i >= 0; i-- {
		r := rows[i]
		entries = append(entries, domain.AuditEntry{
			ID: r.id, Action: r.action, ResourceType: "subscription", ResourceID: subID,
			ActorType: r.actorType, ActorName: r.actorName, Metadata: r.meta,
			CreatedAt: base.Add(time.Duration(i) * time.Minute),
		})
	}

	h := NewHandler(NewService(store, nil))
	h.SetAuditLogger(&auditRecorderMock{entries: entries, total: len(entries)})
	// The threshold's invoice still exists; the proration invoice does not,
	// so only the first is linked (rule 8).
	h.SetInvoiceLabeler(invoiceLabelsFake{"vlx_inv_1": "VLX-000123"})
	rr := httptest.NewRecorder()
	h.activityTimeline(rr, timelineRequest(context.WithValue(context.Background(), auth.TestTenantIDKey(), tenantID), subID))
	if rr.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rr.Code, rr.Body.String())
	}
	var resp struct {
		Events []map[string]any `json:"events"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	byID := map[string]map[string]any{}
	for _, e := range resp.Events {
		l, ok := e["line"].(map[string]any)
		if !ok {
			t.Fatalf("row %v (%v) has no line", e["event_type"], e["description"])
		}
		byID[str(l["id"])] = e
	}
	if len(byID) != len(rows) {
		t.Fatalf("%d rows with a line keyed by its audit row, want %d", len(byID), len(rows))
	}

	type want struct {
		title, status, detail, detailTS, problem string
		actor                                    string // kind/name
	}
	cases := map[string]want{
		"aud_created":    {title: "Subscription created", status: "neutral", actor: "api_key/billing-sync"},
		"aud_extended":   {title: "Trial extended", status: "neutral", detail: "Trial now ends", detailTS: trialEnd.Format(time.RFC3339), actor: "person/alice@example.com"},
		"aud_thresholds": {title: "Spend thresholds set", status: "neutral", detail: "$500.00 total · 2 item thresholds", actor: "api_key/billing-sync"},
		"aud_cancel_sched": {title: "Cancellation scheduled", status: "neutral", detail: "At the end of the current period",
			detailTS: periodEnd.Format(time.RFC3339), actor: "person/alice@example.com"},
		"aud_trial_ran_out":       {title: "Trial ended", status: "neutral", detail: "Subscription is now active", actor: "automatic/"},
		"aud_trial_ended_by_hand": {title: "Trial ended early", status: "neutral", detail: "Subscription is now active", actor: "person/alice@example.com"},
		"aud_paused":              {title: "Collection paused", status: "warning", detail: "Payment retries ran out", actor: "automatic/"},
		"aud_paused_until":        {title: "Collection paused", status: "warning", detail: "Resumes", detailTS: resumes.Format(time.RFC3339), actor: "person/alice@example.com"},
		"aud_resumed":             {title: "Collection resumed", status: "neutral", detail: "On its scheduled date", actor: "automatic/"},
		"aud_qty":                 {title: "Quantity changed", status: "neutral", detail: "plan_a · 3 → 5", actor: "person/alice@example.com"},
		"aud_qty_legacy":          {title: "Quantity changed", status: "neutral", detail: "To 5", actor: "person/alice@example.com"},
		"aud_plan_scheduled": {title: "Plan change scheduled", status: "neutral", detail: "plan_a → plan_b at renewal on",
			detailTS: periodEnd.Format(time.RFC3339), actor: "person/alice@example.com"},
		"aud_plan_applied": {title: "Scheduled plan change applied", status: "neutral", detail: "plan_a → plan_b", actor: "automatic/"},
		"aud_proration_failed": {title: "Proration not applied", status: "warning", detail: "plan_b",
			problem: "The item was added, but its prorated charge was not billed. Velox will not retry it; bill it with a one-off invoice.",
			actor:   "person/alice@example.com"},
		"aud_crossed": {title: "Spend threshold crossed", status: "neutral", detail: "Invoice VLX-000123 issued early · $50.00", actor: "automatic/"},
		"aud_below_zero": {title: "Usage below zero billed as zero", status: "warning", detail: "API calls -120",
			problem: "Usage corrections were larger than the usage they offset. If an earlier invoice billed that usage, issue a credit note on it.",
			actor:   "automatic/"},
		"aud_canceled":         {title: "Subscription canceled", status: "neutral", detail: "Scheduled cancellation · Prorated credit $12.00", actor: "automatic/"},
		"aud_trial_end_cancel": {title: "Subscription canceled", status: "neutral", detail: "At trial end", actor: "automatic/"},
		"aud_item_added":       {title: "Item added", status: "neutral", detail: "plan_b · Quantity 3 · Proration invoice $18.00", actor: "person/alice@example.com"},
		"aud_unknown":          {title: "Subscription changed", status: "neutral", actor: "automatic/"},
		"aud_on_clock":         {title: "Scheduled cancellation cleared", status: "neutral", actor: "person/alice@example.com"},
	}
	for id, w := range cases {
		e := byID[id]
		if e == nil {
			t.Errorf("%s: no row", id)
			continue
		}
		l := e["line"].(map[string]any)
		if got := str(l["title"]); got != w.title {
			t.Errorf("%s: line title %q, want %q", id, got, w.title)
		}
		if got := str(e["description"]); got != w.title {
			t.Errorf("%s: description %q, want the line's title %q", id, got, w.title)
		}
		if got := str(e["status"]); got != w.status {
			t.Errorf("%s: status %q, want %q", id, got, w.status)
		}
		if got := str(e["detail"]); got != w.detail {
			t.Errorf("%s: detail %q, want %q", id, got, w.detail)
		}
		if got := str(e["detail_timestamp"]); got != w.detailTS {
			t.Errorf("%s: detail_timestamp %q, want %q", id, got, w.detailTS)
		}
		if got := str(e["error"]); got != w.problem {
			t.Errorf("%s: error %q, want %q", id, got, w.problem)
		}
		if got := str(l["problem"]); got != w.problem {
			t.Errorf("%s: line problem %q, want %q", id, got, w.problem)
		}
		a, _ := l["actor"].(map[string]any)
		if got := str(a["kind"]) + "/" + str(a["name"]); got != w.actor {
			t.Errorf("%s: line actor %q, want %q", id, got, w.actor)
		}
	}

	// The time parts travel inside the line, for the dashboard to format.
	ext := byID["aud_extended"]["line"].(map[string]any)
	if got := mustJSONString(t, ext["detail"]); got != `[[{"text":"Trial now ends "},{"time":"`+trialEnd.Format(time.RFC3339)+`"}]]` {
		t.Errorf("trial extended detail parts: %s", got)
	}
	// The spend threshold crossing links its invoice.
	crossed := byID["aud_crossed"]["line"].(map[string]any)
	if got := mustJSONString(t, crossed["detail"]); !strings.Contains(got, `{"link":{"id":"vlx_inv_1","kind":"invoice","label":"VLX-000123"}}`) {
		t.Errorf("threshold crossing does not link its invoice: %s", got)
	}

	added := byID["aud_item_added"]["line"].(map[string]any)
	if got := mustJSONString(t, added["detail"]); strings.Contains(got, `"link"`) {
		t.Errorf("a proration invoice that no longer exists is linked: %s", got)
	}

	// B10: the raw Go error stays in the audit entry, never on the timeline.
	// B9: no amount in cents.
	body := rr.Body.String()
	for _, raw := range []string{"pq: deadlock", "¢", "api_calls"} {
		if strings.Contains(body, raw) {
			t.Errorf("the timeline response carries %q", raw)
		}
	}

	// One clock rule: a test-clock row is dated at its simulated instant and
	// carries the real one as recorded_at; a real-clock row carries none.
	onClock := byID["aud_on_clock"]["line"].(map[string]any)
	if got := str(onClock["occurred_at"]); got != sim.Format(time.RFC3339) {
		t.Errorf("test-clock row occurred_at %q, want the simulated %s", got, sim.Format(time.RFC3339))
	}
	if onClock["recorded_at"] == nil {
		t.Error("test-clock row has no recorded_at")
	}
	created := byID["aud_created"]["line"].(map[string]any)
	if got := str(created["occurred_at"]); got != base.Format(time.RFC3339) {
		t.Errorf("real-clock row occurred_at %q, want %s", got, base.Format(time.RFC3339))
	}
	if created["recorded_at"] != nil {
		t.Errorf("real-clock row carries recorded_at %v", created["recorded_at"])
	}
	for _, e := range resp.Events {
		l := e["line"].(map[string]any)
		if tone := str(l["tone"]); tone != "neutral" && tone != "success" && tone != "warning" && tone != "failure" {
			t.Errorf("row %v has tone %q", l["id"], tone)
		}
	}
}

// invoiceLabelsFake knows the numbers of the invoices that exist.
type invoiceLabelsFake map[string]string

func (f invoiceLabelsFake) InvoiceNumber(_ context.Context, _, id string) (string, error) {
	if n, ok := f[id]; ok {
		return n, nil
	}
	return "", errs.ErrNotFound
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

func mustJSONString(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
